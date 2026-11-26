package katui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	connectionPlain = iota
	connectionTLS
	connectionSASLTLS
	connectionSASLPlain
	connectionFile
)

type clusterPicker struct {
	*dialog
	cancelAttempt context.CancelFunc
}

func (u *ui) showClusters() {
	if u.workspace == clustersWorkspace {
		return
	}
	starting := len(u.b.brokers) == 0
	profiles, err := loadConnectionProfiles()
	if starting && len(profiles) == 0 {
		u.connectionDialog("", connectionProfile{}, errorText(err))
		return
	}

	names := sortedKeys(profiles)
	list := u.newList().SetHighlightFullLine(true).
		SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(u.accent).Bold(true))
	var d *dialog
	if starting {
		d = u.dialog("Kafka clusters", list, list)
	} else {
		d = u.newDialog("Kafka clusters", list)
	}
	picker := &clusterPicker{dialog: d}
	k := keyHint
	idleHint := k("↓/j") + " " + k("↑/k") + " Select  ·  " + k("c") + " Connect  ·  " + k("q") + " Quit"
	if !starting {
		idleHint += "\nSwitching clusters stops the active reader."
	}

	newConnection := func() { u.connectionDialog("", connectionProfile{}, "") }
	var connectButton, editButton, newButton *tview.Button
	setChecking := func(checking bool) {
		connectButton.SetDisabled(checking || len(names) == 0)
		editButton.SetDisabled(checking || len(names) == 0)
		newButton.SetDisabled(checking)
	}
	selected := func() (string, connectionProfile) {
		if i := list.GetCurrentItem(); i >= 0 && i < len(names) {
			return names[i], profiles[names[i]]
		}
		return "", connectionProfile{}
	}
	edit := func() {
		if name, profile := selected(); name != "" && picker.cancelAttempt == nil {
			u.connectionDialog(name, profile, "")
		}
	}
	connect := func() {
		name, profile := selected()
		if name == "" || picker.cancelAttempt != nil {
			return
		}
		opts := u.b.options
		opts.profileName = name
		if !starting {
			opts.config, opts.security, opts.sizeConfig = "", nil, ""
		}
		if err := opts.selectConnection(profile); err != nil {
			if errors.Is(err, errProfilePassword) && profile.Security != nil {
				u.clusterPassword(opts, profile)
				return
			}
			d.say(err.Error())
			return
		}
		ctx, cancel := context.WithCancel(u.ctx)
		picker.cancelAttempt = cancel
		setChecking(true)
		d.hint("Checking " + tview.Escape(safe(name, false)) + "…  " + k("Esc") + " Cancel")
		u.openConnection(d, ctx, opts, func(candidate *broker, err error) {
			cancel()
			picker.cancelAttempt = nil
			setChecking(false)
			if err != nil {
				d.hint(tview.Escape(safe(err.Error(), false)) + "\n" + idleHint)
				u.app.SetFocus(list)
				return
			}
			u.nextBroker = candidate
			u.app.Stop()
		})
	}
	for _, name := range names {
		profile := profiles[name]
		label := safe(name, false)
		if !starting && name == u.b.profileName {
			label += " · current"
		}
		description := safe(strings.Join(profile.Brokers, ", "), false)
		if profile.PromptPassword {
			description += " · password required"
		}
		list.AddItem(tview.Escape(label), tview.Escape(description), 0, nil)
		if name == u.b.profileName {
			list.SetCurrentItem(list.GetItemCount() - 1)
		}
	}
	d.cancel = func() {
		if picker.cancelAttempt != nil {
			picker.cancelAttempt()
			picker.cancelAttempt = nil
			setChecking(false)
			d.hint(idleHint)
			u.app.SetFocus(list)
			return
		}
		if starting {
			u.app.Stop()
		} else {
			u.changeWorkspace(u.workspaceBeforeClusters)
		}
	}
	connectButton = d.button("Connect", "Enter", connect)
	editButton = d.button("Edit", "e", edit)
	newButton = d.button("New", "n", newConnection)
	closeLabel := "Back"
	if starting {
		closeLabel = "Quit"
	}
	d.button(closeLabel, "Esc", d.cancel)
	setChecking(false)
	switch {
	case err != nil:
		d.say(err.Error())
	case len(names) == 0:
		d.hint("No saved clusters. " + k("n") + " New connection")
	default:
		d.hint(idleHint)
	}
	d.keymap(func(e *tcell.EventKey) bool {
		if e.Key() == tcell.KeyCtrlC && starting {
			u.interrupted = true
			u.app.Stop()
			return true
		}
		if e.Key() == tcell.KeyRune && e.Rune() == 'q' && e.Modifiers() == 0 {
			if starting {
				u.app.Stop()
			} else {
				u.quit()
			}
			return true
		}
		if picker.cancelAttempt != nil {
			return true
		}
		if e.Key() == tcell.KeyEnter && list.HasFocus() {
			connect()
			return true
		}
		if (e.Key() == tcell.KeyUp || e.Key() == tcell.KeyDown) && !list.HasFocus() {
			u.app.SetFocus(list)
			return false
		}
		if e.Key() != tcell.KeyRune || e.Modifiers() != 0 {
			return false
		}
		switch e.Rune() {
		case 'c':
			connect()
		default:
			return false
		}
		return true
	})
	if !starting {
		u.savePosition()
		u.workspaceBeforeClusters = u.workspace
		u.workspace = clustersWorkspace
		u.clusters = picker
		u.showLocation()
		if len(names) == 0 {
			u.app.SetFocus(newButton)
		}
	}
	if _, exists := profiles[u.b.profileName]; starting && u.b.profileName != "" && exists {
		// An explicitly requested profile reaches the picker only when it
		// needs interactive credentials.
		connect()
	}
}

func (u *ui) clusterPassword(opts options, profile connectionProfile) {
	password := tview.NewInputField().SetLabel("Password ").SetMaskCharacter('•')
	body := tview.NewFlex().SetDirection(tview.FlexRow)
	title := "Connect to " + opts.profileName
	username := tview.NewTextView().SetText("SASL username: " + safe(profile.Security.SASL.Username, false))
	body.AddItem(username, 1, 0, false).AddItem(password, 1, 0, true)
	d := u.dialog(title, body, password)
	d.hint("Enter the password for this session.")
	ctx, cancel := context.WithCancel(u.ctx)
	d.cancel = func() {
		cancel()
		d.close()
	}
	var button *tview.Button
	connect := func() {
		if button.IsDisabled() || password.GetText() == "" {
			return
		}
		security := *profile.Security
		security.SASL.Password = password.GetText()
		profile.Security, profile.PromptPassword = &security, false
		if err := opts.selectConnection(profile); err != nil {
			d.say(err.Error())
			return
		}
		button.SetDisabled(true)
		d.hint("Checking connection settings…  " + keyHint("Esc") + " Cancel")
		u.openConnection(d, ctx, opts, func(candidate *broker, err error) {
			if err != nil {
				button.SetDisabled(false)
				d.say(err.Error())
				return
			}
			u.nextBroker = candidate
			u.app.Stop()
		})
	}
	button = d.button("Connect", "Enter", connect)
	d.button("Back", "Esc", d.cancel)
	d.keymap(func(e *tcell.EventKey) bool {
		if e.Key() == tcell.KeyEnter && password.HasFocus() {
			connect()
			return true
		}
		return false
	})
}

func (u *ui) openConnection(d *dialog, ctx context.Context, opts options, done func(*broker, error)) {
	go func() {
		candidate := &broker{options: opts}
		err := candidate.open()
		if !u.post(func() {
			if d.closed || ctx.Err() != nil {
				candidate.close()
				return
			}
			if err != nil {
				candidate.close()
			}
			done(candidate, err)
		}) {
			candidate.close()
		}
	}()
}

func (u *ui) connectionDialog(name string, saved connectionProfile, notice string) {
	starting := len(u.b.brokers) == 0
	form := tview.NewForm().SetItemPadding(0)
	title := "Connect to Kafka"
	if name != "" {
		title += " · " + name
	}
	d := u.dialog(title, form, form)
	stop := func() {
		d.closed = true
		u.app.Stop()
	}
	if starting && u.modalDepth == 1 {
		d.cancel = stop
	}
	brokers := tview.NewInputField().SetLabel(buttonLabel("Brokers", "Alt+A"))
	profileName := tview.NewInputField().SetLabel(buttonLabel("Profile name", "Alt+N")).SetText(name)
	save := tview.NewCheckbox().SetLabel(buttonLabel("Save profile", "Alt+V")).SetChecked(name != "")
	remember := tview.NewCheckbox().SetLabel(buttonLabel("Remember password", "Alt+Y"))
	config := tview.NewInputField().SetLabel("Client config JSON")
	ca := tview.NewInputField().SetLabel("CA certificate")
	caOnly := tview.NewCheckbox().SetLabel(buttonLabel("Trust only this CA file", "Alt+O"))
	cert := tview.NewInputField().SetLabel("Client certificate")
	key := tview.NewInputField().SetLabel("Client private key")
	serverName := tview.NewInputField().SetLabel(buttonLabel("TLS server name", "Alt+H"))
	username := tview.NewInputField().SetLabel(buttonLabel("Username", "Alt+U"))
	passwordLabel := buttonLabel("Password", "Alt+P")
	var password *tview.InputField
	mechanisms := []string{"SCRAM-SHA-256", "SCRAM-SHA-512", "PLAIN"}
	mechanism := tview.NewDropDown().SetLabel(buttonLabel("SASL mechanism", "Alt+M"))
	mode := connectionPlain
	protocol := tview.NewDropDown().SetLabel(buttonLabel("Security", "Alt+S")).SetOptions([]string{
		"Plaintext", "TLS", "SASL + TLS", "SASL plaintext", "Client config file",
	}, nil)

	fillConnection := func(profile connectionProfile, rememberPassword bool) {
		security := profile.Security
		if security == nil {
			security = &securityConfig{}
		}
		if len(profile.Brokers) > 0 {
			brokers.SetText(strings.Join(profile.Brokers, ", "))
		}
		config.SetText(profile.ClientConfig)
		ca.SetText(security.CAFile)
		caOnly.SetChecked(security.CAFileOnly)
		cert.SetText(security.CertFile)
		key.SetText(security.KeyFile)
		serverName.SetText(security.ServerName)
		username.SetText(security.SASL.Username)
		// A fresh masked field also drops the previous credential's undo history.
		password = tview.NewInputField().SetLabel(passwordLabel).SetText(security.SASL.Password).SetMaskCharacter('•')
		remember.SetChecked(rememberPassword && security.SASL.Password != "")

		tls := security.TLS || security.CAFile != "" || security.CAFileOnly || security.CertFile != "" || security.KeyFile != "" || security.ServerName != ""
		switch {
		case profile.ClientConfig != "":
			mode = connectionFile
		case security.SASL.Mechanism != "" && tls:
			mode = connectionSASLTLS
		case security.SASL.Mechanism != "":
			mode = connectionSASLPlain
		case tls:
			mode = connectionTLS
		default:
			mode = connectionPlain
		}
		mechanism.SetOptions(mechanisms, nil).SetCurrentOption(indexOf(mechanisms, strings.ToUpper(security.SASL.Mechanism)))
		protocol.SetCurrentOption(mode)
	}

	checking := false
	var connectButton *tview.Button
	connect := func() {
		if checking {
			return
		}
		opts := u.b.options
		opts.config, opts.security = "", nil
		if !starting {
			opts.sizeConfig = ""
		}
		if err := opts.setBrokers(brokers.GetText()); err != nil {
			d.say(err.Error())
			return
		}
		opts.profileName = strings.TrimSpace(profileName.GetText())
		storeProfile := save.IsChecked()
		if storeProfile && opts.profileName == "" {
			d.say("Enter a profile name to save this connection.")
			return
		}
		filePath := func(input *tview.InputField) (string, error) {
			value := strings.TrimSpace(input.GetText())
			if value == "" {
				return "", nil
			}
			return filepath.Abs(expandHome(value))
		}
		var err error
		switch mode {
		case connectionFile:
			opts.config, err = filePath(config)
			if err == nil && opts.config == "" {
				d.say("Choose a Kafka client config JSON file.")
				return
			}
		case connectionTLS, connectionSASLTLS, connectionSASLPlain:
			security := &securityConfig{TLS: mode == connectionTLS || mode == connectionSASLTLS}
			if security.TLS {
				security.CAFileOnly = caOnly.IsChecked()
				if security.CAFile, err = filePath(ca); err == nil {
					if security.CertFile, err = filePath(cert); err == nil {
						security.KeyFile, err = filePath(key)
					}
				}
				security.ServerName = strings.TrimSpace(serverName.GetText())
			}
			if mode == connectionSASLTLS || mode == connectionSASLPlain {
				_, security.SASL.Mechanism = mechanism.GetCurrentOption()
				security.SASL.Username, security.SASL.Password = username.GetText(), password.GetText()
				if security.SASL.Username == "" || security.SASL.Password == "" {
					d.say("Enter the SASL username and password.")
					return
				}
			}
			opts.security = security
		}
		if err != nil {
			d.say(err.Error())
			return
		}
		opts.setCachePolicy()
		rememberPassword := remember.IsChecked()
		checking = true
		connectButton.SetDisabled(true)
		d.hint("Checking connection settings…  " + keyHint("Esc") + " Cancel · " + keyHint("q") + " Quit")
		u.openConnection(d, u.ctx, opts, func(candidate *broker, openErr error) {
			checking = false
			connectButton.SetDisabled(false)
			if openErr != nil {
				d.say(openErr.Error())
				return
			}
			if storeProfile {
				profiles, err := loadConnectionProfiles()
				if err == nil && opts.profileName != name {
					if _, exists := profiles[opts.profileName]; exists {
						err = fmt.Errorf("a profile named %q already exists. Select it in Clusters and press e to edit it", opts.profileName)
					}
				}
				if err == nil {
					if name != "" && name != opts.profileName {
						delete(profiles, name)
					}
					profiles[opts.profileName] = opts.connectionProfile(rememberPassword)
					err = saveConnectionProfiles(profiles)
				}
				if err != nil {
					candidate.close()
					d.say("Saving profile: " + err.Error() + ". Uncheck Save profile to connect once.")
					return
				}
			}
			u.nextBroker = candidate
			u.app.Stop()
		})
	}
	connectButton = d.button("Connect", "c", connect)
	cancelLabel := "Cancel"
	if starting && u.modalDepth == 1 {
		cancelLabel = "Quit"
	}
	cancelButton := d.button(cancelLabel, "Esc", d.cancel)
	importProperties := d.button("Import Kafka properties…", "Alt+I", func() {
		if checking {
			return
		}
		u.fileBrowser(filePicker{title: "Import Kafka properties", accept: func(string) bool { return true }}, func(path string) {
			profile, err := readKafkaProperties(path)
			if err != nil {
				d.say("Import failed: " + err.Error())
				return
			}
			fillConnection(profile, false)
			focusForm(u, form, 0)
			d.say("Properties imported. Review the settings and choose Connect when ready.")
		})
	})
	browse := func(input *tview.InputField, title string) func() {
		return func() {
			if checking {
				return
			}
			u.fileBrowser(filePicker{initial: input.GetText(), title: title, accept: func(string) bool { return true }}, func(path string) {
				input.SetText(path)
				focusForm(u, form, form.GetFormItemIndex(input.GetLabel()))
			})
		}
	}
	browseConfig := d.button("Browse file", "b", browse(config, "Choose Kafka connection settings"))
	browseCA := d.button("Pick CA", "t", browse(ca, "Choose CA certificate"))
	browseCert := d.button("Pick cert", "e", browse(cert, "Choose client certificate"))
	browseKey := d.button("Pick key", "k", browse(key, "Choose client private key"))

	render := func(saveProfile bool) {
		focused, _ := form.GetFocusedItemIndex()
		usesTLS := mode == connectionTLS || mode == connectionSASLTLS
		usesSASL := mode == connectionSASLTLS || mode == connectionSASLPlain
		form.Clear(false).AddFormItem(brokers).AddFormItem(protocol)
		if mode == connectionFile {
			form.AddFormItem(config)
		}
		if usesTLS {
			form.AddFormItem(ca).AddFormItem(caOnly).AddFormItem(cert).AddFormItem(key).AddFormItem(serverName)
		}
		if usesSASL {
			selection, _ := mechanism.GetCurrentOption()
			count := len(mechanisms)
			if mode == connectionSASLPlain {
				count--
			}
			mechanism.SetOptions(mechanisms[:count], nil).SetCurrentOption(min(selection, count-1))
			form.AddFormItem(mechanism).AddFormItem(username).AddFormItem(password)
		}
		form.AddFormItem(profileName).AddFormItem(save)
		if usesSASL && saveProfile {
			form.AddFormItem(remember)
		}
		d.footer.Clear().SetDirection(tview.FlexRow)
		primary, files := tview.NewFlex(), tview.NewFlex()
		d.footer.AddItem(primary, 1, 0, false)
		if usesTLS || mode == connectionFile {
			d.footer.AddItem(files, 1, 0, false)
		}
		d.root.ResizeItem(d.footer, d.footer.GetItemCount(), 0)
		for _, action := range []struct {
			button  *tview.Button
			visible bool
			row     *tview.Flex
		}{
			{connectButton, true, primary}, {cancelButton, true, primary}, {importProperties, true, primary},
			{browseConfig, mode == connectionFile, files}, {browseCA, usesTLS, files}, {browseCert, usesTLS, files}, {browseKey, usesTLS, files},
		} {
			action.button.SetDisabled(!action.visible || checking && action.button != cancelButton)
			if action.visible {
				action.row.AddItem(action.button, tview.TaggedStringWidth(action.button.GetLabel())+2, 0, false)
			}
		}
		if focused >= 0 {
			focusForm(u, form, focused)
		}
	}
	protocol.SetSelectedFunc(func(_ string, selected int) { mode = selected; render(save.IsChecked()) })
	save.SetChangedFunc(func(on bool) {
		render(on)
		if on {
			d.say("Saved profiles are local. Remember password stores it in the profile file.")
		} else {
			d.say("Connect once without saving.")
		}
	})
	fieldShortcuts := map[rune]string{
		'a': brokers.GetLabel(), 's': protocol.GetLabel(), 'n': profileName.GetLabel(), 'h': serverName.GetLabel(),
		'm': mechanism.GetLabel(), 'u': username.GetLabel(), 'p': passwordLabel,
	}
	d.keymap(func(e *tcell.EventKey) bool {
		if e.Key() == tcell.KeyCtrlC {
			u.interrupted = true
			stop()
			return true
		}
		if e.Key() == tcell.KeyRune && e.Rune() == 'q' && e.Modifiers() == 0 && (checking || !isEditing(u.app.GetFocus())) {
			if starting {
				stop()
			} else {
				d.close()
				u.quit()
			}
			return true
		}
		if checking {
			return true
		}
		shortcut := alt(e)
		if label, ok := fieldShortcuts[shortcut]; ok {
			focusForm(u, form, form.GetFormItemIndex(label))
			return true
		}
		switch shortcut {
		case 'g':
			connect()
		case 'o':
			if form.GetFormItemIndex(caOnly.GetLabel()) >= 0 {
				caOnly.SetChecked(!caOnly.IsChecked())
			}
		case 'v':
			save.SetChecked(!save.IsChecked())
		case 'y':
			if form.GetFormItemIndex(remember.GetLabel()) >= 0 {
				remember.SetChecked(!remember.IsChecked())
			}
		default:
			return false
		}
		return true
	})
	if starting && u.b.config != "" {
		saved.ClientConfig = u.b.config
	}
	fillConnection(saved, true)
	u.app.SetFocus(form)
	if notice == "" {
		d.hint("Enter host:port or comma-separated brokers.\n" + keyHint("Tab") + " Next field  " + keyHint("Alt+C") + " Connect while typing")
	} else {
		d.say(notice)
	}
}
