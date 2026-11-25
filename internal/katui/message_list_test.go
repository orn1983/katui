package katui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestListSnippetPreservesRuneBoundaries(t *testing.T) {
	for _, value := range []string{
		"abcd", "abcde", "😀😀😀😀", "😀😀😀😀界", "a\xff界\xff", "a\xff界\xffa", strings.Repeat("界", 10_000),
	} {
		if got, want := listSnippet([]byte(value), 4), shortened(value, 4); got != want {
			t.Errorf("list snippet %q, want %q", got, want)
		}
	}
}

func TestMessageListDrawKeepsMouseGeometryAndStyles(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	u.workspace = 2
	u.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	u.table.SetSelectionChangedFunc(func(row, _ int) { u.selectRow(row) })
	u.table.SetBorder(true)
	u.table.SetRect(2, 1, 160, 8)
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(170, 12)
	list := &messageList{u.table, u}
	for i := range 20 {
		r := &storedRecord{id: recordID{partKey{"topic", 0}, int64(i)}, value: []byte("[red]界é")}
		u.buffer(r)
	}
	u.addCheck(u.records[18])
	u.renderTable(u.recordRows())
	u.table.InputHandler()(tcell.NewEventKey(tcell.KeyEnd, 0, 0), nil)
	list.Draw(screen)

	x, y, w, h := u.table.GetInnerRect()
	mouse := tcell.NewEventMouse(x+w-1, y+h-2, tcell.Button1, 0)
	list.MouseHandler()(tview.MouseLeftClick, mouse, func(tview.Primitive) {})
	list.Draw(screen)
	if u.pinned != u.records[18] {
		t.Fatal("click on a scrolled record selected the wrong row")
	}
	_, _, style, _ := screen.GetContent(x+w-1, y+h-2)
	_, bg, attrs := style.Decompose()
	if bg != u.accent || attrs&tcell.AttrUnderline == 0 {
		t.Fatal("checked selection did not highlight the full line")
	}
	_, widths, _ := u.recordRows()
	previewColumn := x
	for _, w := range widths[:len(widths)-1] {
		previewColumn += w + 1
	}
	for offset, want := range []rune("[red]界") {
		r, _, _, _ := screen.GetContent(previewColumn+offset, y+h-2)
		if r != want {
			t.Fatalf("preview glyph %d = %q, want %q", offset, r, want)
		}
	}
	r, combining, _, _ := screen.GetContent(previewColumn+7, y+h-2)
	if r != 'e' || string(combining) != "\u0301" {
		t.Fatalf("lost combining character: base %q, combining %q", r, combining)
	}

	// Clip the viewport just after the first half of a double-width glyph.
	list.SetRect(2, 1, previewColumn-x+6+2, 8)
	list.Draw(screen)
	r, _, _, _ = screen.GetContent(previewColumn+5, y+h-2)
	if r != tview.SemigraphicsHorizontalEllipsis {
		t.Fatalf("clipped wide character must show an ellipsis, got %q", r)
	}
}
