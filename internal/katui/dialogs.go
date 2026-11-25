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
	actions    map[rune]dialogAction
	focusables []tview.Primitive
	cancel     func()
	closed     bool
}

type dialogAction struct {
	button *tview.Button
	run    func()
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
	if len(key) == 1 {
		if d.actions == nil {
			d.actions = map[rune]dialogAction{}
		}
		d.actions[rune(key[0])] = dialogAction{b, action}
	}
	return b
}

func (d *dialog) actionKey(event *tcell.EventKey) rune {
	if key := alt(event); key != 0 {
		return key
	}
	if !isEditing(d.u.app.GetFocus()) {
		return shortcutRune(event)
	}
	return 0
}

func (d *dialog) dropdownOpen() bool {
	for _, p := range d.focusables {
		if dd, ok := p.(*tview.DropDown); ok && dd.IsOpen() && dd.HasFocus() {
			return true
		}
		if f, ok := p.(*tview.Form); ok {
			for i := range f.GetFormItemCount() {
				if dd, ok := f.GetFormItem(i).(*tview.DropDown); ok && dd.IsOpen() && dd.HasFocus() {
					return true
				}
			}
		}
	}
	return false
}

func (d *dialog) keymap(handle func(*tcell.EventKey) bool) {
	d.root.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if d.dropdownOpen() {
			// The list's Escape closes only the choices and returns focus to
			// the same field. The form's Escape would reset to its first field.
			if list, ok := d.u.app.GetFocus().(*tview.List); ok && event.Key() == tcell.KeyEscape {
				list.InputHandler()(event, func(p tview.Primitive) { d.u.app.SetFocus(p) })
				return nil
			}
			return event
		}
		if event.Key() == tcell.KeyEscape {
			d.cancel()
			return nil
		}
		if handle != nil && handle(event) {
			return nil
		}
		if action, ok := d.actions[d.actionKey(event)]; ok {
			if !action.button.IsDisabled() {
				action.run()
			}
			return nil
		}
		if event.Key() == tcell.KeyTab || event.Key() == tcell.KeyBacktab {
			focus := d.u.app.GetFocus()
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
	if event.Key() == tcell.KeyRune && event.Modifiers()&^tcell.ModShift == tcell.ModAlt {
		return event.Rune()
	}
	return 0
}
func isEditing(p tview.Primitive) bool {
	switch p.(type) {
	case *tview.InputField, *tview.TextArea, *tview.DropDown:
		return true
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
		k(":") + " or " + k("Ctrl+P") + " Commands  " + k("?") + " Help  " + k("q") + " Quit",
		"Use the shown lowercase letters on lists, viewers, and buttons.",
		"While typing, use Alt with the shown action letter or Tab to reach a button.",
		"",
		"Kafka clusters",
		k("↑") + " " + k("↓") + " Select  " + k("Enter") + " / " + k("c") + " Connect  " + k("e") + " Edit  " + k("n") + " New",
		k("1") + " / " + k("2") + " / " + k("3") + " Switch workspaces  " + k("Esc") + " Back",
		k("c") + " Connect from the editor  " + k("Esc") + " Back or cancel",
		"",
		"Topics and consumer groups",
		k("h") + " Internal topics  " + k("g") + " Topic consumers",
		k("n") + " Create topic  " + k("a") + " Manage  " + k("d") + " Delete topic  " + k("c") + " Start reader",
		k("Space") + " Toggle partition  " + k("a") + " All  " + k("n") + " None  " + k("c") + " Start reader",
		k("Alt+G") + " Start directly from a filter or form field",
		k("r") + " Review changes  " + k("a") + " Apply after entering the exact target name",
		"",
		"Messages and payloads",
		k("i") + " Switch records/payload  " + k("b") + " Records  " + k("m") + " Format  " + k("F4") + " Full payload",
		k("p") + " Pause  " + k("x") + " Stop  " + k("f") + " Follow latest  ·  Clear buffer in Commands",
		k("Space") + " Check  " + k("a") + " Check matching  " + k("w") + " Compare  " + k("e") + " Uncheck all",
		k("o") + " CSV columns  " + k("u") + " Protobuf  " + k("d") + " Delimiter  " + k("h") + " Header",
		k("y") + " Copy Value  " + k("v") + " Copy View  " + k("s") + " Save value to file",
		"In payload text, " + k("←/→") + " Pan 8 columns  " + k("↑/↓") + " Scroll 4 lines",
		k("Ctrl+←/→") + " Pan half the viewport  " + k("PgUp/PgDn") + " Page",
		k("Space") + " Selection mode  " + k("Arrows") + " Move and extend  " + k("Esc") + " Exit selection",
		k("Shift+arrows") + " Start selection  " + k("Ctrl+A") + " Select all  " + k("Ctrl+C") + " Copy selection",
		"In the tree, " + k("←/→") + " Collapse or expand nodes",
		k("/") + " Search payload in Full view  " + k("n") + " Next  " + k("p") + " Previous  " + k("l") + " Load full",
		"Confirm the larger load with " + k("Enter") + " or cancel with " + k("Esc"),
		"",
		"Files and editors",
		k("/") + " Filter files  " + k("Enter") + " Open  " + k("u") + " Up  " + k("h") + " Home  " + k(".") + " Hidden files",
		k("b") + " Browse schema  " + k("i") + " Add import folder  " + k("l") + " Load schema  " + k("a") + " Apply",
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
	list := u.newList().ShowSecondaryText(false)
	d := u.dialog("Commands", list, list)
	d.hint(keyHint("Enter") + " Run selected command  " + keyHint("Esc") + " Back")
	add := func(label string, key rune, fn func()) { list.AddItem(label, "", key, func() { d.close(); fn() }) }
	add("Topics", '1', func() { u.changeWorkspace(0) })
	add("Consumer Groups", '2', func() { u.changeWorkspace(1) })
	add("Messages", '3', func() { u.changeWorkspace(2) })
	add("Refresh monitoring", 'r', u.requestRefresh)
	add("Kafka clusters", '4', u.showClusters)
	if u.workspace == 2 && len(u.records) > 0 {
		add("Clear message buffer", 'l', u.clearBuffer)
	}
	add("Change color theme", 't', u.theme)
	add("Help", '?', u.help)
	add("Quit", 'q', u.quit)
}
func (u *ui) theme() {
	list := u.newList().ShowSecondaryText(false)
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
