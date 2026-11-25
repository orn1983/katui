package katui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestCLIContinuesSamplingOpenedGroupsAwayFromBrowser(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KATUI_CLI_FIXTURE", dir)
	setCommit := func(offset int64) {
		t.Helper()
		if err := atomicFile(filepath.Join(dir, "offset"), []byte(strconv.FormatInt(offset, 10)+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	setCommit(10)
	b := cliFixture(t, `
for group do :; done
case " $* " in
	*" --list "*) printf '%s\n' alpha beta unopened ;;
	*" --state "*) printf '%s localhost:9092 (1) range Stable 1\n' "$group" ;;
	*" --describe "*)
		read -r offset < "$KATUI_CLI_FIXTURE/offset" || exit 1
		printf '%s events 0 %s 100 %s member /localhost client\n' "$group" "$offset" "$((100-offset))"
		printf '%s\n' "$group" >> "$KATUI_CLI_FIXTURE/reads" ;;
	*) exit 1 ;;
esac
`)
	u, screen, _ := startClusterUI(t, nil, "--backend", "cli", "--kafka-cli", b.cli, "-b", "localhost:9092", "-i", "0.2")
	key := func(k tcell.Key, r rune) { clusterKey(t, screen, k, r, 0) }
	waitKeyboardState(t, u, "group list did not load", func() bool {
		return len(u.rows) == 3 && !u.groups["unopened"].At.IsZero()
	})
	open := func(name string) {
		t.Helper()
		key(tcell.KeyEnter, 0)
		waitKeyboardState(t, u, "group did not open: "+name, func() bool {
			return u.current().group == name && !u.connectionPending()
		})
	}
	open("alpha")
	key(tcell.KeyEscape, 0)
	waitKeyboardState(t, u, "could not return to group list", func() bool {
		return u.current().kind == "groups" && u.selected().group == "alpha"
	})
	key(tcell.KeyDown, 0)
	open("beta")
	key(tcell.KeyRune, '4')
	waitKeyboardState(t, u, "Clusters did not open", func() bool { return u.workspace == clustersWorkspace })

	readCounts := func() map[string]int {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "reads"))
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, group := range strings.Fields(string(data)) {
			counts[group]++
		}
		return counts
	}
	checkBackground := func(offset int64) {
		t.Helper()
		before := readCounts()
		setCommit(offset)
		// Observe only the external CLI until multiple polls complete. Posting
		// events to the simulation here could wake a stalled monitoring loop.
		deadline := time.Now().Add(5 * time.Second)
		for {
			now := readCounts()
			if now["unopened"] != before["unopened"] {
				t.Fatal("a group that was only listed was polled in the background")
			}
			if now["alpha"] >= before["alpha"]+3 && now["beta"] >= before["beta"]+3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("background reads stalled at %v, started at %v", now, before)
			}
			time.Sleep(10 * time.Millisecond)
		}
		u.post(func() {
			for _, name := range []string{"alpha", "beta"} {
				m := u.metrics[observationKey{name, partKey{"events", 0}}]
				if m == nil || m.commit.last == nil || *m.commit.last != offset {
					t.Errorf("%s history did not record the background offset %d", name, offset)
				}
			}
		})
	}
	checkBackground(20)
	key(tcell.KeyCtrlP, 0)
	waitKeyboardState(t, u, "Help & Commands did not open", func() bool { return u.modalDepth == 1 })
	checkBackground(30)
	key(tcell.KeyEscape, 0)
	key(tcell.KeyRune, '2')
	key(tcell.KeyEnter, 0)
	waitKeyboardState(t, u, "returning to the partition did not show its rate history", func() bool {
		return u.current().kind == "group-detail" && u.current().group == "beta" && strings.Contains(u.detail.GetText(true), "Peak ")
	})
}
