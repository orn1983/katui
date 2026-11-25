package katui

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type payloadPane struct {
	u                                        *ui
	root, bar, content                       *tview.Flex
	dialog                                   *dialog
	metadata, navigation, status             *tview.TextView
	viewer, nodeView                         *payloadText
	document                                 payloadDocument
	expanded                                 bool
	mode, loadButton                         *tview.Button
	confirmLoad                              func(bool)
	loadCanceled                             bool
	search                                   *tview.InputField
	tree                                     *tview.TreeView
	delimButton, headerButton, profileButton *tview.Button
	records                                  []*storedRecord
	full, comparison, metadataTab            bool
	modeName, profileTopic                   string
	result                                   payloadResult
	renderCancel, searchCancel, nodeCancel   context.CancelFunc
	revision, searchRevision, nodeRevision   uint64
	textMatches                              [][]int
	treeMatches                              []*payloadNode
	match                                    int
	matchesLimited                           bool
	rendering                                bool
}

func (u *ui) newPayloadPane(full, comparison bool, records []*storedRecord) *payloadPane {
	p := &payloadPane{u: u, full: full, comparison: comparison, records: records, modeName: "raw", match: -1}
	p.root = tview.NewFlex().SetDirection(tview.FlexRow)
	if !full && !comparison {
		p.root.SetBorder(true).SetTitle(" Record inspector ")
	}
	p.bar = tview.NewFlex()
	p.content = tview.NewFlex().SetDirection(tview.FlexRow)
	p.metadata = tview.NewTextView().SetTextColor(tcell.ColorSilver).SetScrollable(true).SetWrap(false)
	p.status = tview.NewTextView().SetDynamicColors(true).SetTextColor(tcell.ColorLightGoldenrodYellow)
	p.navigation = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	p.viewer = newPayloadText(p.copyText)
	p.nodeView = newPayloadText(p.copyText)
	p.viewer.modeChanged, p.nodeView.modeChanged = p.updateNavigation, p.updateNavigation
	if !full && !comparison {
		p.viewer.onSelect, p.nodeView.onSelect = u.stopFollowing, u.stopFollowing
	}
	p.tree = p.newTreeView()
	p.updateNavigation()
	p.mode = tview.NewButton("").SetSelectedFunc(p.chooseFormat)
	p.delimButton = tview.NewButton(buttonLabel("Delimiter", "d")).SetSelectedFunc(p.delimiterDialog)
	p.headerButton = tview.NewButton(buttonLabel("Header", "Alt+H")).SetSelectedFunc(p.toggleHeader)
	p.profileButton = tview.NewButton(buttonLabel("Columns", "o")).SetSelectedFunc(func() { p.editProfile(csvMode(p.modeName)) })
	p.search = tview.NewInputField().SetLabel(buttonLabel("Find", "/") + "  ")
	p.search.SetChangedFunc(func(string) { p.scheduleSearch() })
	p.search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter || key == tcell.KeyEscape {
			p.focusViewer()
		}
	})
	p.metadata.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(ev, nil, p.metadata) })
	if len(records) > 0 {
		p.profileTopic = records[0].id.Topic
		p.modeName = u.profile(p.profileTopic).Mode
	}
	if comparison {
		p.modeName = textMode(p.modeName)
	}
	p.rebuild()
	return p
}

func (p *payloadPane) newTreeView() *tview.TreeView {
	tree := tview.NewTreeView()
	tree.SetFocusFunc(p.updateNavigation)
	tree.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		current := tree.GetCurrentNode()
		if current != nil {
			if e.Key() == tcell.KeyLeft {
				if current.IsExpanded() && len(current.GetChildren()) > 0 {
					current.Collapse()
				} else if n, ok := current.GetReference().(*payloadNode); ok && n.parent != nil {
					tree.SetCurrentNode(n.parent.widget)
					p.showNode(n.parent)
				}
				return nil
			}
			if e.Key() == tcell.KeyRight {
				if !current.IsExpanded() {
					current.Expand()
				} else if children := current.GetChildren(); len(children) > 0 {
					tree.SetCurrentNode(children[0])
					p.showNode(children[0].GetReference().(*payloadNode))
				}
				return nil
			}
		}
		return p.u.viewerKeys(e, nil, nil)
	})
	tree.SetChangedFunc(func(n *tview.TreeNode) {
		if v, ok := n.GetReference().(*payloadNode); ok {
			p.showNode(v)
		}
	})
	tree.SetSelectedFunc(func(n *tview.TreeNode) {
		if n.IsExpanded() {
			n.Collapse()
		} else {
			n.Expand()
		}
	})
	return tree
}

func (p *payloadPane) updateNavigation() {
	k := keyHint
	if p.tree.HasFocus() {
		p.navigation.SetText("Tree · " + k("←/→") + " Collapse/expand  " + k("Tab") + " Node text")
		return
	}
	mode := payloadScroll
	if p.viewer.HasFocus() {
		mode = p.viewer.mode
	} else if p.nodeView.HasFocus() {
		mode = p.nodeView.mode
	}
	line := "Scroll · " + k("Arrows") + " Scroll  " + k("Space") + " Cursor"
	switch mode {
	case payloadCursor:
		line = "Cursor · " + k("Arrows") + " Move  " + k("0/$") + " Line  " + k("v") + " Select"
	case payloadSelection:
		line = "Selection · " + k("Arrows") + " Extend  " + k("0/$") + " Line"
	}
	p.navigation.SetText(line + "  " + k("y") + " Yank\n" + k("Alt+←/→") + " Pan 8  " + k("Ctrl+←/→") + " Pan half  " + k("Esc") + " Back")
}
func (p *payloadPane) rebuild() {
	p.mode.SetLabel(buttonLabel("Format", "m") + " · " + modeLabels[indexOf(modes, p.modeName)])
	p.bar.Clear()
	p.bar.AddItem(p.mode, 0, 1, false)
	if csvMode(p.modeName) {
		p.bar.AddItem(p.delimButton, 13, 0, false).AddItem(p.headerButton, 14, 0, false)
		p.profileButton.SetLabel(buttonLabel("Columns", "o"))
		p.bar.AddItem(p.profileButton, 11, 0, false)
	} else if protoMode(p.modeName) {
		p.profileButton.SetLabel(buttonLabel("Schema", "u"))
		p.bar.AddItem(p.profileButton, 10, 0, false)
	}
	p.root.Clear()
	if !p.full && !p.comparison {
		p.root.AddItem(p.metadata, 3, 0, false)
	} else if p.full {
		source := fmt.Sprintf("%d retained records", len(p.records))
		if len(p.records) == 1 {
			source = strings.SplitN(metadataText(p.records[0]), "\n", 2)[0]
		}
		p.metadata.SetText(source)
		p.root.AddItem(p.metadata, 1, 0, false)
	}
	p.root.AddItem(p.bar, 1, 0, false)
	if p.full {
		p.root.AddItem(p.search, 1, 0, false)
	}
	p.root.AddItem(p.status, 1, 0, false).AddItem(p.content, 0, 1, true).AddItem(p.navigation, 2, 0, false)
	p.layout()
	if p.dialog != nil {
		p.dialog.focusables = p.focusables()
	}
}
func (p *payloadPane) showingTree() bool {
	return !p.metadataTab && p.result.tree != nil && treeMode(p.modeName)
}

func (p *payloadPane) layout() {
	p.content.Clear()
	if !p.showingTree() {
		p.content.AddItem(p.viewer, 0, 1, true)
	} else {
		p.content.AddItem(p.tree, 0, 2, true).AddItem(p.nodeView, 0, 1, false)
	}
}
func (p *payloadPane) focusables() []tview.Primitive {
	controls := []tview.Primitive{p.viewer}
	if p.showingTree() {
		controls = []tview.Primitive{p.tree, p.nodeView}
	}
	if p.full {
		controls = append(controls, p.search)
	}
	controls = append(controls, p.mode)
	if csvMode(p.modeName) {
		controls = append(controls, p.delimButton, p.headerButton, p.profileButton)
	} else if protoMode(p.modeName) {
		controls = append(controls, p.profileButton)
	}
	return controls
}
func (p *payloadPane) cycle(reverse bool) bool {
	seq := p.focusables()
	i := 0
	for index, w := range seq {
		if w.HasFocus() {
			i = index
			break
		}
	}
	if reverse {
		i--
	} else {
		i++
	}
	if i < 0 || i >= len(seq) {
		return false
	}
	p.u.app.SetFocus(seq[i])
	return true
}
func (p *payloadPane) focusViewer() {
	p.root.SetBorderColor(p.u.accent)
	if p.showingTree() {
		p.u.app.SetFocus(p.tree)
	} else {
		p.u.app.SetFocus(p.viewer)
	}
}

func (p *payloadPane) chooseFormat() {
	list := p.u.newList().ShowSecondaryText(false)
	d := p.u.dialog("Payload format", list, list)
	d.cancel = func() {
		d.close()
		p.focusViewer()
	}
	for i, mode := range modes {
		if p.comparison && treeMode(mode) {
			continue
		}
		list.AddItem(modeLabels[i], "", 0, func() {
			d.close()
			p.changeMode(mode)
			p.focusViewer()
		})
		if mode == p.modeName {
			list.SetCurrentItem(list.GetItemCount() - 1)
		}
	}
	d.hint(keyHint("Enter") + " Choose format  " + keyHint("Esc") + " Back")
	d.button("Back", "Esc", d.cancel)
}
func (p *payloadPane) topics() []string {
	set := map[string]bool{}
	for _, r := range p.records {
		set[r.id.Topic] = true
	}
	return sortedKeys(set)
}
func (p *payloadPane) setRecords(records []*storedRecord) {
	p.records = records
	if len(records) > 0 {
		p.profileTopic = records[0].id.Topic
		p.modeName = p.u.profile(p.profileTopic).Mode
		p.metadata.SetText(metadataText(records[0]))
	} else {
		p.profileTopic = ""
		p.metadata.SetText("Select a record")
	}
	p.rebuild()
	p.refresh()
}
func (p *payloadPane) changeMode(mode string) {
	if p.modeName == mode {
		return
	}
	p.modeName = mode
	if topics := p.topics(); len(topics) == 1 {
		selected := p.u.profile(topics[0])
		selected.Mode = mode
		p.u.updateProfile(topics[0], selected, "mode")
	}
	p.rebuild()
	p.refresh()
}
func (p *payloadPane) notice(text string) {
	if p.confirmLoad != nil {
		return
	}
	if p.dialog != nil {
		p.dialog.say(text)
	} else {
		p.u.say(text)
	}
}
func (p *payloadPane) cancelRender() {
	p.revision++
	p.searchRevision++
	p.nodeRevision++
	if p.searchCancel != nil {
		p.searchCancel()
		p.searchCancel = nil
	}
	if p.nodeCancel != nil {
		p.nodeCancel()
		p.nodeCancel = nil
	}
	if p.renderCancel != nil {
		p.renderCancel()
		p.renderCancel = nil
	}
}
func (p *payloadPane) refresh() {
	p.cancelRender()
	p.loadCanceled = false
	hadTreeFocus := p.tree.HasFocus() || p.nodeView.HasFocus()
	if p.tree.GetRoot() != nil {
		// The widget caches visible nodes with references to the decoded payload.
		p.tree = p.newTreeView()
	}
	p.result = payloadResult{}
	p.textMatches = nil
	p.treeMatches = nil
	p.match = -1
	p.status.SetText("")
	if p.dialog != nil {
		p.dialog.say("")
	}
	p.document = payloadDocument{}
	p.viewer.setDocument(payloadDocument{})
	p.nodeView.setDocument(payloadDocument{})
	p.layout()
	if p.dialog != nil {
		p.dialog.focusables = p.focusables()
	}
	if hadTreeFocus {
		p.focusViewer()
	}
	if p.metadataTab {
		p.showMetadata(true)
	}
	if len(p.records) == 0 {
		p.rendering = false
		p.updateLoadButton()
		return
	}
	ctx, cancel := context.WithCancel(p.u.ctx)
	p.renderCancel = cancel
	revision := p.revision
	records := append([]*storedRecord{}, p.records...)
	preferences := map[string]profile{}
	for _, topic := range p.topics() {
		preferences[topic] = p.u.profile(topic)
	}
	mode := p.modeName
	limits := p.u.b.inspectionLimits(p.expanded)
	release := p.u.holdPayloads(records)
	p.rendering = true
	p.updateLoadButton()
	p.status.SetText("Preparing payload…")
	if p.full {
		status := "Preparing payload…"
		if p.expanded {
			status = "Loading full payload…"
		}
		p.status.SetText(status + "  " + keyHint("Esc") + " Cancel")
	}
	go func() {
		defer cancel()
		result := p.u.preparePayload(ctx, records, preferences, mode, p.full, len(records) > 1 || p.comparison, limits)
		doc, capped := indexPayloadText(ctx, result.text)
		if capped {
			result.incomplete = true
			result.warning += "\nDisplay limited to 1,000,000 lines. Export for the original."
		}
		result.text = doc.text
		p.u.post(func() {
			release()
			if p.revision != revision || ctx.Err() != nil {
				return
			}
			p.rendering = false
			p.result = result
			p.document = doc
			p.updateLoadButton()
			if !p.full {
				p.showRenderStatus()
			}
			if p.dialog != nil && result.warning != "" {
				p.dialog.say(result.warning)
			}
			if !p.metadataTab {
				p.viewer.setDocument(doc)
				if result.tree != nil {
					root := result.tree.treeWidget()
					p.tree.SetRoot(root).SetCurrentNode(root)
					root.Expand()
					p.showNode(result.tree)
				}
				p.layout()
				if result.tree != nil && p.viewer.HasFocus() || result.tree == nil && (p.tree.HasFocus() || p.nodeView.HasFocus()) {
					p.focusViewer()
				}
			}
			if p.dialog != nil {
				p.dialog.focusables = p.focusables()
			}
			if p.full {
				p.scheduleSearch()
			}
		})
	}()
}
func (n *payloadNode) treeWidget() *tview.TreeNode {
	if n.widget == nil {
		n.widget = tview.NewTreeNode(shortened(safe(n.label, false), 240)).SetReference(n).SetExpanded(false)
	}
	return n.widget
}

func (n *payloadNode) materialize() {
	w := n.treeWidget()
	if len(w.GetChildren()) == 0 {
		for _, child := range n.children {
			w.AddChild(child.treeWidget())
		}
	}
}

func (p *payloadPane) showNode(n *payloadNode) {
	n.materialize()
	if p.nodeCancel != nil {
		p.nodeCancel()
	}
	p.nodeRevision++
	generation := p.nodeRevision
	if n.parent == nil && (!p.result.incomplete || n.value == nil && len(n.children) > 0) {
		p.nodeView.setDocument(p.document)
		return
	}
	p.nodeView.setDocument(payloadDocument{})
	ctx, cancel := context.WithCancel(p.u.ctx)
	p.nodeCancel = cancel
	limit := p.u.b.inspectionLimits(p.expanded).rendered
	go func() {
		defer cancel()
		raw, err := json.Marshal(n.value)
		var text string
		if err == nil {
			text, err = prettyJSON(ctx, raw, limit)
		}
		if err != nil {
			text = "Node cannot be formatted within the display limits. Select a smaller branch."
		}
		out := renderText{limit: limit}
		out.writeSafe(ctx, text)
		doc, linesLimited := indexPayloadText(ctx, out.String())
		p.u.post(func() {
			if p.nodeRevision == generation && ctx.Err() == nil {
				p.nodeView.setDocument(doc)
				if linesLimited || out.limited {
					p.notice("Node text reached the display limit. Select a smaller branch.")
				}
			}
		})
	}()
}

func (p *payloadPane) showMetadata(on bool) {
	hadFocus := p.viewer.HasFocus() || p.tree.HasFocus() || p.nodeView.HasFocus()
	p.metadataTab = on
	if on {
		var out strings.Builder
		for _, r := range p.records {
			out.WriteString(metadataText(r))
			out.WriteString("\n\n")
		}
		doc, _ := indexPayloadText(p.u.ctx, out.String())
		p.viewer.setDocument(doc)
	} else {
		p.viewer.setDocument(p.document)
	}
	p.layout()
	if hadFocus {
		p.focusViewer()
	}
}

func (p *payloadPane) canLoadFull() bool {
	if !p.full || p.rendering {
		return false
	}
	limits := p.u.b.inspectionLimits(false)
	canRaiseLimits := limits.structured < maxInspectionBytes || limits.rendered < maxInspectionBytes
	return p.result.canExpand && !p.expanded && canRaiseLimits
}

func (p *payloadPane) updateLoadButton() {
	if p.loadButton == nil {
		return
	}
	d := p.dialog
	show := p.canLoadFull() || p.loadCanceled
	label := "Load full"
	if p.loadCanceled {
		label = "Retry"
	}
	p.loadButton.SetLabel(buttonLabel(label, "Alt+L"))
	p.loadButton.SetDisabled(!show)
	d.footer.Clear()
	for _, button := range d.buttons {
		if button != p.loadButton || show {
			d.footer.AddItem(button, tview.TaggedStringWidth(button.GetLabel())+2, 0, false)
		}
	}
}

func (p *payloadPane) loadFull() {
	if p.loadCanceled {
		p.refresh()
		return
	}
	if !p.canLoadFull() || p.confirmLoad != nil {
		return
	}
	d := p.dialog
	buttons, focusables := d.buttons, d.focusables
	previousHint := d.status.GetText(false)
	p.confirmLoad = func(load bool) {
		p.confirmLoad = nil
		d.buttons, d.focusables = buttons, focusables
		p.updateLoadButton()
		p.focusViewer()
		if load {
			p.expanded = true
			p.refresh()
		} else {
			d.hint(previousHint)
		}
	}
	d.buttons, d.focusables = nil, nil
	d.footer.Clear()
	load := d.button("Load", "Enter", func() { p.confirmLoad(true) })
	d.button("Cancel", "Esc", func() { p.confirmLoad(false) })
	d.hint(fmt.Sprintf("Load up to %d MiB each for input and display?\nParsing can use additional memory.", maxInspectionBytes>>20))
	p.u.app.SetFocus(load)
}

func (p *payloadPane) showRenderStatus() {
	notice := tview.Escape(safe(strings.ReplaceAll(p.result.warning, "\n", " · "), false))
	if p.canLoadFull() {
		notice = keyHint("Alt+L") + " Load full · " + notice
	}
	if notice == "" && p.dialog == nil {
		k := keyHint
		notice = k("?") + " Copy actions · " + k("s") + " Save · " + k("z") + " Fullscreen"
	}
	p.status.SetText(notice)
}

func (p *payloadPane) scheduleSearch() {
	if !p.full || p.rendering {
		return
	}
	p.searchRevision++
	revision := p.searchRevision
	if p.searchCancel != nil {
		p.searchCancel()
		p.searchCancel = nil
	}
	query := p.search.GetText()
	tree, text := p.result.tree, p.result.text
	p.match = -1
	p.textMatches = nil
	p.treeMatches = nil
	p.matchesLimited = false
	p.viewer.match = [2]int{}
	if query == "" {
		p.showRenderStatus()
		return
	}
	if len(query) > 4096 {
		p.status.SetText("Search text is limited to 4 KiB")
		return
	}
	ctx, cancel := context.WithCancel(p.u.ctx)
	p.searchCancel = cancel
	p.status.SetText("Searching…")
	go func() {
		defer cancel()
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		pattern, err := regexp.Compile("(?i)" + regexp.QuoteMeta(query))
		if err != nil {
			p.u.post(func() {
				if p.searchRevision == revision {
					p.status.SetText("Could not search for that text")
				}
			})
			return
		}
		const maxSearchMatches = 10_000
		var positions [][]int
		var matches []*payloadNode
		limited := false
		if tree != nil {
			var visit func(*payloadNode)
			visit = func(n *payloadNode) {
				if limited || ctx.Err() != nil {
					return
				}
				if pattern.MatchString(n.search) {
					if len(matches) == maxSearchMatches {
						limited = true
						return
					}
					matches = append(matches, n)
				}
				for _, c := range n.children {
					visit(c)
				}
			}
			visit(tree)
		} else {
			positions = pattern.FindAllStringIndex(text, maxSearchMatches+1)
			if len(positions) > maxSearchMatches {
				positions = positions[:maxSearchMatches]
				limited = true
			}
		}
		if ctx.Err() != nil {
			return
		}
		p.u.post(func() {
			if p.searchRevision != revision {
				return
			}
			p.textMatches = positions
			p.treeMatches = matches
			p.matchesLimited = limited
			p.advance(1)
		})
	}()
}
func (p *payloadPane) advance(change int) {
	total := len(p.textMatches) + len(p.treeMatches)
	if total == 0 {
		p.status.SetText("No matches")
		return
	}
	if p.match < 0 {
		if change > 0 {
			p.match = 0
		} else {
			p.match = total - 1
		}
	} else {
		p.match = (p.match + change + total) % total
	}
	suffix := ""
	if p.matchesLimited {
		suffix = "+"
	}
	p.status.SetText(fmt.Sprintf("Match %d / %d%s  ·  ", p.match+1, total, suffix) + keyHint("n") + " Next  " + keyHint("p") + " Previous")
	if len(p.treeMatches) > 0 {
		node := p.treeMatches[p.match]
		path := []*payloadNode{node}
		for n := node.parent; n != nil; n = n.parent {
			path = append(path, n)
		}
		for i := len(path) - 1; i >= 0; i-- {
			n := path[i]
			n.materialize()
			if i > 0 {
				n.widget.Expand()
			}
		}
		p.tree.SetCurrentNode(node.widget)
		p.showNode(node)
	} else {
		hit := p.textMatches[p.match]
		p.viewer.match = [2]int{hit[0], hit[1]}
		p.viewer.selectBy(hit[0], false)
	}
}
func (p *payloadPane) shortcut(key rune) bool {
	switch key {
	case 'm':
		p.chooseFormat()
	case 'd':
		if !csvMode(p.modeName) {
			return false
		}
		p.delimiterDialog()
	case 'h':
		if !csvMode(p.modeName) {
			return false
		}
		p.toggleHeader()
	case 'o':
		if !csvMode(p.modeName) {
			return false
		}
		p.editProfile(true)
	case 'u':
		if !protoMode(p.modeName) {
			return false
		}
		p.editProfile(false)
	case 'y':
		for _, view := range []*payloadText{p.viewer, p.nodeView} {
			if view.HasFocus() && (view.mode == payloadSelection || view.anchor != view.cursor) {
				view.copy(view.selectedText())
				if view.mode == payloadSelection {
					view.anchor = view.cursor
					view.exitMode()
				}
				return true
			}
		}
		p.copyView()
	case copyValueKey:
		p.copyValue()
	case copyViewKey:
		p.copyView()
	case 's', 'E':
		p.exportValue()
	case 'l', 'L':
		if !p.full {
			return false
		}
		p.loadFull()
	default:
		return false
	}
	return true
}

func (p *payloadPane) chooseTopic(action func(string)) {
	choices := p.topics()
	if len(choices) == 0 {
		p.notice("Select a record first")
		return
	}
	if len(choices) == 1 {
		p.profileTopic = choices[0]
		if action != nil {
			action(choices[0])
		}
		return
	}
	list := p.u.newList().ShowSecondaryText(false)
	d := p.u.dialog("Profile topic", list, list)
	for _, topic := range choices {
		list.AddItem(tview.Escape(safe(topic, false)), "", 0, func() {
			p.profileTopic = topic
			d.close()
			if action != nil {
				action(topic)
			}
		})
	}
	d.button("Cancel", "Esc", d.close)
}
func (p *payloadPane) activeTopic(action func(string)) {
	if p.profileTopic != "" {
		action(p.profileTopic)
	} else {
		p.chooseTopic(action)
	}
}
func (p *payloadPane) toggleHeader() {
	p.activeTopic(func(topic string) {
		profile := p.u.profile(topic)
		profile.header = !profile.header
		p.u.updateProfile(topic, profile, "")
		p.refresh()
		p.notice(fmt.Sprintf("CSV header row %t · %s", profile.header, topic))
	})
}
func (p *payloadPane) delimiterDialog() {
	p.activeTopic(func(topic string) {
		profile := p.u.profile(topic)
		choices := []string{"Auto", ",", ";", "Tab", "|", "Custom"}
		current := profile.delimiter
		if current == "" {
			current = "Auto"
		}
		i := indexOf(choices, current)
		if i == 0 && current != "Auto" {
			i = 5
		}
		form := tview.NewForm()
		selected := dropdown(form, "Delimiter", choices, i, nil)
		custom := field(form, "Custom delimiter", current)
		d := p.u.dialog("CSV delimiter · "+topic, form, form)
		apply := func() {
			idx, value := selected.GetCurrentOption()
			if idx == 5 {
				value = custom.GetText()
			}
			if _, err := delimiter(value, ""); err != nil {
				d.say(err.Error())
				return
			}
			profile.delimiter = value
			p.u.updateProfile(topic, profile, "")
			d.close()
			p.refresh()
		}
		d.button("Apply", "a", apply)
		d.button("Cancel", "Esc", d.close)
	})
}
func (p *payloadPane) editProfile(csv bool) {
	p.chooseTopic(func(topic string) {
		if csv {
			p.csvEditor(topic)
		} else {
			p.protoEditor(topic)
		}
	})
}
