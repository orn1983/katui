package katui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type dialog struct {
	u          *ui
	id         string
	root       *tview.Flex
	body       tview.Primitive
	status     *tview.TextView
	footer     *tview.Flex
	previous   tview.Primitive
	buttons    []*tview.Button
	focusables []tview.Primitive
	cancel     func()
	closed     bool
}

func (u *ui) newDialog(title string, body tview.Primitive) *dialog {
	d := &dialog{u: u, body: body}
	d.status = tview.NewTextView().SetDynamicColors(true).SetTextColor(tcell.ColorLightGoldenrodYellow)
	d.footer = tview.NewFlex()
	d.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(body, 0, 1, true).AddItem(d.status, 2, 0, false).AddItem(d.footer, 1, 0, false)
	d.root.SetBorder(true).SetTitle(" " + tview.Escape(safe(title, false)) + " ").SetBorderColor(u.accent)
	d.cancel = d.close
	d.focusables = []tview.Primitive{body}
	d.keymap(nil)
	return d
}

func (u *ui) dialog(title string, body tview.Primitive, initial tview.Primitive) *dialog {
	u.modalDepth++
	u.modalID++
	if u.modalDepth == 1 {
		u.invalidateMonitor()
	}
	d := u.newDialog(title, body)
	d.id, d.previous = fmt.Sprintf("modal-%d", u.modalID), u.app.GetFocus()
	top := 1
	if u.workspace == clustersWorkspace {
		top = 2 // Keep the workspace bar above cluster editors and password prompts.
	}
	outer := tview.NewGrid().SetRows(top, 0, 1).SetColumns(2, 0, 2).
		AddItem(d.root, 1, 1, 1, 1, 0, 0, true)
	u.pages.AddPage(d.id, outer, true, true)
	u.app.SetFocus(initial)
	return d
}
func (d *dialog) close() {
	if d.closed {
		return
	}
	d.closed = true
	d.u.pages.RemovePage(d.id)
	d.u.modalDepth--
	if d.u.modalDepth == 0 {
		d.u.refreshView()
		d.u.poll()
	}
	if d.previous != nil {
		d.u.app.SetFocus(d.previous)
	} else {
		d.u.focus(d.u.mainPane())
	}
}
func (d *dialog) say(text string) {
	d.hint(tview.Escape(safe(text, true)))
}
func (d *dialog) hint(text string) {
	if !d.closed {
		d.status.SetText(text)
	}
}
func (d *dialog) button(label, key string, action func()) *tview.Button {
	b := tview.NewButton(buttonLabel(label, key)).SetSelectedFunc(action)
	d.buttons = append(d.buttons, b)
	d.footer.AddItem(b, tview.TaggedStringWidth(b.GetLabel())+2, 0, false)
	return b
}
func (d *dialog) keymap(handle func(*tcell.EventKey) bool) {
	d.root.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			if dd, ok := d.u.app.GetFocus().(*tview.DropDown); ok && dd.IsOpen() {
				return event
			}
			if form, ok := d.body.(*tview.Form); ok {
				for i := range form.GetFormItemCount() {
					// An open dropdown transfers focus to its internal list.
					if dd, ok := form.GetFormItem(i).(*tview.DropDown); ok && dd.IsOpen() {
						return event
					}
				}
			}
			d.cancel()
			return nil
		}
		if handle != nil && handle(event) {
			return nil
		}
		if event.Key() == tcell.KeyTab || event.Key() == tcell.KeyBacktab {
			focus := d.u.app.GetFocus()
			if dd, ok := focus.(*tview.DropDown); ok && dd.IsOpen() {
				return event
			}
			if form, ok := d.body.(*tview.Form); ok && form.HasFocus() {
				item, _ := form.GetFocusedItemIndex()
				if event.Key() == tcell.KeyTab && item < form.GetFormItemCount()-1 {
					return event
				}
				if event.Key() == tcell.KeyBacktab && item > 0 {
					return event
				}
			}
			sequence := append([]tview.Primitive{}, d.focusables...)
			for _, b := range d.buttons {
				if !b.IsDisabled() {
					sequence = append(sequence, b)
				}
			}
			if len(sequence) == 0 {
				return nil
			}
			i := 0
			for index, p := range sequence {
				if p == focus || p.HasFocus() {
					i = index
					break
				}
			}
			step := 1
			if event.Key() == tcell.KeyBacktab {
				step = -1
			}
			d.u.app.SetFocus(sequence[(i+step+len(sequence))%len(sequence)])
			return nil
		}
		return event
	})
}
func alt(event *tcell.EventKey) rune {
	if event.Key() == tcell.KeyRune && event.Modifiers()&tcell.ModAlt != 0 {
		return event.Rune()
	}
	return 0
}
func isEditing(p tview.Primitive) bool {
	switch f := p.(type) {
	case *tview.InputField, *tview.TextArea:
		return true
	case *tview.DropDown:
		return f.IsOpen()
	}
	return false
}
func focusForm(u *ui, f *tview.Form, index int) {
	if index >= 0 && index < f.GetFormItemCount() {
		f.SetFocus(index)
		u.app.SetFocus(f)
	}
}
func field(form *tview.Form, label, value string) *tview.InputField {
	f := tview.NewInputField().SetLabel(label).SetText(value)
	form.AddFormItem(f)
	return f
}
func dropdown(form *tview.Form, label string, values []string, index int, changed func(int)) *tview.DropDown {
	dd := tview.NewDropDown().SetLabel(label).SetOptions(values, func(_ string, i int) {
		if changed != nil {
			changed(i)
		}
	}).SetCurrentOption(index)
	form.AddFormItem(dd)
	return dd
}
func check(form *tview.Form, label string, value bool) *tview.Checkbox {
	c := tview.NewCheckbox().SetLabel(label).SetChecked(value)
	form.AddFormItem(c)
	return c
}
func indexOf(labels []string, which string) int {
	for i, value := range labels {
		if value == which {
			return i
		}
	}
	return 0
}
func (u *ui) quit() {
	text := tview.NewTextView().SetText("Quit Katui? Active readers will stop and buffered records will be discarded.").SetWordWrap(true)
	d := u.dialog("Quit Katui", text, text)
	yes := func() { u.app.Stop() }
	d.button("Yes", "y", yes)
	no := d.button("No", "n", d.close)
	u.app.SetFocus(no)
	d.keymap(func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		switch ev.Rune() {
		case 'y':
			yes()
		case 'n':
			d.close()
		case 'q':
		default:
			return false
		}
		return true
	})
}
func (u *ui) help() {
	k := keyHint
	lines := []string{
		"Navigation",
		k("1") + " Topics  " + k("2") + " Consumer Groups  " + k("3") + " Messages  " + k("4") + " Clusters",
		k("Tab") + " / " + k("Shift+Tab") + " Move focus",
		k("↑") + " " + k("↓") + " / " + k("j") + " " + k("k") + " Navigate  " + k("PageUp") + " / " + k("PageDown") + " Page",
		"In tables, " + k("←/→") + " Scroll sideways  " + k("Ctrl+←/→") + " Scroll farther",
		k("Enter") + " Open selection  " + k("Esc") + " Back or cancel",
		k("/") + " Search  " + k("s") + " Sort  " + k("r") + " Refresh  " + k("z") + " Toggle fullscreen",
		k("Ctrl+P") + " Commands  " + k("?") + " Help  " + k("q") + " Quit",
		"",
		"Kafka clusters",
		k("↑") + " " + k("↓") + " Select  " + k("Enter") + " / " + k("c") + " Connect  " + k("e") + " Edit  " + k("n") + " New",
		k("1") + " / " + k("2") + " / " + k("3") + " Switch workspaces  " + k("Esc") + " Back",
		k("Alt+G") + " Connect from the editor  " + k("Esc") + " Back or cancel",
		"",
		"Topics and consumer groups",
		k("h") + " Internal topics  " + k("g") + " Topic consumers",
		k("n") + " Create topic  " + k("a") + " Manage  " + k("d") + " Delete topic  " + k("c") + " Start reader",
		"",
		"Messages and payloads",
		k("b") + " Records  " + k("i") + " Inspector  " + k("m") + " Mode  " + k("F4") + " Full payload",
		k("p") + " Pause  " + k("x") + " Stop  " + k("l") + " Clear buffer  " + k("f") + " Follow latest",
		k("Space") + " Check  " + k("a") + " Check matching  " + k("w") + " Compare  " + k("e") + " Uncheck all",
		k("o") + " CSV columns  " + k("u") + " Protobuf  " + k("d") + " Delimiter  " + k("h") + " Header",
		k("y") + " Copy Value  " + k("v") + " Copy View  " + k("E") + " Export",
		k("Shift+arrows") + " Select text  " + k("Ctrl+A") + " Select all  " + k("Ctrl+C") + " Copy selection",
		"In payload text, " + k("←/→") + " Move cursor  " + k("Shift+←/→") + " Extend or shrink selection",
		k("Alt+←/→") + " Pan 8 columns  " + k("Ctrl+←/→") + " Pan half the viewport (at least 8 columns)",
		"In the tree, " + k("←/→") + " Collapse or expand nodes",
		k("/") + " Find in Full view  " + k("n") + " Next  " + k("p") + " Previous  " + k("L") + " Load full",
		"",
		"Offsets are positions. Retained counts and rates are estimates.",
		"Read-only inspection does not join an application consumer group.",
	}
	text := tview.NewTextView().SetDynamicColors(true).SetWordWrap(true).SetText(strings.Join(lines, "\n"))
	text.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, nil, text) })
	d := u.dialog("Help", text, text)
	d.button("Back", "Esc", d.close)
}
func (u *ui) palette() {
	list := tview.NewList().ShowSecondaryText(false).SetShortcutStyle(shortcutStyle())
	d := u.dialog("Commands", list, list)
	d.hint(keyHint("Enter") + " Run selected command  " + keyHint("Esc") + " Back")
	add := func(label string, key rune, fn func()) { list.AddItem(label, "", key, func() { d.close(); fn() }) }
	add("Topics", '1', func() { u.changeWorkspace(0) })
	add("Consumer Groups", '2', func() { u.changeWorkspace(1) })
	add("Messages", '3', func() { u.changeWorkspace(2) })
	add("Refresh monitoring", 'r', u.requestRefresh)
	add("Kafka clusters", '4', u.showClusters)
	add("Change color theme", 't', u.theme)
	add("Help", '?', u.help)
	add("Quit", 'q', u.quit)
}
func (u *ui) theme() {
	list := tview.NewList().ShowSecondaryText(false)
	d := u.dialog("Color theme", list, list)
	d.hint(keyHint("Enter") + " Apply selected theme  " + keyHint("Esc") + " Back")
	for _, theme := range []struct {
		label string
		color tcell.Color
	}{{"Glacier", tcell.ColorLightSkyBlue}, {"Amber", tcell.ColorLightGoldenrodYellow}, {"Mint", tcell.ColorAquaMarine}} {
		list.AddItem(theme.label, "", 0, func() {
			u.accent = theme.color
			u.header.SetTextColor(u.accent)
			u.table.SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(u.accent).Bold(true))
			d.close()
			u.focus(u.mainPane())
		})
	}
}
