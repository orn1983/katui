package katui

import (
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

// One cached line per visited record, shared by successive live snapshots.
// Its fixed column widths are common to every record in the message list.
type recordDisplayLine struct {
	text                      string
	cell                      *tview.TableCell
	skip                      int
	checked, pinned, complete bool
	accent                    tcell.Color
}

func (r *storedRecord) displayLine(c *browserTableContent, row *tableRow) *recordDisplayLine {
	line := r.listLine
	complete := r.fullAvailable()
	if line != nil && line.skip == c.skip && line.checked == row.checked && line.pinned == row.pinned &&
		line.complete == complete && line.accent == c.accent {
		return line
	}
	text := browserLine(r.listCells(row.checked, row.pinned), c.widths, c.skip)
	cell := tview.NewTableCell(tview.Escape(text)).SetExpansion(1)
	if row.checked {
		cell.SetBackgroundColor(tcell.ColorDarkSlateGray)
		cell.SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(c.accent).Bold(true).Underline(true))
	}
	r.listLine = &recordDisplayLine{text: text, cell: cell, skip: c.skip, checked: row.checked, pinned: row.pinned, complete: complete, accent: c.accent}
	return r.listLine
}

// Let Table maintain selection, page size, scroll offsets, and mouse hit
// testing. A single expanding column needs no text measurement for layout.
type messageList struct {
	*tview.Table
	u *ui
}

func (l *messageList) Focus(focus func(tview.Primitive)) { focus(l.Table) }

type messageListGeometry struct {
	*browserTableContent
	empty tview.TableCell
}

func (g *messageListGeometry) GetCell(row, column int) *tview.TableCell {
	if column != 0 || row < 0 || row > len(g.rows) {
		return nil
	}
	if row == 0 {
		return g.header
	}
	return &g.empty
}

func (l *messageList) Draw(screen tcell.Screen) {
	c := l.u.tableContent
	if c == nil {
		l.Table.Draw(screen)
		return
	}
	l.SetContent(&messageListGeometry{c, tview.TableCell{Expansion: 1, Transparent: true}})
	l.Table.Draw(screen)
	l.SetContent(c)

	x, y, width, height := l.GetInnerRect()
	top, _ := l.GetOffset()
	selected, _ := l.GetSelection()
	for i := top; i < min(len(c.rows), top+height-1); i++ {
		cell := c.GetCell(i+1, 0)
		style := cell.Style
		if i+1 == selected {
			style = cell.SelectedStyle
			if style == tcell.StyleDefault {
				style = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(c.accent).Bold(true)
			}
		}
		rowY := y + i - top + 1
		end := drawRecordLine(screen, x, rowY, width, c.rows[i].record.listLine.text, style)
		if c.rows[i].checked {
			for col := end; col < width; col++ {
				screen.SetContent(x+col, rowY, ' ', nil, style)
			}
		}
	}
}

// The message list contains plain text. Paint directly to avoid parsing it as
// rich text several times per row on every keypress. Treat a base and its
// combining characters as one cluster even when the base is ASCII.
func drawRecordLine(screen tcell.Screen, x, y, width int, text string, style tcell.Style) int {
	col := 0
	for len(text) > 0 && col < width {
		if text[0] < utf8.RuneSelf && (len(text) == 1 || text[1] < utf8.RuneSelf) {
			if col+1 == width && len(text) > 1 {
				break
			}
			screen.SetContent(x+col, y, rune(text[0]), nil, style)
			text = text[1:]
			col++
			continue
		}
		cluster, rest, w, _ := uniseg.FirstGraphemeClusterInString(text, -1)
		if col+w > width || col+w == width && rest != "" {
			break
		}
		if w > 0 {
			rs := []rune(cluster)
			for tail := 1; tail < w; tail++ {
				screen.SetContent(x+col+tail, y, ' ', nil, style)
			}
			screen.SetContent(x+col, y, rs[0], rs[1:], style)
			col += w
		}
		text = rest
	}
	if len(text) > 0 && width > 0 {
		for ; col < width-1; col++ {
			screen.SetContent(x+col, y, ' ', nil, style)
		}
		screen.SetContent(x+width-1, y, tview.SemigraphicsHorizontalEllipsis, nil, style)
		return width
	}
	return col
}
