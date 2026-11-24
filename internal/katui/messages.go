package katui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
)

func (u *ui) resetMessages() {
	if u.inspector != nil {
		u.inspector.setRecords(nil)
	}
	old := append(append([]*storedRecord{}, u.records...), u.checked...)
	old = append(old, u.pinned)
	u.records = nil
	u.checked = nil
	u.pinned = nil
	u.recordBytes = 0
	u.checkedBytes = 0
	u.evictions = 0
	u.followLatest = true
	*u.stacks[2][0] = location{kind: "messages", partition: -1}
	for _, r := range old {
		u.release(r)
	}
}
func (u *ui) release(record *storedRecord) {
	if record == nil || record.fullPath == "" || u.pinned == record || u.payloadJobs[record] > 0 {
		return
	}
	for _, set := range [][]*storedRecord{u.records, u.checked, u.modalRecords} {
		for _, r := range set {
			if r == record {
				return
			}
		}
	}
	record.releasePayload()
}
func (u *ui) buffer(r *storedRecord) {
	for i, old := range u.records {
		if old.id == r.id {
			u.records[i] = r
			u.recordBytes += r.cost - old.cost
			u.release(old)
			u.trimBuffer()
			return
		}
	}
	u.records = append(u.records, r)
	u.recordBytes += r.cost
	u.trimBuffer()
}
func (u *ui) trimBuffer() {
	for len(u.records) > 2000 || u.recordBytes > 16<<20 {
		old := u.records[0]
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
		u.checked = append(u.checked[:i], u.checked[i+1:]...)
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
		u.inspector.setRecords([]*storedRecord{r})
	}
	u.release(old)
}
func (u *ui) recordRows() ([]string, []int, []tableRow) {
	rows := make([]tableRow, 0, len(u.records))
	filter := u.current().filter
	var pinnedOnly *storedRecord
	if !u.followLatest && u.pinned != nil {
		pinnedOnly = u.pinned
		for _, r := range u.records {
			if r.id == pinnedOnly.id {
				pinnedOnly = nil
				break
			}
		}
	}
	addRow := func(r *storedRecord, pinned bool) {
		if !has(string(r.key), filter) && !has(string(r.value), filter) {
			return
		}
		check := "[ ]"
		if u.isChecked(r.id) >= 0 {
			check = "[x]"
		}
		key, value := string(r.key), string(r.value)
		if r.key == nil {
			key = "<null>"
		} else if len(r.key) == 0 {
			key = "<empty>"
		}
		if r.value == nil {
			value = "<tombstone>"
		} else if len(r.value) == 0 {
			value = "<empty>"
		}
		if pinned {
			value = "(pinned) " + value
		}
		rows = append(rows, tableRow{pinnedOnly: pinned, id: r.id.String(), topic: r.id.Topic, partition: r.id.Partition, record: r, cells: []string{
			check, payloadAvailability(r), clock(r.timestamp), shortened(key, 100), fmt.Sprint(r.id.Partition), integer(r.id.offset), shortened(value, 300),
		}})
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

func (u *ui) messageKeys(ev *tcell.EventKey) *tcell.EventKey {
	if u.inspector != nil && u.inspector.mode.IsOpen() {
		return ev
	}
	pane := u.inspector
	switch ev.Rune() {
	case 'p':
		u.pauseReader()
	case 'x':
		u.stopReader()
		u.updateStatus()
	case 'l':
		old := append([]*storedRecord{}, u.records...)
		old = append(old, u.pinned)
		u.records = nil
		u.recordBytes = 0
		u.pinned = nil
		if pane != nil {
			pane.setRecords(nil)
		}
		for _, r := range old {
			u.release(r)
		}
		u.refreshView()
	case 'f':
		u.followLatest = !u.followLatest
		u.refreshView()
		if u.followLatest {
			u.say("Follow latest ON")
		} else {
			u.say("Follow latest OFF")
		}
	case 'i':
		if pane != nil {
			if u.fullscreen == "table" {
				u.fullscreen = ""
				u.refreshView()
			}
			pane.focusFirst()
		}
	case 'm', 'd', 'h', 'o', 'u', 'y', 'v', 'E':
		if pane != nil && pane.shortcut(ev.Rune()) {
			return nil
		}
		return ev
	case 'w':
		u.comparison()
	case 'e':
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
		if !u.table.HasFocus() {
			return ev
		}
		skipped := 0
		for _, row := range u.rows {
			if !row.pinnedOnly && !u.addCheck(row.record) {
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
func (u *ui) fullRecord() {
	if u.pinned == nil {
		u.say("Select a buffered record first")
		return
	}
	u.payloadDialog(true, []*storedRecord{u.pinned}, u.profile(u.pinned.id.Topic).Mode)
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
	length := len(u.modalRecords)
	u.modalRecords = append(u.modalRecords, records...)
	p := u.newPayloadPane(full, !full, records)
	p.modeName = mode
	p.rebuild()
	title := "Checked records"
	if full {
		title = "Full payload"
	}
	d := u.dialog(title, p.root, p.mode)
	p.dialog = d
	d.focusables = p.focusables()
	close := func() {
		p.cancelRender()
		u.modalRecords = u.modalRecords[:length]
		for _, r := range records {
			u.release(r)
		}
		d.close()
		if u.inspector != nil {
			u.inspector.refresh()
		}
	}
	d.cancel = func() {
		if full && p.rendering {
			p.cancelRender()
			p.rendering = false
			p.result.incomplete = true
			p.status.SetText("Loading canceled")
			d.say("Press L to load again or Esc to close.")
			return
		}
		close()
	}
	if full {
		d.button("Load full L", p.loadFull)
		d.button("Prev p", func() { p.advance(-1) })
		d.button("Next n", func() { p.advance(1) })
	} else {
		d.button("Payloads 1", func() { p.showMetadata(false) })
		d.button("Metadata 2", func() { p.showMetadata(true) })
		d.button("Full F4", func() { u.payloadDialog(true, records, p.modeName) })
	}
	d.button("Value y", p.copyValue)
	d.button("View v", p.copyView)
	d.button("Export E", p.exportValue)
	d.button("Back Esc", close)
	d.keymap(func(ev *tcell.EventKey) bool {
		if isEditing(u.app.GetFocus()) {
			return false
		}
		if ev.Key() == tcell.KeyF4 && !full {
			u.payloadDialog(true, records, p.modeName)
			return true
		}
		if ev.Key() != tcell.KeyRune || ev.Modifiers()&^tcell.ModShift != 0 {
			return false
		}
		switch ev.Rune() {
		case '/':
			if full {
				u.app.SetFocus(p.search)
				return true
			}
		case 'n':
			if full {
				p.advance(1)
				return true
			}
		case 'p':
			if full {
				p.advance(-1)
				return true
			}
		case '1':
			if !full {
				p.showMetadata(false)
				return true
			}
		case '2':
			if !full {
				p.showMetadata(true)
				return true
			}
		case 't':
			if !full || len(p.topics()) > 1 {
				p.chooseTopic(nil)
				return true
			}
		}
		return p.shortcut(ev.Rune())
	})
	p.refresh()
	return p
}
