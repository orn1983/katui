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
