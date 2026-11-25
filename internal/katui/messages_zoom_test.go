package katui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestMessagesZoomFollowsFocusedPane(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		exit  tcell.Key
		char  rune
	}{
		{"side-by-side", 140, tcell.KeyRune, 'z'},
		{"stacked", 80, tcell.KeyEscape, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
			first := retainedRecord(t, nil, []byte("first payload"))
			next := retainedRecord(t, nil, []byte("second payload"))
			next.id.offset = first.id.offset + 1
			u.post(func() {
				screen.SetSize(tc.width, 40)
				u.changeWorkspace(2)
				u.buffer(first)
				u.buffer(next)
				u.refreshView()
			})
			wait := func(label string, ready func() bool) {
				t.Helper()
				waitKeyboardState(t, u, label, ready)
			}
			key := func(r rune) { clusterKey(t, screen, tcell.KeyRune, r, 0) }
			fillsBody := func() bool {
				x, y, w, h := u.body.GetRect()
				tx, ty, tw, th := u.table.GetRect()
				return w == tc.width && h > 0 && x == tx && y == ty && w == tw && h == th
			}
			wait("split Messages did not show the first payload", func() bool {
				return u.table.HasFocus() && !fillsBody() && u.inspector.viewer.doc.text == "first payload"
			})
			key('z')
			wait("z on the records list did not give it the entire workspace body", func() bool {
				return fillsBody() && u.table.HasFocus() && u.modalDepth == 0
			})
			key('j')
			key(' ')
			wait("zoomed records lost navigation or checking", func() bool {
				return u.pinned == next && len(u.checked) == 1 && u.checked[0] == next
			})
			key('/')
			key('s')
			wait("search did not keep the records list zoomed", func() bool {
				return fillsBody() && u.search.HasFocus() && u.current().filter == "s"
			})
			clusterKey(t, screen, tcell.KeyEscape, 0, 0)
			wait("leaving search did not return to the zoomed list", func() bool {
				return fillsBody() && u.table.HasFocus() && u.pinned == next
			})
			// Give the delayed preview time to run if it was mistakenly scheduled.
			time.Sleep(200 * time.Millisecond)
			u.post(func() {
				if p := u.inspector; p != nil && (p.rendering || p.viewer.doc.text == "second payload") {
					t.Error("the hidden inspector rendered the newly selected record")
				}
			})
			clusterKey(t, screen, tc.exit, tc.char, 0)
			wait("restoring the split lost list focus or the selected payload", func() bool {
				return u.workspace == 2 && u.table.HasFocus() && !fillsBody() && u.pinned == next &&
					u.inspector.viewer.doc.text == "second payload"
			})
			key('i')
			key('z')
			wait("z from the inspector did not open the current payload fullscreen", func() bool {
				if len(u.modalPayloads) != 1 {
					return false
				}
				full := u.modalPayloads[0]
				x, y, w, h := full.dialog.root.GetRect()
				return x == 0 && y == 0 && w == tc.width && h == 40 && full.viewer.doc.text == "second payload"
			})
			key('z')
			wait("closing fullscreen did not return focus to the selected payload", func() bool {
				return u.modalDepth == 0 && u.inspector.viewer.HasFocus() && u.pinned == next
			})
		})
	}
}
