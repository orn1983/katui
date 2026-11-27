package katui

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

func (u *ui) resetMessages() {
	if u.inspector != nil {
		u.inspector.setRecord(nil)
	}
	old := append(append([]*storedRecord{}, u.records...), u.checked...)
	old = append(old, u.pinned)
	u.records = nil
	clear(u.bufferedByID)
	u.checked = nil
	u.pinned = nil
	u.recordBytes = 0
	u.checkedBytes = 0
	u.evictions = 0
	u.followLatest = false
	*u.stacks[2][0] = location{kind: "messages", partition: -1}
	for _, r := range old {
		u.release(r)
	}
}
func (u *ui) release(record *storedRecord) {
	if record == nil || record.fullPath == "" || u.pinned == record || u.payloadJobs[record] > 0 {
		return
	}
	if u.bufferedByID[record.id] == record || slices.Contains(u.checked, record) {
		return
	}
	for _, pane := range u.modalPayloads {
		if slices.Contains(pane.records, record) {
			return
		}
	}
	record.discardPayload()
}
func (u *ui) buffer(r *storedRecord) {
	old := u.bufferedByID[r.id]
	u.bufferedByID[r.id] = r
	if old != nil {
		u.records[slices.Index(u.records, old)] = r
		u.recordBytes += r.cost - old.cost
		u.release(old)
	} else {
		u.records = append(u.records, r)
		u.recordBytes += r.cost
	}
	u.trimBuffer()
}
func (u *ui) trimBuffer() {
	for len(u.records) > 2000 || u.recordBytes > 16<<20 {
		old := u.records[0]
		delete(u.bufferedByID, old.id)
		u.records[0] = nil
		u.records = u.records[1:]
		u.recordBytes -= old.cost
		u.evictions++
		u.release(old)
	}
}
func (u *ui) isChecked(id recordID) int {
	for i, r := range u.checked {
		if r.id == id {
			return i
		}
	}
	return -1
}
func (u *ui) addCheck(r *storedRecord) bool {
	if u.isChecked(r.id) >= 0 {
		return true
	}
	if len(u.checked) >= 100 || u.checkedBytes+r.cost > 8<<20 {
		return false
	}
	u.checked = append(u.checked, r)
	u.checkedBytes += r.cost
	return true
}
func (u *ui) toggleCheck(r *storedRecord) {
	i := u.isChecked(r.id)
	if i >= 0 {
		was := u.checked[i]
		u.checkedBytes -= was.cost
		u.checked = slices.Delete(u.checked, i, i+1)
		u.release(was)
	} else if !u.addCheck(r) {
		u.say(fmt.Sprintf("Checked limit reached (%d/100, %s of 8 MiB)", len(u.checked), binarySize(ptr(int64(u.checkedBytes)))))
	}
	u.refreshView()
}
func (u *ui) pin(r *storedRecord) {
	if r == u.pinned {
		return
	}
	old := u.pinned
	u.pinned = r
	if u.inspector != nil {
		u.inspector.setRecord(r)
	}
	u.release(old)
}

func (r *storedRecord) matchesListFilter(filter string) bool {
	if filter == "" {
		return true
	}
	// Buffered keys and values are immutable. Only new records need scanning
	// on a live refresh while the user keeps the same filter.
	if r.listFilter != filter {
		r.listFilter = filter
		r.listMatch = strings.Contains(strings.ToLower(string(r.key)), filter) ||
			strings.Contains(strings.ToLower(string(r.value)), filter)
	}
	return r.listMatch
}

func listSnippet(data []byte, runes int) string {
	// One byte beyond the longest possible prefix lets shortened distinguish
	// an exact fit from a truncated value without copying the entire payload.
	return shortened(string(data[:min(len(data), utf8.UTFMax*runes+1)]), runes)
}

func (r *storedRecord) listCells(checked, pinned bool) []string {
	check := "[ ]"
	if checked {
		check = "[x]"
	}
	key := listSnippet(r.key, 100)
	if r.key == nil {
		key = "<null>"
	} else if len(r.key) == 0 {
		key = "<empty>"
	}
	value := listSnippet(r.value, 300)
	if r.value == nil {
		value = "<tombstone>"
	} else if len(r.value) == 0 {
		value = "<empty>"
	}
	if pinned {
		value = shortened("(pinned) "+value, 300)
	}
	return []string{check, payloadAvailability(r), clock(r.timestamp), key, fmt.Sprint(r.id.Partition), integer(r.id.offset), value}
}

func (u *ui) recordRows() ([]string, []int, []tableRow) {
	rows := make([]tableRow, 0, len(u.records))
	filter := strings.ToLower(u.current().filter)
	var pinnedOnly *storedRecord
	if !u.followLatest && u.pinned != nil && u.bufferedByID[u.pinned.id] == nil {
		pinnedOnly = u.pinned
	}
	checked := make(map[recordID]bool, len(u.checked))
	for _, r := range u.checked {
		checked[r.id] = true
	}
	addRow := func(r *storedRecord, pinned bool) {
		if !r.matchesListFilter(filter) {
			return
		}
		if r.listID == "" {
			r.listID = r.id.String()
		}
		rows = append(rows, tableRow{id: r.listID, topic: r.id.Topic, partition: r.id.Partition, record: r, checked: checked[r.id], pinned: pinned})
	}
	if pinnedOnly != nil {
		addRow(pinnedOnly, true)
	}
	for _, r := range u.records {
		addRow(r, false)
	}
	return []string{"✓", "Value", "Time", "Key", "P", "Offset", "Preview"}, []int{3, 12, 14, 13, 5, 12, 64}, rows
}
func payloadAvailability(r *storedRecord) string {
	if !r.fullAvailable() {
		return "Preview only"
	}
	return "Complete"
}

func (u *ui) stopFollowing() {
	if u.followLatest {
		u.followLatest = false
		u.updateControls("messages")
	}
}

func (u *ui) clearBuffer() {
	old := append([]*storedRecord{}, u.records...)
	old = append(old, u.pinned)
	u.records = nil
	clear(u.bufferedByID)
	u.recordBytes = 0
	u.pinned = nil
	if u.inspector != nil {
		u.inspector.setRecord(nil)
	}
	for _, r := range old {
		u.release(r)
	}
	u.refreshView()
}

func (u *ui) messageKeys(ev *tcell.EventKey) *tcell.EventKey {
	pane := u.inspector
	switch shortcutRune(ev) {
	case 'p':
		u.pauseReader()
	case 'x':
		u.stopReader()
		u.updateStatus()
	case 'f':
		u.followLatest = !u.followLatest
		u.refreshView()
	case 'i':
		if pane != nil {
			if pane.root.HasFocus() {
				u.focus(u.table)
			} else {
				u.inspectSelectedRecord()
			}
		}
	case 'm', 'd', 'h', 'o', 'u', 'y', copyValueKey, copyViewKey, 's', 'E':
		if pane != nil && pane.shortcut(ev.Rune()) {
			return nil
		}
		return ev
	case 'w':
		u.comparison()
	case 'n':
		old := u.checked
		u.checked = nil
		u.checkedBytes = 0
		for _, r := range old {
			u.release(r)
		}
		u.refreshView()
	case ' ':
		if u.table.HasFocus() {
			if r := u.selected().record; r != nil {
				u.toggleCheck(r)
			}
			return nil
		}
		return ev
	case 'a':
		skipped := 0
		for _, row := range u.rows {
			if !u.addCheck(row.record) {
				skipped++
			}
		}
		if skipped > 0 {
			u.say(fmt.Sprintf("%d skipped at checked limits (%d/100 · 8 MiB max)", skipped, len(u.checked)))
		}
		u.refreshView()
	default:
		return ev
	}
	return nil
}
func metadataText(r *storedRecord) string {
	key := fmt.Sprintf("%q", safe(string(r.key), false))
	if r.key == nil {
		key = "<null>"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s [%d] @%s  ·  %s  ·  %s\nKey %s", safe(r.id.Topic, false), r.id.Partition, integer(r.id.offset), clock(r.timestamp), binarySize(ptr(int64(r.valueSize))), key)
	if r.value == nil {
		out.WriteString("  ·  tombstone")
	}
	if len(r.headers) > 0 {
		out.WriteString("\nHeaders ")
		for i, h := range r.headers {
			if i > 0 {
				out.WriteString(", ")
			}
			fmt.Fprintf(&out, "%q: ", safe(h.Key, false))
			if h.Value == nil {
				out.WriteString("<null>")
			} else {
				fmt.Fprintf(&out, "%q", safe(string(h.Value), false))
			}
		}
		if r.headerCount > len(r.headers) {
			fmt.Fprintf(&out, "  (+%d more)", r.headerCount-len(r.headers))
		}
	}
	return out.String()
}
func (u *ui) fullscreenRecord() *payloadPane {
	if u.pinned == nil {
		u.say("Select a buffered record first")
		return nil
	}
	return u.payloadDialog(true, []*storedRecord{u.pinned}, u.profile(u.pinned.id.Topic).Mode)
}
func (u *ui) comparison() {
	if len(u.checked) == 0 {
		u.say("Check records with Space before opening comparison")
		return
	}
	records := append([]*storedRecord{}, u.checked...)
	u.payloadDialog(false, records, textMode(u.profile(records[0].id.Topic).Mode))
}
func (u *ui) payloadDialog(full bool, records []*storedRecord, mode string) *payloadPane {
	p := u.newPayloadPane(full, !full, records)
	p.modeName = mode
	p.rebuild()
	title := "Checked records"
	if full {
		title = "Fullscreen"
	}
	d := u.openDialog(title, p.root, p.content, full)
	p.dialog = d
	u.modalPayloads = append(u.modalPayloads, p)
	d.focusables = p.focusables()
	close := func() {
		p.cancelRender()
		u.modalPayloads = slices.Delete(u.modalPayloads, len(u.modalPayloads)-1, len(u.modalPayloads))
		for _, r := range records {
			u.release(r)
		}
		d.close()
		if u.inspector != nil {
			u.inspector.refresh()
		}
	}
	d.cancel = func() {
		if p.confirmLoad != nil {
			p.confirmLoad(false)
			return
		}
		if full && p.rendering {
			p.cancelRender()
			p.rendering = false
			p.result.incomplete = true
			p.loadCanceled = true
			p.updateLoadButton()
			p.status.SetText("Loading canceled")
			d.hint(keyHint("Alt+L") + " Load again  " + keyHint("z") + " Exit Fullscreen")
			return
		}
		if full && p.search.HasFocus() {
			p.focusViewer()
			return
		}
		if v, ok := u.app.GetFocus().(*payloadText); ok && v.exitMode() {
			return
		}
		close()
	}
	if full {
		p.loadButton = d.button("Load full", "l", p.loadFull)
		d.button("Prev", "p", func() { p.advance(-1) })
		d.button("Next", "n", func() { p.advance(1) })
	} else {
		d.button("Payloads", "1", func() { p.showMetadata(false) })
		d.button("Metadata", "2", func() { p.showMetadata(true) })
		d.button("Fullscreen", "z", func() { u.payloadDialog(true, records, p.modeName) })
	}
	d.button("Commands", "?", p.commands)
	d.button("Save", "s", p.exportValue)
	if full {
		d.button("Exit Fullscreen", "z", close)
	} else {
		d.button("Back", "Esc", func() { d.cancel() })
	}
	d.keymap(func(ev *tcell.EventKey) bool {
		if p.confirmLoad != nil {
			return ev.Key() != tcell.KeyEnter && ev.Key() != tcell.KeyTab && ev.Key() != tcell.KeyBacktab
		}
		if ev.Key() == tcell.KeyCtrlP {
			p.commands()
			return true
		}
		if isEditing(u.app.GetFocus()) {
			return false
		}
		if alt(ev) == 'h' && p.shortcut('h') {
			return true
		}
		key := shortcutRune(ev)
		switch key {
		case 'N':
			if full {
				p.advance(-1)
				return true
			}
		case '/':
			if full {
				u.app.SetFocus(p.search)
			} else {
				fullPane := u.payloadDialog(true, records, p.modeName)
				u.app.SetFocus(fullPane.search)
			}
			return true
		case 't':
			if !full || len(p.topics()) > 1 {
				p.chooseTopic(nil)
				return true
			}
		}
		if _, isButton := d.actions[key]; isButton {
			return false
		}
		return p.shortcut(key)
	})
	p.refresh()
	return p
}
