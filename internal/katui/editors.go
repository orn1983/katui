package katui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (p *payloadPane) csvEditor(topic string) {
	u := p.u
	profile := u.profile(topic)
	form := tview.NewForm().SetItemPadding(0)
	names := tview.NewTextArea().SetLabel(buttonLabel("Names", "Alt+C")).SetText(strings.Join(profile.Columns, "\n"), true).SetSize(5, 0)
	form.AddFormItem(names)
	save := check(form, buttonLabel("Save preference", "Alt+S"), false)
	preview := tview.NewTextView().SetScrollable(true).SetWrap(false)
	preview.SetBorder(true).SetTitle(" Pivot preview ")
	body := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(form, 8, 0, true).AddItem(preview, 0, 1, false)
	d := u.dialog("CSV columns · "+topic, body, form)
	d.focusables = []tview.Primitive{form, preview}
	columns := func() []string {
		if names.GetText() == "" {
			return nil
		}
		cols := strings.Split(strings.ReplaceAll(names.GetText(), "\r\n", "\n"), "\n")
		for len(cols) > 0 && cols[len(cols)-1] == "" {
			cols = cols[:len(cols)-1]
		}
		return cols
	}
	var revision uint64
	render := func() {
		revision++
		generation := revision
		updated := profile
		updated.Columns = columns()
		if err := validateColumns(updated.Columns); err != nil {
			d.say(err.Error())
			return
		}
		var record *storedRecord
		for _, r := range p.records {
			if r.id.Topic == topic {
				record = r
				break
			}
		}
		if record == nil || record.value == nil || record.truncated() {
			preview.SetText("Select a complete CSV preview to see the column layout.")
			return
		}
		go func() {
			data, notices, err := parseCSV(context.Background(), string(record.value), updated, false, 5, maxRenderedPayload)
			pivot, _ := csvPivot(context.Background(), data, maxRenderedPayload)
			text := pivot + "\n" + strings.Join(notices, "\n")
			if err != nil {
				text = "CSV: " + err.Error()
			}
			u.post(func() {
				if !d.closed && revision == generation {
					preview.SetText(text)
				}
			})
		}()
	}
	names.SetChangedFunc(render)
	apply := func() {
		cols := columns()
		if err := validateColumns(cols); err != nil {
			d.say(err.Error())
			return
		}
		profile.Columns = cols
		what := ""
		if save.IsChecked() {
			what = "columns"
		}
		u.updateProfile(topic, profile, what)
		d.close()
		p.refresh()
	}
	d.button("Apply", "a", apply)
	d.button("Cancel", "Esc", d.close)
	d.keymap(func(e *tcell.EventKey) bool {
		switch alt(e) {
		case 'c':
			focusForm(u, form, 0)
		case 's':
			save.SetChecked(!save.IsChecked())
		default:
			return false
		}
		return true
	})
	render()
}

func lines(text string) []string {
	var result []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}
func (p *payloadPane) protoEditor(topic string) {
	u := p.u
	profile := u.profile(topic)
	current := protoProfile{Framing: "raw"}
	if profile.Protobuf != nil {
		current = *profile.Protobuf
	}
	form := tview.NewForm().SetItemPadding(0)
	path := field(form, buttonLabel("Schema path", "Alt+P"), current.Path)
	folders := tview.NewTextArea().SetLabel("Import folders").SetText(strings.Join(current.Include, "\n"), true).SetSize(4, 0)
	form.AddFormItem(folders)
	types := dropdown(form, buttonLabel("Message", "Alt+M"), nil, -1, nil)
	types.SetDisabled(true)
	framing := dropdown(form, buttonLabel("Wire format", "Alt+F"), []string{"Raw", "Confluent prefix"}, indexOf([]string{"raw", "confluent"}, current.Framing), nil)
	save := check(form, buttonLabel("Save preference", "Alt+S"), false)
	d := u.dialog("Protobuf schema · "+topic, form, form)
	var schema *loadedSchema
	var generation uint64
	var cancel context.CancelFunc
	var applyButton *tview.Button
	var loadButton *tview.Button
	invalidate := func() {
		generation++
		if cancel != nil {
			cancel()
			cancel = nil
		}
		schema = nil
		types.SetOptions(nil, nil)
		types.SetDisabled(true)
		if applyButton != nil {
			applyButton.SetDisabled(true)
		}
		if loadButton != nil {
			loadButton.SetDisabled(false)
		}
	}
	path.SetChangedFunc(func(string) { invalidate(); d.say("Load the current schema and import folders") })
	folders.SetChangedFunc(func() { invalidate(); d.say("Load the current schema and import folders") })
	browse := func() {
		u.fileBrowser(filePicker{initial: path.GetText(), title: "Choose schema file", accept: schemaExtension, preview: true}, func(name string) { path.SetText(name); focusForm(u, form, 0) })
	}
	addFolder := func() {
		initial := filepath.Dir(expandHome(path.GetText()))
		u.fileBrowser(filePicker{initial: initial, title: "Choose import folder"}, func(name string) {
			f := lines(folders.GetText())
			if !containsString(f, name) {
				f = append(f, name)
			}
			folders.SetText(strings.Join(f, "\n"), true)
			focusForm(u, form, 1)
		})
	}
	load := func() {
		if strings.TrimSpace(path.GetText()) == "" {
			d.say("Choose a schema file")
			return
		}
		invalidate()
		sequence := generation
		var ctx context.Context
		ctx, cancel = context.WithTimeout(u.ctx, 20*time.Second)
		request := protoProfile{Path: strings.TrimSpace(path.GetText()), Include: lines(folders.GetText())}
		d.say("Loading schema…")
		loadButton.SetDisabled(true)
		go func() {
			result, err := loadSchema(ctx, request)
			u.post(func() {
				if d.closed || sequence != generation {
					return
				}
				schema = result
				loadButton.SetDisabled(false)
				if err != nil {
					d.say(err.Error())
					return
				}
				types.SetOptions(result.names, nil).SetCurrentOption(indexOf(result.names, current.Message))
				types.SetDisabled(false)
				applyButton.SetDisabled(false)
				d.say(fmt.Sprintf("Loaded %d message types", len(result.names)))
			})
		}()
	}
	apply := func() {
		if schema == nil {
			d.say("Load the schema before applying")
			return
		}
		i, message := types.GetCurrentOption()
		if i < 0 || schema.messages[message] == nil {
			d.say("Choose a loaded message type")
			return
		}
		f, _ := framing.GetCurrentOption()
		profile.Protobuf = &protoProfile{Path: strings.TrimSpace(path.GetText()), Include: lines(folders.GetText()), Message: message, Framing: []string{"raw", "confluent"}[f]}
		profile.schema = schema
		what := ""
		if save.IsChecked() {
			what = "protobuf"
		}
		u.updateProfile(topic, profile, what)
		d.cancel()
		p.refresh()
	}
	d.cancel = func() {
		if cancel != nil {
			cancel()
		}
		d.close()
	}
	d.button("Browse", "b", browse)
	d.button("Add folder", "i", addFolder)
	loadButton = d.button("Load", "l", load)
	applyButton = d.button("Apply", "a", apply).SetDisabled(true)
	d.button("Cancel", "Esc", d.cancel)
	d.keymap(func(e *tcell.EventKey) bool {
		switch alt(e) {
		case 'p':
			focusForm(u, form, 0)
		case 'm':
			if schema != nil {
				focusForm(u, form, 2)
			}
		case 'f':
			focusForm(u, form, 3)
		case 's':
			save.SetChecked(!save.IsChecked())
		default:
			return false
		}
		return true
	})
	if profile.schema != nil {
		schema = profile.schema
		types.SetOptions(schema.names, nil).SetCurrentOption(indexOf(schema.names, current.Message))
		types.SetDisabled(false)
		applyButton.SetDisabled(false)
	}
}

type fileEntry struct {
	name, path string
	directory  bool
	size       int64
	modified   time.Time
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}

type filePicker struct {
	initial, title string
	accept         func(string) bool // nil selects folders
	preview        bool
}

func (u *ui) fileBrowser(picker filePicker, pick func(string)) {
	folderMode := picker.accept == nil
	path := tview.NewInputField().SetLabel(buttonLabel("Path", "Alt+P") + " ")
	filter := tview.NewInputField().SetLabel(buttonLabel("Filter", "/") + " ")
	table := tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	table.SetBorder(true).SetTitle(" " + buttonLabel("Files", "Alt+B") + " ")
	preview := tview.NewTextView().SetScrollable(true)
	body := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(path, 1, 0, false).AddItem(filter, 1, 0, false).AddItem(table, 0, 1, true).AddItem(preview, 2, 0, false)
	d := u.dialog(picker.title, body, table)
	d.focusables = []tview.Primitive{path, filter, table, preview}
	var here string
	var entries, visible []fileEntry
	hidden := false
	var generation, selectionGeneration uint64
	chosen := func() fileEntry {
		row, _ := table.GetSelection()
		if row > 0 && row <= len(visible) {
			return visible[row-1]
		}
		return fileEntry{}
	}
	redraw := func() {
		name := chosen().name
		visible = nil
		table.Clear().SetCell(0, 0, tableCell("Name", 33).SetSelectable(false)).SetCell(0, 1, tableCell("Size", 14).SetSelectable(false)).SetCell(0, 2, tableCell("Modified", 16).SetSelectable(false))
		selected := 1
		for _, entry := range entries {
			if entry.name != ".." && ((!hidden && strings.HasPrefix(entry.name, ".")) || !has(entry.name, filter.GetText())) {
				continue
			}
			visible = append(visible, entry)
			i := len(visible)
			label := entry.name
			size := "—"
			if entry.directory {
				label += "/"
			} else {
				size = binarySize(ptr(entry.size))
			}
			table.SetCell(i, 0, tableCell(label, 33)).SetCell(i, 1, tableCell(size, 14)).SetCell(i, 2, tableCell(clock(entry.modified), 16))
			if entry.name == name {
				selected = i
			}
		}
		if len(visible) == 0 {
			selected = 0
		}
		table.Select(selected, 0)
	}
	var browse func(string)
	browse = func(name string) {
		generation++
		selectionGeneration++
		sequence := generation
		if name == "" {
			name, _ = os.UserHomeDir()
		}
		d.say("Opening " + name + "…")
		go func() {
			absolute, err := filepath.Abs(expandHome(name))
			var items []fileEntry
			var selectFile string
			if err == nil {
				info, e := os.Stat(absolute)
				if e != nil {
					err = e
				} else if !info.IsDir() {
					if folderMode || !info.Mode().IsRegular() || !picker.accept(absolute) {
						err = fmt.Errorf("unsupported target %s", absolute)
					} else {
						selectFile = filepath.Base(absolute)
						absolute = filepath.Dir(absolute)
					}
				}
			}
			if err == nil {
				var directory *os.File
				directory, err = os.Open(absolute)
				if err == nil {
					var files []os.DirEntry
					files, err = directory.ReadDir(10001)
					_ = directory.Close()
					if err == io.EOF {
						err = nil
					}
					if len(files) > 10000 {
						err = fmt.Errorf("%s has over 10,000 entries", absolute)
					}
					if err == nil {
						for _, entry := range files {
							data, e := entry.Info()
							if entry.Type()&os.ModeSymlink != 0 {
								data, e = os.Stat(filepath.Join(absolute, entry.Name()))
							}
							if e != nil || !data.IsDir() && (folderMode || !data.Mode().IsRegular() || !picker.accept(entry.Name())) {
								continue
							}
							items = append(items, fileEntry{entry.Name(), filepath.Join(absolute, entry.Name()), data.IsDir(), data.Size(), data.ModTime()})
						}
						sort.Slice(items, func(i, j int) bool {
							if items[i].directory != items[j].directory {
								return items[i].directory
							}
							a, b := strings.ToLower(items[i].name), strings.ToLower(items[j].name)
							if a == b {
								return items[i].name < items[j].name
							}
							return a < b
						})
						if parent := filepath.Dir(absolute); parent != absolute {
							items = append([]fileEntry{{name: "..", path: parent, directory: true}}, items...)
						}
					}
				}
			}
			u.post(func() {
				if d.closed || generation != sequence {
					return
				}
				if err != nil {
					d.say(err.Error())
					return
				}
				here = absolute
				entries = items
				path.SetText(absolute)
				filter.SetText("")
				preview.SetText("")
				redraw()
				for i, item := range visible {
					if item.name == selectFile {
						table.Select(i+1, 0)
						break
					}
				}
				d.say(fmt.Sprintf("%d entries", len(entries)))
				u.app.SetFocus(table)
			})
		}()
	}
	open := func() {
		if path.HasFocus() && path.GetText() != here {
			browse(path.GetText())
			return
		}
		entry := chosen()
		if entry.path == "" {
			return
		}
		if entry.directory {
			browse(entry.path)
			return
		}
		d.close()
		pick(entry.path)
	}
	table.SetSelectedFunc(func(int, int) { open() })
	table.SetSelectionChangedFunc(func(int, int) {
		entry := chosen()
		selectionGeneration++
		sequence := selectionGeneration
		preview.SetText(safe(entry.path, false))
		if !picker.preview || entry.path == "" || entry.directory || !strings.EqualFold(filepath.Ext(entry.path), ".proto") {
			return
		}
		go func() {
			f, err := os.Open(entry.path)
			var text string
			if err == nil {
				raw, e := io.ReadAll(io.LimitReader(f, 4096))
				_ = f.Close()
				err = e
				text = safe(string(raw), true)
			}
			if err != nil {
				text = err.Error()
			}
			u.post(func() {
				if !d.closed && selectionGeneration == sequence {
					preview.SetText(text)
				}
			})
		}()
	})
	table.SetInputCapture(func(e *tcell.EventKey) *tcell.EventKey { return u.viewerKeys(e, table, nil) })
	path.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			browse(path.GetText())
		}
	})
	filter.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			u.app.SetFocus(table)
		}
	})
	filter.SetChangedFunc(func(string) { redraw() })
	chooseFolder := func() {
		selected := chosen()
		target := here
		if selected.directory && selected.name != ".." {
			target = selected.path
		}
		if target != "" {
			d.close()
			pick(target)
		}
	}
	home := func() { name, _ := os.UserHomeDir(); browse(name) }
	up := func() {
		if here != "" {
			browse(filepath.Dir(here))
		}
	}
	toggleHidden := func() {
		hidden = !hidden
		redraw()
		d.say(fmt.Sprintf("Show hidden files %t", hidden))
	}
	d.button("Open", "o", open)
	if folderMode {
		d.button("Choose folder", "s", chooseFolder)
	}
	d.button("Up", "u", up)
	d.button("Home", "h", home)
	d.button("Hidden", ".", toggleHidden)
	d.button("Back", "Esc", d.close)
	d.keymap(func(e *tcell.EventKey) bool {
		if table.HasFocus() && (e.Key() == tcell.KeyBackspace || e.Key() == tcell.KeyBackspace2) {
			up()
			return true
		}
		switch d.actionKey(e) {
		case 'p':
			u.app.SetFocus(path)
		case '/', 'f':
			u.app.SetFocus(filter)
		case 'b':
			u.app.SetFocus(table)
		default:
			return false
		}
		return true
	})
	browse(picker.initial)
}
