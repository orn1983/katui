package katui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Measure an arrow press through the application event loop up to its drawn
// frame, including selection, table layout and the visible inspector.
func BenchmarkBufferedMessageScroll(b *testing.B) {
	for _, layout := range []string{"split", "zoom"} {
		b.Run(layout, func(b *testing.B) {
			backend := &broker{options: options{noCache: true, interval: time.Hour}}
			u := newUI(b.Context(), backend)
			// The Messages workspace needs an active connection, but browsing
			// already buffered records does not issue any broker requests.
			backend.brokers = []string{"benchmark"}
			u.workspace = 2
			u.readTopic = "events"
			payload := []byte(strings.Repeat("example record value ", 75))
			for i := range 2000 {
				u.buffer(&storedRecord{id: recordID{partKey{u.readTopic, 0}, int64(i)}, value: payload, valueSize: len(payload), cost: len(payload) + 128})
			}

			screen := tcell.NewSimulationScreen("UTF-8")
			exit := make(chan error, 1)
			go func() { exit <- u.runOnScreen(screen) }()
			defer func() {
				u.app.Stop()
				if err := <-exit; err != nil {
					b.Error(err)
				}
			}()

			drawn := make(chan int, 1)
			u.post(func() { screen.SetSize(120, 40) })
			u.post(func() {
				if layout == "zoom" {
					u.keys(tcell.NewEventKey(tcell.KeyRune, 'z', 0))
				}
				last := u.current().row
				u.app.SetAfterDrawFunc(func(tcell.Screen) {
					if row := u.current().row; row != last {
						last = row
						drawn <- row
					}
				})
			})

			row, step := 0, 1
			for b.Loop() {
				key := tcell.KeyDown
				if step < 0 {
					key = tcell.KeyUp
				}
				screen.InjectKey(key, 0, 0)
				row += step
				if got := <-drawn; got != row {
					b.Fatalf("drew row %d after arrow, want %d", got, row)
				}
				if row == 1999 || row == 0 {
					step = -step
				}
			}
		})
	}
}
