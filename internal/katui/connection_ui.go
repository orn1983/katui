package katui

import (
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

func (u *ui) clustersDialog() {
	profiles, err := loadConnectionProfiles(u.b.backend)
	starting := len(u.b.brokers) == 0
	if starting {
		if name := u.b.profileName; name != "" && err == nil {
			u.connectionDialog(name, profiles[name], "")
			return
		}
		if len(profiles) == 0 {
			u.connectionDialog("", connectionProfile{}, errorText(err))
			return
		}
	}
	list := tview.NewList().SetMainTextColor(u.accent)
	d := u.dialog("Kafka clusters", list, list)
	if starting {
		d.cancel = u.app.Stop
	}
	newConnection := func() { u.connectionDialog("", connectionProfile{}, "") }
	for _, name := range sortedKeys(profiles) {
		profile := profiles[name]
		list.AddItem(tview.Escape(safe(name, false)), tview.Escape(safe(strings.Join(profile.Brokers, ", "), false)), 0, func() {
			u.connectionDialog(name, profile, "")
		})
	}
	open := func() {
		if i := list.GetCurrentItem(); i >= 0 && i < list.GetItemCount() {
			list.GetItemSelectedFunc(i)()
		}
	}
	openButton := d.button("Open", open)
	d.button("New n", newConnection)
	closeLabel := "Back Esc"
	if starting {
		closeLabel = "Quit Esc"
	}
	d.button(closeLabel, d.cancel)
	if err != nil {
		d.say(err.Error())
	} else if len(profiles) == 0 {
		d.say("No saved clusters. Choose New to add one.")
		u.app.SetFocus(d.buttons[1])
	} else if !starting {
		d.say("Open a profile to connect or edit it. Switching clusters stops the active reader.")
	}
	openButton.SetDisabled(len(profiles) == 0)
	d.keymap(func(e *tcell.EventKey) bool {
		if e.Key() == tcell.KeyCtrlC && starting {
			u.interrupted = true
			u.app.Stop()
			return true
		}
		if e.Key() != tcell.KeyRune || e.Modifiers() != 0 {
			return false
		}
		switch e.Rune() {
		case 'n':
			newConnection()
		case 'q':
			if starting {
				u.app.Stop()
			} else {
				u.quit()
			}
		default:
			return false
		}
		return true
	})
}

func (u *ui) connectionDialog(name string, saved connectionProfile, notice string) {
	starting := len(u.b.brokers) == 0
	cli := u.b.backend == "cli"
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
	brokers := tview.NewInputField().SetLabel("Brokers").SetText(strings.Join(saved.Brokers, ", "))
	profileName := tview.NewInputField().SetLabel("Profile name").SetText(name)
	save := tview.NewCheckbox().SetLabel("Save profile").SetChecked(name != "")
	remember := tview.NewCheckbox().SetLabel("Remember password")
	credentials := &securityConfig{}
	if saved.Security != nil {
		credentials = saved.Security
	}
	remember.SetChecked(credentials.SASL.Password != "")

	path := saved.ClientConfig
	if cli {
		path = saved.CommandConfig
	}
	if starting {
		if cli && u.b.commandConfig != "" {
			path = u.b.commandConfig
		}
		if !cli && u.b.config != "" {
			path = u.b.config
		}
	}
	configLabel := "Client config JSON"
	if cli {
		configLabel = "Kafka CLI properties"
	}
	config := tview.NewInputField().SetLabel(configLabel).SetText(path)
	ca := tview.NewInputField().SetLabel("CA certificate").SetText(credentials.CAFile)
	cert := tview.NewInputField().SetLabel("Client certificate").SetText(credentials.CertFile)
	key := tview.NewInputField().SetLabel("Client private key").SetText(credentials.KeyFile)
	serverName := tview.NewInputField().SetLabel("TLS server name").SetText(credentials.ServerName)
	username := tview.NewInputField().SetLabel("Username").SetText(credentials.SASL.Username)
	password := tview.NewInputField().SetLabel("Password").SetMaskCharacter('•').SetText(credentials.SASL.Password)
	mechanisms := []string{"SCRAM-SHA-256", "SCRAM-SHA-512", "PLAIN"}
	mechanism := tview.NewDropDown().SetLabel("SASL mechanism").SetOptions(mechanisms, nil).
		SetCurrentOption(indexOf(mechanisms, strings.ToUpper(credentials.SASL.Mechanism)))
	mode := connectionPlain
	switch {
	case path != "":
		mode = connectionFile
	case credentials.SASL.Mechanism != "" && credentials.TLS:
		mode = connectionSASLTLS
	case credentials.SASL.Mechanism != "":
		mode = connectionSASLPlain
	case credentials.TLS || credentials.CAFile != "" || credentials.CertFile != "" || credentials.ServerName != "":
		mode = connectionTLS
	}
	protocol := tview.NewDropDown().SetLabel("Security").SetOptions([]string{
		"Plaintext", "TLS", "SASL + TLS", "SASL plaintext", "Client config file",
	}, nil).SetCurrentOption(mode)

	checking := false
	var connectButton *tview.Button
	connect := func() {
		if checking {
			return
		}
		opts := u.b.options
		opts.config, opts.commandConfig, opts.security = "", "", nil
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
		switch {
		case cli:
			opts.commandConfig, err = filePath(config)
		case mode == connectionFile:
			opts.config, err = filePath(config)
			if err == nil && opts.config == "" {
				d.say("Choose a Kafka client config JSON file.")
				return
			}
		case mode != connectionPlain:
			security := &securityConfig{TLS: mode == connectionTLS || mode == connectionSASLTLS}
			if security.TLS {
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
		candidate := &broker{options: opts}
		checking = true
		connectButton.SetDisabled(true)
		d.say("Checking connection settings…  Esc Cancel · q Quit")
		go func() {
			openErr := candidate.open()
			if !u.post(func() {
				if d.closed {
					candidate.close()
					return
				}
				checking = false
				connectButton.SetDisabled(false)
				if openErr != nil {
					d.say(openErr.Error())
					return
				}
				if storeProfile {
					profiles, err := loadConnectionProfiles(opts.backend)
					if err == nil && opts.profileName != name {
						if _, exists := profiles[opts.profileName]; exists {
							err = fmt.Errorf("a profile named %q already exists; open it from Clusters to edit it", opts.profileName)
						}
					}
					if err == nil {
						if name != "" && name != opts.profileName {
							delete(profiles, name)
						}
						profiles[opts.profileName] = opts.connectionProfile(rememberPassword)
						err = saveConnectionProfiles(opts.backend, profiles)
					}
					if err != nil {
						candidate.close()
						d.say("Saving profile: " + err.Error() + ". Uncheck Save profile to connect once.")
						return
					}
				}
				u.nextBroker = candidate
				u.app.Stop()
			}) {
				candidate.close()
			}
		}()
	}
	connectButton = d.button("Connect Alt+G", connect)
	cancelLabel := "Cancel Esc"
	if starting && u.modalDepth == 1 {
		cancelLabel = "Quit Esc"
	}
	cancelButton := d.button(cancelLabel, d.cancel)
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
	browseConfig := d.button("Browse file", browse(config, "Choose Kafka connection settings"))
	browseCA := d.button("Browse CA", browse(ca, "Choose CA certificate"))
	browseCert := d.button("Browse cert", browse(cert, "Choose client certificate"))
	browseKey := d.button("Browse key", browse(key, "Choose client private key"))

	render := func() {
		focused, _ := form.GetFocusedItemIndex()
		usesTLS := mode == connectionTLS || mode == connectionSASLTLS
		usesSASL := mode == connectionSASLTLS || mode == connectionSASLPlain
		form.Clear(false).AddFormItem(brokers)
		if !cli {
			form.AddFormItem(protocol)
		}
		if cli || mode == connectionFile {
			form.AddFormItem(config)
		}
		if usesTLS {
			form.AddFormItem(ca).AddFormItem(cert).AddFormItem(key).AddFormItem(serverName)
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
		if usesSASL && save.IsChecked() {
			form.AddFormItem(remember)
		}
		d.footer.Clear()
		for _, action := range []struct {
			button  *tview.Button
			visible bool
		}{
			{connectButton, true}, {cancelButton, true}, {browseConfig, cli || mode == connectionFile},
			{browseCA, usesTLS}, {browseCert, usesTLS}, {browseKey, usesTLS},
		} {
			action.button.SetDisabled(!action.visible || checking && action.button != cancelButton)
			if action.visible {
				d.footer.AddItem(action.button, len(action.button.GetLabel())+2, 0, false)
			}
		}
		if focused >= 0 {
			focusForm(u, form, focused)
		}
	}
	protocol.SetSelectedFunc(func(_ string, selected int) { mode = selected; render() })
	save.SetChangedFunc(func(on bool) {
		render()
		if on {
			d.say("Saved profiles are local. Remember password stores it in the profile file.")
		} else {
			d.say("Connect once without saving.")
		}
	})
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
		switch alt(e) {
		case 'g':
			connect()
		case 'b':
			if cli || mode == connectionFile {
				browse(config, "Choose Kafka connection settings")()
			}
		case 'c':
			if mode == connectionTLS || mode == connectionSASLTLS {
				browse(ca, "Choose CA certificate")()
			}
		case 'e':
			if mode == connectionTLS || mode == connectionSASLTLS {
				browse(cert, "Choose client certificate")()
			}
		case 'k':
			if mode == connectionTLS || mode == connectionSASLTLS {
				browse(key, "Choose client private key")()
			}
		default:
			return false
		}
		return true
	})
	render()
	u.app.SetFocus(form)
	if notice == "" {
		notice = "Enter host:port or comma-separated brokers. Tab moves between fields."
	}
	d.say(notice)
}
