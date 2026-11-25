package katui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestPartitionToggleKeepsLabelAligned(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(screen.Fini)
	s := newPartitionSelector(&ui{})
	s.ids = []partKey{{"events", 0}}
	s.list.SetRect(0, 0, 40, 5)
	s.draw()
	s.list.Draw(screen)
	before, _, _ := s.list.GetCell(0, 1).GetLastPosition()
	s.toggle(0)
	s.list.Draw(screen)
	after, _, _ := s.list.GetCell(0, 1).GetLastPosition()
	if before <= 0 || before != after || !s.checked[s.ids[0]] {
		t.Fatalf("partition label moved from column %d to %d on toggle (selected %v)", before, after, s.checked[s.ids[0]])
	}
}

func TestConsumeStartsAtExactOffsetFromFilter(t *testing.T) {
	b, cluster, producer := migrationBroker(t)
	seedMigrationOffsets(t, t.Context(), b, producer)
	metadata, err := b.metadata(t.Context(), migrationTopic)
	if err != nil {
		t.Fatal(err)
	}
	u, screen, _ := startClusterUI(t, nil, "-b", cluster.ListenAddrs()[0])
	if !u.post(func() {
		u.metadata = metadata
		u.consume(migrationTopic, 0)
	}) {
		t.Fatal("could not open Consume")
	}
	key := func(k tcell.Key, r rune, m tcell.ModMask) { clusterKey(t, screen, k, r, m) }
	key(tcell.KeyRune, 's', tcell.ModAlt)
	key(tcell.KeyEnd, 0, 0)
	key(tcell.KeyEnter, 0, 0)
	key(tcell.KeyRune, 'e', tcell.ModAlt)
	waitKeyboardState(t, u, "exact offset did not expose its input field", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetLabel() == buttonLabel("Offset per partition", "Alt+E")
	})
	u.post(func() { u.app.GetFocus().(*tview.InputField).SetText("1") })
	key(tcell.KeyRune, 'v', tcell.ModAlt)
	waitKeyboardState(t, u, "could not focus value filter", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetLabel() == buttonLabel("Value filter", "Alt+V")
	})
	for _, r := range "sec" {
		key(tcell.KeyRune, r, 0)
	}
	waitKeyboardState(t, u, "the value filter did not receive its text", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetText() == "sec"
	})
	key(tcell.KeyRune, 'c', tcell.ModAlt)
	waitKeyboardState(t, u, "Alt+C from the value filter did not read offset 1", func() bool {
		return u.modalDepth == 0 && u.workspace == 2 && len(u.records) == 1 &&
			u.records[0].id.offset == 1 && u.readOptions.value == "sec" && u.readOptions.start == startOffset
	})
	key(tcell.KeyRune, 'x', 0)
	waitKeyboardState(t, u, "reader could not be stopped after the first fetch", func() bool { return u.readerState.state == "STOPPED" })
}

func TestMessagesFollowAndBulkSelection(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	if !u.post(func() { u.changeWorkspace(2) }) {
		t.Fatal("Messages did not open")
	}
	appendRecord := func(offset int64) {
		t.Helper()
		record, err := saveRecord(&kgo.Record{Topic: migrationTopic, Offset: offset, Value: []byte("message")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !u.post(func() {
			u.buffer(record)
			u.refreshView()
		}) {
			t.Fatal("record did not reach Messages")
		}
	}
	waitRecord := func(message string, offset int64, following bool) {
		t.Helper()
		box := "[ ] Follow"
		if following {
			box = "[x] Follow"
		}
		waitKeyboardState(t, u, message, func() bool {
			activeStyle := "[black:" + u.accent.String() + ":b]" + tview.Escape("[x] Follow")
			colored := strings.Contains(u.controls.GetText(false), activeStyle)
			return u.pinned != nil && u.pinned.id.offset == offset && u.followLatest == following &&
				colored == following && strings.Contains(u.controls.GetText(true), box)
		})
	}
	appendRecord(0)
	appendRecord(1)
	waitRecord("Messages moved off the first record by default", 0, false)
	clusterKey(t, screen, tcell.KeyRune, 'f', 0)
	waitRecord("Follow did not jump to the newest record", 1, true)
	appendRecord(2)
	waitRecord("Follow did not pick up the incoming record", 2, true)
	clusterKey(t, screen, tcell.KeyUp, 0, 0)
	waitRecord("moving to an older record did not turn off Follow", 1, false)
	clusterKey(t, screen, tcell.KeyRune, 'f', 0)
	waitRecord("Follow did not resume", 2, true)
	clusterKey(t, screen, tcell.KeyRune, 'f', 0)
	waitRecord("Follow did not turn off", 2, false)
	appendRecord(3)
	waitRecord("incoming record moved the selection after Follow was turned off", 2, false)

	clusterKey(t, screen, tcell.KeyRune, 'f', 0)
	waitRecord("Follow did not catch up after holding a record", 3, true)
	clusterKey(t, screen, tcell.KeyRune, 'i', 0)
	waitKeyboardState(t, u, "i did not enter the payload", func() bool { return u.inspector.viewer.HasFocus() })
	clusterKey(t, screen, tcell.KeyRune, ' ', 0)
	waitRecord("Cursor mode did not clear the Follow indicator", 3, false)
	clusterKey(t, screen, tcell.KeyRune, 'b', 0)
	waitKeyboardState(t, u, "b did not return to the records", u.table.HasFocus)

	clusterKey(t, screen, tcell.KeyRune, 'a', 0)
	waitKeyboardState(t, u, "Select all did not check every visible record", func() bool {
		return len(u.checked) == 4 && strings.Contains(u.controls.GetText(true), "a Select all")
	})
	clusterKey(t, screen, tcell.KeyRune, ' ', 0)
	waitKeyboardState(t, u, "Space did not toggle just the current record", func() bool {
		return len(u.checked) == 3 && u.isChecked(u.pinned.id) == -1
	})
	clusterKey(t, screen, tcell.KeyRune, 'n', 0)
	waitKeyboardState(t, u, "Unselect all did not clear the checked records", func() bool {
		return len(u.checked) == 0 && u.checkedBytes == 0 && strings.Contains(u.controls.GetText(true), "n Unselect all")
	})
}

func TestInspectorSelectionKeepsOriginalRecord(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"mouse drag", "first"},
		{"select all", "first payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSH_CONNECTION", "test") // Use the simulated terminal clipboard.
			screen := clipboardScreen{tcell.NewSimulationScreen("UTF-8"), make(chan string, 1)}
			u, _ := startClusterUIWithScreen(t, nil, screen, "-b", "127.0.0.1:1")
			original := retainedRecord(t, nil, []byte("first payload"))
			arrival := retainedRecord(t, nil, []byte("second payload"))
			arrival.id.offset = 1
			u.post(func() {
				screen.SetSize(150, 45)
				u.changeWorkspace(2)
				u.buffer(original)
				u.refreshView()
			})
			clusterKey(t, screen, tcell.KeyRune, 'f', 0)
			clusterKey(t, screen, tcell.KeyRune, 'i', 0)
			var x, y int
			waitKeyboardState(t, u, "inspector did not open with Follow enabled", func() bool {
				view := u.inspector.viewer
				var w, h int
				x, y, w, h = view.GetInnerRect()
				return u.followLatest && view.HasFocus() && view.doc.text == "first payload" && w >= 6 && h > 0
			})

			if tc.name == "mouse drag" {
				if err := screen.PostEvent(tcell.NewEventMouse(x, y, tcell.Button1, 0)); err != nil {
					t.Fatal(err)
				}
				waitKeyboardState(t, u, "drag did not pin the record on mouse-down", func() bool {
					return u.inspector.viewer.dragging && !u.followLatest
				})
			} else {
				clusterKey(t, screen, tcell.KeyCtrlA, 0, tcell.ModCtrl)
				waitKeyboardState(t, u, "select all did not select the first payload", func() bool {
					return u.inspector.viewer.selectedText() == tc.want && !u.followLatest
				})
			}

			u.post(func() { u.buffer(arrival); u.refreshView() })
			if tc.name == "mouse drag" {
				for _, button := range []tcell.ButtonMask{tcell.Button1, tcell.ButtonNone} {
					if err := screen.PostEvent(tcell.NewEventMouse(x+5, y, button, 0)); err != nil {
						t.Fatal(err)
					}
				}
			}
			clusterKey(t, screen, tcell.KeyCtrlC, 0, tcell.ModCtrl)
			select {
			case copied := <-screen.copied:
				if copied != tc.want {
					t.Fatalf("copied %q after a new arrival, want %q", copied, tc.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("selected payload did not reach the clipboard")
			}
			clusterKey(t, screen, tcell.KeyRune, 'f', 0)
			waitKeyboardState(t, u, "Follow did not resume at the buffered arrival", func() bool {
				return u.followLatest && u.pinned == arrival && u.inspector.viewer.doc.text == "second payload"
			})
		})
	}
}

func TestReaderBuffersBehindModalAndRestoresInspectorFocus(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	first := retainedRecord(t, nil, []byte(`{"first":1}`))
	next := retainedRecord(t, nil, []byte(`{"next":2}`))
	next.id.offset = 1
	u.post(func() {
		u.changeWorkspace(2)
		u.sessionProfiles[first.id.Topic] = profile{Mode: "tree"}
		u.buffer(first)
		u.refreshView()
	})
	waitKeyboardState(t, u, "first tree not rendered", func() bool { return u.inspector.showingTree() })
	u.post(func() {
		u.followLatest = true
		u.inspector.focusViewer()
		u.palette()
		u.buffer(next)
		u.readerViewDirty.Store(true)
		screen.SetSize(120, 36)
	})
	waitKeyboardState(t, u, "resize or an arriving record redrew the hidden inspector", func() bool {
		return u.width == 120 && !u.resizePending.Load() && u.modalDepth == 1 && !u.readerViewDirty.Load() && u.pinned == first && len(u.records) == 2
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitKeyboardState(t, u, "closing Help did not focus the latest record's tree", func() bool {
		return u.modalDepth == 0 && u.pinned == next && u.inspector.showingTree() && u.inspector.tree.HasFocus()
	})

	// A new record can arrive just before a resize, between reader refreshes.
	// Replacing a focused tree during the draw itself would deadlock tview.
	last := retainedRecord(t, nil, []byte(`{"last":3}`))
	last.id.offset = 2
	ready := make(chan struct{}, 1)
	u.post(func() {
		u.buffer(last)
		screen.SetSize(80, 28)
		u.app.SetAfterDrawFunc(func(tcell.Screen) {
			if u.width == 80 && u.pinned == last && u.inspector.showingTree() && u.inspector.tree.HasFocus() {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		})
	})
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("resizing the focused inspector blocked the UI or lost tree focus")
	}
}

func TestNestedPayloadDialogRestoresCurrentFormat(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	record := retainedRecord(t, nil, []byte(`{"first":1}`))
	var full *payloadPane
	u.post(func() {
		full = u.payloadDialog(true, []*storedRecord{record}, "tree")
	})
	waitKeyboardState(t, u, "fullscreen tree did not render", func() bool { return full.showingTree() })
	u.post(func() {
		u.app.SetFocus(full.nodeView)
		full.commands()
		full.changeMode("raw")
	})
	waitKeyboardState(t, u, "new format did not render behind Commands", func() bool {
		return u.modalDepth == 2 && !full.rendering && full.viewer.doc.text != ""
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitKeyboardState(t, u, "focus returned to discarded tree-node text", func() bool {
		return u.modalDepth == 1 && full.viewer.HasFocus()
	})
}

func TestClearingInspectorDiscardsTreeAndRestoresFocus(t *testing.T) {
	u, _, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	record := retainedRecord(t, nil, []byte(`{"order":{"id":42}}`))
	u.post(func() {
		u.changeWorkspace(2)
		u.buffer(record)
		u.refreshView()
		u.inspector.changeMode("tree")
		u.inspector.focusViewer()
	})
	waitKeyboardState(t, u, "tree was not rendered and focused", func() bool {
		p := u.inspector
		return p.tree.HasFocus() && p.tree.GetRoot() != nil && p.tree.GetRowCount() > 1
	})
	u.post(u.clearBuffer)
	waitKeyboardState(t, u, "cleared inspector kept the previous decoded tree or a hidden focus", func() bool {
		p := u.inspector
		return p.tree.GetRoot() == nil && p.tree.GetRowCount() == 0 && p.nodeView.doc.text == "" && p.viewer.HasFocus()
	})
}

func TestMessagesStayResponsiveWhileSpillStorageIsBusy(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	dir := t.TempDir()
	store := newPayloadStore(dir, payloadQuota)
	record := retainedRecord(t, store, []byte(strings.Repeat("x", 65_537)))
	if !u.post(func() {
		u.changeWorkspace(2)
		u.store = store
		u.buffer(record)
		u.refreshView()
	}) {
		t.Fatal("Messages did not open")
	}
	waitKeyboardState(t, u, "initial payload did not finish rendering", func() bool {
		return len(u.payloadJobs) == 0 && u.pinned == record
	})

	// A slow spill write holds this lock. Clearing a buffered spill used to
	// wait here on the terminal thread before it could process another key.
	store.mu.Lock()
	locked := true
	defer func() {
		if locked {
			store.mu.Unlock()
		}
	}()
	cleared := make(chan bool, 1)
	go func() { cleared <- u.post(u.clearBuffer) }()
	select {
	case ok := <-cleared:
		if !ok {
			t.Fatal("terminal stopped instead of clearing records")
		}
	case <-time.After(time.Second):
		t.Fatal("clearing the buffer blocked behind spill I/O")
	}
	clusterKey(t, screen, tcell.KeyRune, 'i', 0)
	focused := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			var viewerFocused bool
			if !u.post(func() { viewerFocused = u.inspector.viewer.HasFocus() }) {
				break
			}
			if viewerFocused {
				focused <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		focused <- false
	}()
	select {
	case ok := <-focused:
		if !ok {
			t.Fatal("i did not move focus to the payload while storage was busy")
		}
	case <-time.After(time.Second):
		t.Fatal("terminal did not process input while storage was busy")
	}
	store.mu.Unlock()
	locked = false
	waitKeyboardState(t, u, "spill was not reclaimed after disk I/O resumed", func() bool { return store.used.Load() == 0 })
}
