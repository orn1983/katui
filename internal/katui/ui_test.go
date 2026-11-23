package katui

import (
	"context"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kadm"
)

func TestUIUpdatesAndModalCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	u := newUI(ctx, &broker{options: options{noCache: true, interval: time.Second}})
	u.workspace = 2 // The message workspace does not poll a broker.
	screen := tcell.NewSimulationScreen("UTF-8")
	u.app.SetScreen(screen)
	stopped := make(chan error, 1)
	go func() { stopped <- u.run() }()

	clipboardKey := make(chan struct{}, 1)
	posted := make(chan bool, 1)
	go func() {
		posted <- u.post(func() {
			text := tview.NewTextView()
			dialog := u.dialog("Payload", text, text)
			dialog.keymap(func(event *tcell.EventKey) bool {
				if event.Key() == tcell.KeyCtrlC {
					clipboardKey <- struct{}{}
					return true
				}
				return false
			})
		})
	}()
	select {
	case ok := <-posted:
		if !ok {
			t.Fatal("the running UI rejected an update")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the running UI did not apply the update")
	}
	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clipboardKey:
	case err := <-stopped:
		t.Fatalf("Ctrl+C stopped the app while the modal was open: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C did not reach the modal")
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the UI did not stop on cancellation")
	}
	go func() {
		posted <- u.post(func() { t.Error("an update ran after UI shutdown") })
	}()
	select {
	case ok := <-posted:
		if ok {
			t.Fatal("the stopped UI reported an applied update")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped UI blocked a caller")
	}
}

func TestEraseTopicPreservesInFlightMetadata(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	metadata := kadm.TopicDetails{"old": {Topic: "old"}, "other": {Topic: "other"}}
	u.metadata = metadata
	part := partKey{"old", 1}
	u.sizes[part] = 42

	u.eraseTopic("old")

	if _, exists := metadata["old"]; !exists {
		t.Fatal("erase mutated a metadata snapshot still held by a sampler")
	}
	if _, exists := u.metadata["old"]; exists {
		t.Fatal("erased topic remains in the current metadata")
	}
	if _, exists := u.metadata["other"]; !exists {
		t.Fatal("erase removed an unrelated topic")
	}
	if _, exists := u.sizes[part]; exists {
		t.Fatal("erased topic's disk size remains")
	}
}

func TestGroupPartitionWithoutTopicListsMatchingTopics(t *testing.T) {
	b := &broker{options: options{noCache: true, group: "g", partition: 2}}
	u := newUI(t.Context(), b)
	u.groups["g"] = groupSample{Partitions: []partition{
		{partKey: partKey{"a", 1}},
		{partKey: partKey{"a", 2}},
		{partKey: partKey{"b", 2}},
	}}

	loc := u.current()
	_, _, rows := u.browserRows(loc)
	if loc.kind != "group" || len(rows) != 2 || rows[0].topic != "a" || rows[1].topic != "b" {
		t.Fatalf("expected both topics with partition 2, got %s %+v", loc.kind, rows)
	}
}
