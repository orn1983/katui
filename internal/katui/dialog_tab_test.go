package katui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestDeleteTopicTabSkipsDisabledFields(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	u.post(func() { u.topicForm("example", opDeleteTopic) })
	clusterKey(t, screen, tcell.KeyTab, 0, 0)
	waitKeyboardState(t, u, "Tab from Operation should focus Review", func() bool {
		button, ok := u.app.GetFocus().(*tview.Button)
		return ok && strings.Contains(button.GetLabel(), "Review")
	})
	clusterKey(t, screen, tcell.KeyBacktab, 0, 0)
	waitKeyboardState(t, u, "Backtab should return to Operation", func() bool {
		field, ok := u.app.GetFocus().(*tview.DropDown)
		return ok && strings.Contains(field.GetLabel(), "Operation")
	})
}

func TestCSVColumnsTabKeepsMultilineEditing(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	u.post(func() {
		u.changeWorkspace(2)
		u.inspector.csvEditor("example")
	})
	clusterKey(t, screen, tcell.KeyRune, 'a', 0)
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	clusterKey(t, screen, tcell.KeyRune, 'b', 0)
	editing := func() bool {
		text, ok := u.app.GetFocus().(*tview.TextArea)
		return ok && text.GetText() == "a\nb"
	}
	save := func() bool { _, ok := u.app.GetFocus().(*tview.Checkbox); return ok }
	waitKeyboardState(t, u, "Enter should insert a newline in column names", editing)
	clusterKey(t, screen, tcell.KeyTab, 0, 0)
	waitKeyboardState(t, u, "Tab should reach Save preference", save)
	clusterKey(t, screen, tcell.KeyTab, 0, 0)
	waitKeyboardState(t, u, "Tab should leave the form for the preview", func() bool {
		preview, ok := u.app.GetFocus().(*tview.TextView)
		return ok && strings.Contains(preview.GetTitle(), "preview")
	})
	clusterKey(t, screen, tcell.KeyBacktab, 0, 0)
	waitKeyboardState(t, u, "Backtab should enter the form at Save preference", save)
	clusterKey(t, screen, tcell.KeyBacktab, 0, 0)
	waitKeyboardState(t, u, "Backtab should return to column names", editing)
}

func TestModalPreventsBackgroundMouseActivation(t *testing.T) {
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	var foreground *tview.Button
	var d *dialog
	backgroundClicks := 0
	u.post(func() {
		background := tview.NewButton("Background action").SetSelectedFunc(func() { backgroundClicks++ })
		u.pages.AddPage("background", background, true, true)
		text := tview.NewTextView()
		d = u.dialog("Foreground", text, text)
		foreground = d.button("Foreground action", "f", func() {})
	})
	var x, y int
	waitKeyboardState(t, u, "foreground action was never drawn", func() bool {
		var width, height int
		x, y, width, height = foreground.GetRect()
		return x > 0 && y > 0 && width > 0 && height > 0
	})
	mouse := func(x, y int, button tcell.ButtonMask) {
		t.Helper()
		if err := screen.PostEvent(tcell.NewEventMouse(x, y, button, 0)); err != nil {
			t.Fatal(err)
		}
	}
	mouse(0, 0, tcell.Button1)
	mouse(0, 0, tcell.ButtonNone)
	mouse(x, y, tcell.Button1)
	waitKeyboardState(t, u, "foreground action did not receive mouse focus", foreground.HasFocus)
	u.post(func() {
		if backgroundClicks != 0 {
			t.Errorf("clicked through the modal and activated the background action %d times", backgroundClicks)
		}
		page := u.pages.GetPage(d.id).(*modalPage)
		d.close()
		if page.Primitive != nil {
			t.Error("unmounted page still owns the dialog widget tree")
		}
	})
}
