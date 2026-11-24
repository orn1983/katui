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

type payloadDocument struct {
	text  string
	lines []int // byte offsets of physical line starts
}

func indexPayloadText(ctx context.Context, text string) (payloadDocument, bool) {
	d := payloadDocument{text: text, lines: []int{0}}
	for from := 0; from < len(text); {
		if len(d.lines)%4096 == 0 && ctx.Err() != nil {
			return payloadDocument{}, false
		}
		i := strings.IndexByte(text[from:], '\n')
		if i < 0 {
			break
		}
		if len(d.lines) == maxPayloadLines {
			d.text = text[:from+i]
			return d, true
		}
		from += i + 1
		d.lines = append(d.lines, from)
	}
	return d, false
}

// payloadText is a read-only viewport. Rendering or selecting a small region
// does not require reformatting the rest of a large payload.
type payloadText struct {
	*tview.Box
	doc            payloadDocument
	top, left      int
	cursor, anchor int
	match          [2]int
	dragging       bool
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

func (v *payloadText) selectBy(cursor int, extend bool) {
	v.cursor = runeStart(v.doc.text, max(0, min(len(v.doc.text), cursor)))
	if !extend {
		v.anchor = v.cursor
	}
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
	column := v.cursor - v.doc.lines[row]
	if column < v.left {
		v.left = column
	} else if w > 0 {
		start, end := v.line(row)
		left := runeStart(v.doc.text[start:end], min(v.left, end-start))
		distance := column - left
		// A UTF-8 byte count bounds normal cell width. Measure only
		// nearby text to keep search jumps across long lines cheap.
		nearby := v.doc.text[start+left : v.cursor]
		if distance >= 4*w || uniseg.StringWidth(nearby)+4*strings.Count(nearby, "\t") >= w {
			v.left = max(0, column-(w-1)/4)
		}
	}
}

func (v *payloadText) offsetAt(row, column int) int {
	start, end := v.line(row)
	at := start + runeStart(v.doc.text[start:end], min(v.left, end-start))
	g := uniseg.NewGraphemes(v.doc.text[at:end])
	for g.Next() {
		w := g.Width()
		if g.Str() == "\t" {
			w = 4
		}
		if column < max(1, w) {
			a, _ := g.Positions()
			return at + a
		}
		column -= max(1, w)
	}
	return end
}

func (v *payloadText) Draw(screen tcell.Screen) {
	v.Box.DrawForSubclass(screen, v)
	x, y, w, h := v.GetInnerRect()
	selectionLow, selectionHigh := min(v.anchor, v.cursor), max(v.anchor, v.cursor)
	for row := v.top; row < min(v.top+h, len(v.doc.lines)); row++ {
		start, end := v.line(row)
		at := start + runeStart(v.doc.text[start:end], min(v.left, end-start))
		g := uniseg.NewGraphemes(v.doc.text[at:end])
		col := 0
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
			case v.HasFocus() && at+a == v.cursor:
				style = style.Reverse(true)
			}
			if g.Str() == "\t" {
				for range min(4, w-col) {
					screen.SetContent(x+col, y+row-v.top, ' ', nil, style)
					col++
				}
				continue
			}
			width := max(1, g.Width())
			if width > w-col {
				break
			}
			runes := g.Runes()
			screen.SetContent(x+col, y+row-v.top, runes[0], runes[1:], style)
			col += width
		}
		if v.HasFocus() && end == v.cursor && col < w {
			screen.SetContent(x+col, y+row-v.top, ' ', nil, tcell.StyleDefault.Reverse(true))
		}
	}
}

func (v *payloadText) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return v.WrapInputHandler(func(event *tcell.EventKey, _ func(tview.Primitive)) {
		cursor := v.cursor
		row := v.row(cursor)
		start, end := v.line(row)
		delta := 0
		switch event.Key() {
		case tcell.KeyCtrlA:
			v.anchor, v.cursor = 0, len(v.doc.text)
			return
		case tcell.KeyCtrlC:
			lo, hi := min(v.anchor, v.cursor), max(v.anchor, v.cursor)
			v.copy(v.doc.text[lo:hi])
			return
		case tcell.KeyLeft:
			_, n := utf8.DecodeLastRuneInString(v.doc.text[:cursor])
			cursor -= n
		case tcell.KeyRight:
			_, n := utf8.DecodeRuneInString(v.doc.text[cursor:])
			cursor += n
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
			_, _, _, height := v.GetInnerRect()
			delta = max(1, height-1)
			if event.Key() == tcell.KeyPgUp {
				delta = -delta
			}
		case tcell.KeyRune:
			switch event.Rune() {
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
			lineStart, lineEnd := v.line(max(0, min(len(v.doc.lines)-1, row+delta)))
			cursor = min(lineEnd, lineStart+cursor-start)
		}
		v.selectBy(cursor, event.Modifiers()&tcell.ModShift != 0)
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
			v.top = min(len(v.doc.lines)-1, v.top+3)
			return true, nil
		}
		return false, nil
	})
}
