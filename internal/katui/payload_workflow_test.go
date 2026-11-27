package katui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
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
	for _, child := range node.childNodes() {
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
	u := newUI(t.Context(), &broker{options: options{structuredInput: 1 << 20, renderedOutput: 2 << 20}})
	profiles := map[string]profile{"events": {delimiter: ",", header: true, Protobuf: &protoProfile{Path: schema, Message: "Record", Framing: "raw"}}}
	for _, mode := range []string{"pretty", "tree", "csv-pivot", "csv-table", "proto-pretty", "proto-tree"} {
		t.Run(mode, func(t *testing.T) {
			size := u.b.structuredInput + 1024
			if mode == "pretty" {
				size = u.b.renderedOutput + 1024
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
			preview := u.preparePayload(t.Context(), records, profiles, mode, false, false, u.b.inspectionLimits(false, false))
			if !preview.incomplete || strings.Contains(preview.text, marker) {
				t.Fatalf("ordinary preview did not truncate %s", mode)
			}
			full := u.preparePayload(t.Context(), records, profiles, mode, true, false, u.b.inspectionLimits(true, false))
			if !full.incomplete || !full.canExpand || !strings.Contains(full.warning, "structured input exceeds") || full.tree != nil {
				t.Fatalf("ordinary Full view accepted an oversized %s payload: %s", mode, full.warning)
			}
			load := u.preparePayload(t.Context(), records, profiles, mode, true, false, u.b.inspectionLimits(true, true))
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
	if r := u.preparePayload(ctx, []*storedRecord{{value: []byte("{}")}}, profiles, "pretty", true, false, u.b.inspectionLimits(true, true)); r.text != "" {
		t.Fatal("canceled render returned a payload")
	}
}

func TestPayloadLimitsAreConfigurable(t *testing.T) {
	defaults, err := arguments(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []options{defaults, {}} {
		if limits := o.inspectionLimits(true, false); limits != (payloadLimits{8 << 20, 32 << 20}) {
			t.Fatalf("fullscreen defaults = %+v", limits)
		}
		if limits := o.inspectionLimits(false, false); limits != (payloadLimits{2 << 20, 8 << 20}) {
			t.Fatalf("inline preview limits = %+v", limits)
		}
		if limits := o.inspectionLimits(true, true); limits != (payloadLimits{math.MaxInt, math.MaxInt}) {
			t.Fatalf("expanded fullscreen limits = %+v", limits)
		}
	}
	o, err := arguments([]string{"--structured-input-mib", "256", "--rendered-output-mib", "512", "--payload-storage-mib", "0"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.inspectionLimits(true, false) != (payloadLimits{256 << 20, 512 << 20}) || !o.payloadStorageSet || o.payloadStorage != 0 {
		t.Fatalf("unexpected configured limits %+v", o)
	}
	for _, args := range [][]string{
		{"--structured-input-mib", "0"}, {"--rendered-output-mib", "9223372036854775807"}, {"--payload-storage-mib", "-1"},
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
	deadline := time.Now().Add(2 * time.Second)
	for store.used.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
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
	doc := indexPayloadText(t.Context(), "abcéfg\nsecond line")
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

func TestSelectionMovesByWholeCharacters(t *testing.T) {
	var copied string
	view := newPayloadText(func(text string) { copied = text })
	doc := indexPayloadText(t.Context(), "e\u0301 and more text")
	view.setDocument(doc)
	view.SetRect(0, 0, 12, 1)
	key := view.InputHandler()
	key(tcell.NewEventKey(tcell.KeyRight, 0, 0), nil)
	if view.left == 0 || view.cursor != 0 || view.mode != payloadScroll {
		t.Fatal("plain Right in Scroll should pan the viewport")
	}
	key(tcell.NewEventKey(tcell.KeyHome, 0, 0), nil)
	key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), nil)
	key(tcell.NewEventKey(tcell.KeyRune, 'v', 0), nil)
	key(tcell.NewEventKey(tcell.KeyRight, 0, 0), nil)
	key(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl), nil)
	if copied != "e\u0301" {
		t.Fatalf("selecting one character copied %q", copied)
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

func TestFullscreenSearchMatchKeys(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	record := retainedRecord(t, nil, []byte("N1 N2 N3"))
	var p *payloadPane
	u.post(func() { p = u.payloadDialog(true, []*storedRecord{record}, "raw") })
	clusterKey(t, screen, tcell.KeyRune, '/', 0)
	clusterKey(t, screen, tcell.KeyRune, 'N', tcell.ModShift)
	waitKeyboardState(t, u, "Find did not accept uppercase N as search text", func() bool {
		return p.search.GetText() == "N" && len(p.textMatches) == 3 && p.match == 0
	})
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	for _, step := range []struct {
		key  rune
		mod  tcell.ModMask
		want int
	}{
		{'N', tcell.ModShift, 2},
		{'N', 0, 1}, // Some terminals send the uppercase rune without a Shift modifier.
		{'p', 0, 0},
		{'n', 0, 1},
	} {
		clusterKey(t, screen, tcell.KeyRune, step.key, step.mod)
		waitKeyboardState(t, u, "wrong match after pressing "+string(step.key), func() bool {
			return p.viewer.HasFocus() && p.match == step.want
		})
	}
}

func TestFullViewSearchClearsObsoleteMatch(t *testing.T) {
	u, _, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	record := retainedRecord(t, nil, []byte("first payload"))
	var p *payloadPane
	u.post(func() {
		p = u.payloadDialog(true, []*storedRecord{record}, "raw")
		p.search.SetText("payload")
	})
	waitKeyboardState(t, u, "search did not highlight the match", func() bool { return p.viewer.match == [2]int{6, 13} })
	u.post(func() { p.search.SetText("missing") })
	waitKeyboardState(t, u, "unmatched query kept the obsolete highlight", func() bool {
		return p.status.GetText(true) == "No matches" && p.viewer.match == [2]int{}
	})
	u.post(func() { p.search.SetText("") })
	waitKeyboardState(t, u, "clearing search kept a stale match status", func() bool {
		return !strings.Contains(strings.ToLower(p.status.GetText(true)), "match")
	})
}

func TestFullViewConfirmationSearchAndCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := &broker{options: options{brokers: []string{"localhost:1"}, noCache: true, interval: time.Second, structuredInput: 1 << 20, renderedOutput: 2 << 20}}
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
	record := retainedRecord(t, store, []byte(`{"content":"`+strings.Repeat("x", b.renderedOutput+1024)+marker+`"}`))
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
	clusterKey(t, screen, tcell.KeyRune, 'l', 0)
	waitFor("l did not pan right in Fullscreen", func() bool { return p.viewer.left == 8 && p.confirmLoad == nil })
	clusterKey(t, screen, tcell.KeyRune, 'h', 0)
	waitFor("h did not pan left in Fullscreen", func() bool { return p.viewer.left == 0 })
	u.post(func() {
		if !p.viewer.HasFocus() {
			t.Error("Full view did not open on the payload text")
		}
	})
	u.post(func() {
		p.copyView()
		if !strings.Contains(p.dialog.status.GetText(false), "Rendered view is incomplete") {
			t.Error("Copy View did not explain why it refused the truncated view")
		}
	})
	u.post(func() {
		p.refresh()
		p.dialog.cancel()
		if p.rendering || p.renderJob != nil || p.dialog.closed || !p.result.incomplete || !p.loadCanceled {
			t.Error("Esc did not cancel just the loading operation")
		}
	})
	clusterKey(t, screen, tcell.KeyRune, 'l', tcell.ModAlt)
	waitFor("retry did not restore the original preview", func() bool { return !p.rendering && p.canLoadFull() })
	clusterKey(t, screen, tcell.KeyRune, 'l', tcell.ModAlt)
	waitFor("load confirmation was not shown inline", func() bool {
		return !p.expanded && !p.rendering && p.confirmLoad != nil && u.modalDepth == 1 && p.viewer.doc.text != ""
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitFor("Esc did not restore the payload after canceling the prompt", func() bool {
		return p.confirmLoad == nil && !p.expanded && p.viewer.HasFocus() && u.modalDepth == 1 && p.viewer.doc.text != ""
	})
	clusterKey(t, screen, tcell.KeyRune, 'l', tcell.ModAlt)
	waitFor("load confirmation did not reopen", func() bool { return p.confirmLoad != nil })
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	waitFor("confirmed full render was truncated", func() bool { return p.expanded && !p.rendering && !p.result.incomplete })
	u.post(func() {
		if !p.loadButton.IsDisabled() || p.dialog.footer.GetItemCount() != len(p.dialog.buttons)-1 {
			t.Error("Load full was still offered after the full payload finished")
		}
	})
	u.post(func() { p.search.SetText(marker) })
	waitFor("search did not jump past the old display boundary", func() bool {
		return p.viewer.match[0] > b.renderedOutput && p.viewer.left > b.renderedOutput
	})
	u.post(func() {
		p.focusViewer()
		p.viewer.selectBy(p.viewer.match[0], false)
		p.viewer.setMode(payloadSelection)
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
	if err := screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'y', 0)); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-screen.copied:
		if text != marker {
			t.Fatalf("clipboard received %q", text)
		}
	case <-ctx.Done():
		t.Fatal("y did not copy the selected search result")
	}
	waitFor("y did not leave Selection mode in Fullscreen", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.cursor == cursor &&
			p.viewer.selectedText() == "" && p.viewer.left == left-pan
	})
}

func TestPayloadTextPansWithoutChangingSelection(t *testing.T) {
	var copied string
	view := newPayloadText(func(text string) { copied = text })
	content := strings.Repeat("0123456789", 40) + "\t" + strings.Repeat("字😀", 150) + "end"
	doc := indexPayloadText(t.Context(), "short\n"+content)
	if doc.width != 1007 {
		t.Fatalf("document is %d display columns, want 1007", doc.width)
	}
	view.setDocument(doc)
	view.SetRect(0, 0, 40, 3)
	key := func(k tcell.Key, mod tcell.ModMask) {
		view.InputHandler()(tcell.NewEventKey(k, 0, mod), nil)
	}
	key(tcell.KeyRight, tcell.ModAlt)
	if view.left != 8 || view.cursor != 0 || view.anchor != view.cursor {
		t.Fatal("Alt+Right did not pan eight columns while keeping the unselected cursor")
	}
	view.selectBy(len("short\n")+1, false)
	view.InputHandler()(tcell.NewEventKey(tcell.KeyRune, ' ', 0), nil)
	view.InputHandler()(tcell.NewEventKey(tcell.KeyRune, 'v', 0), nil)
	key(tcell.KeyRight, 0)
	key(tcell.KeyRight, 0)
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

func TestFullTreeSearchReachesLastRecordInLargeArray(t *testing.T) {
	const rows = 55_000
	const finalValue = "parcel-final-destination"
	repeated := "parcel-" + strings.Repeat("a", 51)
	data := []byte(`[` + strings.Repeat(`"`+repeated+`",`, rows-1) + `"` + finalValue + `"]`)
	store := newPayloadStore(t.TempDir(), payloadQuota)
	t.Cleanup(store.close)
	record := retainedRecord(t, store, data)
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	var p *payloadPane
	u.post(func() {
		p = u.payloadDialog(true, []*storedRecord{record}, "tree")
		p.search.SetText("parcel-")
	})
	waitUIState(t, u, 15*time.Second, "search missed entries in collapsed branches", func() bool {
		return len(p.treeMatches) == rows && p.match == 0 && !p.result.incomplete
	})
	u.post(p.focusViewer)
	clusterKey(t, screen, tcell.KeyRune, 'p', 0)
	waitKeyboardState(t, u, "previous match did not reveal the last payload", func() bool {
		return p.match == rows-1 && p.tree.GetCurrentNode().GetReference().(*payloadNode).value == finalValue &&
			p.nodeView.doc.text == `"`+finalValue+`"`
	})
	clusterKey(t, screen, tcell.KeyRune, 'n', 0)
	waitKeyboardState(t, u, "next match failed to wrap to the first value", func() bool {
		return p.match == 0 && p.tree.GetCurrentNode().GetReference().(*payloadNode).value == repeated
	})
}

func TestFullTextSearchPastFormerLineAndMatchLimits(t *testing.T) {
	data := []byte(strings.Repeat("match\n", 10_001) + strings.Repeat("\n", 990_000) + "last-match")
	store := newPayloadStore(t.TempDir(), payloadQuota)
	t.Cleanup(store.close)
	record := retainedRecord(t, store, data)
	u, _, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	var p *payloadPane
	u.post(func() {
		p = u.payloadDialog(true, []*storedRecord{record}, "raw")
		p.search.SetText("MATCH")
	})
	waitUIState(t, u, 15*time.Second, "raw search did not include all matches", func() bool {
		return len(p.textMatches) == 10_002
	})
	u.post(func() {
		p.advance(-1)
		if p.viewer.match != [2]int{len(data) - len("match"), len(data)} {
			t.Errorf("last match = %v, want the final five bytes", p.viewer.match)
		}
	})
}
