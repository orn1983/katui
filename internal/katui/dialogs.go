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
	d := &dialog{u: u}
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
	return u.openDialog(title, body, initial, false)
}

func (u *ui) openDialog(title string, body, initial tview.Primitive, fullscreen bool) *dialog {
	u.modalDepth++
	u.modalID++
	if u.modalDepth == 1 {
		u.invalidateMonitor()
	}
	d := u.newDialog(title, body)
	d.id, d.previous = fmt.Sprintf("modal-%d", u.modalID), u.app.GetFocus()
	var page tview.Primitive = d.root
	if !fullscreen {
		top := 1
		if u.workspace == clustersWorkspace {
			top = 2 // Keep the workspace bar above cluster editors and password prompts.
		}
		page = tview.NewGrid().SetRows(top, 0, 1).SetColumns(2, 0, 2).
			AddItem(d.root, 1, 1, 1, 1, 0, 0, true)
	}
	u.pages.AddPage(d.id, page, true, true)
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
			d.tab(event)
			return nil
		}
		return event
	})
}

// Let tview traverse enabled form fields, then continue into the rest of the
// dialog when the form wraps. This also works for forms nested beside a table.
func (d *dialog) tab(event *tcell.EventKey) {
	reverse := event.Key() == tcell.KeyBacktab
	items := append([]tview.Primitive(nil), d.focusables...)
	for _, button := range d.buttons {
		if !button.IsDisabled() {
			items = append(items, button)
		}
	}
	if len(items) == 0 {
		return
	}
	setFocus := func(p tview.Primitive) { d.u.app.SetFocus(p) }
	index := 0
	for i, p := range items {
		if !p.HasFocus() {
			continue
		}
		index = i
		if form, ok := p.(*tview.Form); ok {
			before, _ := form.GetFocusedItemIndex()
			form.InputHandler()(event, setFocus)
			after, _ := form.GetFocusedItemIndex()
			if reverse && after < before || !reverse && after > before {
				return
			}
		}
		break
	}
	step := 1
	if reverse {
		step = -1
	}
	target := items[(index+step+len(items))%len(items)]
	if form, ok := target.(*tview.Form); ok {
		// All dialogs start their form with an enabled field. Backtab enters
		// at the last enabled field (which may precede disabled trailing ones).
		focusForm(d.u, form, 0)
		if reverse {
			form.InputHandler()(event, setFocus)
		}
		return
	}
	setFocus(target)
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
		k("/") + " Search  " + k("q") + " Quit",
		k("?") + " or " + k("Ctrl+P") + " Help & Commands  ·  Press " + k("?") + " again for the keyboard reference",
		"While typing, use the indicated Alt shortcuts or Tab to a button and press Enter.",
		"",
		"Kafka clusters",
		k("↑") + " " + k("↓") + " Select  " + k("Enter") + " / " + k("c") + " Connect  " + k("e") + " Edit  " + k("n") + " New",
		k("1") + " / " + k("2") + " / " + k("3") + " Switch workspaces  " + k("Esc") + " Back",
		"In connection setup, " + k("Alt+A") + " Brokers  " + k("Alt+S") + " Security (API backend)",
		k("c") + " Connect  " + k("Alt+C") + " Connect while typing  " + k("Esc") + " Back or cancel",
		"",
		"Topics and consumer groups",
		k("s") + " Sort  " + k("r") + " Refresh or retry monitoring  " + k("z") + " Zoom",
		k("h") + " Internal topics  " + k("g") + " Topic consumers",
		"Open a group, then a partition to see its consumption chart. A gap is a missing sample. A flat line is zero.",
		"Offset discontinuities counts samples where the log end or committed offset moved backward.",
		"Topics, record reading, and administration require the API backend.",
		k("n") + " Create topic  " + k("a") + " Manage  " + k("d") + " Delete topic",
		k("r") + " Review changes  " + k("Alt+A") + " Apply after entering the exact target name",
		"",
		"Consume",
		k("c") + " Open Consume  " + k("Alt+P") + " Focus partitions in the dialog",
		k("Space") + " Toggle partition  " + k("a") + " All  " + k("n") + " None",
		k("Alt+S") + " Start position  ·  Choose Exact offset to set a starting offset for each selected partition",
		"Keep following controls whether the reader waits for new records.",
		k("Alt+C") + " Start consuming, including while typing",
		"",
		"Messages and payloads",
		k("i") + " Switch records/payload  " + k("b") + " Records  " + k("m") + " Format  " + k("z") + " Toggle Fullscreen",
		k("p") + " Pause or resume reader  " + k("x") + " Stop reader  ·  Clear buffer in Commands",
		k("f") + " Follow newest record  ·  Off by default. The Follow checkbox shows the current mode.",
		k("Space") + " Toggle record in the list  " + k("a") + " Select all visible records  " + k("n") + " Unselect all",
		k("w") + " Compare selected records  ·  In comparison, " + k("1") + " Payloads  " + k("2") + " Metadata",
		k("o") + " CSV columns  " + k("u") + " Protobuf  " + k("d") + " Delimiter  " + k("Alt+H") + " Header",
		k(string(copyValueKey)) + " Copy Value  " + k(string(copyViewKey)) + " Copy View  " + k("s") + " Save value to file",
		"In Scroll mode, " + k("←/→") + " Pan 8 columns  " + k("↑/↓") + " Scroll 4 lines",
		k("h") + " " + k("j") + " " + k("k") + " " + k("l") + " Move like Left, Down, Up, and Right in payload text",
		k("Alt+←/→") + " Pan 8 columns  " + k("Ctrl+←/→") + " Pan half the viewport  " + k("PgUp/PgDn") + " Page",
		k("Space") + " Toggle Cursor  ·  In Cursor, " + k("Arrows") + " Move  " + k("v") + " Toggle Selection",
		"In Cursor and Selection, " + k("0") + " Line start  " + k("$") + " Line end",
		"In Selection, " + k("Arrows") + " Extend or shrink  " + k("Esc") + " Return to Cursor",
		k("Shift+arrows") + " Extend selection  " + k("Ctrl+A") + " Select all  " + k("Ctrl+C") + " Copy selected text",
		k("y") + " Yank and exit Selection  ·  In Scroll or Cursor, copy the selected text or the rendered view",
		"In the tree, " + k("←/→") + " Collapse or expand nodes  " + k("Tab") + " Focus node text",
		k("/") + " Search payload in Fullscreen  " + k("Enter") + " Return to payload  " + k("n") + " Next  " + k("p") + " Previous",
		k("Alt+L") + " Load full if available  " + k("Enter") + " Confirm  " + k("Esc") + " Cancel",
		"",
		"File browser",
		k("Alt+P") + " Path  " + k("/") + " Filter  " + k("Alt+B") + " Files",
		k("Enter") + " Open  " + k("u") + " Up  " + k("h") + " Home  " + k(".") + " Hidden files  " + k("s") + " Choose folder when picking a directory",
		"",
		"Protobuf schema editor",
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
	d := u.dialog("Help & Commands", list, list)
	d.hint(keyHint("Enter") + " Run selected command  " + keyHint("Esc") + " Back")
	add := func(label string, key rune, fn func()) { list.AddItem(label, "", key, func() { d.close(); fn() }) }
	if u.b.backend != "cli" {
		add("Topics", '1', func() { u.changeWorkspace(0) })
	}
	add("Consumer Groups", '2', func() { u.changeWorkspace(1) })
	if u.b.backend != "cli" {
		add("Messages", '3', func() { u.changeWorkspace(2) })
	}
	add("Refresh monitoring", 'r', u.requestRefresh)
	add("Kafka clusters", '4', u.showClusters)
	if u.workspace == 2 && len(u.records) > 0 {
		add("Clear message buffer", 'l', u.clearBuffer)
	}
	if u.workspace == 2 && u.inspector != nil && u.pinned != nil {
		d.addCopyCommands(list, u.inspector)
	}
	add("Change color theme", 't', u.theme)
	add("Keyboard shortcuts", '?', u.help)
	add("Quit", 'q', u.quit)
}

func (p *payloadPane) commands() {
	list := p.u.newList().ShowSecondaryText(false)
	d := p.u.dialog("Payload Help & Commands", list, list)
	d.addCopyCommands(list, p)
	list.AddItem("Keyboard shortcuts", "", '?', func() { d.close(); p.u.help() })
	d.hint(keyHint("Enter") + " Run selected command  " + keyHint("Esc") + " Back")
}

func (d *dialog) addCopyCommands(list *tview.List, p *payloadPane) {
	list.AddItem("Copy Value", "", copyValueKey, func() { d.close(); p.copyValue() })
	list.AddItem("Copy View", "", copyViewKey, func() { d.close(); p.copyView() })
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
