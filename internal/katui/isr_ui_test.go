package katui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kadm"
)

func TestTopicOverviewISR(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	u.metadataAt = time.Now()
	u.metadata = kadm.TopicDetails{}
	for name, count := range map[string]int{"a[blue]": 2, "b-critical": 1, "c-healthy": 3} {
		replicas := []int32{1, 2, 3}
		u.metadata[name] = kadm.TopicDetail{Topic: name, Partitions: kadm.PartitionDetails{
			0: {Partition: 0, Leader: 1, Replicas: replicas, ISR: replicas[:count]},
			1: {Partition: 1, Leader: 2, Replicas: replicas, ISR: replicas},
		}}
	}
	u.metadata["d-unknown"] = kadm.TopicDetail{Topic: "d-unknown"}
	u.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
	u.table.SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(u.accent).Bold(true))
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	draw := func(width, skip, selected int) {
		t.Helper()
		screen.SetSize(width, 10)
		u.table.SetRect(0, 0, width, 10)
		u.current().horizontal = skip
		u.renderTable(u.browserRows(u.current()))
		u.table.Select(selected, 0)
		u.table.Draw(screen)
	}
	line := func(y int) string {
		var text strings.Builder
		width, _ := screen.Size()
		for x := range width {
			r, _, _, _ := screen.GetContent(x, y)
			text.WriteRune(r)
		}
		return text.String()
	}
	draw(80, 0, 2)
	for i, want := range []string{"Topic ISR/Replicas Affected P", "a[blue] 5/6 1", "b-critical 4/6 1", "c-healthy 6/6 0", "d-unknown ? ?"} {
		got := strings.Join(strings.Fields(line(i)), " ")
		if !strings.HasPrefix(got, want) {
			t.Errorf("screen row %d = %q, want prefix %q", i, got, want)
		}
	}
	ratioColumn := strings.Index(line(0), "ISR/Replicas")
	if ratioColumn < 0 {
		t.Fatal("ISR column missing")
	}
	colorAt := func(x, y int) (tcell.Color, tcell.Color) {
		_, _, style, _ := screen.GetContent(x, y)
		fg, bg, _ := style.Decompose()
		return fg, bg
	}
	if fg, _ := colorAt(ratioColumn, 1); fg != tcell.ColorYellow {
		t.Errorf("missing one follower: got foreground %v", fg)
	}
	for _, width := range []int{80, 240} {
		draw(width, 0, 2)
		for x := range width {
			if fg, bg := colorAt(x, 2); fg != tcell.ColorBlack || bg != tcell.ColorRed {
				t.Fatalf("selected critical row at column %d of %d has fg %v bg %v", x, width, fg, bg)
			}
		}
	}
	draw(40, ratioColumn+1, 1)
	if !strings.HasPrefix(line(1), "/6") {
		t.Fatalf("pan into ISR column rendered %q", line(1))
	}
	if fg, bg := colorAt(0, 1); fg != tcell.ColorBlack || bg != tcell.ColorYellow {
		t.Errorf("selected warning row after horizontal scroll has fg %v bg %v", fg, bg)
	}
	if fg, _ := colorAt(0, 2); fg != tcell.ColorRed {
		t.Errorf("critical row after horizontal scroll has fg %v", fg)
	}
}

func TestPartialAndStaleTopicISRLabels(t *testing.T) {
	for _, tc := range []struct {
		health          isrSummary
		ratio, affected string
		color           tcell.Color
	}{
		{isrSummary{}, "?", "?", tcell.ColorDefault},
		{isrSummary{degraded: 1}, "?", "≥1", tcell.ColorYellow},
		{isrSummary{complete: true, inSync: 3, assigned: 3, stale: true}, "3/3 stale", "0", tcell.ColorDefault},
	} {
		ratio, affected, color := tc.health.overview()
		if ratio != tc.ratio || affected != tc.affected || color != tc.color {
			t.Errorf("overview(%+v) = %q, %q, %v", tc.health, ratio, affected, color)
		}
	}
}
