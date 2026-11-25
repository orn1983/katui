package katui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestFullLiveMessageBufferNavigation(t *testing.T) {
	const capacity = 2000
	u, screen, _ := startClusterUI(t, nil, "-b", "127.0.0.1:1")
	key := func(r rune) { screen.InjectKey(tcell.KeyRune, r, 0) }
	record := func(offset int, value string) *storedRecord {
		r := retainedRecord(t, nil, []byte(value))
		r.id.offset = int64(offset)
		return r
	}
	// Only the last record contains the search text, beyond its 300-rune list preview.
	value := strings.Repeat("x", 400) + "Needle"
	last := record(capacity-1, value)
	batch := make([]*storedRecord, capacity)
	for i := range capacity - 1 {
		batch[i] = record(i, "ordinary payload")
	}
	batch[capacity-1] = last
	u.post(func() {
		u.readTopic, u.readerState.state = last.id.Topic, "READING"
		for _, r := range batch {
			u.buffer(r)
		}
	})
	key('3')
	waitKeyboardState(t, u, "entering Messages did not draw the full buffer", func() bool {
		return u.workspace == 2 && u.table.HasFocus() && len(u.rows) == capacity && u.rows[0].cell != nil
	})
	u.post(func() {
		formatted := 0
		for _, row := range u.rows {
			if row.cell != nil {
				formatted++
			}
		}
		_, height := screen.Size()
		if formatted > 2*height {
			t.Errorf("entering Messages formatted %d rows for a %d-line terminal", formatted, height)
		}
	})
	screen.InjectKey(tcell.KeyEnd, 0, 0)
	key(' ')
	waitKeyboardState(t, u, "End and Space did not select and check the last record", func() bool {
		return u.pinned == last && len(u.checked) == 1 && u.checked[0] == last &&
			u.rows[capacity-1].checked && u.rows[capacity-1].cell != nil
	})
	key('1')
	waitKeyboardState(t, u, "could not leave Messages", func() bool { return u.workspace == 0 })
	arrive := func(r *storedRecord) {
		u.post(func() {
			u.buffer(r)
			u.readerViewDirty.Store(true)
		})
	}
	arrive(record(capacity, "ordinary arrival"))
	key('3')
	waitKeyboardState(t, u, "returning to a live buffer lost its selection or checked row", func() bool {
		return u.workspace == 2 && len(u.rows) == capacity && u.evictions == 1 &&
			u.selected().record == last && u.selected().checked && u.pinned == last
	})
	key('/')
	for _, c := range "NEEDLE" {
		key(c)
	}
	waitKeyboardState(t, u, "search missed text beyond the list preview", func() bool {
		return u.current().filter == "NEEDLE" && len(u.rows) == 1 && u.rows[0].record == last &&
			u.rows[0].cell != nil && !strings.Contains(u.rows[0].cell.Text, "Needle")
	})
	next := record(capacity+1, value)
	arrive(next)
	waitKeyboardState(t, u, "live refresh did not include a new match or preserve the checked record", func() bool {
		return len(u.rows) == 2 && u.rows[1].record == next && u.selected().record == last && u.selected().checked
	})
	screen.InjectKey(tcell.KeyEscape, 0, 0)
	key('j')
	waitKeyboardState(t, u, "the new filtered row could not be inspected", func() bool {
		return u.table.HasFocus() && u.pinned == next && len(u.rows) == 2 && !u.selected().checked && u.rows[1].cell != nil
	})
}

func TestMessageBufferIndexTracksReplacementsAndEvictions(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	first := &storedRecord{id: recordID{partKey{"a", 1}, 7}, cost: 100}
	second := &storedRecord{id: recordID{partKey{"a", 0}, 7}, cost: 100}
	replacement := &storedRecord{id: first.id, cost: 50}
	u.buffer(first)
	u.buffer(second)
	u.buffer(replacement)
	if len(u.records) != 2 || u.records[0] != replacement || u.bufferedByID[first.id] != replacement || u.recordBytes != 150 {
		t.Fatal("replacing an offset lost its position or kept the old record")
	}
	// Keep the evicted record inspectable, then read its offset again.
	u.pinned = replacement
	u.buffer(&storedRecord{id: recordID{partKey{"b", 0}, 7}, cost: (16 << 20) - 100})
	_, _, rows := u.recordRows()
	if len(rows) != 3 || !rows[0].pinned || rows[0].record != replacement || u.bufferedByID[first.id] != nil {
		t.Fatal("an evicted pinned record must appear once outside the buffer")
	}
	u.buffer(first)
	_, _, rows = u.recordRows()
	if len(rows) != 2 || rows[0].pinned || rows[1].record != first {
		t.Fatal("reading an evicted offset again left a duplicate pinned row")
	}
	u.clearBuffer()
	u.buffer(replacement)
	if len(u.records) != 1 || len(u.bufferedByID) != 1 || u.records[0] != replacement {
		t.Fatal("clearing the buffer retained old offsets")
	}
	u.resetMessages()
	if len(u.bufferedByID) != 0 {
		t.Fatal("a new reader retained the previous reader's offsets")
	}
}
