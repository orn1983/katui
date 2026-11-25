package katui

import (
	"slices"
	"sync"

	"github.com/gdamore/tcell/v2"
)

// tview clears and repaints the screen for each key. Compose that repaint in
// memory so temporary blanks never dirty otherwise unchanged terminal cells.
// The UI event loop owns the frame. Input, cursor and terminal modes are
// handled by the underlying screen.
type frameScreen struct {
	tcell.Screen
	frame   tcell.CellBuffer
	initErr error
	finish  sync.Once
}

func (u *ui) runOnScreen(terminal tcell.Screen) error {
	s := &frameScreen{Screen: terminal}
	// SetScreen initializes the screen immediately but discards the error.
	u.app.SetScreen(s)
	if s.initErr != nil {
		return s.initErr
	}
	defer s.Fini() // Also restore the terminal if setup fails before app.Run.
	return u.run()
}

func (s *frameScreen) Init() error {
	s.initErr = s.Screen.Init()
	if s.initErr == nil {
		s.frame.Resize(s.Screen.Size())
	}
	return s.initErr
}

func (s *frameScreen) Fini() { s.finish.Do(s.Screen.Fini) }

func (s *frameScreen) Clear() { s.Fill(' ', tcell.StyleDefault) }

func (s *frameScreen) Fill(ch rune, style tcell.Style) {
	s.frame.Resize(s.Screen.Size())
	s.frame.Fill(ch, style)
}

func (s *frameScreen) SetSize(w, h int) {
	s.Screen.SetSize(w, h)
	s.frame.Resize(s.Screen.Size())
}

func (s *frameScreen) SetCell(x, y int, style tcell.Style, ch ...rune) {
	if len(ch) == 0 {
		s.SetContent(x, y, ' ', nil, style)
	} else {
		s.SetContent(x, y, ch[0], ch[1:], style)
	}
}

func (s *frameScreen) SetContent(x, y int, ch rune, combining []rune, style tcell.Style) {
	s.frame.SetContent(x, y, ch, combining, style)
}

func (s *frameScreen) GetContent(x, y int) (rune, []rune, tcell.Style, int) {
	return s.frame.GetContent(x, y)
}

func (s *frameScreen) flush() {
	w, h := s.frame.Size()
	for y := range h {
		for x := range w {
			ch, combining, style, _ := s.frame.GetContent(x, y)
			prev, prevCombining, prevStyle, _ := s.Screen.GetContent(x, y)
			if ch != prev || style != prevStyle || !slices.Equal(combining, prevCombining) {
				s.Screen.SetContent(x, y, ch, combining, style)
			}
		}
	}
}

func (s *frameScreen) Show() {
	s.flush()
	s.Screen.Show()
}

func (s *frameScreen) Sync() {
	s.flush()
	s.Screen.Sync()
}
