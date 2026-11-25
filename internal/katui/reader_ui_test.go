package katui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

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
	key(tcell.KeyRune, 'g', tcell.ModAlt)
	waitKeyboardState(t, u, "Alt+G from the value filter did not read offset 1", func() bool {
		return u.modalDepth == 0 && u.workspace == 2 && len(u.records) == 1 &&
			u.records[0].id.offset == 1 && u.readOptions.value == "sec" && u.readOptions.start == startOffset
	})
	key(tcell.KeyRune, 'x', 0)
	waitKeyboardState(t, u, "reader could not be stopped after the first fetch", func() bool { return u.readerState.state == "STOPPED" })
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
