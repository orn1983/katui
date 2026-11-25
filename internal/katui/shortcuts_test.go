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
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
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
	clusterKey(t, screen, tcell.KeyRight, 0, 0)
	wait("selection mode did not extend the selection and pin the current record", func() bool {
		return p.viewer.selecting && p.viewer.cursor == 1 && p.viewer.anchor == 0 && !u.followLatest
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("Esc left Messages instead of selection mode", func() bool { return !p.viewer.selecting && p.viewer.HasFocus() && u.workspace == 2 })
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
		return u.modalDepth == 0 && p.viewer.HasFocus() && p.modeName == "pretty"
	})
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
	clusterKey(t, screen, tcell.KeyRune, '/', 0)
	clusterKey(t, screen, tcell.KeyRune, 'm', 0)
	wait("the inspector search shortcut did not enter payload search", func() bool {
		input, ok := u.app.GetFocus().(*tview.InputField)
		return ok && input.GetText() == "m" && len(u.payloadJobs) == 0 && u.modalDepth == 1
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("Esc did not return from search to the Full view payload", func() bool {
		focus, ok := u.app.GetFocus().(*payloadText)
		return ok && focus != p.viewer && u.modalDepth == 1
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	wait("closing Full view did not return to the inspector", func() bool { return p.viewer.HasFocus() && u.modalDepth == 0 })
	clusterKey(t, screen, tcell.KeyRune, ':', tcell.ModShift)
	wait("the command palette did not open", func() bool { return u.modalDepth == 1 })
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
