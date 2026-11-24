package katui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Use the same high-contrast keycap on normal, selected, and themed backgrounds.
const shortcutForeground = "#ffe29a"
const shortcutBackground = "#3c321c"

func keyHint(key string) string {
	return "[" + shortcutForeground + ":" + shortcutBackground + ":b]" + tview.Escape(key) + "[-:-:-]"
}

func buttonLabel(label, key string) string {
	if key == "" {
		return label
	}
	return label + " " + keyHint(key)
}

func shortcutStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(tcell.GetColor(shortcutForeground)).Background(tcell.GetColor(shortcutBackground)).Bold(true)
}
