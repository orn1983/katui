package katui

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const (
	startEnd = iota
	startRecent
	startBeginning
	startTimestamp
	startOffset
)

var starts = []string{"Live/end", "Recent offsets", "Retained beginning", "Timestamp", "Exact offset"}
var operators = []string{"Contains", "Starts with", "Equals"}
var errReaderOffset = errors.New("starting offset is outside the available range")

type consumeOptions struct {
	partitions            []int32
	start, keyOp, valueOp int
	recent, limit, offset int64
	timestamp, key, value string
	follow                bool
}
type readerSession struct {
	cancel context.CancelFunc
	paused atomic.Bool
	wake   chan struct{}
}
type readerUpdate struct {
	state, info      string
	scanned, matched int64
	records          []*storedRecord
}

type recordID struct {
	partKey
	offset int64
}

func (id recordID) String() string { return fmt.Sprintf("%s@%d", id.partKey, id.offset) }

type storedRecord struct {
	id                           recordID
	timestamp                    time.Time
	key, value                   []byte
	headers                      []kgo.RecordHeader
	headerCount, valueSize, cost int
	fullPath                     string
	store                        *payloadStore
}

func (r *storedRecord) fullAvailable() bool {
	return !r.truncated() || r.fullPath != ""
}

func (r *storedRecord) openFull() (io.ReadCloser, error) {
	if !r.fullAvailable() {
		return nil, errPayloadQuota
	}
	if r.fullPath == "" {
		return io.NopCloser(bytes.NewReader(r.value)), nil
	}
	file, err := os.Open(r.fullPath)
	if err != nil {
		return nil, fmt.Errorf("original unavailable: %w", err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != int64(r.valueSize) {
		file.Close()
		return nil, errors.New("retained payload is incomplete")
	}
	return file, nil
}

func (r *storedRecord) full() ([]byte, error) {
	if !r.truncated() && r.fullPath == "" {
		return r.value, nil
	}
	file, err := r.openFull()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(r.valueSize)+1))
	if err == nil && len(data) != r.valueSize {
		err = errors.New("retained payload is incomplete")
	}
	return data, err
}

func (r *storedRecord) truncated() bool { return r.valueSize > len(r.value) }

func (r *storedRecord) releasePayload() {
	if r.store != nil && r.fullPath != "" {
		r.store.remove(r.fullPath)
	}
}

func (r *storedRecord) discardPayload() {
	if r.store != nil && r.fullPath != "" {
		r.store.discard(r.fullPath)
	}
}

func clip(data []byte, n int) []byte {
	if data == nil {
		return nil
	}
	return append([]byte{}, data[:min(n, len(data))]...)
}
func saveRecord(r *kgo.Record, store *payloadStore) (*storedRecord, error) {
	record := &storedRecord{id: recordID{partKey{r.Topic, r.Partition}, r.Offset}, timestamp: r.Timestamp, key: clip(r.Key, 4096),
		value: clip(r.Value, 65536), headerCount: len(r.Headers), valueSize: len(r.Value), store: store}
	if r.Timestamp.Equal(time.UnixMilli(-1)) {
		record.timestamp = time.Time{}
	}
	record.cost = 128 + len(record.key) + len(record.value) + len(record.id.Topic)
	for _, header := range r.Headers[:min(len(r.Headers), 64)] {
		name := strings.Clone(header.Key[:min(len(header.Key), 256)])
		value := clip(header.Value, 1024)
		record.headers = append(record.headers, kgo.RecordHeader{Key: name, Value: value})
		record.cost += len(name) + len(value) + 32
	}
	if record.truncated() {
		path, err := store.save(r.Value)
		if err != nil && !errors.Is(err, errPayloadQuota) {
			return nil, err
		}
		record.fullPath = path
	}
	return record, nil
}

func timestamp(text string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if at, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
			return at, nil
		}
	}
	return time.Time{}, errors.New("use an ISO 8601 timestamp, e.g. 2025-03-14T12:00:00Z")
}
func predicate(data []byte, pattern string, op int) bool {
	if pattern == "" {
		return true
	}
	s := strings.ToLower(strings.ToValidUTF8(string(data), "�"))
	switch op {
	case 1:
		return strings.HasPrefix(s, pattern)
	case 2:
		return s == pattern
	default:
		return strings.Contains(s, pattern)
	}
}

type partitionSelector struct {
	list    *tview.Table
	ids     []partKey
	checked map[partKey]bool
	changed func()
}

func newPartitionSelector(u *ui) *partitionSelector {
	s := &partitionSelector{list: tview.NewTable().SetSelectable(true, false), checked: map[partKey]bool{}}
	s.list.SetBorder(true).SetTitle(" Partitions · " + keyHint("Alt+P") + " · " + keyHint("Space") + " ")
	s.list.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if e.Key() == tcell.KeyRune && e.Rune() == ' ' {
			row, _ := s.list.GetSelection()
			s.toggle(row)
			return nil
		}
		return u.viewerKeys(e, s.list, nil)
	})
	s.list.SetSelectedFunc(func(r, _ int) { s.toggle(r) })
	return s
}
func (s *partitionSelector) toggle(i int) {
	if i >= 0 && i < len(s.ids) {
		k := s.ids[i]
		s.checked[k] = !s.checked[k]
		s.draw()
	}
}
func (s *partitionSelector) every(on bool) {
	for _, id := range s.ids {
		s.checked[id] = on
	}
	s.draw()
}
func (s *partitionSelector) selected() []partKey {
	keys := []partKey{}
	for _, id := range s.ids {
		if s.checked[id] {
			keys = append(keys, id)
		}
	}
	return keys
}
func (s *partitionSelector) draw() {
	row, col := s.list.GetSelection()
	top, left := s.list.GetOffset()
	s.list.Clear()
	for i, id := range s.ids {
		state := "○ OFF"
		color := tcell.ColorSilver
		if s.checked[id] {
			state = "✓ ON"
			color = tcell.ColorLightGreen
		}
		s.list.SetCell(i, 0, tview.NewTableCell(state).SetTextColor(color)).SetCell(i, 1, tview.NewTableCell(tview.Escape(safe(id.String(), false))))
	}
	s.list.Select(max(0, min(row, len(s.ids)-1)), col).SetOffset(top, left)
	if s.changed != nil {
		s.changed()
	}
}
func (u *ui) consume(topic string, id int32) {
	opts, previous := u.readers[topic]
	if !previous {
		opts = consumeOptions{recent: 100, limit: 1000, follow: true}
	}
	selector := newPartitionSelector(u)
	form := tview.NewForm().SetItemPadding(0)
	start := dropdown(form, buttonLabel("Start", "Alt+S"), starts, opts.start, nil)
	recent := field(form, buttonLabel("Recent per partition", "Alt+R"), strconv.FormatInt(opts.recent, 10))
	stamp := field(form, buttonLabel("Timestamp", "Alt+T"), opts.timestamp)
	offset := field(form, buttonLabel("Offset per partition", "Alt+E"), strconv.FormatInt(opts.offset, 10))
	key := field(form, buttonLabel("Key filter", "Alt+K"), opts.key)
	keyOp := dropdown(form, buttonLabel("Key operator", "Alt+O"), operators, opts.keyOp, nil)
	value := field(form, buttonLabel("Value filter", "Alt+V"), opts.value)
	valueOp := dropdown(form, buttonLabel("Value operator", "Alt+W"), operators, opts.valueOp, nil)
	limit := field(form, buttonLabel("Matching limit", "Alt+L"), strconv.FormatInt(opts.limit, 10))
	follow := check(form, buttonLabel("Keep following", "Alt+F"), opts.follow)
	// Only put the active start field in the focus order. tview redispatches
	// focus when a disabled field is selected, including via a field shortcut.
	renderForm := func(mode int) {
		form.Clear(false).AddFormItem(start)
		switch mode {
		case startRecent:
			form.AddFormItem(recent)
		case startTimestamp:
			form.AddFormItem(stamp)
		case startOffset:
			form.AddFormItem(offset)
		}
		form.AddFormItem(key).AddFormItem(keyOp).AddFormItem(value).AddFormItem(valueOp).AddFormItem(limit).AddFormItem(follow)
	}
	renderForm(opts.start)
	start.SetSelectedFunc(func(_ string, mode int) {
		renderForm(mode)
		focusForm(u, form, 0)
	})
	split := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(selector.list, 0, 1, true).AddItem(form, 0, 2, false)
	d := u.dialog("Consume · "+topic, split, selector.list)
	d.focusables = []tview.Primitive{selector.list, form}
	if u.width >= 100 {
		split.SetDirection(tview.FlexColumn)
	}
	d.say("Loading partitions…")
	loading := true
	selector.changed = func() {
		d.hint(fmt.Sprintf("%d / %d partitions selected\n", len(selector.selected()), len(selector.ids)) + keyHint("Alt+C") + " Start from any field")
	}
	goRead := func() {
		if loading {
			return
		}
		selected := selector.selected()
		if len(selected) == 0 {
			d.say("Select at least one partition")
			return
		}
		opts.partitions = nil
		for _, p := range selected {
			opts.partitions = append(opts.partitions, p.Partition)
		}
		opts.start, _ = start.GetCurrentOption()
		opts.keyOp, _ = keyOp.GetCurrentOption()
		opts.valueOp, _ = valueOp.GetCurrentOption()
		opts.key = key.GetText()
		opts.value = value.GetText()
		opts.timestamp = stamp.GetText()
		opts.follow = follow.IsChecked()
		switch opts.start {
		case startRecent:
			n := nonnegative(recent.GetText())
			if n == nil {
				d.say("Recent offsets must be a nonnegative integer")
				return
			}
			opts.recent = *n
		case startTimestamp:
			if _, err := timestamp(opts.timestamp); err != nil {
				d.say(err.Error())
				return
			}
		case startOffset:
			n := nonnegative(offset.GetText())
			if n == nil {
				d.say("Starting offset must be a nonnegative integer")
				return
			}
			opts.offset = *n
		}
		n := nonnegative(limit.GetText())
		if n == nil {
			d.say("Matching limit must be a nonnegative integer")
			return
		}
		opts.limit = *n
		if err := u.beginReader(topic, opts); err != nil {
			d.say(err.Error())
			return
		}
		u.readers[topic] = opts
		d.close()
		u.refreshView()
		u.focus(u.table)
	}
	d.button("All", "a", func() { selector.every(true) })
	d.button("None", "n", func() { selector.every(false) })
	ctx, cancel := context.WithTimeout(u.ctx, u.b.timeout)
	d.cancel = func() {
		cancel()
		d.close()
	}
	d.button("Cancel", "Esc", d.cancel)
	goBtn := d.button("Start", "Alt+C", goRead).SetDisabled(true)
	fieldKeys := map[rune]tview.FormItem{
		's': start, 'r': recent, 't': stamp, 'e': offset, 'k': key,
		'o': keyOp, 'v': value, 'w': valueOp, 'l': limit, 'f': follow,
	}
	d.keymap(func(e *tcell.EventKey) bool {
		if loading && e.Key() == tcell.KeyRune && e.Rune() == 'q' && e.Modifiers() == 0 && !isEditing(u.app.GetFocus()) {
			d.cancel()
			u.quit()
			return true
		}
		switch a := alt(e); a {
		case 'p':
			u.app.SetFocus(selector.list)
		case 'c':
			if !goBtn.IsDisabled() {
				goRead()
			}
		default:
			if item := fieldKeys[a]; item != nil {
				focusForm(u, form, form.GetFormItemIndex(item.GetLabel()))
				return true
			}
			if (e.Key() == tcell.KeyTab || e.Key() == tcell.KeyBacktab) && form.HasFocus() {
				item, _ := form.GetFocusedItemIndex()
				if e.Key() == tcell.KeyTab && item < form.GetFormItemCount()-1 || e.Key() == tcell.KeyBacktab && item > 0 {
					return passFormKey(form, e, u)
				}
			}
			return false
		}
		return true
	})
	populate := func(md kadm.TopicDetail) {
		if d.closed {
			return
		}
		loading = false
		if md.Err != nil {
			d.say(md.Err.Error())
			return
		}
		for _, p := range md.Partitions.Numbers() {
			k := partKey{topic, p}
			selector.ids = append(selector.ids, k)
			selector.checked[k] = id == p || id < 0 && (!previous || slices.Contains(opts.partitions, p))
		}
		selector.draw()
		goBtn.SetDisabled(len(selector.ids) == 0)
	}
	if md, ok := u.metadata[topic]; ok && md.Err == nil {
		cancel()
		populate(md)
		return
	}
	go func() {
		defer cancel()
		metadata, err := u.b.metadata(ctx, topic)
		u.post(func() {
			if err != nil {
				d.say(err.Error())
				return
			}
			if md, ok := metadata[topic]; ok {
				populate(md)
			} else {
				d.say("Topic is unavailable")
			}
		})
	}()
}
func passFormKey(form *tview.Form, e *tcell.EventKey, u *ui) bool {
	item, _ := form.GetFocusedItemIndex()
	if e.Key() == tcell.KeyTab {
		item++
	} else {
		item--
	}
	focusForm(u, form, item)
	return true
}
func (u *ui) beginReader(topic string, opts consumeOptions) error {
	if u.store == nil {
		dir, err := os.MkdirTemp("", "katui-payloads-*")
		if err != nil {
			return err
		}
		limit := int64(payloadQuota)
		if u.b.payloadStorageSet {
			limit = u.b.payloadStorage
		}
		u.store = newPayloadStore(dir, limit)
	}
	u.stopReader()
	u.resetMessages()
	u.readTopic = topic
	u.readOptions = opts
	if u.workspace != 2 {
		u.workspaceBeforeMessages = u.workspace
		u.changeWorkspace(2)
	}
	ctx, cancel := context.WithCancel(u.ctx)
	session := &readerSession{cancel: cancel, wake: make(chan struct{}, 1)}
	u.reader = session
	u.readerState = readerUpdate{state: "CONNECTING"}
	go u.b.readRecords(ctx, session, topic, opts, u.store, func(update readerUpdate) {
		posted := u.post(func() {
			if u.reader != session {
				for _, r := range update.records {
					r.discardPayload()
				}
				return
			}
			if session.paused.Load() && update.state == "READING" {
				update.state = "PAUSED"
			}
			u.readerState = update
			u.readerState.records = nil
			for _, r := range update.records {
				u.buffer(r)
			}
			if u.workspace == 2 && len(update.records) > 0 {
				u.readerViewDirty.Store(true)
			}
		})
		if !posted {
			for _, r := range update.records {
				r.releasePayload()
			}
		}
	})
	return nil
}
func (u *ui) stopReader() {
	if u.reader != nil {
		u.reader.cancel()
		u.reader = nil
		u.readerState.state = "STOPPED"
		u.readerState.info = ""
	}
}
func (u *ui) pauseReader() {
	if u.reader == nil || u.readerState.state == "COMPLETE" || u.readerState.state == "ERROR" {
		return
	}
	r := u.reader
	paused := !r.paused.Load()
	r.paused.Store(paused)
	if paused {
		u.readerState.state = "PAUSED"
	} else {
		u.readerState.state = "READING"
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	u.updateStatus()
}

type readPosition struct {
	next, end int64
}

type readerFetch struct {
	leader     int32
	partitions []int32
}

func readerFetches(leaders map[int32][]int32, follow bool) []readerFetch {
	fetches := make([]readerFetch, 0, len(leaders))
	for leader, ids := range leaders {
		if follow {
			fetches = append(fetches, readerFetch{leader, ids})
			continue
		}
		// The first nonempty partition can use the whole response budget.
		// For a finite scan, fetch each partition separately so an empty
		// response can distinguish a compacted tail from an exhausted budget.
		for _, id := range ids {
			fetches = append(fetches, readerFetch{leader, []int32{id}})
		}
	}
	return fetches
}

func (b *broker) readerPositions(ctx context.Context, topic string, o consumeOptions) (map[int32]readPosition, error) {
	high, err := b.admin.ListEndOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	low, err := b.admin.ListStartOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	var at kadm.ListedOffsets
	if o.start == startTimestamp {
		t, err := timestamp(o.timestamp)
		if err != nil {
			return nil, err
		}
		at, err = b.admin.ListOffsetsAfterMilli(ctx, t.UnixMilli(), topic)
		if err != nil {
			return nil, err
		}
	}
	positions := map[int32]readPosition{}
	for _, id := range o.partitions {
		k := partKey{topic, id}
		lo, err := listed(low, k)
		if err != nil {
			return nil, err
		}
		hi, err := listed(high, k)
		if err != nil {
			return nil, err
		}
		if *hi < *lo {
			return nil, fmt.Errorf("%s: captured end precedes retained beginning", k)
		}
		next := *hi
		switch o.start {
		case startRecent:
			next = max(*lo, *hi-o.recent)
		case startBeginning:
			next = *lo
		case startTimestamp:
			n, err := listed(at, k)
			if err != nil {
				return nil, err
			}
			next = max(*lo, min(*hi, *n))
		case startOffset:
			if o.offset < *lo || o.offset > *hi {
				return nil, fmt.Errorf("%s: %w (requested %d, available %d–%d)", k, errReaderOffset, o.offset, *lo, *hi)
			}
			next = o.offset
		}
		positions[id] = readPosition{next, *hi}
	}
	return positions, nil
}

func (b *broker) readRecords(ctx context.Context, s *readerSession, topic string, o consumeOptions, store *payloadStore, send func(readerUpdate)) {
	state := readerUpdate{state: "CONNECTING"}
	defer func() {
		for _, r := range state.records {
			r.releasePayload()
		}
	}()
	emit := func(name, info string) { state.state = name; state.info = info; send(state); state.records = nil }
	// The reader owns its transport, so recovery can discard stale connections
	// without resetting its cursors or disturbing monitoring.
	opts, err := b.kafkaOptions(b.config)
	if err != nil {
		emit("ERROR", err.Error())
		return
	}
	connection := &broker{options: b.options}
	defer func() {
		if connection.client != nil {
			connection.client.Close()
		}
	}()

	var positions map[int32]readPosition
	var md kadm.TopicDetail
	var identity kadm.TopicID
	haveIdentity := false
	needMetadata := true
	retry := time.Second
	var nextMetadata time.Time
	var reportedScanned int64
	decompressor := kgo.DefaultDecompressor()
	key, value := strings.ToLower(o.key), strings.ToLower(o.value)
	for ctx.Err() == nil {
		if s.paused.Load() {
			emit("PAUSED", "")
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		req, cancel := context.WithTimeout(ctx, b.timeout)
		var err error
		if connection.client == nil {
			connection.client, err = kgo.NewClient(opts...)
			if err != nil {
				cancel()
				emit("ERROR", err.Error())
				return
			}
			connection.admin = kadm.NewClient(connection.client)
		}
		if needMetadata || time.Now().After(nextMetadata) {
			topics, e := connection.metadata(req, topic)
			if e != nil {
				err = e
			} else if t, ok := topics[topic]; !ok {
				err = kerr.UnknownTopicOrPartition
			} else if t.Err != nil {
				err = t.Err
			} else if haveIdentity && t.ID != identity {
				cancel()
				emit("ERROR", "The topic identity changed; start a new reader.")
				return
			} else {
				md = t
				identity = t.ID
				haveIdentity = true
				needMetadata = false
				nextMetadata = time.Now().Add(30 * time.Second)
			}
		}
		if err == nil && positions == nil {
			positions, err = connection.readerPositions(req, topic, o)
		}
		if err == nil {
			active := 0
			leaders := map[int32][]int32{}
			for _, id := range o.partitions {
				p := positions[id]
				if o.follow || p.next < p.end {
					active++
					m, ok := md.Partitions[id]
					if !ok {
						err = kerr.UnknownTopicOrPartition
						break
					}
					if m.Err != nil {
						err = m.Err
						break
					}
					if m.Leader < 0 {
						err = kerr.LeaderNotAvailable
						break
					}
					leaders[m.Leader] = append(leaders[m.Leader], id)
				}
			}
			if active == 0 {
				cancel()
				emit("COMPLETE", "Captured end reached")
				return
			}
			// Parsing and cursor movement use the broker's batches, including
			// control batches and compacted offset gaps.
			for _, nextFetch := range readerFetches(leaders, o.follow) {
				ids := nextFetch.partitions
				if err != nil {
					break
				}
				request := kmsg.NewPtrFetchRequest()
				request.MaxWaitMillis = 250
				request.MinBytes = 1
				request.MaxBytes = 8 << 20
				request.SessionID = 0
				request.SessionEpoch = -1
				t := kmsg.NewFetchRequestTopic()
				t.Topic = topic
				t.TopicID = [16]byte(md.ID)
				for _, id := range ids {
					item := kmsg.NewFetchRequestTopicPartition()
					item.Partition = id
					item.FetchOffset = positions[id].next
					item.PartitionMaxBytes = 2 << 20
					item.CurrentLeaderEpoch = md.Partitions[id].LeaderEpoch
					t.Partitions = append(t.Partitions, item)
				}
				request.Topics = []kmsg.FetchRequestTopic{t}
				response, e := connection.client.Broker(int(nextFetch.leader)).Request(req, request)
				if e != nil {
					err = e
					break
				}
				fetch := response.(*kmsg.FetchResponse)
				if err = kerr.ErrorForCode(fetch.ErrorCode); err != nil {
					break
				}
				seen := map[int32]bool{}
				for _, ft := range fetch.Topics {
					if fetch.Version >= 13 && ft.TopicID != t.TopicID || fetch.Version < 13 && ft.Topic != topic {
						continue
					}
					for i := range ft.Partitions {
						p := &ft.Partitions[i]
						if !slices.Contains(ids, p.Partition) || seen[p.Partition] {
							continue
						}
						seen[p.Partition] = true
						position := positions[p.Partition]
						result, next := kgo.ProcessFetchPartition(kgo.ProcessFetchPartitionOpts{Topic: topic, Partition: p.Partition, Offset: position.next}, p, decompressor, nil)
						if result.Err != nil {
							err = result.Err
							continue
						}
						for _, r := range result.Records {
							if !o.follow && r.Offset >= position.end {
								break
							}
							if r.Offset < position.next {
								continue
							}
							position.next = r.Offset + 1
							positions[p.Partition] = position
							state.scanned++
							if !predicate(r.Key, key, o.keyOp) || !predicate(r.Value, value, o.valueOp) {
								continue
							}
							stored, e := saveRecord(r, store)
							if e != nil {
								cancel()
								emit("ERROR", "Retaining payload: "+e.Error())
								return
							}
							state.matched++
							state.records = append(state.records, stored)
							if len(state.records) >= 64 {
								emit("READING", "")
							}
							if o.limit > 0 && state.matched >= o.limit {
								cancel()
								emit("COMPLETE", "Matching record limit reached")
								return
							}
						}
						position.next = max(position.next, next)
						// A successful single-partition fetch can establish an empty tail.
						if !o.follow && len(ids) == 1 && len(p.RecordBatches) == 0 && p.HighWatermark >= position.end && p.LogStartOffset <= position.next {
							position.next = max(position.next, position.end)
						}
						positions[p.Partition] = position
					}
				}
				if len(seen) != len(ids) && err == nil {
					err = io.ErrUnexpectedEOF
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if state.scanned != reportedScanned || len(state.records) > 0 || state.state != "READING" {
				emit("READING", "")
				reportedScanned = state.scanned
			}
			retry = time.Second
			continue
		}
		var kafkaErr *kerr.Error
		var certificateError *tls.CertificateVerificationError
		if errors.Is(err, errReaderOffset) || errors.As(err, &certificateError) || (errors.As(err, &kafkaErr) && !kafkaErr.Retriable) {
			emit("ERROR", err.Error())
			return
		}
		connection.client.Close()
		connection.client = nil
		needMetadata = true
		emit("RECONNECTING", fmt.Sprintf("retry in %s · %v", retry, err))
		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return
		}
		retry = min(30*time.Second, retry*2)
	}
}
