package katui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
	"github.com/twmb/franz-go/pkg/kadm"
)

type location struct {
	kind, topic, group, filter, selected string
	partition                            int32
	sort, row, top, horizontal           int
}
type tableRow struct {
	pinnedOnly   bool
	id           string
	cells        []string
	topic, group string
	partition    int32
	record       *storedRecord
	rankInteger  int64
	rank         float64
	order        int
}

type ui struct {
	ctx                            context.Context
	b                              *broker
	nextBroker                     *broker
	app                            *tview.Application
	screen                         tcell.Screen
	pages                          *tview.Pages
	shell, body                    *tview.Flex
	header, tabs, controls, status *tview.TextView
	search                         *tview.InputField
	table                          *tview.Table
	detail                         *tview.TextView
	rows                           []tableRow
	layout                         string
	workspace                      int
	workspaceBeforeMessages        int
	showInternal, updatingTable    bool
	stacks                         [3][]*location
	pickerHistory                  map[string]*location
	modalID, modalDepth            int
	fullscreen                     string
	width, height                  int
	accent                         tcell.Color
	message                        string
	interrupted                    bool
	shuttingDown                   chan struct{}
	screenReady                    chan struct{}
	updateEvent                    *tcell.EventKey
	updates                        chan func()
	monitoring                     atomic.Bool
	monitorPaused                  bool
	metadata                       kadm.TopicDetails
	topics                         map[string]topicSample
	sizes                          map[partKey]int64
	groupNames                     []string
	groups                         map[string]groupSample
	groupErrors                    map[string]string
	cached                         map[string]bool
	pendingGroups, cacheBusy       bool
	relationshipAt                 time.Time
	relationshipError              string
	metrics                        map[observationKey]*measurement
	deleted                        map[string]time.Time
	epoch                          uint64
	monitorCtx                     context.Context
	monitorCancel                  context.CancelFunc
	busy                           map[string]bool
	next                           map[string]time.Time
	failures                       map[string]int
	errors                         map[string]string
	prefs                          preferences
	sessionProfiles                map[string]profile
	readers                        map[string]consumeOptions
	reader                         *readerSession
	readerState                    readerUpdate
	readOptions                    consumeOptions
	readTopic                      string
	records                        []*storedRecord
	recordBytes                    int
	evictions                      int
	checked                        []*storedRecord
	modalRecords                   []*storedRecord
	payloadJobs                    map[*storedRecord]int
	checkedBytes                   int
	pinned                         *storedRecord
	followLatest                   bool
	inspector                      *payloadPane
	store                          *payloadStore
}

func newUI(ctx context.Context, b *broker) *ui {
	u := &ui{ctx: ctx, b: b, app: tview.NewApplication(), pages: tview.NewPages(), accent: tcell.ColorLightSkyBlue,
		shuttingDown: make(chan struct{}), screenReady: make(chan struct{}), updates: make(chan func(), 16),
		updateEvent:   tcell.NewEventKey(tcell.KeyF64, 0, tcell.ModNone),
		pickerHistory: map[string]*location{}, topics: map[string]topicSample{}, groups: map[string]groupSample{},
		sizes: map[partKey]int64{}, metrics: map[observationKey]*measurement{}, groupErrors: map[string]string{}, cached: map[string]bool{},
		deleted: map[string]time.Time{}, errors: map[string]string{}, sessionProfiles: map[string]profile{},
		readers: map[string]consumeOptions{}, followLatest: true,
	}
	u.stacks = [3][]*location{{{kind: "topics", partition: -1}}, {{kind: "groups", partition: -1}}, {{kind: "messages", partition: -1}}}
	if b.view == "groups" {
		u.workspace = 1
	}
	if b.group != "" {
		u.workspace = 1
		group := &location{kind: "group", group: b.group, topic: b.topic, partition: -1}
		u.stacks[1] = append(u.stacks[1], group)
		if b.topic == "" {
			group.partition = int32(b.partition)
		} else if b.partition >= 0 {
			u.stacks[1] = append(u.stacks[1], &location{kind: "group-detail", group: b.group, topic: b.topic, partition: int32(b.partition)})
		}
	} else if b.topic != "" {
		u.workspace = 0
		u.stacks[0] = append(u.stacks[0], &location{kind: "topic", topic: b.topic, partition: -1})
		if b.partition >= 0 {
			u.stacks[0] = append(u.stacks[0], &location{kind: "topic-detail", topic: b.topic, partition: int32(b.partition)})
		}
	}
	u.invalidateMonitor()
	if len(b.brokers) != 0 {
		u.loadCache()
		u.prefs = loadPreferences(expandHome(b.inspector), b.cluster)
		u.message = errorText(u.prefs.err)
	}
	return u
}

func (u *ui) run() error {
	ctx, cancel := context.WithCancel(u.ctx)
	u.ctx = ctx
	defer func() {
		close(u.shuttingDown)
		cancel()
		u.stopReader()
		if u.store != nil {
			u.store.close()
		}
	}()
	u.header = tview.NewTextView().SetTextColor(u.accent)
	u.tabs = tview.NewTextView().SetDynamicColors(true)
	u.controls = tview.NewTextView().SetDynamicColors(true).SetWordWrap(true)
	u.status = tview.NewTextView().SetDynamicColors(true).SetTextColor(tcell.ColorSilver).SetWrap(false)
	u.search = tview.NewInputField().SetLabel(buttonLabel("Search", "/") + "  ")
	u.search.SetChangedFunc(func(value string) { u.current().filter = value; u.refreshView() })
	u.search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter || key == tcell.KeyEscape {
			u.focus(u.mainPane())
		}
	})
	u.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	u.table.SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(u.accent).Bold(true))
	u.table.SetSelectionChangedFunc(func(row, _ int) { u.selectRow(row) })
	u.table.SetSelectedFunc(func(row, _ int) {
		if row > 0 {
			u.openRow()
		}
	})
	u.table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(event, u.table, nil) })
	u.detail = tview.NewTextView().SetScrollable(true).SetWrap(false)
	u.detail.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(event, nil, u.detail) })
	u.body = tview.NewFlex()
	u.shell = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.header, 1, 0, false).AddItem(u.tabs, 1, 0, false).
		AddItem(u.controls, 1, 0, false).AddItem(u.search, 1, 0, false).
		AddItem(u.body, 0, 1, true).AddItem(u.status, 2, 0, false)
	u.pages.AddPage("main", u.shell, true, true)
	u.app.SetRoot(u.pages, true).EnableMouse(true).EnablePaste(true).SetInputCapture(u.keys)
	u.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		u.monitoring.Store(len(u.b.brokers) != 0 && !u.monitorPaused && u.workspace != 2 && u.modalDepth == 0)
		if u.screen == nil {
			u.screen = screen
			close(u.screenReady)
			go u.heartbeat(screen)
		}
		w, h := screen.Size()
		if w != u.width || h != u.height {
			u.width = w
			u.height = h
			u.layout = ""
			u.refreshView()
		}
		return false
	})
	u.showLocation()
	if len(u.b.brokers) == 0 {
		u.clustersDialog()
	}
	err := u.app.Run()
	if u.interrupted {
		return errInterrupted
	}
	return err
}

func (u *ui) heartbeat(screen tcell.Screen) {
	ticker := time.NewTicker(max(25*time.Millisecond, min(250*time.Millisecond, u.b.interval/4)))
	defer ticker.Stop()
	canceled := u.ctx.Done()
	for {
		select {
		case <-u.shuttingDown:
			return
		case <-canceled:
			canceled = nil
		case <-ticker.C:
		}
		if u.ctx.Err() != nil || u.monitoring.Load() || len(u.updates) > 0 {
			_ = screen.PostEvent(u.updateEvent)
		}
	}
}

// post runs fn on the event loop and waits for completion or UI shutdown.
// The heartbeat retries the wakeup if the terminal event queue is full or
// tview is processing a bracketed paste.
func (u *ui) post(fn func()) bool {
	select {
	case <-u.shuttingDown:
		return false
	case <-u.screenReady:
	}
	applied := make(chan struct{})
	select {
	case <-u.shuttingDown:
		return false
	case u.updates <- func() {
		fn()
		close(applied)
	}:
	}
	_ = u.screen.PostEvent(u.updateEvent)
	select {
	case <-applied:
		return true
	case <-u.shuttingDown:
		// An update may have completed just before the event loop stopped.
		select {
		case <-applied:
			return true
		default:
			return false
		}
	}
}

func (u *ui) applyUpdates() {
	if u.ctx.Err() != nil {
		u.app.Stop()
		return
	}
	// Limit each batch so user input gets a turn even if workers are busy.
	for range len(u.updates) {
		update := <-u.updates
		update()
	}
	u.poll()
	if u.modalDepth == 0 {
		u.updateStatus()
	}
}
func (u *ui) say(message string) {
	u.message = safe(message, false)
	if u.status != nil {
		u.updateStatus()
	}
}

func (u *ui) current() *location {
	stack := u.stacks[u.workspace]
	return stack[len(stack)-1]
}
func (u *ui) mainPane() tview.Primitive {
	switch u.current().kind {
	case "topic-detail", "group-detail":
		return u.detail
	}
	return u.table
}
func (u *ui) focus(p tview.Primitive) {
	if p == nil {
		return
	}
	if u.table != nil {
		u.table.SetBorderColor(tcell.ColorDarkSlateGray)
	}
	if u.detail != nil {
		u.detail.SetBorderColor(tcell.ColorDarkSlateGray)
	}
	if u.inspector != nil {
		u.inspector.root.Box.SetBorderColor(tcell.ColorDarkSlateGray)
	}
	switch p {
	case u.table:
		u.table.SetBorderColor(u.accent)
	case u.detail:
		u.detail.SetBorderColor(u.accent)
	}
	u.app.SetFocus(p)
}
func (u *ui) savePosition() {
	if u.table == nil {
		return
	}
	if u.current().kind != "topic-detail" && u.current().kind != "group-detail" {
		u.current().top, _ = u.table.GetOffset()
	}
}
func (u *ui) changeWorkspace(w int) {
	if w == 0 && u.b.backend == "cli" {
		u.say("Topics require API backend")
		return
	}
	if w == u.workspace {
		return
	}
	u.savePosition()
	if w == 2 {
		u.workspaceBeforeMessages = u.workspace
	}
	u.workspace = w
	u.showLocation()
}
func (u *ui) showLocation() {
	u.fullscreen = ""
	u.invalidateMonitor()
	loc := u.current()
	filter := loc.filter
	u.search.SetText(filter)
	u.refreshView()
	u.table.SetOffset(loc.top, 0)
	u.detail.ScrollToBeginning()
	u.focus(u.mainPane())
	u.poll()
}
func (u *ui) push(loc *location) {
	u.savePosition()
	u.stacks[u.workspace] = append(u.stacks[u.workspace], loc)
	u.showLocation()
}
func (u *ui) back() {
	if u.fullscreen != "" {
		u.fullscreen = ""
		u.refreshView()
		u.focus(u.mainPane())
		return
	}
	if u.workspace == 2 {
		if u.workspaceBeforeMessages == 2 {
			u.changeWorkspace(0)
		} else {
			u.changeWorkspace(u.workspaceBeforeMessages)
		}
		return
	}
	stack := u.stacks[u.workspace]
	if len(stack) > 1 {
		u.stacks[u.workspace] = stack[:len(stack)-1]
		u.showLocation()
	}
}

func (u *ui) viewerKeys(event *tcell.EventKey, table *tview.Table, view *tview.TextView) *tcell.EventKey {
	if event.Key() == tcell.KeyRune {
		switch event.Rune() {
		case 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, 0)
		case 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, 0)
		}
	}
	if event.Key() == tcell.KeyLeft || event.Key() == tcell.KeyRight {
		delta := 8
		if event.Modifiers()&tcell.ModCtrl != 0 {
			delta = max(8, u.width/2)
		}
		if event.Key() == tcell.KeyLeft {
			delta = -delta
		}
		if view != nil {
			row, col := view.GetScrollOffset()
			view.ScrollTo(row, max(0, col+delta))
			return nil
		}
		if table == u.table {
			loc := u.current()
			_, widths, _ := u.browserRows(loc)
			total := max(0, len(widths)-1)
			for _, width := range widths {
				total += width
			}
			_, _, visible, _ := table.GetInnerRect()
			loc.horizontal = max(0, min(max(0, total-visible), loc.horizontal+delta))
			u.refreshView()
			return nil
		}
		if table != nil {
			row, col := table.GetOffset()
			col = max(0, min(table.GetColumnCount()-1, col+delta/2))
			table.SetOffset(row, col)
			return nil
		}
	}
	return event
}

func (u *ui) keys(ev *tcell.EventKey) *tcell.EventKey {
	// Identify our wakeup by pointer so a user's F64 remains an ordinary key.
	if ev == u.updateEvent {
		u.applyUpdates()
		return nil
	}
	if ev.Key() == tcell.KeyCtrlC {
		if _, ok := u.app.GetFocus().(*payloadText); ok {
			return tcell.NewEventKey(ev.Key(), ev.Rune(), ev.Modifiers())
		}
	}
	if u.modalDepth == 0 && u.connectionPending() {
		switch {
		case ev.Key() == tcell.KeyEscape:
			u.cancelConnection()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'q' && ev.Modifiers() == 0:
			u.quit()
			return nil
		case ev.Key() == tcell.KeyCtrlC:
			u.interrupted = true
			u.app.Stop()
			return nil
		}
	}
	if ev.Key() == tcell.KeyCtrlC {
		switch {
		case u.modalDepth > 0 || u.search.HasFocus():
			// tview stops the app when the capture returns the original Ctrl+C.
			return tcell.NewEventKey(ev.Key(), ev.Rune(), ev.Modifiers())
		case u.workspace == 2 && u.inspector != nil && u.inspector.root.HasFocus():
			u.say("Focus payload text to copy a selection")
		default:
			u.interrupted = true
			u.app.Stop()
		}
		return nil
	}
	if u.modalDepth > 0 {
		return ev
	}
	if u.inspector != nil && u.workspace == 2 && u.inspector.mode.IsOpen() {
		return ev
	}
	if ev.Key() == tcell.KeyCtrlP {
		u.palette()
		return nil
	}
	if u.search.HasFocus() {
		if ev.Key() == tcell.KeyTab || ev.Key() == tcell.KeyBacktab {
			u.focus(u.mainPane())
			return nil
		}
		return ev
	}
	if ev.Key() == tcell.KeyEscape {
		u.back()
		return nil
	}
	if ev.Key() == tcell.KeyTab || ev.Key() == tcell.KeyBacktab {
		u.cycleFocus(ev.Key() == tcell.KeyBacktab)
		return nil
	}
	if ev.Key() == tcell.KeyF4 && u.workspace == 2 {
		u.fullRecord()
		return nil
	}
	if ev.Key() != tcell.KeyRune || ev.Modifiers()&tcell.ModAlt != 0 {
		return ev
	}
	row := u.selected()
	loc := u.current()
	topic := loc.topic
	if row.topic != "" {
		topic = row.topic
	}
	group := loc.group
	if row.group != "" {
		group = row.group
	}
	switch ev.Rune() {
	case '1', '2', '3':
		u.changeWorkspace(int(ev.Rune() - '1'))
	case '/':
		u.fullscreen = ""
		u.refreshView()
		u.focus(u.search)
	case '4':
		u.clustersDialog()
	case 'q':
		u.quit()
	case '?':
		u.help()
	case 'z':
		if u.fullscreen != "" {
			u.fullscreen = ""
			u.refreshView()
			u.focus(u.mainPane())
		} else {
			u.fullscreen = "table"
			if u.workspace == 2 && u.inspector != nil && u.inspector.root.HasFocus() {
				u.fullscreen = "inspector"
			}
			u.refreshView()
		}
	case 'b':
		if u.fullscreen == "inspector" {
			u.fullscreen = ""
			u.refreshView()
		}
		u.focus(u.mainPane())
	case 's':
		if u.workspace != 2 {
			loc.sort++
			u.refreshView()
		}
	case 'r':
		u.requestRefresh()
	case 'c':
		if u.workspace == 2 && u.readTopic != "" {
			topic = u.readTopic
		}
		if topic != "" && u.b.backend == "api" {
			id := loc.partition
			if row.partition >= 0 && (loc.kind == "topic" || loc.kind == "group") {
				id = row.partition
			}
			u.consume(topic, id)
		}
	default:
		if u.workspace == 2 {
			return u.messageKeys(ev)
		}
		switch ev.Rune() {
		case 'h':
			if loc.kind == "topics" {
				u.showInternal = !u.showInternal
				u.refreshView()
			}
		case 'g':
			if topic != "" && group == "" {
				u.openPicker(topic)
			}
		case 'n':
			if loc.kind == "topics" {
				u.topicForm("", "Create topic")
			}
		case 'd':
			if topic != "" && group == "" {
				u.topicForm(topic, "Delete topic")
			}
		case 'a':
			if u.b.backend != "api" {
				u.say("Administration requires API backend")
			} else if group != "" {
				part := row.partition
				if loc.partition >= 0 {
					part = loc.partition
				}
				u.groupForm(group, loc.topic, topic, part)
			} else if topic != "" {
				u.topicForm(topic, "Configure topic")
			}
		default:
			return ev
		}
	}
	return nil
}

func (u *ui) cycleFocus(reverse bool) {
	if u.fullscreen != "" {
		if u.fullscreen == "inspector" && u.inspector != nil {
			u.inspector.cycle(reverse)
		}
		return
	}
	if u.workspace == 2 && u.inspector != nil {
		if u.inspector.root.HasFocus() {
			if u.inspector.cycle(reverse) {
				return
			}
			if reverse {
				u.focus(u.table)
			} else {
				u.focus(u.search)
			}
		} else if reverse {
			u.inspector.focusLast()
		} else {
			u.inspector.focusFirst()
		}
	} else {
		u.focus(u.search)
	}
}

func (u *ui) requestRefresh() {
	u.monitorPaused = false
	u.invalidateMonitor()
	u.poll()
	if u.workspace != 2 {
		u.say("Refresh requested")
	}
}
func (u *ui) openPicker(topic string) {
	loc := u.pickerHistory[topic]
	if loc == nil {
		loc = &location{kind: "picker", topic: topic, partition: -1}
		u.pickerHistory[topic] = loc
	}
	u.push(loc)
}

func (u *ui) selected() tableRow {
	if u.current().kind == "topic-detail" || u.current().kind == "group-detail" {
		return tableRow{partition: -1}
	}
	i := u.current().row
	if i >= 0 && i < len(u.rows) {
		return u.rows[i]
	}
	return tableRow{partition: -1}
}
func (u *ui) selectRow(row int) {
	if u.updatingTable || row <= 0 || row > len(u.rows) {
		return
	}
	loc := u.current()
	if u.workspace == 2 && loc.selected != u.rows[row-1].id {
		u.followLatest = false
	}
	loc.selected = u.rows[row-1].id
	loc.row = row - 1
	if u.workspace == 2 {
		if rec := u.rows[row-1].record; rec != nil {
			u.pin(rec)
		}
	}
}

func (u *ui) openRow() {
	row := u.selected()
	if row.id == "" {
		return
	}
	switch u.current().kind {
	case "topics":
		u.push(&location{kind: "topic", topic: row.topic, partition: -1})
	case "topic":
		u.push(&location{kind: "topic-detail", topic: row.topic, partition: row.partition})
	case "groups", "picker":
		u.push(&location{kind: "group", group: row.group, topic: u.current().topic, partition: -1})
	case "group":
		u.push(&location{kind: "group-detail", group: row.group, topic: row.topic, partition: row.partition})
	case "messages":
		if len(u.checked) > 1 {
			u.comparison()
		} else if u.inspector != nil {
			u.inspector.focusFirst()
		}
	}
}

func (u *ui) refreshView() {
	if u.shell == nil {
		return
	}
	loc := u.current()
	header := "Katui"
	if u.b.cluster != "" {
		if u.b.profileName != "" {
			header += "  ·  " + safe(u.b.profileName, false)
		}
		header += fmt.Sprintf("  ·  %s  ·  poll %s", safe(u.b.cluster, false), u.b.interval)
	}
	u.header.SetText(header)
	k := keyHint
	labels := []string{"Topics", "Consumer Groups", "Messages"}
	for i, label := range labels {
		if i == u.workspace {
			label = "[black:" + u.accent.String() + ":b] " + label + " [-:-:-]"
		}
		labels[i] = k(strconv.Itoa(i+1)) + " " + label
	}
	u.tabs.SetText(strings.Join(labels, "  ") + "   " + k("4") + " Clusters")
	open, back, manage := k("Enter")+" Open", k("Esc")+" Back", k("a")+" Manage"
	partitions, consume, consumers := k("Enter")+" Partition", k("c")+" Consume", k("g")+" Consumers"
	sortRefresh := k("s") + " Sort  " + k("r") + " Refresh"
	actions := map[string]string{
		"topics":       strings.Join([]string{open, consumers, consume, manage, k("n") + " Create", k("d") + " Delete", k("h") + " Internal"}, "  "),
		"topic":        strings.Join([]string{back, partitions, consumers, consume, manage, k("d") + " Delete"}, "  "),
		"topic-detail": strings.Join([]string{back, consumers, consume, manage, k("d") + " Delete"}, "  "),
		"groups":       open + " group  " + manage + "  " + sortRefresh,
		"picker":       back + "  " + open + " group  " + k("/") + " Search  " + sortRefresh,
		"group":        strings.Join([]string{back, partitions, manage, consume, k("s") + " Sort"}, "  "),
		"group-detail": strings.Join([]string{back, manage, consume, k("r") + " Refresh"}, "  "),
		"messages":     strings.Join([]string{back, consume, k("p") + " Pause", k("x") + " Stop", k("l") + " Clear", k("f") + " Follow", k("w") + " Compare", k("e") + " Uncheck"}, "  "),
	}
	controls := actions[loc.kind]
	u.controls.SetText(controls)
	controlRows := 1
	if u.width > 0 && tview.TaggedStringWidth(controls) > u.width {
		controlRows = 2
	}
	layout := fmt.Sprintf("%s/%s/%t/%d", loc.kind, u.fullscreen, u.width < 100, controlRows)
	if layout != u.layout {
		u.layout = layout
		u.body.Clear()
		statusRows := 2
		if loc.kind == "messages" {
			statusRows = 4
		}
		u.shell.Clear().AddItem(u.header, 1, 0, false)
		if u.fullscreen == "" {
			u.shell.AddItem(u.tabs, 1, 0, false).AddItem(u.controls, controlRows, 0, false).AddItem(u.search, 1, 0, false)
		}
		u.shell.AddItem(u.body, 0, 1, true).AddItem(u.status, statusRows, 0, false)
		if loc.kind == "messages" {
			if u.inspector == nil {
				u.inspector = u.newPayloadPane(false, false, nil)
			}
			if u.width < 100 {
				u.body.SetDirection(tview.FlexRow)
			} else {
				u.body.SetDirection(tview.FlexColumn)
			}
			if u.fullscreen != "inspector" {
				u.body.AddItem(u.table, 0, 1, true)
			}
			if u.fullscreen != "table" {
				u.body.AddItem(u.inspector.root, 0, 1, false)
			}
		} else {
			u.body.SetDirection(tview.FlexColumn).AddItem(u.mainPane(), 0, 1, true)
		}
	}
	title := loc.kind
	if loc.topic != "" {
		title += " · " + safe(loc.topic, false)
	}
	if loc.group != "" {
		title += " · " + safe(loc.group, false)
	}
	if loc.kind == "messages" {
		title = "Records · " + safe(u.readTopic, false)
	}
	u.table.SetBorder(true).SetTitle(" " + tview.Escape(title) + " ")
	u.detail.SetBorder(true).SetTitle(" " + tview.Escape(title) + " ")
	if loc.kind == "topic-detail" || loc.kind == "group-detail" {
		text := u.partitionDetail(loc)
		if text != u.detail.GetText(false) {
			u.detail.SetText(text)
		}
	} else {
		header, width, rows := u.browserRows(loc)
		u.renderTable(header, width, rows)
	}
	u.updateStatus()
}

func (u *ui) renderTable(header []string, width []int, rows []tableRow) {
	loc := u.current()
	row := loc.row
	for i, r := range rows {
		if r.id == loc.selected {
			row = i
			break
		}
	}
	if loc.kind == "messages" && u.followLatest {
		row = len(rows) - 1
	}
	row = max(0, min(len(rows)-1, row))
	u.updatingTable = true
	top, _ := u.table.GetOffset()
	u.table.Clear()
	u.table.SetCell(0, 0, browserCell(header, width, loc.horizontal).SetTextColor(u.accent).SetSelectable(false))
	for i, r := range rows {
		u.table.SetCell(i+1, 0, browserCell(r.cells, width, loc.horizontal))
	}
	u.rows = rows
	if len(rows) > 0 {
		loc.selected = rows[row].id
		loc.row = row
		u.table.Select(row+1, 0).SetOffset(top, 0)
		if loc.kind == "messages" && rows[row].record != nil {
			u.pin(rows[row].record)
		}
	} else {
		loc.selected = ""
		loc.row = 0
		u.table.Select(0, 0).SetOffset(0, 0)
	}
	u.updatingTable = false
}

// Browser rows use a fixed display width for each column. A single table
// cell per row lets the viewport scroll in terminal cells within a column.
func browserCell(values []string, widths []int, skip int) *tview.TableCell {
	var out strings.Builder
	for i, value := range values {
		if i > 0 {
			out.WriteByte(' ')
		}
		value = safe(value, false)
		w := widths[i]
		if size := uniseg.StringWidth(value); size <= w {
			out.WriteString(value)
			out.WriteString(strings.Repeat(" ", w-size))
		} else {
			part, _ := splitDisplay(value, w-1)
			out.WriteString(part)
			out.WriteString(strings.Repeat(" ", max(0, w-1-uniseg.StringWidth(part))))
			out.WriteRune('…')
		}
	}
	_, visible := splitDisplay(out.String(), skip)
	return tview.NewTableCell(tview.Escape(visible)).SetExpansion(1)
}
func splitDisplay(s string, cells int) (string, string) {
	g := uniseg.NewGraphemes(s)
	width := 0
	for g.Next() {
		start, end := g.Positions()
		w := g.Width()
		if width+w > cells {
			if width < cells {
				return s[:start], strings.Repeat(" ", width+w-cells) + s[end:]
			}
			return s[:start], s[start:]
		}
		width += w
	}
	return s, ""
}

func tableCell(value string, width int) *tview.TableCell {
	value = safe(value, false)
	escaped := tview.Escape(value)
	n := tview.TaggedStringWidth(escaped)
	if n < width {
		escaped += strings.Repeat(" ", width-n)
	}
	return tview.NewTableCell(escaped).SetMaxWidth(width)
}
func safe(s string, multiline bool) string {
	var out strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		if multiline && (r == '\n' || r == '\t') {
			out.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c {
			quoted := strconv.QuoteToASCII(string(r))
			out.WriteString(quoted[1 : len(quoted)-1])
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
func integer(n int64) string {
	text := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(text) - 3; i > start; i -= 3 {
		text = text[:i] + "," + text[i:]
	}
	return text
}
func number(n *int64) string {
	if n == nil {
		return "?"
	}
	return integer(*n)
}
func binarySize(n *int64) string {
	if n == nil {
		return "?"
	}
	if *n < 1024 {
		return integer(*n) + " B"
	}
	value := float64(*n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= 1024
		if value < 1024 || unit == "PiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return "?"
}
func speed(rate float64, known bool) string {
	if !known {
		return "?"
	}
	return fmt.Sprintf("%.1f", rate)
}
func percent(rate float64, known bool) string {
	if !known {
		return "?"
	}
	return fmt.Sprintf("%.1f%%", rate)
}
func clock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("01-02 15:04:05")
}
func has(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}
func age(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return max(0, time.Since(t)).Round(time.Second).String()
}
func sortedRows(rows []tableRow, rank bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rank && rows[i].rankInteger != rows[j].rankInteger {
			return rows[i].rankInteger > rows[j].rankInteger
		}
		if rank && rows[i].rank != rows[j].rank {
			return rows[i].rank > rows[j].rank
		}
		if rows[i].order != rows[j].order {
			return rows[i].order < rows[j].order
		}
		return rows[i].id < rows[j].id
	})
}
