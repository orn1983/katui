package katui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/encoding/protowire"
)

func retainedRecord(t *testing.T, store *payloadStore, value []byte) *storedRecord {
	t.Helper()
	rec, err := saveRecord(&kgo.Record{Topic: "events", Value: value}, store)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func treeContains(node *payloadNode, text string) bool {
	if node == nil {
		return false
	}
	if strings.Contains(node.search, text) {
		return true
	}
	for _, child := range node.children {
		if treeContains(child, text) {
			return true
		}
	}
	return false
}

func TestLargePayloadOverrideAcrossFormats(t *testing.T) {
	const marker = "needle past the old size limit"
	dir := t.TempDir()
	schema := filepath.Join(dir, "message.proto")
	if err := os.WriteFile(schema, []byte(`syntax = "proto3"; message Record { string content = 1; }`), 0600); err != nil {
		t.Fatal(err)
	}
	u := newUI(t.Context(), &broker{})
	profiles := map[string]profile{"events": {delimiter: ",", header: true, Protobuf: &protoProfile{Path: schema, Message: "Record", Framing: "raw"}}}
	for _, mode := range []string{"pretty", "tree", "csv-pivot", "csv-table", "proto-pretty", "proto-tree"} {
		t.Run(mode, func(t *testing.T) {
			size := maxStructuredInput + 1024
			if mode == "pretty" {
				size = maxRenderedPayload + 1024
			}
			content := strings.Repeat("x", size) + marker
			value := []byte(`{"content":"` + content + `"}`)
			if csvMode(mode) {
				value = []byte("content\n" + content + "\n")
			} else if protoMode(mode) {
				value = protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), content)
			}
			store := newPayloadStore(t.TempDir(), payloadQuota)
			defer store.close()
			records := []*storedRecord{retainedRecord(t, store, value)}
			preview := u.preparePayload(t.Context(), records, profiles, mode, false, false, u.b.inspectionLimits(false))
			if !preview.incomplete || strings.Contains(preview.text, marker) {
				t.Fatalf("ordinary preview did not truncate %s", mode)
			}
			full := u.preparePayload(t.Context(), records, profiles, mode, true, false, u.b.inspectionLimits(false))
			if !full.incomplete || !full.canExpand || !strings.Contains(full.warning, "structured input exceeds") || full.tree != nil {
				t.Fatalf("ordinary Full view accepted an oversized %s payload: %s", mode, full.warning)
			}
			load := u.preparePayload(t.Context(), records, profiles, mode, true, false, u.b.inspectionLimits(true))
			if load.incomplete || !strings.Contains(load.text, marker) || !strings.Contains(load.text, "content") {
				t.Fatalf("explicit load did not format the entire %s payload: %s", mode, load.warning)
			}
			if treeMode(mode) && !treeContains(load.tree, marker) {
				t.Fatalf("full %s tree did not index the last value", mode)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r := u.preparePayload(ctx, []*storedRecord{{value: []byte("{}")}}, profiles, "pretty", true, false, u.b.inspectionLimits(true)); r.text != "" {
		t.Fatal("canceled render returned a payload")
	}
}

func TestPayloadLimitsAreConfigurable(t *testing.T) {
	o, err := arguments([]string{"--structured-input-mib", "6", "--rendered-output-mib", "12", "--payload-storage-mib", "0"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.inspectionLimits(false) != (payloadLimits{6 << 20, 12 << 20}) || !o.payloadStorageSet || o.payloadStorage != 0 {
		t.Fatalf("unexpected configured limits %+v", o)
	}
	for _, args := range [][]string{
		{"--structured-input-mib", "0"}, {"--rendered-output-mib", "129"}, {"--payload-storage-mib", "-1"},
	} {
		if _, err := arguments(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestQuotaCopyAndExport(t *testing.T) {
	value := []byte(strings.Repeat("z", 65536) + "\r\nfull ß value\n")
	store := newPayloadStore(t.TempDir(), int64(len(value)))
	defer store.close()
	first := retainedRecord(t, store, value)
	next := retainedRecord(t, store, value)
	if payloadAvailability(first) != "Complete" || payloadAvailability(next) != "Preview only" ||
		store.used.Load() != int64(len(value)) || store.missed.Load() != 1 {
		t.Fatal("complete/preview status or usage does not reflect the full quota")
	}
	if got, err := completeValueText(t.Context(), []*storedRecord{first}); err != nil || got != string(value) {
		t.Fatalf("complete text copy returned %d bytes with error %v", len(got), err)
	}
	if got, err := completeValueText(t.Context(), []*storedRecord{first, next}); !errors.Is(err, errPayloadQuota) || got != "" {
		t.Fatalf("copy accepted an incomplete value: %d bytes, %v", len(got), err)
	}
	path := filepath.Join(t.TempDir(), "value")
	if err := exportRecord(t.Context(), next, path); !errors.Is(err, errPayloadQuota) {
		t.Fatalf("export accepted a preview: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("export created a file for an unavailable value: %v", err)
	}
	if err := exportRecord(t.Context(), first, path); err != nil {
		t.Fatal(err)
	}
	if err := exportRecord(t.Context(), first, path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("export overwrote existing file: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("export returned %d bytes with error %v", len(got), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := exportRecord(ctx, first, path+".cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export returned %v", err)
	}
	u := newUI(t.Context(), &broker{})
	u.workspace, u.store, u.status = 2, store, tview.NewTextView()
	u.updateStatus()
	if status := u.status.GetText(false); !strings.Contains(status, "WARNING: payload quota reached (1 preview-only)") || !strings.Contains(status, "Temp 64.0 KiB / 64.0 KiB") {
		t.Fatalf("Messages footer lacks quota warning or usage: %q", status)
	}
	release := u.holdPayloads([]*storedRecord{first})
	u.release(first)
	if store.used.Load() == 0 {
		t.Fatal("an in-flight copy/export lost ownership of its payload")
	}
	if err := os.Truncate(first.fullPath, 65536); err != nil {
		t.Fatal(err)
	}
	if _, err := completeValueText(t.Context(), []*storedRecord{first}); err == nil {
		t.Fatal("copy accepted a short retained file")
	}
	release()
	if store.used.Load() != 0 || !retainedRecord(t, store, value).fullAvailable() {
		t.Fatal("space wasn't recovered once the complete value was released")
	}
	binary := retainedRecord(t, store, []byte{0, 255})
	if _, err := completeValueText(t.Context(), []*storedRecord{binary}); err == nil {
		t.Fatal("binary copy silently changed the original bytes")
	}
	if err := exportRecord(t.Context(), binary, path+".bin"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path + ".bin"); err != nil || !bytes.Equal(got, []byte{0, 255}) {
		t.Fatalf("binary export changed bytes: %v, %v", got, err)
	}
}

func TestPayloadTextSelectsWithKeyboardAndMouse(t *testing.T) {
	var copied string
	view := newPayloadText(func(text string) { copied = text })
	doc, _ := indexPayloadText(t.Context(), "abcéfg\nsecond line")
	view.setDocument(doc)
	view.SetRect(0, 0, 20, 3)
	key := func(k tcell.Key, mod tcell.ModMask) {
		view.InputHandler()(tcell.NewEventKey(k, 0, mod), nil)
	}
	key(tcell.KeyHome, 0)
	for range 4 {
		key(tcell.KeyRight, tcell.ModShift)
	}
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != "abcé" {
		t.Fatalf("keyboard selection: copied %q", copied)
	}
	mouse := func(x, y int, action tview.MouseAction, button tcell.ButtonMask) {
		view.MouseHandler()(action, tcell.NewEventMouse(x, y, button, 0), func(tview.Primitive) {})
	}
	mouse(0, 1, tview.MouseLeftDown, tcell.Button1)
	mouse(6, 1, tview.MouseMove, tcell.Button1)
	mouse(6, 1, tview.MouseLeftUp, tcell.ButtonNone)
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != "second" {
		t.Fatalf("mouse selection: copied %q", copied)
	}
	key(tcell.KeyCtrlA, tcell.ModCtrl)
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != doc.text {
		t.Fatalf("select all: copied %q", copied)
	}
}

func TestClipboardLocalAndTerminalPaths(t *testing.T) {
	var requested string
	terminal := func(data []byte) bool { requested = string(data); return true }
	if status, err := deliverClipboard(t.Context(), "é", "", terminal); err != nil ||
		requested != "é" || !strings.Contains(status, "OSC 52") {
		t.Fatalf("terminal clipboard requested %q, status %q, error %v", requested, status, err)
	}
	large := strings.Repeat("a", terminalClipboardBytes+1)
	requested = ""
	if _, err := deliverClipboard(t.Context(), large, "", terminal); err == nil || requested != "" {
		t.Fatal("oversized terminal copy sent a truncated request")
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	destination := filepath.Join(dir, "clipboard")
	t.Setenv("KATUI_TEST_CLIPBOARD", destination)
	pbcopy := filepath.Join(dir, "pbcopy")
	if err := os.WriteFile(pbcopy, []byte("#!/bin/sh\ncat > \"$KATUI_TEST_CLIPBOARD\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := deliverClipboard(t.Context(), large, pbcopy, terminal); err != nil || requested != "" {
		t.Fatalf("local clipboard did not accept a large value: %v", err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != large {
		t.Fatalf("local command received %d bytes with error %v", len(got), err)
	}
	if _, err := deliverClipboard(t.Context(), "fallback", pbcopy+".missing", terminal); err != nil || requested != "fallback" {
		t.Fatalf("failed local copy did not fall back to the terminal: %v", err)
	}
}

type clipboardScreen struct {
	tcell.SimulationScreen
	copied chan string
}

func (s clipboardScreen) SetClipboard(data []byte) { s.copied <- string(data) }

func TestFullViewConfirmationSearchAndCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := &broker{options: options{brokers: []string{"localhost:1"}, noCache: true, interval: time.Second}}
	u := newUI(ctx, b)
	u.workspace = 2
	t.Setenv("SSH_CONNECTION", "test")
	screen := clipboardScreen{tcell.NewSimulationScreen("UTF-8"), make(chan string, 1)}
	u.app.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- u.run() }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("simulated UI did not stop")
		}
	}()
	store := newPayloadStore(t.TempDir(), payloadQuota)
	defer store.close()
	marker := "needle-after-preview"
	record := retainedRecord(t, store, []byte(`{"content":"`+strings.Repeat("x", maxRenderedPayload+1024)+marker+`"}`))
	var p *payloadPane
	if !u.post(func() {
		p = u.payloadDialog(true, []*storedRecord{record}, "pretty")
	}) {
		t.Fatal("couldn't open Full view")
	}
	waitFor := func(message string, condition func() bool) {
		t.Helper()
		var status, warning string
		var expanded, rendering bool
		var depth int
		deadline := time.Now().Add(5 * time.Second)
		for ctx.Err() == nil && time.Now().Before(deadline) {
			ok := false
			if !u.post(func() {
				ok = condition()
				expanded, rendering, depth = p.expanded, p.rendering, u.modalDepth
				status, warning = p.status.GetText(false), p.result.warning
			}) {
				break
			}
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: expanded=%t, rendering=%t, modals=%d, status=%q, warning=%q", message, expanded, rendering, depth, status, warning)
	}
	waitFor("default render did not reach the size warning", func() bool { return !p.rendering && p.result.canExpand })
	u.post(func() {
		p.copyView()
		if !strings.Contains(p.dialog.status.GetText(false), "Rendered view is incomplete") {
			t.Error("Copy View did not explain why it refused the truncated view")
		}
	})
	u.post(func() {
		p.refresh()
		p.dialog.cancel()
		if p.rendering || p.renderCancel != nil || p.dialog.closed || !p.result.incomplete {
			t.Error("Esc did not cancel just the loading operation")
		}
	})
	u.post(p.loadFull)
	u.post(func() {
		if p.expanded || u.modalDepth != 2 {
			t.Error("load started before confirmation")
		}
	})
	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'y', 0)); err != nil {
		t.Fatal(err)
	}
	waitFor("confirmed full render was truncated", func() bool { return p.expanded && !p.rendering && !p.result.incomplete })
	u.post(func() { p.search.SetText(marker) })
	waitFor("search did not jump past the old display boundary", func() bool {
		return p.viewer.match[0] > maxRenderedPayload && p.viewer.left > maxRenderedPayload
	})
	u.post(func() {
		p.focusViewer()
		p.viewer.selectBy(p.viewer.match[0], false)
		p.viewer.selectBy(p.viewer.match[1], true)
	})
	var left, anchor, cursor, pan int
	u.post(func() {
		left, anchor, cursor = p.viewer.left, p.viewer.anchor, p.viewer.cursor
		_, _, width, _ := p.viewer.GetInnerRect()
		pan = 8 + max(8, width/2)
	})
	for _, mod := range []tcell.ModMask{tcell.ModAlt, tcell.ModCtrl} {
		if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyLeft, 0, mod)); err != nil {
			t.Fatal(err)
		}
	}
	waitFor("Full view did not pan independently of its selection", func() bool {
		return p.viewer.left == left-pan && p.viewer.anchor == anchor && p.viewer.cursor == cursor
	})
	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-screen.copied:
		if text != marker {
			t.Fatalf("clipboard received %q", text)
		}
	case <-ctx.Done():
		t.Fatal("Ctrl+C did not copy the selected search result")
	}
}

func TestPayloadTextPansWithoutChangingSelection(t *testing.T) {
	var copied string
	view := newPayloadText(func(text string) { copied = text })
	content := strings.Repeat("0123456789", 40) + "\t" + strings.Repeat("字😀", 150) + "end"
	doc, _ := indexPayloadText(t.Context(), "short\n"+content)
	if doc.width != 1007 {
		t.Fatalf("document is %d display columns, want 1007", doc.width)
	}
	view.setDocument(doc)
	view.SetRect(0, 0, 40, 3)
	key := func(k tcell.Key, mod tcell.ModMask) {
		view.InputHandler()(tcell.NewEventKey(k, 0, mod), nil)
	}
	key(tcell.KeyDown, 0)
	key(tcell.KeyRight, tcell.ModAlt)
	if view.left != 8 || view.cursor != len("short\n") || view.anchor != view.cursor {
		t.Fatal("Alt+Right did not pan eight columns while keeping the unselected cursor")
	}
	// Cursor movement after a pan deliberately follows the cursor again.
	key(tcell.KeyRight, 0)
	key(tcell.KeyRight, tcell.ModShift)
	key(tcell.KeyRight, tcell.ModShift)
	anchor, cursor := view.anchor, view.cursor
	key(tcell.KeyLeft, tcell.ModAlt)
	key(tcell.KeyRight, tcell.ModAlt)
	key(tcell.KeyRight, tcell.ModCtrl)
	if view.left != 28 || view.anchor != anchor || view.cursor != cursor {
		t.Fatalf("pan changed selection or used the wrong width: left=%d, selection=%d–%d", view.left, view.anchor, view.cursor)
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	view.Draw(screen)
	if char, _, _, _ := screen.GetContent(0, 1); char != '8' || view.left != 28 {
		t.Fatalf("drawing snapped back or drew the wrong column: %q at left=%d", char, view.left)
	}
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != "12" {
		t.Fatalf("copy after pan returned %q", copied)
	}

	for range 100 {
		key(tcell.KeyRight, tcell.ModCtrl)
	}
	if view.left != 968 {
		t.Fatalf("right boundary = %d", view.left)
	}
	view.SetRect(0, 0, 60, 3)
	view.Draw(screen)
	if view.left != 948 {
		t.Fatalf("resizing did not clamp the viewport: %d", view.left)
	}
	key(tcell.KeyLeft, tcell.ModCtrl)
	if view.left != 918 {
		t.Fatalf("pan did not adapt to the new width: %d", view.left)
	}
	view.SetRect(0, 0, 12, 3)
	key(tcell.KeyLeft, tcell.ModCtrl)
	if view.left != 910 {
		t.Fatalf("narrow terminal did not retain the minimum eight-column pan: %d", view.left)
	}
	for range 130 {
		key(tcell.KeyLeft, tcell.ModCtrl)
	}
	if view.left != 0 {
		t.Fatalf("left boundary = %d", view.left)
	}

	view.left = 8
	mouse := func(x int, action tview.MouseAction, button tcell.ButtonMask) {
		view.MouseHandler()(action, tcell.NewEventMouse(x, 1, button, 0), func(tview.Primitive) {})
	}
	mouse(0, tview.MouseLeftDown, tcell.Button1)
	mouse(3, tview.MouseMove, tcell.Button1)
	mouse(3, tview.MouseLeftUp, tcell.ButtonNone)
	key(tcell.KeyLeft, tcell.ModShift)
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != "89" {
		t.Fatalf("mouse selection and Shift+Left after pan copied %q", copied)
	}
	key(tcell.KeyCtrlA, tcell.ModCtrl)
	key(tcell.KeyLeft, tcell.ModAlt)
	key(tcell.KeyCtrlC, tcell.ModCtrl)
	if copied != doc.text {
		t.Fatal("pan changed the Select All region")
	}
	start, end := view.line(1)
	point := doc.position(1, doc.text[start:end], 805, true)
	if point.cell != 804 || doc.text[start+point.byteOffset:start+point.byteOffset+3] != "字" {
		t.Fatalf("mixed Unicode and tab columns were misaligned: %+v", point)
	}
}
