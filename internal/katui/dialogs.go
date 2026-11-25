package katui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type dialog struct {
	page         *modalPage
	u            *ui
	id           string
	root         *tview.Flex
	status       *tview.TextView
	footer       *tview.Flex
	origin       *location
	restoreFocus func()
	buttons      []*tview.Button
	actions      map[rune]dialogAction
	focusables   []tview.Primitive
	cancel       func()
	closed       bool
}

// Pages forwards unhandled mouse events to lower visible pages. A dialog owns
// the whole layer, including empty space around its frame.
type modalPage struct{ tview.Primitive }

func (p *modalPage) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	handle := p.Primitive.MouseHandler()
	return func(action tview.MouseAction, event *tcell.EventMouse, focus func(tview.Primitive)) (bool, tview.Primitive) {
		_, capture := handle(action, event, focus)
		return true, capture
	}
}

type dialogAction struct {
	button  *tview.Button
	run     func()
	altOnly bool
}

func (u *ui) newDialog(title string, body tview.Primitive) *dialog {
	d := &dialog{u: u}
	d.status = tview.NewTextView().SetDynamicColors(true).SetTextColor(tcell.ColorLightGoldenrodYellow)
	d.footer = tview.NewFlex()
	d.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(body, 0, 1, true).AddItem(d.status, 2, 0, false).AddItem(d.footer, 1, 0, false)
	// A Flex is transparent by default. Cover the underlying workspace inside the dialog frame.
	d.root.Box = tview.NewBox()
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
	d := u.newDialog(title, body)
	var page tview.Primitive = d.root
	if !fullscreen {
		top := 1
		if u.workspace == clustersWorkspace {
			top = 2 // Keep the workspace bar above cluster editors and password prompts.
		}
		page = tview.NewGrid().SetRows(top, 0, 1).SetColumns(2, 0, 2).
			AddItem(d.root, 1, 1, 1, 1, 0, 0, true)
	}
	return u.showDialog(d, page, initial)
}

func (u *ui) showDialog(d *dialog, page, initial tview.Primitive) *dialog {
	u.modalDepth++
	u.modalID++
	if u.modalDepth == 1 {
		u.invalidateMonitor()
	}
	d.origin = u.current()
	d.id, d.restoreFocus = fmt.Sprintf("modal-%d", u.modalID), u.dialogReturnFocus()
	d.page = &modalPage{page}
	u.pages.AddPage(d.id, d.page, true, true)
	u.app.SetFocus(initial)
	return d
}
func (d *dialog) close() {
	if d.closed {
		return
	}
	d.closed = true
	d.u.pages.RemovePage(d.id)
	// tview retains the removed page in its backing array. Release the widget
	// tree (and any record payloads its callbacks hold) after unmounting it.
	if d.page != nil {
		d.page.Primitive = nil
	}
	d.u.modalDepth--
	if d.u.modalDepth == 0 {
		if d.u.current() != d.origin {
			// The action removed the location that owned the dialog.
			d.u.showLocation()
			return
		}
		d.u.refreshView()
		d.u.poll()
	}
	d.restoreFocus()
}

// A record or format may change while a dialog is open. Restore the logical
// payload pane so input never goes to a tree that has since been discarded.
func (u *ui) dialogReturnFocus() func() {
	focus := u.app.GetFocus()
	p := u.inspector
	if len(u.modalPayloads) > 0 {
		p = u.modalPayloads[len(u.modalPayloads)-1]
	}
	if p != nil {
		switch focus {
		case p.viewer, p.tree:
			return p.focusViewer
		case p.nodeView:
			return func() {
				if p.showingTree() {
					u.app.SetFocus(p.nodeView)
				} else {
					p.focusViewer()
				}
			}
		}
	}
	if focus == nil {
		return func() { u.focus(u.mainPane()) }
	}
	return func() { u.app.SetFocus(focus) }
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
	key, altOnly := strings.CutPrefix(key, "Alt+")
	if len(key) == 1 {
		if d.actions == nil {
			d.actions = map[rune]dialogAction{}
		}
		if altOnly {
			key = strings.ToLower(key)
		}
		d.actions[rune(key[0])] = dialogAction{button: b, run: action, altOnly: altOnly}
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
		if action, ok := d.actions[d.actionKey(event)]; ok && (!action.altOnly || alt(event) != 0) {
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

const quitMessage = "Active readers will stop and buffered records will be discarded."

type quitFrame struct{ *tview.Flex }

func (f quitFrame) SetRect(x, y, width, height int) {
	w := min(width, 50)
	h := min(height, 5+len(tview.WordWrap(quitMessage, max(1, w-4))))
	f.Flex.SetRect(x+(width-w)/2, y+(height-h)/2, w, h)
}

func (u *ui) quit() {
	text := tview.NewTextView().SetText(quitMessage).SetWordWrap(true).SetTextAlign(tview.AlignCenter)
	d := u.newDialog("Quit Katui?", text)
	d.root.RemoveItem(d.status)
	d.root.SetBorderPadding(1, 0, 1, 1)
	d.focusables = nil
	d.footer.AddItem(nil, 0, 1, false)
	d.button("Yes", "y", u.app.Stop)
	no := d.button("No", "n", d.close)
	d.footer.AddItem(nil, 0, 1, false)
	u.showDialog(d, quitFrame{d.root}, no)
}
func (u *ui) help() {
	k := keyHint
	api := u.b.backend == "api"
	workspaces := k("2") + " Consumer Groups  " + k("4") + " Clusters"
	setup := k("Alt+A") + " Brokers  " + k("Alt+B") + " Browse for Kafka CLI properties"
	if api {
		workspaces = k("1") + " Topics  " + k("2") + " Consumer Groups  " + k("3") + " Messages  " + k("4") + " Clusters"
		setup = k("Alt+A") + " Brokers  " + k("Alt+S") + " Security"
	}
	lines := []string{
		"Navigation",
		workspaces,
		k("Tab") + " / " + k("Shift+Tab") + " Move focus",
		k("↓/j") + " Down  " + k("↑/k") + " Up  " + k("PageUp") + " / " + k("PageDown") + " Page",
		"In tables, " + k("←/→") + " Scroll sideways  " + k("Ctrl+←/→") + " Scroll farther",
		k("Enter") + " Open selection  " + k("Esc") + " Back or cancel",
		k("/") + " Search  " + k("q") + " Quit",
		k("?") + " or " + k("Ctrl+P") + " Help & Commands  ·  Press " + k("?") + " again for the keyboard reference",
		"While typing, use the indicated Alt shortcuts or Tab to a button and press Enter.",
		"",
		"Kafka clusters",
		k("↓/j") + " " + k("↑/k") + " Select  " + k("Enter") + " / " + k("c") + " Connect  " + k("e") + " Edit  " + k("n") + " New",
		"In connection setup, " + setup,
		k("c") + " Connect  " + k("Alt+C") + " Connect while typing  " + k("Esc") + " Back or cancel",
		"",
		"Monitoring",
		k("s") + " Sort  " + k("r") + " Refresh or retry monitoring  " + k("z") + " Zoom",
		"Opened groups keep sampling at your configured interval while you browse or use a dialog. Switching clusters or quitting ends the session.",
		"Open a group, then a partition to see its consumption chart. A heavier segment of the bottom border marks a measured zero. A missing sample leaves the border unchanged.",
		"Offset discontinuities shows how often the log end or committed offset moved backward.",
		"Offsets are positions. Counts and rates based on offsets are estimates.",
	}
	if api {
		lines = append(lines,
			"",
			"Topics and administration",
			k("h")+" Show internal / Hide internal  "+k("g")+" Topic consumers",
			"ISR/Replicas totals in-sync and assigned replicas. Affected P counts partitions with a shortfall or missing leader. Yellow means one missing replica per affected partition. Red means a partition lacks a leader, has zero ISR, or is missing two or more replicas.",
			k("n")+" Create topic  "+k("a")+" Manage  "+k("d")+" Delete topic",
			"In topic forms, "+k("Alt+S")+" Stage  "+k("Alt+U")+" Unstage  "+k("Alt+D")+" Stage broker default (Configure only)",
			"In Configure topic, "+k("Alt+F")+" filters settings and "+k("Alt+B")+" moves to the results.",
			"Topic settings ending in .ms accept milliseconds or durations such as 1h30m, 1.2d or 6.5w. The value helper shows exact milliseconds. Use m for minutes and mo for 30-day months.",
			k("Alt+R")+" Review changes. Type the exact target name, then press "+k("Alt+A")+" to apply, or press "+k("Enter")+" to focus the confirmation button and "+k("Enter")+" again to activate it.",
			"Review requires a Kafka cluster ID. Apply checks the cluster ID again before writing.",
			"",
			"Consume",
			k("c")+" Open Consume  "+k("Alt+P")+" Focus partitions in the dialog",
			k("Space")+" Toggle partition  "+k("a")+" All  "+k("n")+" None",
			k("Alt+S")+" Start position  ·  Exact offset uses the same starting offset in every selected partition",
			"Keep following controls whether the reader waits for new records.",
			"Reading records does not join a consumer group or commit offsets.",
			k("Alt+C")+" Start consuming, including while typing",
			"",
			"Messages and payloads",
			k("i")+" Switch records/payload  "+k("b")+" Records  "+k("m")+" Format  "+k("z")+" Zoom focused pane",
			"Zoom the records list to browse without loading previews. "+k("z")+" or "+k("Esc")+" restores the split. "+k("Enter")+" inspects the highlighted record.",
			k("p")+" Pause or resume reader  "+k("x")+" Stop reader  ·  Clear buffer in Commands",
			k("f")+" Toggle Follow (off by default). Entering Cursor or selecting inspector text pins the current record. Press "+k("f")+" to follow again.",
			k("Space")+" Check or uncheck the row  "+k("a")+" Select all visible records  "+k("n")+" Unselect all",
			"Checked rows stay highlighted when you move to another message.",
			k("w")+" Compare selected records  ·  In comparison, "+k("1")+" Payloads  "+k("2")+" Metadata",
			k("o")+" CSV columns  "+k("u")+" Protobuf  "+k("d")+" Delimiter  "+k("Alt+H")+" Header",
			k(string(copyValueKey))+" Copy Raw Value  "+k(string(copyViewKey))+" Copy View  "+k("s")+" Save value to file",
			"In Scroll mode, "+k("←/→")+" Pan 8 columns  "+k("↑/↓")+" Scroll 4 lines",
			k("Arrows/hjkl")+" Navigate payload text and trees  ·  h left, j down, k up, l right",
			k("Alt+←/→")+" Pan 8 columns  "+k("Ctrl+←/→")+" Pan half the viewport  "+k("PgUp/PgDn")+" Page",
			k("Space")+" Toggle Cursor  ·  In Cursor, "+k("Arrows/hjkl")+" Move  "+k("v")+" Toggle Selection",
			"In Cursor and Selection, "+k("0")+" Line start  "+k("$")+" Line end",
			"In Selection, "+k("Arrows/hjkl")+" Extend or shrink  "+k("Esc")+" Return to Cursor",
			k("Shift+arrows")+" Extend selection  "+k("Ctrl+A")+" Select all  "+k("Ctrl+C")+" Copy selected text",
			k("y")+" Yank and exit Selection  ·  In Scroll or Cursor, copy the selected text or the rendered view",
			"In the tree, "+k("←/→")+" Collapse or expand nodes  "+k("Tab")+" Focus node text",
			k("/")+" Search payload in Fullscreen  "+k("Enter")+" Return to payload  "+k("n")+" Next  "+k("p")+" Previous",
			k("Alt+L")+" Load full if available  "+k("Enter")+" Confirm  "+k("Esc")+" Cancel",
			"",
			"Protobuf schema editor",
			k("b")+" Browse schema  "+k("i")+" Add import folder  "+k("l")+" Load schema  "+k("a")+" Apply",
		)
	}
	lines = append(lines, "",
		"File browser",
		k("Alt+P")+" Path  "+k("/")+" Filter  "+k("Alt+B")+" Files",
		k("Enter")+" Open  "+k("u")+" Up  "+k("h")+" Home  "+k(".")+" Hidden files",
	)
	if api {
		lines = append(lines, k("s")+" Choose folder when picking a directory")
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
	list.AddItem("Copy Raw Value", "", copyValueKey, func() { d.close(); p.copyValue() })
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
