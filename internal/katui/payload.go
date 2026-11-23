package katui

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/tview"
)

var modes = []string{"raw", "pretty", "tree", "csv-table", "csv-pivot", "proto-pretty", "proto-tree"}
var modeLabels = []string{"Raw", "JSON formatted text", "JSON tree", "CSV table", "CSV pivot", "Protobuf JSON", "Protobuf tree"}

const (
	maxRenderedPayload  = 8 << 20
	maxStructuredInput  = 2 << 20
	maxFullTreeNodes    = 50_000
	maxFullTreeDepth    = 128
	maxFullCSVRows      = 50_000
	maxFullCSVColumns   = 256
	payloadDisplayLimit = "Display limited to 8 MiB. Use Copy Value to access the retained original."
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
	text, warning string
	tree          *payloadNode
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
func prettyJSON(value []byte) (string, error) {
	if len(value) > maxRenderedPayload {
		return "", errors.New("formatted JSON exceeds the display limit")
	}
	// json.Indent only accepts a bytes.Buffer. Bound its output first,
	// including indentation at each delimiter outside a quoted string.
	estimate, depth := len(value), 0
	quoted, escaped := false, false
	for _, ch := range value {
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
		if estimate > maxRenderedPayload {
			return "", errors.New("formatted JSON exceeds the display limit")
		}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, value, "", "  "); err != nil {
		return "", err
	}
	return safe(out.String(), true), nil
}
func shortened(s string, n int) string {
	if len(s) <= n || utf8.RuneCountInString(s) <= n {
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
func buildTree(label string, value any, full bool) (*payloadNode, bool) {
	maxNodes, maxDepth := 2000, 30
	if full {
		maxNodes, maxDepth = maxFullTreeNodes, maxFullTreeDepth
	}
	count := 0
	limited := false
	canAdd := func() bool {
		if count >= maxNodes {
			limited = true
			return false
		}
		return true
	}
	var build func(string, any, *payloadNode, int) *payloadNode
	build = func(label string, value any, parent *payloadNode, depth int) *payloadNode {
		count++
		node := &payloadNode{label: label, value: value, parent: parent, search: label}
		switch value := value.(type) {
		case map[string]any:
			node.label += fmt.Sprintf(" {%d}", len(value))
		case []any:
			node.label += fmt.Sprintf(" [%d]", len(value))
		default:
			scalar := prettyValue(value)
			if text, ok := value.(string); ok {
				node.search += " " + text
			} else {
				node.search += " " + scalar
			}
			node.label += " = " + scalar
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
	names  []string
	rows   [][]string
	source string
}

func parseCSV(raw string, p profile, full bool, limit int) (csvData, []string, error) {
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
	columnCap, cellCap := false, false
	maxColumns := 64
	if full {
		limit, maxColumns = maxFullCSVRows, maxFullCSVColumns
	}
	capped := func(row []string) []string {
		if len(row) > maxColumns {
			row = row[:maxColumns]
			columnCap = true
		}
		for i, value := range row {
			value = safe(value, false)
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
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return data, nil, err
		}
		if len(data.rows) < limit {
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
	return data, notices, nil
}

func csvField(row []string, i int) string {
	if i >= len(row) {
		return "∅"
	}
	return row[i]
}

type renderText struct {
	builder strings.Builder
	limited bool
}

func (out *renderText) WriteString(text string) (int, error) {
	if out.limited {
		return len(text), nil
	}
	available := maxRenderedPayload - out.builder.Len()
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

func csvTable(sets []csvData, combined bool) (string, bool) {
	grouped := map[string][]csvData{}
	var order []string
	for _, data := range sets {
		key := strings.Join(data.names, "\x00")
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], data)
	}
	var out renderText
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
				widths[i] = max(widths[i], tview.TaggedStringWidth(tview.Escape(value)))
			}
		}
		measure(names)
		var rows [][]string
		for _, data := range sets {
			for i, row := range data.rows {
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
				if i+1 < len(widths) {
					out.WriteString(strings.Repeat(" ", max(0, w-tview.TaggedStringWidth(tview.Escape(cell)))))
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
			printRow(row)
			if out.limited {
				return out.String(), true
			}
		}
		out.WriteString("\n")
	}
	return out.String(), out.limited
}
func csvPivot(data csvData) (string, bool) {
	var out renderText
	for i, row := range data.rows {
		fmt.Fprintf(&out, "Row %d\n", i+1)
		for j, name := range data.names {
			fmt.Fprintf(&out, "  %s: %s\n", name, csvField(row, j))
			if out.limited {
				return out.String(), true
			}
		}
	}
	return out.String(), out.limited
}

func renderValue(r *storedRecord, limit int) ([]byte, bool, error) {
	if r.valueSize <= limit || r.fullPath == "" {
		value, err := r.full()
		return value, false, err
	}
	file, err := os.Open(r.fullPath)
	if err != nil {
		return nil, false, fmt.Errorf("original unavailable; reconsume the record: %w", err)
	}
	defer file.Close()
	// Read one extra byte so that a valid UTF-8 sequence may be cut cleanly.
	value, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(value) <= limit {
		return value, false, err
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit], true, nil
}

func (u *ui) preparePayload(ctx context.Context, records []*storedRecord, profiles map[string]profile, mode string, full, combined bool) payloadResult {
	var out renderText
	var notices []string
	var tableSets []csvData
	var nodes []*payloadNode
	schemas := map[string]*loadedSchema{}
	remaining := 200
	remainingBytes := maxRenderedPayload
	displayLimited := false
	addText := func(id, text string) {
		if combined {
			fmt.Fprintf(&out, "%s\n", id)
		}
		out.WriteString(text)
		if combined {
			out.WriteString("\n\n")
		}
	}
	for _, r := range records {
		if ctx.Err() != nil {
			break
		}
		if out.limited || remainingBytes == 0 {
			displayLimited = true
			break
		}
		p := profiles[r.id.Topic]
		id := safe(r.id.String(), false)
		value := r.value
		inputLimited := false
		if full {
			var err error
			value, inputLimited, err = renderValue(r, remainingBytes)
			if err != nil {
				notices = append(notices, id+": "+err.Error())
				addText(id, err.Error())
				continue
			}
		}
		remainingBytes -= len(value)
		if value == nil {
			addText(id, "<tombstone>")
			continue
		}
		text := safe(string(value), true)
		if inputLimited {
			displayLimited = true
			addText(id, text)
			break
		}
		if !full && r.truncated() {
			if r.fullPath == "" {
				notices = append(notices, id+": "+errPayloadQuota.Error())
			} else {
				notices = append(notices, id+": preview truncated; F4 opens the original.")
			}
			addText(id, text)
			continue
		}
		if mode == "raw" {
			addText(id, text)
			continue
		}
		if len(value) > maxStructuredInput {
			notices = append(notices, id+": structured display limited to 2 MiB of input. Showing raw text.")
			addText(id, text)
			continue
		}
		if csvMode(mode) {
			data, warnings, err := parseCSV(strings.ToValidUTF8(string(value), "�"), p, full, remaining)
			data.source = id
			if err != nil {
				notices = append(notices, id+": CSV: "+err.Error())
				addText(id, text)
				continue
			}
			if combined {
				remaining -= len(data.rows)
			}
			notices = append(notices, warnings...)
			if mode == "csv-table" {
				tableSets = append(tableSets, data)
			} else {
				body, limited := csvPivot(data)
				displayLimited = displayLimited || limited
				if body == "" {
					body = strings.Join(warnings, " ")
				}
				addText(id, body)
			}
			continue
		}
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
			if err == nil {
				var warning string
				value, warning, err = schema.decode(p.Protobuf, value)
				if warning != "" {
					notices = append(notices, id+": "+warning)
				}
			}
		}
		var node *payloadNode
		if err == nil {
			var formatted string
			formatted, err = prettyJSON(value)
			if err == nil {
				text = formatted
				if treeMode(mode) {
					var obj any
					obj, err = jsonValue(value)
					if err == nil {
						var limited bool
						node, limited = buildTree(id, obj, full)
						if limited {
							if full {
								notices = append(notices, "Tree display limited to 50,000 nodes.")
							} else {
								notices = append(notices, "Tree preview limited to depth 30 and 2,000 nodes; use F4 for more.")
							}
						}
					}
				}
			}
		}
		if err != nil {
			notices = append(notices, id+": "+err.Error())
		}
		if node != nil {
			nodes = append(nodes, node)
		}
		addText(id, text)
	}
	if len(tableSets) > 0 {
		table, limited := csvTable(tableSets, combined)
		displayLimited = displayLimited || limited
		out.WriteString(table)
	}
	if out.limited || displayLimited {
		notices = append(notices, payloadDisplayLimit)
	}
	text := out.String()
	if !full && combined && len(text) > 1000000 {
		text = shortened(text, 1000000)
		notices = append(notices, "Comparison preview limited to 1,000,000 characters; use F4.")
	}
	result := payloadResult{text: text, warning: strings.Join(notices, "\n")}
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
