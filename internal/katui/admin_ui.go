package katui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) topicForm(topic, initial string) {
	form := tview.NewForm().SetItemPadding(0)
	operations := []string{opConfig, opDeleteTopic}
	if initial == opCreate {
		operations = []string{opCreate}
	}
	var d *dialog
	var render func()
	op := dropdown(form, buttonLabel("Operation", "Alt+O"), operations, indexOf(operations, initial), func(int) {
		if render != nil {
			render()
		}
	})
	name := field(form, buttonLabel("Name", "Alt+N"), topic)
	name.SetDisabled(initial != opCreate)
	partitions := field(form, buttonLabel("Partitions", "Alt+P"), "1")
	replicas := field(form, buttonLabel("Replicas", "Alt+F"), "1")
	partitions.SetDisabled(initial != opCreate)
	replicas.SetDisabled(initial != opCreate)
	key := field(form, buttonLabel("Setting key", "Alt+K"), "")
	value := field(form, buttonLabel("Setting value", "Alt+V"), "")
	search := tview.NewInputField().SetLabel(buttonLabel("Settings", "Alt+/") + " ")
	table := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	table.SetBorder(true).SetTitle(" " + buttonLabel("Current settings", "Alt+B") + " ")
	stagedText := tview.NewTextView().SetScrollable(true).SetWrap(false)
	stagedText.SetBorder(true).SetTitle(" Staged settings ")
	side := tview.NewFlex().SetDirection(tview.FlexRow)
	body := tview.NewFlex()
	if u.width < 100 {
		body.SetDirection(tview.FlexRow)
	}
	d = u.dialog(initial, body, form)
	d.focusables = []tview.Primitive{form, search, table, stagedText}
	settings := map[string]setting{}
	staged := map[string]*string{}
	var visible []string
	updateStaged := func() {
		var out strings.Builder
		for _, key := range sortedKeys(staged) {
			if staged[key] == nil {
				fmt.Fprintf(&out, "%s → inherit broker default\n", key)
			} else {
				fmt.Fprintf(&out, "%s → %q\n", key, *staged[key])
			}
		}
		if out.Len() == 0 {
			out.WriteString("No staged changes")
		}
		stagedText.SetText(safe(out.String(), true))
	}
	drawSettings := func() {
		selected := ""
		row, col := table.GetSelection()
		if row > 0 && row <= len(visible) {
			selected = visible[row-1]
		}
		top, left := table.GetOffset()
		visible = nil
		table.Clear()
		headers, widths := []string{"Setting", "Display", "Raw", "Source"}, []int{28, 16, 20, 20}
		for i, heading := range headers {
			table.SetCell(0, i, tableCell(heading, widths[i]).SetSelectable(false))
		}
		active := 1
		for _, k := range sortedKeys(settings) {
			s := settings[k]
			raw, display := s.value, s.value
			if s.sensitive {
				raw = "<sensitive>"
				display = raw
			} else if !s.available {
				raw = "?"
				display = "unavailable"
			} else if n, err := strconv.ParseInt(s.value, 10, 64); err == nil {
				switch {
				case strings.HasSuffix(k, ".ms") && n >= 0:
					display = integer(n/1000) + "s " + integer(n%1000) + "ms"
				case strings.HasSuffix(k, ".bytes") && n >= 0:
					display = binarySize(&n)
				}
			}
			source := s.source
			if s.readOnly {
				source += " · read-only"
			}
			if !has(strings.Join([]string{k, raw, display, source}, " "), search.GetText()) {
				continue
			}
			visible = append(visible, k)
			i := len(visible)
			if k == selected {
				active = i
			}
			for j, text := range []string{k, display, raw, source} {
				table.SetCell(i, j, tableCell(text, widths[j]))
			}
		}
		table.Select(active, col).SetOffset(top, left)
	}
	search.SetChangedFunc(func(string) { drawSettings() })
	search.SetDoneFunc(func(e tcell.Key) {
		if e == tcell.KeyEnter {
			u.app.SetFocus(table)
		}
	})
	table.SetSelectedFunc(func(row, _ int) {
		if row < 1 || row > len(visible) {
			return
		}
		k := visible[row-1]
		s := settings[k]
		key.SetText(k)
		if s.available {
			value.SetText(s.value)
		} else {
			value.SetText("")
		}
		focusForm(u, form, 5)
	})
	table.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, table, nil) })
	stagedText.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, nil, stagedText) })
	busy := false
	stage := func(remove bool) {
		_, operation := op.GetCurrentOption()
		if busy || operation == opDeleteTopic {
			return
		}
		k := strings.TrimSpace(key.GetText())
		if k == "" {
			d.say("Enter a configuration key")
			return
		}
		if operation == opConfig {
			if err := modifiable(k, settings[k]); err != nil {
				d.say(err.Error())
				return
			}
		}
		if remove {
			if operation == opCreate {
				delete(staged, k)
			} else {
				staged[k] = nil
			}
		} else {
			v, err := configValue(k, value.GetText())
			if err != nil {
				d.say(err.Error())
				return
			}
			staged[k] = ptr(v)
		}
		updateStaged()
		d.say(fmt.Sprintf("%d staged settings", len(staged)))
	}
	unstage := func() { delete(staged, strings.TrimSpace(key.GetText())); updateStaged() }
	var reviewButton *tview.Button
	review := func() {
		if busy || d.closed {
			return
		}
		_, operation := op.GetCurrentOption()
		in := adminInput{operation: operation, target: name.GetText(), settings: maps.Clone(staged)}
		if operation == opCreate {
			p, e := strconv.ParseInt(partitions.GetText(), 10, 32)
			r, e2 := strconv.ParseInt(replicas.GetText(), 10, 16)
			if e != nil || e2 != nil || p <= 0 || r <= 0 {
				d.say("Use positive partitions and replication factor 1–32767")
				return
			}
			in.partitions = int32(p)
			in.replicas = int16(r)
		}
		u.prepareReview(d, in, &busy, reviewButton)
	}
	d.button("Stage", "s", func() { stage(false) })
	d.button("Default", "d", func() { stage(true) })
	d.button("Unstage", "u", unstage)
	reviewButton = d.button("Review", "r", review)
	d.button("Cancel", "Esc", d.close)
	render = func() {
		_, operation := op.GetCurrentOption()
		edit := operation != opDeleteTopic
		key.SetDisabled(!edit)
		value.SetDisabled(!edit)
		body.Clear().AddItem(form, 0, 1, true)
		side.Clear()
		if operation == opConfig {
			side.AddItem(search, 1, 0, false).AddItem(table, 0, 2, true).AddItem(stagedText, 0, 1, false)
			body.AddItem(side, 0, 1, false)
			d.focusables = []tview.Primitive{form, search, table, stagedText}
		} else if operation == opCreate {
			body.AddItem(stagedText, 0, 1, false)
			d.focusables = []tview.Primitive{form, stagedText}
		} else {
			d.focusables = []tview.Primitive{form}
		}
		for i := 0; i < 3; i++ {
			d.buttons[i].SetDisabled(!edit)
		}
		updateStaged()
	}
	d.keymap(func(e *tcell.EventKey) bool {
		_, operation := op.GetCurrentOption()
		switch alt(e) {
		case 'o':
			focusForm(u, form, 0)
		case 'n':
			if operation == opCreate {
				focusForm(u, form, 1)
			}
		case 'p':
			if operation == opCreate {
				focusForm(u, form, 2)
			}
		case 'f':
			if operation == opCreate {
				focusForm(u, form, 3)
			}
		case '/':
			if operation == opConfig {
				u.app.SetFocus(search)
			}
		case 'b':
			if operation == opConfig {
				u.app.SetFocus(table)
			}
		case 'k':
			if operation != opDeleteTopic {
				focusForm(u, form, 4)
			}
		case 'v':
			if operation != opDeleteTopic {
				focusForm(u, form, 5)
			}
		case 'g':
			review()
		default:
			if form.HasFocus() && (e.Key() == tcell.KeyTab || e.Key() == tcell.KeyBacktab) {
				i, _ := form.GetFocusedItemIndex()
				if e.Key() == tcell.KeyTab && i < form.GetFormItemCount()-1 || e.Key() == tcell.KeyBacktab && i > 0 {
					return passFormKey(form, e, u)
				}
			}
			return false
		}
		return true
	})
	render()
	focusForm(u, form, 0)
	if initial == opCreate {
		focusForm(u, form, 1)
		return
	}
	d.say("Reading current topic settings…")
	go func() {
		ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
		defer cancel()
		s, err := u.b.settings(ctx, topic)
		u.post(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.say(err.Error())
				return
			}
			settings = s
			drawSettings()
			d.say("")
		})
	}()
}

func (u *ui) groupForm(group, scope, topic string, partition int32) {
	form := tview.NewForm().SetItemPadding(0)
	selector := newPartitionSelector(u)
	body := tview.NewFlex().SetDirection(tview.FlexRow)
	if u.width >= 100 {
		body.SetDirection(tview.FlexColumn)
	}
	var render func()
	op := dropdown(form, buttonLabel("Operation", "Alt+O"), []string{opReset, opRebalance, opDeleteGroup}, 0, func(int) {
		if render != nil {
			render()
		}
	})
	var value *tview.InputField
	mode := dropdown(form, buttonLabel("Mode", "Alt+M"), []string{"Timestamp", "Absolute offset", "Earliest retained", "Latest"}, 0, func(i int) {
		if value != nil {
			value.SetDisabled(i >= 2)
		}
	})
	value = field(form, buttonLabel("Value", "Alt+V"), "")
	d := u.dialog("Manage group · "+group, body, form)
	notice := tview.NewTextView().SetWordWrap(true)
	selector.changed = func() {
		d.say(fmt.Sprintf("%d / %d committed partitions selected", len(selector.selected()), len(selector.ids)))
	}
	var reviewButton *tview.Button
	busy := false
	review := func() {
		if busy || d.closed {
			return
		}
		_, operation := op.GetCurrentOption()
		m, _ := mode.GetCurrentOption()
		in := adminInput{operation: operation, target: group, selected: selector.selected(), mode: m, value: value.GetText()}
		if operation == opReset && len(in.selected) == 0 {
			d.say("Select at least one committed partition")
			return
		}
		u.prepareReview(d, in, &busy, reviewButton)
	}
	all := d.button("All", "a", func() { selector.every(true) })
	none := d.button("None", "n", func() { selector.every(false) })
	reviewButton = d.button("Review", "r", review)
	d.button("Cancel", "Esc", d.close)
	render = func() {
		_, operation := op.GetCurrentOption()
		reset := operation == opReset
		mode.SetDisabled(!reset)
		index, _ := mode.GetCurrentOption()
		value.SetDisabled(!reset || index >= 2)
		all.SetDisabled(!reset)
		none.SetDisabled(!reset)
		body.Clear().AddItem(form, 0, 1, true)
		if reset {
			body.AddItem(selector.list, 0, 2, false)
			d.focusables = []tview.Primitive{form, selector.list}
			selector.changed()
		} else {
			notice.SetText("This operation affects the entire consumer group across all topics. Review shows the exact effect.")
			body.AddItem(notice, 0, 1, false)
			d.focusables = []tview.Primitive{form}
			d.say("Group-wide operation")
		}
	}
	render()
	focusForm(u, form, 0)
	d.keymap(func(e *tcell.EventKey) bool {
		_, operation := op.GetCurrentOption()
		reset := operation == opReset
		switch alt(e) {
		case 'o':
			focusForm(u, form, 0)
		case 'p':
			if reset {
				u.app.SetFocus(selector.list)
			}
		case 'm':
			if reset {
				focusForm(u, form, 1)
			}
		case 'v':
			if reset {
				focusForm(u, form, 2)
			}
		case 'g':
			review()
		default:
			if form.HasFocus() && (e.Key() == tcell.KeyTab || e.Key() == tcell.KeyBacktab) {
				i, _ := form.GetFocusedItemIndex()
				last := 0
				if reset {
					last = 2
					if index, _ := mode.GetCurrentOption(); index >= 2 {
						last = 1
					}
				}
				if e.Key() == tcell.KeyTab && i < last || e.Key() == tcell.KeyBacktab && i > 0 {
					return passFormKey(form, e, u)
				}
			}
			return false
		}
		return true
	})
	d.say("Loading stored offsets…")
	go func() {
		ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
		defer cancel()
		offsets, err := u.b.storedOffsets(ctx, group)
		u.post(func() {
			if d.closed {
				return
			}
			if err != nil {
				d.say(err.Error())
				return
			}
			for k := range offsets {
				if scope != "" && k.Topic != scope {
					continue
				}
				selector.ids = append(selector.ids, k)
				selector.checked[k] = k.Topic == topic && partition == k.Partition
			}
			slices.SortFunc(selector.ids, comparePart)
			selector.draw()
		})
	}()
}

func (u *ui) prepareReview(d *dialog, in adminInput, busy *bool, button *tview.Button) {
	*busy = true
	button.SetDisabled(true)
	d.say("Validating and preparing a read-only review…")
	go func() {
		ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
		defer cancel()
		plan, err := u.b.prepareAdmin(ctx, in)
		u.post(func() {
			if d.closed {
				return
			}
			*busy = false
			button.SetDisabled(false)
			if err != nil {
				d.say(err.Error())
				return
			}
			d.close()
			u.adminReview(plan)
		})
	}()
}
func (u *ui) adminReview(plan adminPlan) {
	preview := tview.NewTextView().SetText(safe(plan.review, true)).SetScrollable(true).SetWordWrap(true)
	preview.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, nil, preview) })
	confirmation := tview.NewInputField().SetLabel("Exact target name  ")
	body := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(preview, 0, 1, false).AddItem(confirmation, 1, 0, true)
	d := u.dialog("Review · "+plan.operation, body, confirmation)
	d.focusables = []tview.Primitive{confirmation, preview}
	d.say("Type the exact target name, then choose Apply. One attempt is allowed.")
	var applyButton, cancelButton *tview.Button
	attempted, inFlight := false, false
	apply := func() {
		if attempted || d.closed || confirmation.GetText() != plan.target {
			return
		}
		attempted = true
		inFlight = true
		applyButton.SetDisabled(true)
		cancelButton.SetDisabled(true)
		confirmation.SetDisabled(true)
		d.say("Revalidating reviewed state and applying once…")
		go func() {
			ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
			defer cancel()
			sent, err := u.b.applyAdmin(ctx, plan)
			u.post(func() {
				inFlight = false
				cancelButton.SetDisabled(false).SetLabel(buttonLabel("Back", "Esc"))
				u.app.SetFocus(cancelButton)
				if err != nil {
					outcome := "Review rejected. "
					if sent {
						outcome = "Apply failed. Kafka may have changed some state. "
					}
					outcome += err.Error() + " · A new review is required."
					d.say(outcome)
					u.say(outcome)
				} else {
					outcome := "Kafka accepted " + strings.ToLower(plan.operation) + " for " + plan.target
					if plan.operation == opDeleteTopic {
						u.eraseTopic(plan.target)
						outcome += ". Waiting for metadata to confirm deletion."
					}
					if plan.operation == opRebalance {
						outcome += ". Group convergence is pending."
					}
					d.say(outcome)
					u.say(outcome)
				}
				u.invalidateMonitor()
			})
		}()
	}
	d.cancel = func() {
		if !inFlight {
			d.close()
		}
	}
	applyButton = d.button("Apply", "a", apply).SetDisabled(true)
	cancelButton = d.button("Cancel", "Esc", d.cancel)
	confirmation.SetChangedFunc(func(value string) {
		if !attempted {
			applyButton.SetDisabled(value != plan.target)
		}
	})
	confirmation.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter && !applyButton.IsDisabled() {
			u.app.SetFocus(applyButton)
		}
	})
	d.keymap(func(e *tcell.EventKey) bool {
		return inFlight && e.Key() == tcell.KeyEnter
	})
}
