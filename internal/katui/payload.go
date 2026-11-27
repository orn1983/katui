package katui

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

var modes = []string{"raw", "pretty", "tree", "csv-table", "csv-pivot", "proto-pretty", "proto-tree"}
var modeLabels = []string{"Raw", "JSON formatted text", "JSON tree", "CSV table", "CSV pivot", "Protobuf JSON", "Protobuf tree"}

const (
	defaultRenderedOutput  = 32 << 20
	defaultStructuredInput = 8 << 20
	maxPreviewOutput       = 8 << 20
	maxPreviewInput        = 2 << 20
	maxFullCSVRows         = 50_000
	maxFullCSVColumns      = 256
)

func validMode(mode string) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}
func textMode(mode string) string {
	switch mode {
	case "tree":
		return "pretty"
	case "proto-tree":
		return "proto-pretty"
	}
	return mode
}
func csvMode(mode string) bool   { return strings.HasPrefix(mode, "csv-") }
func protoMode(mode string) bool { return strings.HasPrefix(mode, "proto-") }
func treeMode(mode string) bool  { return mode == "tree" || mode == "proto-tree" }

type payloadResult struct {
	text, warning         string
	incomplete, canExpand bool
	tree                  *payloadNode
}
type payloadNode struct {
	label, search string
	value         any
	parent        *payloadNode
	children      []*payloadNode
	lazyChildren  func() []*payloadNode
	widget        *tview.TreeNode
	widgetDepth   int
}

func prettyValue(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return "?"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

var errRenderLimit = errors.New("formatted JSON exceeds the display limit")

func prettyJSON(ctx context.Context, value []byte, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The standard validator is fast on ordinary documents; its scanner also
	// rejects very deep valid JSON, which the token walker can validate.
	if !json.Valid(value) {
		if err := walkJSONTokens(ctx, value, nil); err != nil {
			return "", err
		}
	}

	var out strings.Builder
	var padding []byte
	depth := 0
	newline := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		available := limit - out.Len()
		if available < 1 || depth > (available-1)/2 {
			return errRenderLimit
		}
		for len(padding)/2 < depth {
			padding = append(padding, ' ', ' ')
		}
		out.WriteByte('\n')
		out.Write(padding[:2*depth])
		return nil
	}
	// Keep scalar spellings, duplicate keys, order and trailing whitespace.
	trailing := len(bytes.TrimRight(value, " \t\r\n"))
	quoted, escaped, afterOpen := false, false, false
	for i, ch := range value[:trailing] {
		if i%32768 == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !quoted {
			indent := afterOpen
			switch ch {
			case ' ', '\t', '\r', '\n':
				continue
			case '}', ']':
				depth--
				indent = !afterOpen // Keep empty containers on one line.
			}
			if indent {
				if err := newline(); err != nil {
					return "", err
				}
			}
		}
		if out.Len() >= limit {
			return "", errRenderLimit
		}
		out.WriteByte(ch)
		if quoted {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				quoted = false
			}
			continue
		}
		afterOpen = false
		switch ch {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			afterOpen = true
		case ',':
			if err := newline(); err != nil {
				return "", err
			}
		case ':':
			if out.Len() >= limit {
				return "", errRenderLimit
			}
			out.WriteByte(' ')
		}
	}
	if len(value)-trailing > limit-out.Len() {
		return "", errRenderLimit
	}
	out.Write(value[trailing:])
	return out.String(), ctx.Err()
}
func shortened(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := range s {
		if n == 0 {
			return s[:i] + "…"
		}
		n--
	}
	return s
}

// objectRange shares the sorted keys and backing map of a wide JSON object.
type objectRange struct {
	object map[string]any
	keys   []string
}

func newTreeNode(label string, value any, parent *payloadNode) *payloadNode {
	n := &payloadNode{label: shortened(label, 240), value: value, parent: parent, search: label}
	switch v := value.(type) {
	case map[string]any:
		n.label += fmt.Sprintf(" {%d}", len(v))
	case []any:
		n.label += fmt.Sprintf(" [%d]", len(v))
	case string:
		n.search += " " + v
		n.label += " = " + prettyValue(shortened(v, 240))
	default:
		text := prettyValue(v)
		n.search += " " + text
		n.label += " = " + text
	}
	return n
}

func (n *payloadNode) childNodes() []*payloadNode {
	if n.lazyChildren != nil {
		return n.lazyChildren()
	}
	return n.children
}

// Browsing and background search share node identities so a search result can
// be expanded and selected even if its branch has never been opened.
func fullTreeNode(label string, value any, parent *payloadNode) *payloadNode {
	n := newTreeNode(label, value, parent)
	switch v := value.(type) {
	case []any:
		n.lazyChildren = sync.OnceValue(func() []*payloadNode { return n.arrayChildren(v, 0) })
	case map[string]any:
		n.lazyChildren = sync.OnceValue(func() []*payloadNode { return n.objectChildren(v, sortedKeys(v)) })
	}
	return n
}

func (n *payloadNode) arrayChildren(values []any, offset int) []*payloadNode {
	var children []*payloadNode
	step := rangeSize(len(values))
	for i := 0; i < len(values); i += step {
		first := offset + i
		if step == 1 {
			children = append(children, fullTreeNode(fmt.Sprintf("[%d]", first), values[i], n))
			continue
		}
		items := values[i:min(i+step, len(values))]
		group := &payloadNode{label: fmt.Sprintf("[%d … %d]", first, first+len(items)-1), parent: n, value: items}
		group.lazyChildren = sync.OnceValue(func() []*payloadNode { return group.arrayChildren(items, first) })
		children = append(children, group)
	}
	return children
}

func (n *payloadNode) objectChildren(object map[string]any, keys []string) []*payloadNode {
	var children []*payloadNode
	step := rangeSize(len(keys))
	for i := 0; i < len(keys); i += step {
		if step == 1 {
			key := keys[i]
			children = append(children, fullTreeNode(key, object[key], n))
			continue
		}
		names := keys[i:min(i+step, len(keys))]
		group := &payloadNode{label: names[0] + " … " + names[len(names)-1], parent: n, value: objectRange{object, names}}
		group.lazyChildren = sync.OnceValue(func() []*payloadNode { return group.objectChildren(object, names) })
		children = append(children, group)
	}
	return children
}

func buildTree(ctx context.Context, label string, value any, full bool) (*payloadNode, bool) {
	if full {
		root := fullTreeNode(label, value, nil)
		root.childNodes() // Sort a wide root object on the render worker.
		return root, false
	}
	const maxNodes, maxDepth = 2000, 30
	count := 0
	limited := false
	var build func(string, any, *payloadNode, int) *payloadNode
	build = func(label string, value any, parent *payloadNode, depth int) *payloadNode {
		n := newTreeNode(label, value, parent)
		count++
		canAdd := func() bool {
			if depth >= maxDepth || count >= maxNodes || ctx.Err() != nil {
				limited = true
				return false
			}
			return true
		}
		switch v := value.(type) {
		case []any:
			for i, item := range v {
				if !canAdd() {
					break
				}
				n.children = append(n.children, build(fmt.Sprintf("[%d]", i), item, n, depth+1))
			}
		case map[string]any:
			if len(v) > 0 && canAdd() {
				for _, key := range sortedKeys(v) {
					if !canAdd() {
						break
					}
					n.children = append(n.children, build(key, v[key], n, depth+1))
				}
			}
		}
		return n
	}
	return build(label, value, nil, 0), limited
}

func rangeSize(n int) int {
	step := 1
	for n/step > 100 || n/step == 100 && n%step > 0 {
		step *= 100
	}
	return step
}

func delimiter(option string, value string) (rune, error) {
	switch option {
	case "", "Auto":
		sample := value
		runes := 0
		for i := range value {
			if runes == 8192 {
				sample = value[:i]
				break
			}
			runes++
		}
		best, maxScore := ',', 0
		for _, r := range []rune{',', ';', '\t', '|'} {
			reader := csv.NewReader(strings.NewReader(sample))
			reader.Comma = r
			reader.FieldsPerRecord = -1
			counts := map[int]int{}
			for i := 0; i < 25; i++ {
				row, err := reader.Read()
				if err != nil {
					break
				}
				if len(row) > 1 {
					counts[len(row)]++
					if n := counts[len(row)]*100 + min(len(row), 99); n > maxScore {
						maxScore = n
						best = r
					}
				}
			}
		}
		return best, nil
	case "\\t", "Tab":
		return '\t', nil
	}
	r, n := utf8.DecodeRuneInString(option)
	if r == utf8.RuneError || n != len(option) || !unicode.IsPrint(r) || r == '"' {
		return 0, errors.New("delimiter must be one printable CSV separator or \\t")
	}
	return r, nil
}

type csvData struct {
	names     []string
	rows      [][]string
	source    string
	truncated bool
	limited   bool
}

func parseCSV(ctx context.Context, raw string, p profile, full bool, rowLimit, cellBudget int) (csvData, []string, error) {
	sep, err := delimiter(p.delimiter, raw)
	if err != nil {
		return csvData{}, nil, err
	}
	reader := csv.NewReader(strings.NewReader(raw))
	reader.Comma = sep
	reader.FieldsPerRecord = -1
	var data csvData
	var names []string
	n := len(p.Columns)
	columnCap, cellCap, budgetCap := false, false, false
	maxColumns := 64
	if full {
		maxColumns = maxFullCSVColumns
	}
	capped := func(row []string) []string {
		if len(row) > maxColumns {
			row = row[:maxColumns]
			columnCap = true
		}
		for i, value := range row {
			out := renderText{limit: cellBudget}
			out.writeSafeMode(ctx, value, false)
			value = out.String()
			budgetCap = budgetCap || out.limited
			cellBudget -= len(value)
			if !full {
				shorter := shortened(value, 120)
				cellCap = cellCap || shorter != value
				value = shorter
			}
			row[i] = value
		}
		n = max(n, len(row))
		return row
	}
	if p.header {
		header, err := reader.Read()
		if err != nil && err != io.EOF {
			return data, nil, err
		}
		names = capped(header)
	}
	rowCap := false
	for ctx.Err() == nil && !budgetCap {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return data, nil, err
		}
		if len(data.rows) < rowLimit {
			data.rows = append(data.rows, capped(row))
		} else {
			rowCap = true
			if full {
				break
			}
		}
	}
	for i := 0; i < n; i++ {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if i < len(p.Columns) {
			name = safe(p.Columns[i], false)
		}
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("Column %d", i+1)
		}
		data.names = append(data.names, name)
	}
	var notices []string
	if p.header && len(data.rows) == 0 && !rowCap {
		notices = append(notices, "Header row only. No data rows remain.")
	}
	if rowCap {
		if full {
			notices = append(notices, "CSV display limited to 50,000 rows.")
		} else {
			notices = append(notices, "CSV preview row limit reached. Press z in the payload pane for Fullscreen.")
		}
	}
	if columnCap {
		if full {
			notices = append(notices, "CSV display limited to 256 columns.")
		} else {
			notices = append(notices, "CSV preview limited to 64 columns. Press z in the payload pane for Fullscreen.")
		}
	}
	if cellCap {
		notices = append(notices, "CSV preview cells limited to 120 characters. Press z in the payload pane for Fullscreen.")
	}
	if budgetCap {
		notices = append(notices, "CSV cell text reached the display budget.")
	}
	data.truncated = rowCap || columnCap || cellCap || budgetCap
	data.limited = budgetCap
	return data, notices, ctx.Err()
}

func csvField(row []string, i int) string {
	if i >= len(row) {
		return "∅"
	}
	return row[i]
}

type renderText struct {
	builder strings.Builder
	limit   int
	limited bool
}

func (out *renderText) WriteString(text string) (int, error) {
	if out.limited {
		return len(text), nil
	}
	available := out.limit - out.builder.Len()
	if len(text) > available {
		out.limited = true
		for available > 0 && !utf8.RuneStart(text[available]) {
			available--
		}
		_, _ = out.builder.WriteString(text[:available])
	} else {
		_, _ = out.builder.WriteString(text)
	}
	return len(text), nil
}

func (out *renderText) Write(data []byte) (int, error) { return out.WriteString(string(data)) }

func (out *renderText) String() string { return out.builder.String() }

func csvTable(ctx context.Context, sets []csvData, combined bool, limit int) (string, bool) {
	grouped := map[string][]csvData{}
	var order []string
	for _, data := range sets {
		key := strings.Join(data.names, "\x00")
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], data)
	}
	out := renderText{limit: limit}
	for _, key := range order {
		sets := grouped[key]
		names := sets[0].names
		if len(names) == 0 {
			continue
		}
		if combined {
			names = append([]string{"Source", "Row"}, names...)
		}
		widths := make([]int, len(names))
		measure := func(values []string) {
			for i, value := range values {
				widths[i] = max(widths[i], uniseg.StringWidth(value))
			}
		}
		measure(names)
		var rows [][]string
		for _, data := range sets {
			for i, row := range data.rows {
				if ctx.Err() != nil {
					return "", true
				}
				if combined {
					row = append([]string{data.source, strconv.Itoa(i + 1)}, row...)
				}
				rows = append(rows, row)
				measure(row)
			}
		}
		printRow := func(row []string) {
			for i, w := range widths {
				cell := csvField(row, i)
				out.WriteString(cell)
				if out.limited {
					return
				}
				if i+1 < len(widths) {
					padding := max(0, w-uniseg.StringWidth(cell))
					out.WriteString(strings.Repeat(" ", min(padding, out.limit-out.builder.Len())))
					out.WriteString(" │ ")
				}
				if out.limited {
					return
				}
			}
			out.WriteString("\n")
		}
		printRow(names)
		for _, row := range rows {
			if ctx.Err() != nil {
				return "", true
			}
			printRow(row)
			if out.limited {
				return out.String(), true
			}
		}
		out.WriteString("\n")
	}
	return out.String(), out.limited
}
func csvPivot(ctx context.Context, data csvData, limit int) (string, bool) {
	out := renderText{limit: limit}
	for i, row := range data.rows {
		if ctx.Err() != nil {
			return "", true
		}
		fmt.Fprintf(&out, "Row %d\n", i+1)
		for j, name := range data.names {
			out.WriteString("  ")
			out.WriteString(name)
			out.WriteString(": ")
			out.WriteString(csvField(row, j))
			out.WriteString("\n")
			if out.limited {
				return out.String(), true
			}
		}
	}
	return out.String(), out.limited
}

type payloadLimits struct {
	structured, rendered int
}

func (o options) inspectionLimits(full, expanded bool) payloadLimits {
	if full && expanded {
		return payloadLimits{math.MaxInt, math.MaxInt}
	}
	limits := payloadLimits{o.structuredInput, o.renderedOutput}
	if limits.structured == 0 {
		limits.structured = defaultStructuredInput
	}
	if limits.rendered == 0 {
		limits.rendered = defaultRenderedOutput
	}
	if !full {
		limits.structured = min(limits.structured, maxPreviewInput)
		limits.rendered = min(limits.rendered, maxPreviewOutput)
	}
	return limits
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

func renderValue(ctx context.Context, r *storedRecord, limit int) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if r.fullPath == "" && r.fullAvailable() && len(r.value) <= limit {
		return r.value, false, nil
	}
	file, err := r.openFull()
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	// Read one extra byte so that a valid UTF-8 sequence may be cut cleanly.
	readSize := min(limit, r.valueSize)
	if readSize < math.MaxInt {
		readSize++
	}
	value, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, int64(readSize)))
	if err != nil {
		return nil, false, err
	}
	if len(value) != min(r.valueSize, readSize) {
		return nil, false, errors.New("retained payload changed while reading")
	}
	if len(value) <= limit {
		return value, false, nil
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit], true, nil
}

func (out *renderText) writeSafe(ctx context.Context, text string) {
	out.writeSafeMode(ctx, text, true)
}

func (out *renderText) writeSafeMode(ctx context.Context, text string, multiline bool) {
	for len(text) > 0 && !out.limited && ctx.Err() == nil {
		n := min(len(text), 16<<10)
		for n < len(text) && n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		if n == 0 {
			n = min(len(text), 16<<10)
		}
		out.WriteString(safe(text[:n], multiline))
		text = text[n:]
	}
}

func (u *ui) preparePayload(ctx context.Context, records []*storedRecord, profiles map[string]profile, mode string, full, combined bool, limits payloadLimits) payloadResult {
	outputLimit := limits.rendered
	if !full && combined {
		outputLimit = min(outputLimit, 1_000_000)
	}
	out := renderText{limit: outputLimit}
	result := payloadResult{}
	var notices []string
	var tableSets []csvData
	var nodes []*payloadNode
	schemas := map[string]*loadedSchema{}
	remainingRows := 200
	if full {
		remainingRows = maxFullCSVRows
	}
	remainingBytes := max(limits.structured, limits.rendered)
	write := func(id, text string) {
		if combined {
			fmt.Fprintf(&out, "%s\n", id)
		}
		out.writeSafe(ctx, text)
		if combined {
			out.WriteString("\n\n")
		}
	}
	for _, r := range records {
		if ctx.Err() != nil {
			return payloadResult{}
		}
		if out.limited || remainingBytes == 0 {
			result.incomplete, result.canExpand = true, true
			break
		}
		p := profiles[r.id.Topic]
		id := safe(r.id.String(), false)
		value := r.value
		structured := mode != "raw"
		if !r.fullAvailable() {
			result.incomplete = true
			notices = append(notices, id+": "+errPayloadQuota.Error())
			write(id, string(value))
			continue
		}
		if !full && r.truncated() {
			result.incomplete = true
			notices = append(notices, id+" preview truncated. Press z in the payload pane for Fullscreen.")
			write(id, string(value))
			continue
		}
		if full {
			structuredBytes := min(limits.structured, remainingBytes)
			if structured && r.valueSize > structuredBytes {
				result.incomplete, result.canExpand = true, true
				notices = append(notices, fmt.Sprintf("%s: structured input exceeds %s. Showing raw text.", id, binarySize(ptr(int64(structuredBytes)))))
				structured = false
			}
			readLimit := remainingBytes
			if !structured {
				readLimit = min(readLimit, outputLimit-out.builder.Len())
			}
			var limited bool
			var err error
			value, limited, err = renderValue(ctx, r, readLimit)
			if err != nil {
				result.incomplete = true
				notices = append(notices, id+": "+err.Error())
				write(id, err.Error())
				continue
			}
			if limited {
				result.incomplete, result.canExpand = true, true
			}
		}
		remainingBytes -= len(value)
		if r.value == nil && r.valueSize == 0 {
			write(id, "<tombstone>")
			continue
		}
		if !structured {
			write(id, string(value))
			continue
		}
		if csvMode(mode) {
			data, warnings, err := parseCSV(ctx, string(value), p, full, remainingRows, outputLimit)
			data.source = id
			if err != nil {
				result.incomplete = true
				notices = append(notices, id+": CSV: "+err.Error())
				write(id, string(value))
				continue
			}
			if combined {
				remainingRows -= len(data.rows)
			}
			result.incomplete = result.incomplete || data.truncated
			result.canExpand = result.canExpand || data.limited
			notices = append(notices, warnings...)
			if mode == "csv-table" {
				tableSets = append(tableSets, data)
			} else {
				body, limited := csvPivot(ctx, data, outputLimit)
				result.incomplete = result.incomplete || limited
				result.canExpand = result.canExpand || limited
				if body == "" {
					body = strings.Join(warnings, " ")
				}
				write(id, body)
			}
			continue
		}
		raw := value
		var err error
		if protoMode(mode) {
			schema := schemas[r.id.Topic]
			if schema == nil {
				load, cancel := context.WithTimeout(ctx, 20*time.Second)
				schema, err = u.getSchema(load, p)
				cancel()
				if err == nil {
					schemas[r.id.Topic] = schema
				}
			}
			if err == nil && ctx.Err() == nil {
				var warning string
				value, warning, err = schema.decode(p.Protobuf, value)
				if warning != "" {
					notices = append(notices, id+": "+warning)
				}
			}
		}
		if err == nil && treeMode(mode) && ctx.Err() == nil {
			var obj any
			obj, err = jsonValueContext(ctx, value)
			if err == nil {
				node, limited := buildTree(ctx, id, obj, full)
				nodes = append(nodes, node)
				if limited {
					notices = append(notices, "Tree preview limited to 2,000 nodes and 30 levels. Press z in the payload pane for Fullscreen.")
				}
			}
		}
		var formatted string
		if err == nil {
			formatted, err = prettyJSON(ctx, value, outputLimit)
		}
		if err != nil {
			result.incomplete = true
			result.canExpand = result.canExpand || errors.Is(err, errRenderLimit)
			notices = append(notices, id+": "+err.Error())
			write(id, string(raw))
		} else {
			write(id, formatted)
		}
	}
	if len(tableSets) > 0 && ctx.Err() == nil {
		table, limited := csvTable(ctx, tableSets, combined, outputLimit)
		result.incomplete = result.incomplete || limited
		result.canExpand = result.canExpand || limited
		out.WriteString(table)
	}
	if out.limited {
		result.incomplete, result.canExpand = true, true
	}
	if result.canExpand {
		notices = append(notices, fmt.Sprintf("Input/read budget %s and display budget %s. Copy Raw Value and Export use the retained original.",
			binarySize(ptr(int64(max(limits.structured, limits.rendered)))), binarySize(ptr(int64(outputLimit)))))
	}
	result.text, result.warning = out.String(), strings.Join(notices, "\n")
	if len(nodes) == 1 && len(records) == 1 {
		result.tree = nodes[0]
	} else if len(nodes) > 0 {
		root := &payloadNode{label: "Records"}
		for _, n := range nodes {
			n.parent = root
			root.children = append(root.children, n)
		}
		result.tree = root
	}
	return result
}
