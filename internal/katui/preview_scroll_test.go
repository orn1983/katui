package katui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestScrollingDefersPreviewAndCopiesCurrentRecord(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "test")
	screen := clipboardScreen{tcell.NewSimulationScreen("UTF-8"), make(chan string, 1)}
	u, _ := startClusterUIWithScreen(t, nil, screen, "-b", "127.0.0.1:1")
	records := make([]*storedRecord, 3)
	for i, value := range []string{"first", "second", "third"} {
		records[i] = retainedRecord(t, nil, []byte(value))
		records[i].id.offset = int64(i)
	}
	u.post(func() {
		u.changeWorkspace(2)
		for _, r := range records {
			u.buffer(r)
		}
		u.refreshView()
		if !u.inspector.rendering || u.inspector.previewTimer != nil {
			t.Error("the first visible record did not start rendering immediately")
		}
	})
	waitKeyboardState(t, u, "first preview never appeared", func() bool {
		return u.inspector.viewer.doc.text == "first"
	})
	u.post(func() {
		p := u.inspector
		for _, row := range []int{2, 3, 2} {
			u.table.Select(row, 0)
			if u.pinned != records[row-1] || len(p.records) != 1 || p.records[0] != u.pinned {
				t.Error("selection did not bind the highlighted record immediately")
			}
			if p.previewTimer == nil || p.rendering || len(u.payloadJobs) != 0 || p.viewer.doc.text != "" {
				t.Error("scrolling started a decoder or left the prior payload on screen")
			}
		}
	})
	waitKeyboardState(t, u, "settled selection did not render", func() bool {
		return u.inspector.viewer.doc.text == "second"
	})
	u.post(func() {
		u.table.Select(3, 0)
		u.inspector.copyView()
		if !u.inspector.rendering || u.inspector.previewTimer != nil {
			t.Error("Copy View did not start preparing the current record immediately")
		}
		u.table.Select(1, 0)
	})
	select {
	case got := <-screen.copied:
		if got != "third" {
			t.Fatalf("Copy View copied %q instead of the selected record", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pending preview never reached the clipboard")
	}
	waitKeyboardState(t, u, "the clipboard render replaced the new selection", func() bool {
		return u.inspector.viewer.doc.text == "first" && len(u.payloadJobs) == 0
	})
	u.post(func() {
		u.table.Select(2, 0)
		u.inspector.copyView()
		u.table.Select(3, 0)
		u.inspector.copyValue()
	})
	select {
	case got := <-screen.copied:
		if got != "third" {
			t.Fatalf("superseded render copied %q after a newer copy request", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("newer raw copy never reached the clipboard")
	}
	clusterKey(t, screen, tcell.KeyRune, 'z', 0)
	clusterKey(t, screen, tcell.KeyRune, 'k', 0)
	clusterKey(t, screen, tcell.KeyRune, copyViewKey, 0)
	select {
	case got := <-screen.copied:
		if got != "second" {
			t.Fatalf("zoomed list copied %q, want the highlighted second record", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Copy View from the zoomed list never reached the clipboard")
	}
	waitKeyboardState(t, u, "copying in list zoom displayed the inspector", func() bool {
		return u.tableZoom && u.table.HasFocus() && u.inspector.viewer.doc.text == ""
	})
	clusterKey(t, screen, tcell.KeyRune, 'z', 0)
	waitKeyboardState(t, u, "the split did not restore its current payload", func() bool {
		return u.inspector.viewer.doc.text == "second"
	})
	u.post(func() {
		u.search.SetText("no matching message")
		u.inspector.copyView()
		if u.pinned != nil || len(u.inspector.records) != 0 || u.inspector.viewer.doc.text != "" || u.message != "Select a record first" {
			t.Error("an empty filtered list still exposed the previous payload")
		}
		u.search.SetText("third")
	})
	waitKeyboardState(t, u, "a matching filter did not restore the selected preview", func() bool {
		return u.pinned == records[2] && u.inspector.viewer.doc.text == "third"
	})
}
