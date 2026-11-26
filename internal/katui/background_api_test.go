package katui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestAPIMonitorsOpenedGroupsWhileBrowsingMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	b, cluster, producer := migrationBroker(t)
	seedMigrationOffsets(t, ctx, b, producer)
	const otherGroup, unopenedGroup = "other-reader", "unopened-reader"
	commit := func(group string, offset int64) {
		t.Helper()
		var offsets kadm.Offsets
		offsets.AddOffset(migrationTopic, 0, offset, -1)
		if err := b.admin.CommitAllOffsets(ctx, group, offsets); err != nil {
			t.Fatal(err)
		}
	}
	commit(otherGroup, 1)
	commit(unopenedGroup, 1)
	groups := []string{migrationGroup, otherGroup}
	u, screen, _ := startClusterUI(t, nil, "-b", cluster.ListenAddrs()[0], "--view", "groups", "-i", "0.1")
	for i, group := range groups {
		waitKeyboardState(t, u, group+" missing from group browser", func() bool {
			for row, r := range u.rows {
				if u.current().kind == "groups" && r.group == group {
					u.table.Select(row+1, 0)
					return true
				}
			}
			return false
		})
		clusterKey(t, screen, tcell.KeyEnter, 0, 0)
		waitKeyboardState(t, u, group+" did not open with its committed offset", func() bool {
			m := u.metrics[observationKey{group, partKey{migrationTopic, 0}}]
			return u.current().group == group && m != nil && m.commit.last != nil && *m.commit.last == 1
		})
		if i == 0 {
			u.post(u.back)
		}
	}
	clusterKey(t, screen, tcell.KeyRune, '3', 0)
	waitKeyboardState(t, u, "Messages did not open", func() bool { return u.workspace == 2 })

	for _, group := range groups {
		commit(group, 2)
	}
	// Observe the broker without waking the UI. Two later sampling rounds
	// ensure the first offset change has had time to reach the chart.
	requests := make(chan []string, 32)
	cluster.ControlKey(int16(kmsg.DescribeGroups), func(req kmsg.Request) (kmsg.Response, error, bool) {
		cluster.KeepControl()
		select {
		case requests <- req.(*kmsg.DescribeGroupsRequest).Groups:
		default:
		}
		return nil, nil, false
	})
	waitForBackgroundSamples := func() {
		t.Helper()
		counts := map[string]int{}
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for counts[migrationGroup] < 3 || counts[otherGroup] < 3 {
			select {
			case names := <-requests:
				if slices.Contains(names, unopenedGroup) {
					t.Fatalf("background poll included unopened group %q", unopenedGroup)
				}
				for _, name := range names {
					counts[name]++
				}
			case <-deadline.C:
				t.Fatalf("background sample counts for the 100 ms interval: %v", counts)
			}
		}
	}
	waitForBackgroundSamples()
	u.post(func() {
		for _, group := range groups {
			m := u.metrics[observationKey{group, partKey{migrationTopic, 0}}]
			if m == nil || m.commit.last == nil || *m.commit.last != 2 {
				t.Errorf("committed offset for %s did not advance in Messages", group)
				continue
			}
			positive := false
			for _, p := range m.history {
				positive = positive || p.known && p.rate > 0
			}
			if !positive {
				t.Errorf("missing background rate in %s chart: %+v", group, m.history)
			}
		}
	})
	clusterKey(t, screen, tcell.KeyCtrlP, 0, 0)
	waitKeyboardState(t, u, "Help & Commands did not open", func() bool { return u.modalDepth == 1 })
	waitForBackgroundSamples()
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	clusterKey(t, screen, tcell.KeyRune, '2', 0)
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	waitKeyboardState(t, u, "reopened partition detail did not display the background samples", func() bool {
		if u.current().kind != "group-detail" || u.current().group != otherGroup {
			return false
		}
		view := u.detail.GetText(false)
		return strings.Contains(view, "Committed 2\n") && strings.Contains(view, "Scale ") && !strings.Contains(view, "Peak 0.0/s")
	})
}
