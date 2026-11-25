package katui

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

const maxPayloadLines = 1_000_000
const columnCheckpointBytes = 256

type payloadColumn struct {
	byteOffset int
	cell       int
}

type payloadDocument struct {
	text  string
	lines []int // byte offsets of physical line starts
	width int
	// Only long lines that require grapheme measurement need checkpoints.
	columns map[int][]payloadColumn
}

func indexPayloadText(ctx context.Context, text string) (payloadDocument, bool) {
	d := payloadDocument{text: text, lines: []int{0}}
	for from := 0; ; {
		if ctx.Err() != nil {
			return payloadDocument{}, false
		}
		end := len(text)
		newline := strings.IndexByte(text[from:], '\n')
		if newline >= 0 {
			end = from + newline
		}
		row := len(d.lines) - 1
		width, points := indexPayloadLine(ctx, text[from:end])
		d.width = max(d.width, width)
		if points != nil {
			if d.columns == nil {
				d.columns = map[int][]payloadColumn{}
			}
			d.columns[row] = points
		}
		if newline < 0 {
			break
		}
		if len(d.lines) == maxPayloadLines {
			d.text = text[:end]
			return d, true
		}
		from = end + 1
		d.lines = append(d.lines, from)
	}
	return d, false
}

func graphemeWidth(g *uniseg.Graphemes) int {
	if g.Str() == "\t" {
		return 4
	}
	return max(1, g.Width())
}

func indexPayloadLine(ctx context.Context, text string) (int, []payloadColumn) {
	for i := 0; i < len(text); i++ {
		if i%32768 == 0 && ctx.Err() != nil {
			return 0, nil
		}
		if text[i] < ' ' || text[i] >= utf8.RuneSelf {
			width := 0
			var points []payloadColumn
			if len(text) > columnCheckpointBytes {
				points = []payloadColumn{{}}
			}
			g := uniseg.NewGraphemes(text)
			for g.Next() {
				start, _ := g.Positions()
				if len(points) != 0 && start-points[len(points)-1].byteOffset >= columnCheckpointBytes {
					if ctx.Err() != nil {
						return 0, nil
					}
					points = append(points, payloadColumn{start, width})
				}
				width += graphemeWidth(g)
			}
			return width, points
		}
	}
	return len(text), nil
}

// position finds the start of the grapheme at a byte offset or display cell.
// Long ASCII lines need no index and long mixed lines scan from a checkpoint.
func (d payloadDocument) position(row int, line string, target int, byCell bool) payloadColumn {
	points := d.columns[row]
	if len(line) > columnCheckpointBytes && len(points) == 0 {
		i := min(len(line), target)
		return payloadColumn{i, i}
	}
	pos := payloadColumn{}
	if len(points) != 0 {
		i := sort.Search(len(points), func(i int) bool {
			if byCell {
				return points[i].cell > target
			}
			return points[i].byteOffset > target
		})
		pos = points[i-1]
	}
	base := pos.byteOffset
	g := uniseg.NewGraphemes(line[base:])
	for g.Next() {
		_, end := g.Positions()
		width := graphemeWidth(g)
		if byCell && pos.cell+width > target || !byCell && base+end > target {
			break
		}
		pos.byteOffset = base + end
		pos.cell += width
	}
	return pos
}

type payloadTextMode int

const (
	payloadScroll payloadTextMode = iota
	payloadCursor
	payloadSelection
)

// payloadText is a read-only viewport. Rendering or selecting a small region
// does not require reformatting the rest of a large payload.
type payloadText struct {
	*tview.Box
	doc            payloadDocument
	top, left      int
	cursor, anchor int
	match          [2]int
	dragging       bool
	mode           payloadTextMode
	modeChanged    func()
	onSelect       func()
	copy           func(string)
}

func newPayloadText(copySelection func(string)) *payloadText {
	v := &payloadText{Box: tview.NewBox(), copy: copySelection}
	v.setDocument(payloadDocument{})
	return v
}

func (v *payloadText) setDocument(doc payloadDocument) {
	if len(doc.lines) == 0 {
		doc.lines = []int{0}
	}
	v.doc = doc
	v.top, v.left, v.cursor, v.anchor = 0, 0, 0, 0
	v.match = [2]int{}
	v.dragging = false
	v.setMode(payloadScroll)
}

func (v *payloadText) Focus(delegate func(tview.Primitive)) {
	v.Box.Focus(delegate)
	if v.modeChanged != nil {
		v.modeChanged()
	}
}

func (v *payloadText) Blur() {
	v.Box.Blur()
	if v.modeChanged != nil {
		v.modeChanged()
	}
}

func (v *payloadText) setMode(mode payloadTextMode) {
	if v.mode != mode {
		v.mode = mode
		if v.modeChanged != nil {
			v.modeChanged()
		}
	}
}

func (v *payloadText) exitMode() bool {
	switch v.mode {
	case payloadSelection:
		v.setMode(payloadCursor)
	case payloadCursor:
		v.setMode(payloadScroll)
	default:
		return false
	}
	return true
}

func (v *payloadText) row(offset int) int {
	return sort.SearchInts(v.doc.lines, offset+1) - 1
}

func (v *payloadText) line(row int) (start, end int) {
	start, end = v.doc.lines[row], len(v.doc.text)
	if row+1 < len(v.doc.lines) {
		end = v.doc.lines[row+1] - 1
	}
	return
}

func runeStart(text string, offset int) int {
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	return offset
}

func (v *payloadText) selectRange(anchor, cursor int) {
	if v.onSelect != nil {
		v.onSelect()
	}
	v.anchor, v.cursor = anchor, cursor
}

func (v *payloadText) selectBy(cursor int, extend bool) {
	cursor = runeStart(v.doc.text, max(0, min(len(v.doc.text), cursor)))
	anchor := cursor
	if extend {
		anchor = v.anchor
	}
	v.selectRange(anchor, cursor)
	v.ensureVisible()
}

func (v *payloadText) ensureVisible() {
	_, _, w, h := v.GetInnerRect()
	row := v.row(v.cursor)
	if row < v.top {
		v.top = row
	} else if h > 0 && row >= v.top+h {
		v.top = row - h + 1
	}
	start, end := v.line(row)
	column := v.doc.position(row, v.doc.text[start:end], v.cursor-start, false).cell
	if column < v.left {
		v.left = column
	} else if w > 0 && column >= v.left+w {
		v.left = max(0, column-w/2)
	}
}

func (v *payloadText) maxLeft(width int) int {
	// Include the cursor position just after the last grapheme.
	return max(0, v.doc.width+1-max(1, width))
}

func (v *payloadText) maxTop(height int) int {
	return max(0, len(v.doc.lines)-max(1, height))
}

func (v *payloadText) placeVisibleCursor() {
	_, _, width, height := v.GetInnerRect()
	row := v.row(v.cursor)
	start, end := v.line(row)
	column := v.doc.position(row, v.doc.text[start:end], v.cursor-start, false).cell
	if row < v.top || row >= v.top+max(1, height) || column < v.left || column >= v.left+max(1, width) {
		v.cursor = v.offsetAt(min(v.top, len(v.doc.lines)-1), 0)
		v.anchor = v.cursor
	}
}

func (v *payloadText) offsetAt(row, column int) int {
	start, end := v.line(row)
	return start + v.doc.position(row, v.doc.text[start:end], v.left+column, true).byteOffset
}

func (v *payloadText) selectedText() string {
	return v.doc.text[min(v.anchor, v.cursor):max(v.anchor, v.cursor)]
}

func (v *payloadText) Draw(screen tcell.Screen) {
	v.Box.DrawForSubclass(screen, v)
	x, y, w, h := v.GetInnerRect()
	v.left = min(v.left, v.maxLeft(w))
	v.top = min(v.top, v.maxTop(h))
	selectionLow, selectionHigh := min(v.anchor, v.cursor), max(v.anchor, v.cursor)
	for row := v.top; row < min(v.top+h, len(v.doc.lines)); row++ {
		start, end := v.line(row)
		pos := v.doc.position(row, v.doc.text[start:end], v.left, true)
		at := start + pos.byteOffset
		g := uniseg.NewGraphemes(v.doc.text[at:end])
		col := pos.cell - v.left
		for col < w && g.Next() {
			a, b := g.Positions()
			style := tcell.StyleDefault
			matched := at+a < v.match[1] && at+b > v.match[0]
			selected := at+a < selectionHigh && at+b > selectionLow
			switch {
			case selected:
				style = style.Foreground(tcell.ColorBlack).Background(tcell.ColorLightSkyBlue)
			case matched:
				style = style.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow)
			case v.mode != payloadScroll && v.HasFocus() && at+a == v.cursor:
				style = style.Reverse(true)
			}
			width := graphemeWidth(g)
			if g.Str() == "\t" {
				for i := max(0, col); i < min(col+width, w); i++ {
					screen.SetContent(x+i, y+row-v.top, ' ', nil, style)
				}
			} else if col >= 0 && col+width <= w {
				runes := g.Runes()
				screen.SetContent(x+col, y+row-v.top, runes[0], runes[1:], style)
			}
			col += width
		}
		if v.mode != payloadScroll && v.HasFocus() && end == v.cursor && col >= 0 && col < w {
			screen.SetContent(x+col, y+row-v.top, ' ', nil, tcell.StyleDefault.Reverse(true))
		}
	}
}

func (v *payloadText) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return v.WrapInputHandler(func(event *tcell.EventKey, _ func(tview.Primitive)) {
		key, mod, letter := event.Key(), event.Modifiers(), shortcutRune(event)
		_, _, width, height := v.GetInnerRect()
		if (key == tcell.KeyLeft || key == tcell.KeyRight) &&
			(mod&(tcell.ModAlt|tcell.ModCtrl) != 0 || v.mode == payloadScroll && mod&tcell.ModShift == 0) {
			delta := 8
			if mod&tcell.ModCtrl != 0 {
				delta = max(delta, width/2)
			}
			if key == tcell.KeyLeft {
				delta = -delta
			}
			v.left = max(0, min(v.maxLeft(width), v.left+delta))
			return
		}
		switch {
		case key == tcell.KeyCtrlC:
			v.copy(v.selectedText())
			return
		case key == tcell.KeyCtrlA:
			v.selectRange(0, len(v.doc.text))
			return
		case letter == ' ':
			if v.mode == payloadScroll {
				v.placeVisibleCursor()
				v.selectRange(v.cursor, v.cursor)
				v.setMode(payloadCursor)
			} else {
				v.setMode(payloadScroll)
			}
			return
		case letter == 'v':
			switch v.mode {
			case payloadCursor:
				v.selectRange(v.cursor, v.cursor)
				v.setMode(payloadSelection)
			case payloadSelection:
				v.setMode(payloadCursor)
			}
			return
		}
		if v.mode != payloadSelection && mod&tcell.ModShift != 0 {
			switch key {
			case tcell.KeyLeft, tcell.KeyRight, tcell.KeyUp, tcell.KeyDown, tcell.KeyHome, tcell.KeyEnd, tcell.KeyPgUp, tcell.KeyPgDn:
				if v.mode == payloadScroll {
					v.placeVisibleCursor()
				}
				v.setMode(payloadSelection)
			}
		}
		// Map these after Shift+arrow handling so a shifted '$' only moves the cursor.
		if v.mode != payloadScroll {
			switch letter {
			case '0':
				key = tcell.KeyHome
			case '$':
				key = tcell.KeyEnd
			}
		}
		if v.mode == payloadScroll {
			switch {
			case key == tcell.KeyUp || letter == 'k':
				v.top = max(0, v.top-4)
				return
			case key == tcell.KeyDown || letter == 'j':
				v.top = min(v.maxTop(height), v.top+4)
				return
			case key == tcell.KeyPgUp:
				v.top = max(0, v.top-max(1, height-1))
				return
			case key == tcell.KeyPgDn:
				v.top = min(v.maxTop(height), v.top+max(1, height-1))
				return
			case key == tcell.KeyHome:
				v.left = 0
				if mod&tcell.ModCtrl != 0 {
					v.top = 0
				}
				return
			case key == tcell.KeyEnd:
				v.left = v.maxLeft(width)
				if mod&tcell.ModCtrl != 0 {
					v.top = v.maxTop(height)
				}
				return
			}
		}
		cursor := v.cursor
		row := v.row(cursor)
		start, end := v.line(row)
		delta := 0
		switch key {
		case tcell.KeyLeft:
			if cursor > start {
				cursor = start + v.doc.position(row, v.doc.text[start:end], cursor-start-1, false).byteOffset
			} else {
				cursor = max(0, cursor-1) // Cross the newline between rows.
			}
		case tcell.KeyRight:
			if cursor < end {
				at := start + v.doc.position(row, v.doc.text[start:end], cursor-start, false).byteOffset
				cluster, _, _, _ := uniseg.FirstGraphemeClusterInString(v.doc.text[at:end], -1)
				cursor = at + len(cluster)
			} else {
				cursor = min(len(v.doc.text), cursor+1)
			}
		case tcell.KeyHome:
			cursor = start
			if event.Modifiers()&tcell.ModCtrl != 0 {
				cursor = 0
			}
		case tcell.KeyEnd:
			cursor = end
			if event.Modifiers()&tcell.ModCtrl != 0 {
				cursor = len(v.doc.text)
			}
		case tcell.KeyUp:
			delta = -1
		case tcell.KeyDown:
			delta = 1
		case tcell.KeyPgUp, tcell.KeyPgDn:
			delta = max(1, height-1)
			if key == tcell.KeyPgUp {
				delta = -delta
			}
		case tcell.KeyRune:
			switch letter {
			case 'j':
				delta = 1
			case 'k':
				delta = -1
			default:
				return
			}
		default:
			return
		}
		if delta != 0 {
			column := v.doc.position(row, v.doc.text[start:end], cursor-start, false).cell
			nextRow := max(0, min(len(v.doc.lines)-1, row+delta))
			lineStart, lineEnd := v.line(nextRow)
			cursor = lineStart + v.doc.position(nextRow, v.doc.text[lineStart:lineEnd], column, true).byteOffset
		}
		v.selectBy(cursor, v.mode == payloadSelection)
	})
}

func (v *payloadText) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return v.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, focus func(tview.Primitive)) (bool, tview.Primitive) {
		x, y, w, h := v.GetInnerRect()
		mx, my := event.Position()
		if h <= 0 || w <= 0 || !v.dragging && (mx < x || mx >= x+w || my < y || my >= y+h) {
			return false, nil
		}
		selectAtMouse := func(extend bool) {
			row := min(len(v.doc.lines)-1, v.top+max(0, min(h-1, my-y)))
			v.selectBy(v.offsetAt(row, max(0, min(w-1, mx-x))), extend)
		}
		switch action {
		case tview.MouseLeftDown:
			focus(v)
			v.dragging = true
			selectAtMouse(event.Modifiers()&tcell.ModShift != 0)
			return true, v
		case tview.MouseMove:
			if v.dragging {
				if my < y {
					v.top = max(0, v.top-1)
				}
				if my >= y+h {
					v.top = min(len(v.doc.lines)-1, v.top+1)
				}
				selectAtMouse(true)
				return true, v
			}
		case tview.MouseLeftUp:
			if v.dragging {
				selectAtMouse(true)
				v.dragging = false
				return true, nil
			}
		case tview.MouseScrollUp:
			v.top = max(0, v.top-3)
			return true, nil
		case tview.MouseScrollDown:
			v.top = min(v.maxTop(h), v.top+3)
			return true, nil
		}
		return false, nil
	})
}
