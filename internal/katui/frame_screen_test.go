package katui

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

type observedTerminal struct {
	tcell.SimulationScreen
	initErr                      error
	starts, stops, writes, syncs int
	mouse, paste                 bool
	clipboard                    string
}

func (s *observedTerminal) Init() error {
	s.starts++
	if s.initErr != nil {
		return s.initErr
	}
	return s.SimulationScreen.Init()
}
func (s *observedTerminal) Fini() {
	s.stops++
	s.SimulationScreen.Fini()
}
func (s *observedTerminal) SetContent(x, y int, ch rune, combining []rune, style tcell.Style) {
	s.writes++
	s.SimulationScreen.SetContent(x, y, ch, combining, style)
}
func (s *observedTerminal) Sync() {
	s.syncs++
	s.SimulationScreen.Sync()
}
func (s *observedTerminal) EnableMouse(flags ...tcell.MouseFlags) {
	s.mouse = true
	s.SimulationScreen.EnableMouse(flags...)
}
func (s *observedTerminal) EnablePaste() {
	s.paste = true
	s.SimulationScreen.EnablePaste()
}
func (s *observedTerminal) SetClipboard(data []byte) {
	s.clipboard = string(data)
}

func TestFrameScreenMatchesDirectDrawing(t *testing.T) {
	direct := tcell.NewSimulationScreen("UTF-8")
	terminal := &observedTerminal{SimulationScreen: tcell.NewSimulationScreen("UTF-8")}
	frame := &frameScreen{Screen: terminal}
	for _, screen := range []tcell.Screen{direct, frame} {
		if err := screen.Init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(screen.Fini)
		screen.SetSize(7, 2)
	}
	style := tcell.StyleDefault.Foreground(tcell.ColorDodgerBlue)
	draw := func(f func(tcell.Screen)) {
		t.Helper()
		for _, s := range []tcell.Screen{direct, frame} {
			f(s)
			s.Show()
		}
		want, wantW, wantH := direct.GetContents()
		got, w, h := terminal.GetContents()
		if w != wantW || h != wantH || !reflect.DeepEqual(got, want) {
			t.Fatalf("buffered and direct screens differ at %dx%d", w, h)
		}
	}
	paint := func(s tcell.Screen) {
		s.Clear()
		s.SetCell(1, 0, style, 'e', '\u0301')
		s.SetCell(3, 0, style, '界')
		s.SetCell(6, 0, style, '界') // Clipped at the right edge.
		s.ShowCursor(1, 1)
	}
	draw(func(s tcell.Screen) { s.Fill('.', tcell.StyleDefault) })
	draw(paint)
	terminal.writes = 0
	draw(paint)
	if terminal.writes != 0 {
		t.Errorf("identical final frame rewrote %d terminal cells", terminal.writes)
	}
	if x, y, visible := terminal.GetCursor(); !visible || x != 1 || y != 1 {
		t.Errorf("cursor at (%d,%d), visible=%v", x, y, visible)
	}
	draw(func(s tcell.Screen) {
		s.SetCell(1, 0, style, 'e', '\u0300')
		s.SetCell(3, 0, style, 'a')
		s.SetCell(4, 0, style, 'b') // Exposes the previous wide glyph's trailing cell.
		s.SetCell(6, 0, style, 'x')
	})
	if terminal.writes != 4 {
		t.Errorf("changed four cells, forwarded %d", terminal.writes)
	}
	draw(func(s tcell.Screen) {
		s.SetSize(10, 3)
		s.Clear()
		s.SetCell(9, 2, style, 'x')
	})
	draw(func(s tcell.Screen) { s.SetSize(4, 1); s.Fill('*', style) })
	// A real resync must repair externally corrupted output even when the
	// desired frame and the terminal's logical cell buffer are identical.
	front, _, _ := terminal.GetContents()
	front[0].Runes = []rune{'?'}
	frame.Sync()
	if terminal.syncs != 1 || string(front[0].Runes) != "*" {
		t.Fatal("Sync did not restore the physical screen")
	}
}

func TestFrameScreenLifecycle(t *testing.T) {
	backend := &broker{options: options{backend: "api", noCache: true, interval: time.Hour}}
	u := newUI(t.Context(), backend)
	bad := &observedTerminal{initErr: errors.New("terminal setup failed")}
	if err := u.runOnScreen(bad); !errors.Is(err, bad.initErr) || bad.starts != 1 || bad.stops != 0 {
		t.Fatalf("failed terminal initialization: err=%v starts=%d stops=%d", err, bad.starts, bad.stops)
	}
	u = newUI(t.Context(), backend)
	backend.brokers = []string{"benchmark"}
	u.workspace = 2
	terminal := &observedTerminal{SimulationScreen: tcell.NewSimulationScreen("UTF-8")}
	done := make(chan error, 1)
	go func() { done <- u.runOnScreen(terminal) }()
	u.post(func() {
		u.screen.SetClipboard([]byte("payload"))
		u.app.Stop()
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if terminal.starts != 1 || terminal.stops != 1 || !terminal.mouse || !terminal.paste || terminal.clipboard != "payload" {
		t.Fatalf("terminal lifecycle: init=%d fini=%d mouse=%v paste=%v clipboard=%q", terminal.starts, terminal.stops, terminal.mouse, terminal.paste, terminal.clipboard)
	}
}
