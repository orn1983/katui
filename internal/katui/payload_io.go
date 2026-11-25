package katui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Keep the encoded OSC 52 sequence below common terminal limits.
const terminalClipboardBytes = 64 << 10

func deliverClipboard(ctx context.Context, text, localCommand string, terminal func([]byte) bool) (string, error) {
	if localCommand != "" {
		local, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(local, localCommand)
		cmd.Stdin = strings.NewReader(text)
		err := cmd.Run()
		cancel()
		if err == nil {
			return "Copied to the macOS clipboard", nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(text) > terminalClipboardBytes {
		return "", errors.New("terminal clipboard is limited to 64 KiB. Select less text or use Export")
	}
	if terminal == nil || !terminal([]byte(text)) {
		return "", errors.New("clipboard unavailable")
	}
	return "Sent a terminal clipboard request (OSC 52)", nil
}

func (p *payloadPane) copyText(text string) {
	if text == "" {
		p.notice("Select text with Space then v, Shift+arrows, or a mouse drag")
		return
	}
	go p.sendClipboard(text)
}

func (p *payloadPane) sendClipboard(text string) {
	local := ""
	if runtime.GOOS == "darwin" && os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_CLIENT") == "" && os.Getenv("SSH_TTY") == "" {
		local = "/usr/bin/pbcopy"
	}
	result, err := deliverClipboard(p.u.ctx, text, local, func(data []byte) bool {
		return p.u.post(func() { p.u.screen.SetClipboard(data) })
	})
	p.u.post(func() {
		if err != nil {
			p.notice(err.Error())
		} else {
			p.notice(result)
		}
	})
}

func (p *payloadPane) copyValue() {
	if len(p.records) == 0 {
		p.notice("Select a record first")
		return
	}
	records := append([]*storedRecord(nil), p.records...)
	release := p.u.holdPayloads(records)
	p.notice("Reading complete value for clipboard…")
	go func() {
		text, err := completeValueText(p.u.ctx, records)
		p.u.post(release)
		if err != nil {
			p.u.post(func() { p.notice(err.Error()) })
			return
		}
		p.sendClipboard(text)
	}()
}

func completeValueText(ctx context.Context, records []*storedRecord) (string, error) {
	out := renderText{limit: maxInspectionBytes}
	for i, r := range records {
		if !r.fullAvailable() {
			return "", fmt.Errorf("%s: %w", r.id, errPayloadQuota)
		}
		if i > 0 {
			out.WriteString("\n\n")
		}
		if r.valueSize > out.limit-out.builder.Len() {
			return "", errors.New("complete value exceeds the 128 MiB clipboard input limit. Use Export")
		}
		value, limited, err := renderValue(ctx, r, r.valueSize)
		if err != nil {
			return "", fmt.Errorf("%s: %w", r.id, err)
		}
		if limited {
			return "", errors.New("retained payload changed while reading")
		}
		if value == nil {
			out.WriteString("<tombstone>")
		} else {
			if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 {
				return "", fmt.Errorf("%s: value is binary. Use Export for the original bytes", r.id)
			}
			out.Write(value)
		}
		if out.limited {
			return "", errors.New("complete value exceeds the 128 MiB clipboard limit. Use Export")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func (p *payloadPane) copyView() {
	if p.rendering && !p.metadataTab {
		p.notice("Payload is still being prepared")
		return
	}
	if !p.metadataTab && p.result.incomplete {
		p.notice("Rendered view is incomplete. Use Load full, Copy Value, or select the displayed text.")
		return
	}
	text := p.result.text
	if p.metadataTab {
		text = p.viewer.doc.text
	}
	go p.sendClipboard(text)
}

func (u *ui) holdPayloads(records []*storedRecord) func() {
	if u.payloadJobs == nil {
		u.payloadJobs = map[*storedRecord]int{}
	}
	for _, r := range records {
		u.payloadJobs[r]++
	}
	return func() {
		for _, r := range records {
			u.payloadJobs[r]--
			if u.payloadJobs[r] == 0 {
				delete(u.payloadJobs, r)
				u.release(r)
			}
		}
	}
}

func exportRecord(ctx context.Context, r *storedRecord, path string) error {
	src, err := r.openFull()
	if err != nil {
		return err
	}
	defer src.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = io.CopyN(dst, contextReader{ctx, src}, int64(r.valueSize))
	err = errors.Join(err, dst.Close(), ctx.Err())
	if err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

func (p *payloadPane) exportValue() {
	if len(p.records) == 0 {
		p.notice("Select a record first")
		return
	}
	records := append([]*storedRecord(nil), p.records...)
	release := p.u.holdPayloads(records)
	form := tview.NewForm().SetItemPadding(0)
	var choice *tview.DropDown
	if len(records) > 1 {
		labels := make([]string, 0, len(records))
		for _, r := range records {
			labels = append(labels, tview.Escape(safe(r.id.String(), false))+" · "+payloadAvailability(r))
		}
		choice = dropdown(form, "Record", labels, 0, nil)
	}
	path := field(form, "New file", "")
	d := p.u.dialog("Export complete value", form, form)
	d.hint("Type a new file path.  " + keyHint("Enter") + " Save")
	var cancel context.CancelFunc
	var exportButton *tview.Button
	writing := false
	close := func() {
		if writing {
			cancel()
			d.say("Canceling export…")
			return
		}
		release()
		d.close()
	}
	d.cancel = close
	export := func() {
		if writing {
			return
		}
		name := expandHome(strings.TrimSpace(path.GetText()))
		if name == "" {
			d.say("Enter an output path")
			return
		}
		i := 0
		if choice != nil {
			i, _ = choice.GetCurrentOption()
		}
		if i < 0 || i >= len(records) {
			return
		}
		r := records[i]
		if !r.fullAvailable() {
			d.say(errPayloadQuota.Error())
			return
		}
		var ctx context.Context
		ctx, cancel = context.WithCancel(p.u.ctx)
		writing = true
		exportButton.SetDisabled(true)
		d.hint("Writing original bytes…  " + keyHint("Esc") + " Cancel")
		go func() {
			err := exportRecord(ctx, r, name)
			cancel()
			p.u.post(func() {
				writing = false
				exportButton.SetDisabled(false)
				if err != nil {
					d.say(err.Error())
					return
				}
				close()
				p.notice("Exported complete payload to " + name)
			})
		}()
	}
	exportButton = d.button("Save", "s", export)
	d.button("Back", "Esc", close)
	d.keymap(func(e *tcell.EventKey) bool {
		if e.Key() == tcell.KeyEnter && path.HasFocus() || alt(e) == 'e' {
			export()
			return true
		}
		return false
	})
}
