package katui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type payloadPane struct {
	u                                        *ui
	root, bar, content                       *tview.Flex
	dialog                                   *dialog
	metadata, status, viewer, nodeView       *tview.TextView
	mode                                     *tview.DropDown
	search                                   *tview.InputField
	tree                                     *tview.TreeView
	delimButton, headerButton, profileButton *tview.Button
	records                                  []*storedRecord
	full, comparison, metadataTab            bool
	modeName, profileTopic                   string
	result                                   payloadResult
	renderCancel, searchCancel               context.CancelFunc
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
	p.status = tview.NewTextView().SetTextColor(tcell.ColorLightGoldenrodYellow)
	p.viewer = tview.NewTextView().SetScrollable(true).SetWrap(false).SetDynamicColors(true)
	p.nodeView = tview.NewTextView().SetScrollable(true).SetWrap(false)
	p.tree = tview.NewTreeView()
	p.mode = tview.NewDropDown().SetLabel("Mode m  ")
	p.mode.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		if p.mode.IsOpen() {
			return u.viewerKeys(e, nil, nil)
		}
		return e
	})
	p.delimButton = tview.NewButton("Delimiter d").SetSelectedFunc(p.delimiterDialog)
	p.headerButton = tview.NewButton("Header h").SetSelectedFunc(p.toggleHeader)
	p.profileButton = tview.NewButton("Columns o").SetSelectedFunc(func() { p.editProfile(csvMode(p.modeName)) })
	p.search = tview.NewInputField().SetLabel("Find /  ")
	p.search.SetChangedFunc(func(string) { p.scheduleSearch() })
	p.search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter || key == tcell.KeyEscape {
			p.focusViewer()
		}
	})
	for _, v := range []*tview.TextView{p.viewer, p.nodeView, p.metadata} {
		v.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(ev, nil, v) })
	}
	p.tree.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey {
		current := p.tree.GetCurrentNode()
		if current != nil {
			if e.Key() == tcell.KeyLeft {
				if current.IsExpanded() && len(current.GetChildren()) > 0 {
					current.Collapse()
				} else if n, ok := current.GetReference().(*payloadNode); ok && n.parent != nil {
					p.tree.SetCurrentNode(n.parent.widget)
					p.showNode(n.parent)
				}
				return nil
			}
			if e.Key() == tcell.KeyRight {
				if !current.IsExpanded() {
					current.Expand()
				} else if children := current.GetChildren(); len(children) > 0 {
					p.tree.SetCurrentNode(children[0])
					p.showNode(children[0].GetReference().(*payloadNode))
				}
				return nil
			}
		}
		return u.viewerKeys(e, nil, nil)
	})
	p.tree.SetChangedFunc(func(n *tview.TreeNode) {
		if v, ok := n.GetReference().(*payloadNode); ok {
			p.showNode(v)
		}
	})
	p.tree.SetSelectedFunc(func(n *tview.TreeNode) {
		if n.IsExpanded() {
			n.Collapse()
		} else {
			n.Expand()
		}
	})
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
func (p *payloadPane) rebuild() {
	options, labels := []string{}, []string{}
	for i, m := range modes {
		if !p.comparison || !treeMode(m) {
			options = append(options, m)
			labels = append(labels, modeLabels[i])
		}
	}
	p.mode.SetOptions(labels, func(_ string, i int) {
		if i >= 0 {
			p.changeMode(options[i])
		}
	}).SetCurrentOption(indexOf(options, p.modeName))
	p.bar.Clear()
	p.bar.AddItem(p.mode, 0, 1, true)
	if csvMode(p.modeName) {
		p.bar.AddItem(p.delimButton, 13, 0, false).AddItem(p.headerButton, 10, 0, false)
		p.profileButton.SetLabel("Columns o")
		p.bar.AddItem(p.profileButton, 11, 0, false)
	} else if protoMode(p.modeName) {
		p.profileButton.SetLabel("Schema u")
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
	p.root.AddItem(p.bar, 1, 0, true)
	if p.full {
		p.root.AddItem(p.search, 1, 0, false)
	}
	p.root.AddItem(p.status, 1, 0, false).AddItem(p.content, 0, 1, false)
	p.layout()
	if p.dialog != nil {
		p.dialog.focusables = p.focusables()
	}
}
func (p *payloadPane) layout() {
	p.content.Clear()
	if p.metadataTab || p.result.tree == nil || !treeMode(p.modeName) {
		p.content.AddItem(p.viewer, 0, 1, true)
	} else {
		p.content.AddItem(p.tree, 0, 2, true).AddItem(p.nodeView, 0, 1, false)
	}
}
func (p *payloadPane) focusables() []tview.Primitive {
	controls := []tview.Primitive{p.mode}
	if csvMode(p.modeName) {
		controls = append(controls, p.delimButton, p.headerButton, p.profileButton)
	} else if protoMode(p.modeName) {
		controls = append(controls, p.profileButton)
	}
	if p.full {
		controls = append(controls, p.search)
	}
	if p.result.tree != nil && !p.metadataTab && treeMode(p.modeName) {
		controls = append(controls, p.tree, p.nodeView)
	} else {
		controls = append(controls, p.viewer)
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
func (p *payloadPane) focusFirst() { p.root.SetBorderColor(p.u.accent); p.u.app.SetFocus(p.mode) }
func (p *payloadPane) focusLast() {
	seq := p.focusables()
	p.root.SetBorderColor(p.u.accent)
	p.u.app.SetFocus(seq[len(seq)-1])
}
func (p *payloadPane) focusViewer() {
	if p.result.tree != nil && !p.metadataTab && treeMode(p.modeName) {
		p.u.app.SetFocus(p.tree)
	} else {
		p.u.app.SetFocus(p.viewer)
	}
}
func (p *payloadPane) topics() []string {
	set := map[string]bool{}
	for _, r := range p.records {
		set[r.id.Topic] = true
	}
	result := make([]string, 0, len(set))
	for topic := range set {
		result = append(result, topic)
	}
	sort.Strings(result)
	return result
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
	if p.renderCancel != nil {
		p.renderCancel()
		p.renderCancel = nil
	}
}
func (p *payloadPane) refresh() {
	p.cancelRender()
	p.result = payloadResult{}
	p.textMatches = nil
	p.treeMatches = nil
	p.match = -1
	p.status.SetText("")
	p.viewer.SetText("").ScrollToBeginning()
	p.nodeView.SetText("").ScrollToBeginning()
	p.layout()
	if p.metadataTab {
		p.showMetadata(true)
	}
	if len(p.records) == 0 {
		p.rendering = false
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
	p.rendering = true
	p.status.SetText("Preparing payload…")
	go func() {
		result := p.u.preparePayload(ctx, records, preferences, mode, p.full, len(records) > 1 || p.comparison)
		if ctx.Err() != nil {
			return
		}
		p.u.post(func() {
			if p.revision != revision {
				return
			}
			p.rendering = false
			p.result = result
			notice := safe(strings.ReplaceAll(result.warning, "\n", " · "), false)
			if notice == "" && p.dialog == nil {
				notice = "y Copy value  v Copy view  F4 Full"
			}
			p.status.SetText(notice)
			if p.dialog != nil && result.warning != "" {
				p.dialog.say(result.warning)
			}
			if !p.metadataTab {
				p.setTextMatch(nil)
				if result.tree != nil {
					root := p.addNode(result.tree)
					p.tree.SetRoot(root).SetCurrentNode(root)
					root.Expand()
					p.showNode(result.tree)
				}
				p.layout()
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
func (p *payloadPane) addNode(node *payloadNode) *tview.TreeNode {
	if node.widget != nil {
		return node.widget
	}
	w := tview.NewTreeNode(shortened(safe(node.label, false), 240)).SetReference(node).SetExpanded(false)
	node.widget = w
	// Each visible node has bounded fan-out in full mode.
	for _, child := range node.children {
		placeholder := tview.NewTreeNode(shortened(safe(child.label, false), 240)).SetReference(child).SetExpanded(false)
		child.widget = placeholder
		w.AddChild(placeholder)
	}
	return w
}
func (p *payloadPane) materialize(node *payloadNode) {
	w := node.widget
	if w == nil {
		w = p.addNode(node)
	}
	if len(w.GetChildren()) == 0 {
		for _, child := range node.children {
			w.AddChild(p.addNode(child))
		}
	}
}
func (p *payloadPane) showNode(n *payloadNode) {
	p.materialize(n)
	p.nodeRevision++
	generation := p.nodeRevision
	if n.value == nil && n.parent == nil && len(n.children) > 0 {
		p.nodeView.SetText(p.result.text)
		return
	}
	ctx := p.u.ctx
	go func() {
		text := safe(prettyValue(n.value), true)
		if ctx.Err() != nil {
			return
		}
		p.u.post(func() {
			if p.nodeRevision == generation {
				p.nodeView.SetText(text).ScrollToBeginning()
			}
		})
	}()
}
func (p *payloadPane) showMetadata(on bool) {
	p.metadataTab = on
	if on {
		var out strings.Builder
		for _, r := range p.records {
			out.WriteString(metadataText(r))
			out.WriteString("\n\n")
		}
		p.viewer.SetText(tview.Escape(out.String()))
	} else {
		p.setTextMatch(nil)
	}
	p.viewer.ScrollToBeginning()
	p.layout()
}
func (p *payloadPane) setTextMatch(match []int) {
	text := p.result.text
	if len(match) == 0 {
		p.viewer.SetText(tview.Escape(text))
		return
	}
	start, end := match[0], match[1]
	p.viewer.SetText(tview.Escape(text[:start]) + "[black:yellow]" + tview.Escape(text[start:end]) + "[-:-]" + tview.Escape(text[end:]))
	row := strings.Count(text[:start], "\n")
	line := strings.LastIndexByte(text[:start], '\n') + 1
	col := tview.TaggedStringWidth(tview.Escape(text[line:start]))
	p.viewer.ScrollTo(row, max(0, col-5))
}
func (p *payloadPane) scheduleSearch() {
	if !p.full {
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
	if query == "" {
		p.status.SetText(safe(strings.ReplaceAll(p.result.warning, "\n", " · "), false))
		p.setTextMatch(nil)
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
	p.status.SetText(fmt.Sprintf("Match %d / %d%s  ·  n next / p previous", p.match+1, total, suffix))
	if len(p.treeMatches) > 0 {
		node := p.treeMatches[p.match]
		path := []*payloadNode{node}
		for n := node.parent; n != nil; n = n.parent {
			path = append(path, n)
		}
		for i := len(path) - 1; i >= 0; i-- {
			n := path[i]
			p.materialize(n)
			if i > 0 {
				n.widget.Expand()
			}
		}
		p.tree.SetCurrentNode(node.widget)
		p.showNode(node)
	} else {
		p.setTextMatch(p.textMatches[p.match])
	}
}
func (u *ui) clipboard(text string) {
	if u.screen == nil {
		return
	}
	u.screen.SetClipboard([]byte(text))
}
func (p *payloadPane) copyValue() {
	if len(p.records) == 0 {
		p.notice("Select a record first")
		return
	}
	records := append([]*storedRecord{}, p.records...)
	revision := p.revision
	go func() {
		var values []string
		var err error
		for _, r := range records {
			var v []byte
			v, err = r.full()
			if err != nil {
				break
			}
			if v == nil {
				values = append(values, "<tombstone>")
			} else {
				values = append(values, safe(string(v), true))
			}
		}
		p.u.post(func() {
			if p.revision != revision {
				return
			}
			if err != nil {
				p.notice(err.Error())
				return
			}
			p.u.clipboard(strings.Join(values, "\n\n"))
			p.notice("Clipboard transfer requested")
		})
	}()
}
func (p *payloadPane) copyView() {
	if p.rendering && !p.metadataTab {
		p.notice("Payload is still being prepared")
		return
	}
	text := p.result.text
	if p.metadataTab {
		text = p.viewer.GetText(true)
	}
	p.u.clipboard(text)
	p.notice("Clipboard transfer requested")
}
func (p *payloadPane) shortcut(key rune) bool {
	switch key {
	case 'm':
		p.u.app.SetFocus(p.mode)
		p.mode.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, 0), func(p2 tview.Primitive) { p.u.app.SetFocus(p2) })
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
		p.copyValue()
	case 'v':
		p.copyView()
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
	list := tview.NewList().ShowSecondaryText(false)
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
	d.button("Cancel", d.close)
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
		d.button("Apply Alt+A", apply)
		d.button("Cancel", d.close)
		d.keymap(func(e *tcell.EventKey) bool {
			if alt(e) == 'a' {
				apply()
				return true
			}
			return false
		})
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
