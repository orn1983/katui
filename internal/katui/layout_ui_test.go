package katui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestQuitStaysCenteredAndRestoresFocus(t *testing.T) {
	u, screen, done := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	key := func(r rune) { clusterKey(t, screen, tcell.KeyRune, r, 0) }
	u.post(func() {
		screen.SetSize(120, 40)
		u.focus(u.table)
	})
	key('q')
	checkFrame := func(width, height int) {
		t.Helper()
		var debug string
		defer func() {
			if t.Failed() {
				t.Log(debug)
			}
		}()
		waitKeyboardState(t, u, "quit prompt was not centered, readable, and focused on No", func() bool {
			_, page := u.pages.GetFrontPage()
			no, ok := u.app.GetFocus().(*tview.Button)
			x, y, w, h := page.GetRect()
			debug = fmt.Sprintf("frame=%d,%d %dx%d depth=%d focus=%T", x, y, w, h, u.modalDepth, u.app.GetFocus())
			if !ok || !strings.HasPrefix(no.GetLabel(), "No") || u.modalDepth != 1 {
				return false
			}
			if w != min(50, width) || h >= height || x != (width-w)/2 || y != (height-h)/2 {
				return false
			}
			var lines []string
			for i := y; i < y+h; i++ {
				var line strings.Builder
				for j := x + 1; j < x+w-1; j++ {
					ch, _, _, _ := screen.GetContent(j, i)
					line.WriteRune(ch)
				}
				lines = append(lines, strings.TrimSpace(line.String()))
			}
			content := strings.Join(lines, " ")
			debug += ": " + content
			return strings.Contains(lines[0], "Quit Katui?") && strings.Contains(content, quitMessage) &&
				strings.Contains(content, "Yes") && strings.Contains(content, "No")
		})
	}
	checkFrame(120, 40)
	var searchX, searchY int
	u.post(func() { searchX, searchY, _, _ = u.search.GetRect() })
	for _, b := range []tcell.ButtonMask{tcell.Button1, tcell.ButtonNone} {
		if err := screen.PostEvent(tcell.NewEventMouse(searchX+2, searchY, b, 0)); err != nil {
			t.Fatal(err)
		}
	}
	u.post(func() { screen.SetSize(34, 18) })
	checkFrame(34, 18)
	key('n')
	waitKeyboardState(t, u, "No did not return to the topic list", func() bool { return u.modalDepth == 0 && u.table.HasFocus() })
	key('q')
	checkFrame(34, 18)
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitKeyboardState(t, u, "Esc did not restore focus", func() bool { return u.modalDepth == 0 && u.table.HasFocus() })
	key('q')
	checkFrame(34, 18)
	key('y')
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Yes did not quit")
	}
}

func TestTreeDividerInSplitAndFullscreen(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	rec := retainedRecord(t, nil, []byte(`{"order":{"id":42}}`))
	var p *payloadPane
	u.post(func() {
		screen.SetSize(120, 40)
		u.changeWorkspace(2)
		u.buffer(rec)
		u.refreshView()
		p = u.inspector
		p.changeMode("tree")
	})
	checkDivider := func() {
		t.Helper()
		waitKeyboardState(t, u, "expected a horizontal divider between tree and node text", func() bool {
			if p.rendering || !p.showingTree() || !strings.Contains(p.nodeView.doc.text, "42") {
				return false
			}
			_, ty, _, th := p.tree.GetRect()
			x, y, w, h := p.nodeView.GetInnerRect()
			if ty+th != y-1 || h < 1 || w < 2 {
				return false
			}
			for j := x; j < x+w; j++ {
				ch, _, _, _ := screen.GetContent(j, y-1)
				if ch != tview.Borders.Horizontal {
					return false
				}
			}
			return true
		})
	}
	checkDivider()
	u.post(func() { p = u.fullscreenRecord() })
	checkDivider()
	u.post(func() { p.changeMode("raw") })
	waitKeyboardState(t, u, "raw mode did not reclaim the tree and divider space", func() bool {
		cx, cy, cw, ch := p.content.GetInnerRect()
		vx, vy, vw, vh := p.viewer.GetRect()
		return !p.rendering && p.viewer.doc.text == string(rec.value) && cx == vx && cy == vy && cw == vw && ch == vh
	})
}
