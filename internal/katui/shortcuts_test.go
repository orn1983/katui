package katui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kgo"
)

func waitKeyboardState(t *testing.T, u *ui, failure string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		if !u.post(func() { ok = condition() }) {
			break
		}
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(failure)
}

func TestMessageNavigationEntersPayloadBeforeFormatControls(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "test")
	screen := clipboardScreen{tcell.NewSimulationScreen("UTF-8"), make(chan string, 1)}
	u, _ := startClusterUIWithScreen(t, nil, screen, "-b", "127.0.0.1:1")
	checkClipboard := func(want string) {
		t.Helper()
		select {
		case got := <-screen.copied:
			if got != want {
				t.Fatalf("clipboard got %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no clipboard request")
		}
	}
	value := "[" + strings.Repeat("1,\n", 30) + "2]"
	if !u.post(func() {
		u.changeWorkspace(2)
		for i := range 2 {
			record, err := saveRecord(&kgo.Record{
				Topic: "events", Offset: int64(i),
				Value: []byte(value),
			}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			u.buffer(record)
			u.addCheck(record)
		}
		u.refreshView()
	}) {
		t.Fatal("messages did not open")
	}
	p := u.inspector
	wait := func(failure string, condition func() bool) {
		t.Helper()
		waitKeyboardState(t, u, failure, condition)
	}
	wait("payload preview did not load", func() bool { return !p.rendering && len(p.document.lines) > 10 })
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyTab, 0, 0),
		tcell.NewEventKey(tcell.KeyBacktab, 0, 0),
		tcell.NewEventKey(tcell.KeyEnter, 0, 0),
		tcell.NewEventKey(tcell.KeyRune, 'i', 0),
	} {
		clusterKey(t, screen, ev.Key(), ev.Rune(), ev.Modifiers())
		wait("entering the inspector did not focus the payload", func() bool { return p.viewer.HasFocus() && u.modalDepth == 0 })
		clusterKey(t, screen, tcell.KeyPgDn, 0, 0)
		wait("page down did not navigate the payload", func() bool { return p.viewer.top > 0 && p.viewer.cursor == 0 && p.modeName == "raw" })
		clusterKey(t, screen, tcell.KeyHome, 0, tcell.ModCtrl)
		wait("the viewport did not return to the payload start", func() bool { return p.viewer.top == 0 })
		clusterKey(t, screen, tcell.KeyRune, 'b', 0)
		wait("b did not return to records", func() bool { return u.table.HasFocus() })
	}
	clusterKey(t, screen, tcell.KeyRune, 'i', 0)
	wait("i did not enter the payload", p.viewer.HasFocus)
	clusterKey(t, screen, tcell.KeyRune, ' ', 0)
	clusterKey(t, screen, tcell.KeyRune, 'l', 0)
	wait("Space did not enter Cursor mode and pin the current record", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.cursor == 1 && p.viewer.anchor == 1 && !u.followLatest
	})
	clusterKey(t, screen, tcell.KeyRune, 'y', 0)
	checkClipboard(value)
	clusterKey(t, screen, tcell.KeyRune, 'h', 0)
	wait("h did not move the cursor left", func() bool { return p.viewer.cursor == 0 && p.viewer.anchor == 0 })
	clusterKey(t, screen, tcell.KeyRight, 0, 0)
	clusterKey(t, screen, tcell.KeyRune, 'v', 0)
	clusterKey(t, screen, tcell.KeyRune, '$', tcell.ModShift)
	wait("$ did not extend the selection to the end of the line", func() bool {
		return p.viewer.mode == payloadSelection && p.viewer.selectedText() == "1,"
	})
	clusterKey(t, screen, tcell.KeyRune, 'y', 0)
	checkClipboard("1,")
	wait("y did not clear the selection and return to Cursor mode", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.cursor == 3 && p.viewer.anchor == 3
	})
	clusterKey(t, screen, tcell.KeyRune, '0', 0)
	wait("0 did not move the cursor to the start of the line", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.cursor == 0 && p.viewer.anchor == 0
	})
	clusterKey(t, screen, tcell.KeyRune, '$', tcell.ModShift)
	wait("$ did not move to the end of the line in Cursor mode", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.cursor == 3 && p.viewer.anchor == 3
	})
	clusterKey(t, screen, tcell.KeyRune, 'v', 0)
	clusterKey(t, screen, tcell.KeyRune, '0', 0)
	wait("0 did not extend the selection back to the start of the line", func() bool {
		return p.viewer.mode == payloadSelection && p.viewer.cursor == 0 && p.viewer.selectedText() == "[1,"
	})
	clusterKey(t, screen, tcell.KeyRune, 'y', 0)
	checkClipboard("[1,")
	clusterKey(t, screen, tcell.KeyRune, 'v', 0)
	wait("v did not start a new, empty selection", func() bool {
		return p.viewer.mode == payloadSelection && p.viewer.selectedText() == ""
	})
	clusterKey(t, screen, tcell.KeyRune, 'y', 0)
	wait("y with an empty visual selection did not return to Cursor mode", func() bool {
		return p.viewer.mode == payloadCursor && p.viewer.selectedText() == ""
	})
	clusterKey(t, screen, tcell.KeyRune, 'g', 0)
	checkClipboard(value)
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("Esc did not return to Scroll mode", func() bool {
		return p.viewer.mode == payloadScroll && p.viewer.HasFocus() && u.workspace == 2
	})
	clusterKey(t, screen, tcell.KeyRune, 'i', 0)
	wait("i did not toggle back to the record list", u.table.HasFocus)
	clusterKey(t, screen, tcell.KeyRune, 'l', 0)
	clusterKey(t, screen, tcell.KeyRune, 'm', 0)
	wait("m did not open the format picker", func() bool { return u.modalDepth == 1 && len(u.records) == 2 })
	clusterKey(t, screen, tcell.KeyDown, 0, 0)
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("canceling the format picker changed the payload format", func() bool {
		return u.modalDepth == 0 && p.viewer.HasFocus() && p.modeName == "raw"
	})
	clusterKey(t, screen, tcell.KeyRune, 'm', 0)
	clusterKey(t, screen, tcell.KeyRune, 'j', 0)
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	wait("choosing a format did not return focus to the payload", func() bool {
		return u.modalDepth == 0 && p.viewer.HasFocus() && p.modeName == "pretty" && !p.rendering
	})
	var rendered string
	u.post(func() { rendered = p.document.text })
	if rendered == value {
		t.Fatal("formatted view should differ from the original value")
	}
	clusterKey(t, screen, tcell.KeyRune, 'y', 0)
	checkClipboard(rendered)
	clusterKey(t, screen, tcell.KeyRight, 0, tcell.ModShift)
	wait("Shift+Right did not select part of the formatted view", func() bool {
		return p.viewer.selectedText() != "" && p.viewer.selectedText() != rendered
	})
	clusterKey(t, screen, tcell.KeyRune, '.', 0)
	checkClipboard(rendered)
	path := filepath.Join(t.TempDir(), "saved-value.json")
	clusterKey(t, screen, tcell.KeyRune, 's', 0)
	wait("s did not open Save value", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetLabel() == "New file" && u.modalDepth == 1
	})
	u.post(func() { u.app.GetFocus().(*tview.InputField).SetText(path) })
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	wait("Enter did not save from the path field", func() bool { return u.modalDepth == 0 && p.viewer.HasFocus() })
	if data, err := os.ReadFile(path); err != nil || string(data) != value {
		t.Fatalf("Save did not preserve the original bytes: %v", err)
	}
	clusterKey(t, screen, tcell.KeyRune, 'z', 0)
	wait("z did not open Fullscreen at the terminal size", func() bool {
		_, front := u.pages.GetFrontPage()
		root, ok := front.(*tview.Flex)
		if !ok || u.modalDepth != 1 || strings.TrimSpace(root.GetTitle()) != "Fullscreen" {
			return false
		}
		x, y, width, height := root.GetRect()
		w, h := screen.Size()
		return x == 0 && y == 0 && width == w && height == h
	})
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	wait("? did not open Fullscreen commands", func() bool {
		list, ok := u.app.GetFocus().(*tview.List)
		return ok && u.modalDepth == 2 && list.GetItemCount() == 3
	})
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	wait("the keyboard reference did not open from Fullscreen commands", func() bool {
		view, ok := u.app.GetFocus().(*tview.TextView)
		return ok && u.modalDepth == 2 && strings.HasPrefix(view.GetText(true), "Navigation")
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("closing the keyboard reference did not return to Fullscreen", func() bool { return u.modalDepth == 1 })
	clusterKey(t, screen, tcell.KeyRune, 'z', 0)
	wait("z did not exit Fullscreen", func() bool { return p.viewer.HasFocus() && u.modalDepth == 0 })
	clusterKey(t, screen, tcell.KeyRune, '/', 0)
	clusterKey(t, screen, tcell.KeyRune, 'z', 0)
	wait("the inspector search shortcut did not enter payload search", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetText() == "z" && len(u.payloadJobs) == 0 && u.modalDepth == 1
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("Esc did not return from search to the Fullscreen payload", func() bool {
		focus, ok := u.app.GetFocus().(*payloadText)
		return ok && focus != p.viewer && u.modalDepth == 1
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("closing Fullscreen did not return to the inspector", func() bool {
		return p.viewer.HasFocus() && u.modalDepth == 0 && !p.rendering
	})
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	wait("Help & Commands did not list both copy actions", func() bool {
		list, ok := u.app.GetFocus().(*tview.List)
		if !ok || u.modalDepth != 1 {
			return false
		}
		var hasValue, hasView bool
		for i := range list.GetItemCount() {
			label, _ := list.GetItemText(i)
			hasValue = hasValue || label == "Copy Value"
			hasView = hasView || label == "Copy View"
		}
		return hasValue && hasView
	})
	clusterKey(t, screen, tcell.KeyRune, 'g', 0)
	checkClipboard(value)
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	wait("Help & Commands did not reopen", func() bool { return u.modalDepth == 1 })
	clusterKey(t, screen, tcell.KeyRune, '.', 0)
	checkClipboard(rendered)
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	wait("Help & Commands did not reopen", func() bool { return u.modalDepth == 1 })
	clusterKey(t, screen, tcell.KeyRune, 'l', 0)
	wait("Clear message buffer was not available in Commands", func() bool { return u.modalDepth == 0 && len(u.records) == 0 })
}

func TestDialogLettersRespectTextAndDropdownFocus(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	var input *tview.InputField
	var menu *tview.DropDown
	var button *tview.Button
	runs := 0
	if !u.post(func() {
		u.changeWorkspace(2)
		form := tview.NewForm()
		input = field(form, "Text", "")
		menu = dropdown(form, "Choice", []string{"One", "Two"}, 0, nil)
		d := u.dialog("Shortcut test", form, form)
		button = d.button("Review", "r", func() { runs++ })
		d.button("Back", "Esc", d.close)
	}) {
		t.Fatal("test form did not open")
	}
	wait := func(failure string, condition func() bool) {
		t.Helper()
		waitKeyboardState(t, u, failure, condition)
	}
	clusterKey(t, screen, tcell.KeyRune, 'r', 0)
	wait("the action letter did not reach the input field", func() bool { return input.GetText() == "r" && runs == 0 })
	clusterKey(t, screen, tcell.KeyRune, 'r', tcell.ModAlt)
	wait("Alt did not make the same action available while typing", func() bool { return input.GetText() == "r" && runs == 1 })
	clusterKey(t, screen, tcell.KeyTab, 0, 0)
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	wait("Enter did not open the focused choice list", menu.IsOpen)
	clusterKey(t, screen, tcell.KeyRune, 'r', tcell.ModAlt)
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("Esc closed the form instead of the choice list", func() bool { return !menu.IsOpen() && u.modalDepth == 1 && runs == 1 })
	clusterKey(t, screen, tcell.KeyTab, 0, 0)
	wait("Tab did not reach the action button", button.HasFocus)
	clusterKey(t, screen, tcell.KeyRune, 'r', 0)
	wait("the plain action letter did not work outside text entry", func() bool { return runs == 2 })
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	wait("Enter did not activate the focused button", func() bool { return runs == 3 })
}
