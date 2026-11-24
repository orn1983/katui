package katui

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

var modes = []string{"raw", "pretty", "tree", "csv-table", "csv-pivot", "proto-pretty", "proto-tree"}
var modeLabels = []string{"Raw", "JSON formatted text", "JSON tree", "CSV table", "CSV pivot", "Protobuf JSON", "Protobuf tree"}

const (
	maxRenderedPayload = 8 << 20
	maxStructuredInput = 2 << 20
	maxFullTreeNodes   = 50_000
	maxFullTreeDepth   = 128
	maxFullCSVRows     = 50_000
	maxFullCSVColumns  = 256
	maxInspectionBytes = 128 << 20
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
	widget        *tview.TreeNode
}

func jsonValue(value []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var obj any
	if err := decoder.Decode(&obj); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return obj, nil
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
	if len(value) > limit {
		return "", errRenderLimit
	}
	// json.Indent only accepts a bytes.Buffer. Bound its output first,
	// including indentation at each delimiter outside a quoted string.
	estimate, depth := len(value), 0
	quoted, escaped := false, false
	for i, ch := range value {
		if i%32768 == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
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
		switch ch {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > maxFullTreeDepth {
				return "", errors.New("JSON nesting exceeds the display limit")
			}
			estimate += 1 + 2*depth
		case '}', ']':
			depth = max(0, depth-1)
			estimate += 1 + 2*depth
		case ',':
			estimate += 1 + 2*depth
		case ':':
			estimate++
		}
		if estimate > limit {
			return "", errRenderLimit
		}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, value, "", "  "); err != nil {
		return "", err
	}
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
func buildTree(ctx context.Context, label string, value any, full bool) (*payloadNode, bool) {
	maxNodes, maxDepth := 2000, 30
	if full {
		maxNodes, maxDepth = maxFullTreeNodes, maxFullTreeDepth
	}
	count := 0
	limited := false
	canAdd := func() bool {
		if ctx.Err() != nil || count >= maxNodes {
			limited = true
			return false
		}
		return true
	}
	var build func(string, any, *payloadNode, int) *payloadNode
	build = func(label string, value any, parent *payloadNode, depth int) *payloadNode {
		count++
		node := &payloadNode{label: shortened(label, 240), value: value, parent: parent, search: label}
		switch value := value.(type) {
		case map[string]any:
			node.label += fmt.Sprintf(" {%d}", len(value))
		case []any:
			node.label += fmt.Sprintf(" [%d]", len(value))
		default:
			display := value
			if text, ok := value.(string); ok {
				node.search += " " + text
				display = shortened(text, 240)
			} else {
				node.search += " " + prettyValue(value)
			}
			node.label += " = " + prettyValue(display)
			return node
		}
		if depth >= maxDepth || !canAdd() {
			limited = true
			return node
		}
		switch v := value.(type) {
		case []any:
			step := rangeSize(len(v))
			for first := 0; first < len(v); first += step {
				if !canAdd() {
					break
				}
				if !full || step == 1 {
					if !full {
						step = 1
					}
					node.children = append(node.children, build(fmt.Sprintf("[%d]", first), v[first], node, depth+1))
					continue
				}
				last := min(first+step, len(v))
				group := &payloadNode{label: fmt.Sprintf("[%d … %d]", first, last-1), parent: node, value: v[first:last]}
				count++
				var ranges func(*payloadNode, int, int)
				ranges = func(parent *payloadNode, lo, hi int) {
					size := rangeSize(hi - lo)
					for i := lo; i < hi; i += size {
						if !canAdd() {
							return
						}
						if size == 1 {
							parent.children = append(parent.children, build(fmt.Sprintf("[%d]", i), v[i], parent, depth+1))
							continue
						}
						end := min(hi, i+size)
						n := &payloadNode{label: fmt.Sprintf("[%d … %d]", i, end-1), parent: parent, value: v[i:end]}
						count++
						parent.children = append(parent.children, n)
						ranges(n, i, end)
					}
				}
				ranges(group, first, last)
				node.children = append(node.children, group)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var ranges func(*payloadNode, []string)
			ranges = func(parent *payloadNode, keys []string) {
				step := 1
				if full {
					step = rangeSize(len(keys))
				}
				for first := 0; first < len(keys); first += step {
					if !canAdd() {
						break
					}
					if step == 1 {
						key := keys[first]
						parent.children = append(parent.children, build(key, v[key], parent, depth+1))
						continue
					}
					last := min(first+step, len(keys))
					subset := map[string]any{}
					for _, k := range keys[first:last] {
						subset[k] = v[k]
					}
					n := &payloadNode{label: fmt.Sprintf("%s … %s", keys[first], keys[last-1]), parent: parent, value: subset}
					count++
					parent.children = append(parent.children, n)
					ranges(n, keys[first:last])
				}
			}
			ranges(node, keys)
		}
		return node
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
			notices = append(notices, "CSV preview row limit reached; use F4 for more rows.")
		}
	}
	if columnCap {
		if full {
			notices = append(notices, "CSV display limited to 256 columns.")
		} else {
			notices = append(notices, "CSV preview limited to 64 columns; use F4 for more columns.")
		}
	}
	if cellCap {
		notices = append(notices, "CSV preview cells limited to 120 characters; use F4 for complete fields.")
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
					out.WriteString(strings.Repeat(" ", min(padding, out.limit-out.builder.Len()+1)))
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

func (o options) inspectionLimits(override bool) payloadLimits {
	if override {
		return payloadLimits{maxInspectionBytes, maxInspectionBytes}
	}
	limits := payloadLimits{o.structuredInput, o.renderedOutput}
	if limits.structured == 0 {
		limits.structured = maxStructuredInput
	}
	if limits.rendered == 0 {
		limits.rendered = maxRenderedPayload
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
	value, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(value) != min(r.valueSize, limit+1) {
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
			notices = append(notices, id+": preview truncated. Open F4 for full payload.")
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
			obj, err = jsonValue(value)
			if err == nil {
				node, limited := buildTree(ctx, id, obj, full)
				nodes = append(nodes, node)
				if limited {
					if full {
						notices = append(notices, "Tree limited to 50,000 nodes and 128 levels.")
					} else {
						notices = append(notices, "Tree preview limited to 2,000 nodes and 30 levels. Open F4 for more.")
					}
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
		notices = append(notices, fmt.Sprintf("Input/read budget %s and display budget %s. Copy Value and Export use the retained original.",
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
