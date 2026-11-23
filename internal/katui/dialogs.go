package katui

import (
	"fmt"

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

func (u *ui) dialog(title string, body tview.Primitive, initial tview.Primitive) *dialog {
	u.modalDepth++
	u.modalID++
	if u.modalDepth == 1 {
		u.invalidateMonitor()
	}
	d := &dialog{u: u, id: fmt.Sprintf("modal-%d", u.modalID), body: body, previous: u.app.GetFocus()}
	d.status = tview.NewTextView().SetTextColor(tcell.ColorLightGoldenrodYellow)
	d.footer = tview.NewFlex()
	d.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(body, 0, 1, true).AddItem(d.status, 2, 0, false).AddItem(d.footer, 1, 0, false)
	d.root.SetBorder(true).SetTitle(" " + tview.Escape(safe(title, false)) + " ").SetBorderColor(u.accent)
	d.cancel = d.close
	d.focusables = []tview.Primitive{body}
	outer := tview.NewGrid().SetRows(1, 0, 1).SetColumns(2, 0, 2).
		AddItem(d.root, 1, 1, 1, 1, 0, 0, true)
	u.pages.AddPage(d.id, outer, true, true)
	d.keymap(nil)
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
	if !d.closed {
		d.status.SetText(safe(text, true))
	}
}
func (d *dialog) button(label string, action func()) *tview.Button {
	b := tview.NewButton(label).SetSelectedFunc(action)
	d.buttons = append(d.buttons, b)
	d.footer.AddItem(b, len([]rune(label))+2, 0, false)
	return b
}
func (d *dialog) keymap(handle func(*tcell.EventKey) bool) {
	d.root.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			if dd, ok := d.u.app.GetFocus().(*tview.DropDown); ok && dd.IsOpen() {
				return event
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
	d.button("Yes y", yes)
	no := d.button("No n", d.close)
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
	text := tview.NewTextView().SetScrollable(true).SetText(`1 / 2 / 3       Topics / Consumer Groups / Messages
Tab / Shift+Tab Move focus
Arrows / j / k  Navigate
PageUp / Down   Page
Enter           Open the selected row
Esc             Back / close
/               Search; Enter returns to results
s / r           Sort / refresh
z               Fullscreen / restore
c               Start a read-only reader
Ctrl+P          Command palette
q               Quit confirmation

Topics         h Internal topics · g Topic consumers
               n Create · a Manage · d Delete
Groups         a Manage selected group or partition
Messages       b Records · i Inspector · m Mode · F4 Full payload
               p Pause · x Stop · l Clear rolling buffer · f Follow latest
               Space Check · a Check matching · w Compare · e Uncheck all
               o CSV columns · u Protobuf · d Delimiter · h Header
               y Copy whole value · v Copy view
Full payload   / Search · n Next match · p Previous match

Offsets are positions. Retained counts and rates are estimates.
Read-only inspection does not join an application consumer group.`)
	text.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, nil, text) })
	d := u.dialog("Help", text, text)
	d.button("Back Esc", d.close)
}
func (u *ui) palette() {
	list := tview.NewList().ShowSecondaryText(false)
	d := u.dialog("Commands", list, list)
	add := func(label string, key rune, fn func()) { list.AddItem(label, "", key, func() { d.close(); fn() }) }
	add("Topics", '1', func() { u.changeWorkspace(0) })
	add("Consumer Groups", '2', func() { u.changeWorkspace(1) })
	add("Messages", '3', func() { u.changeWorkspace(2) })
	add("Refresh monitoring", 'r', u.requestRefresh)
	add("Change color theme", 't', u.theme)
	add("Help", '?', u.help)
	add("Quit", 'q', u.quit)
}
func (u *ui) theme() {
	list := tview.NewList().ShowSecondaryText(false)
	d := u.dialog("Color theme", list, list)
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
