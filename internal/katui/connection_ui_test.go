package katui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kfake"
)

func startClusterUI(t *testing.T, profiles map[string]connectionProfile, args ...string) (*ui, tcell.SimulationScreen, <-chan struct{}) {
	t.Helper()
	screen := tcell.NewSimulationScreen("UTF-8")
	u, done := startClusterUIWithScreen(t, profiles, screen, args...)
	return u, screen, done
}

func startClusterUIWithScreen(t *testing.T, profiles map[string]connectionProfile, screen tcell.Screen, args ...string) (*ui, <-chan struct{}) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := saveConnectionProfiles("api", profiles); err != nil {
		t.Fatal(err)
	}
	opts, err := arguments(append([]string{"--no-cache"}, args...), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b := &broker{options: opts}
	if len(opts.brokers) != 0 {
		if err := b.open(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.close)
	}
	ctx, cancel := context.WithCancel(t.Context())
	u := newUI(ctx, b)
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = u.runOnScreen(screen)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Error(runErr)
			}
			if u.nextBroker != nil {
				u.nextBroker.close()
			}
		case <-time.After(5 * time.Second):
			t.Error("cluster UI did not stop")
		}
	})
	select {
	case <-u.screenReady:
	case <-done:
		t.Fatalf("cluster UI exited before drawing: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("cluster UI did not start")
	}
	return u, done
}

func clusterKey(t *testing.T, screen tcell.Screen, key tcell.Key, char rune, modifiers tcell.ModMask) {
	t.Helper()
	if err := screen.PostEvent(tcell.NewEventKey(key, char, modifiers)); err != nil {
		t.Fatal(err)
	}
}

func expectWorkspaceBar(t *testing.T, u *ui, screen tcell.Screen, text, selected string) {
	t.Helper()
	waitKeyboardState(t, u, "workspace bar moved or lost its highlight on "+selected, func() bool {
		for x, want := range text {
			r, _, _, _ := screen.GetContent(x, 1)
			if r != want {
				return false
			}
		}
		_, _, style, _ := screen.GetContent(strings.Index(text, selected), 1)
		_, bg, _ := style.Decompose()
		return bg == u.accent
	})
}

func TestCLIGroupNavigation(t *testing.T) {
	// An empty CLI group listing is sufficient to exercise the workspace.
	command, err := exec.LookPath("true")
	if err != nil {
		t.Skip("requires the true utility")
	}
	u, screen, _ := startClusterUI(t, nil, "--backend", "cli", "--kafka-cli", command, "-b", "127.0.0.1:1")
	const tabs = "2 Consumer Groups  4 Clusters"
	expectWorkspaceBar(t, u, screen, tabs, "Consumer Groups")
	clusterKey(t, screen, tcell.KeyRune, '4', 0)
	expectWorkspaceBar(t, u, screen, tabs, "Clusters")
	clusterKey(t, screen, tcell.KeyRune, '2', 0)
	expectWorkspaceBar(t, u, screen, tabs, "Consumer Groups")
	u.post(func() {
		tabs := u.tabs.GetText(true)
		if !strings.Contains(tabs, "Consumer Groups") || !strings.Contains(tabs, "Clusters") ||
			strings.Contains(tabs, "Topics") || strings.Contains(tabs, "Messages") {
			t.Errorf("unexpected CLI workspace labels: %q", tabs)
		}
		for _, kind := range []string{"groups", "group", "group-detail"} {
			labels := u.updateControls(kind)
			if strings.Contains(labels, "Manage") || strings.Contains(labels, "Consume") {
				t.Errorf("%s advertises unsupported CLI actions: %q", kind, labels)
			}
		}
	})
	clusterKey(t, screen, tcell.KeyRune, '3', 0)
	waitKeyboardState(t, u, "CLI Messages shortcut opened a reader", func() bool {
		return u.workspace == 1 && strings.Contains(u.message, "Messages require API backend")
	})
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	clusterKey(t, screen, tcell.KeyRune, '?', tcell.ModShift)
	waitKeyboardState(t, u, "CLI keyboard reference did not open", func() bool {
		_, ok := u.app.GetFocus().(*tview.TextView)
		return ok && u.modalDepth == 1
	})
	u.post(func() {
		help := u.app.GetFocus().(*tview.TextView).GetText(true)
		for _, label := range []string{"2 Consumer Groups", "4 Clusters", "Monitoring", "Kafka CLI properties", "File browser"} {
			if !strings.Contains(help, label) {
				t.Errorf("CLI keyboard reference is missing %q", label)
			}
		}
		for _, label := range []string{"1 Topics", "3 Messages", "Create topic", "Start consuming", "Protobuf schema editor"} {
			if strings.Contains(help, label) {
				t.Errorf("CLI keyboard reference advertises %q", label)
			}
		}
	})
}

func waitClusterSelection(t *testing.T, u *ui, done <-chan struct{}, name, address string) {
	t.Helper()
	select {
	case <-done:
		if u.nextBroker == nil {
			t.Fatal("the app exited without choosing a cluster")
		}
		if u.nextBroker.profileName != name || u.nextBroker.cluster != address {
			t.Fatalf("selected %q at %q, want %q at %q", u.nextBroker.profileName, u.nextBroker.cluster, name, address)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shortcut did not connect")
	}
}

func TestClustersWorkspaceKeepsNavigationVisible(t *testing.T) {
	cluster := kfake.MustCluster(kfake.NumBrokers(1))
	t.Cleanup(cluster.Close)
	address := cluster.ListenAddrs()[0]
	u, screen, _ := startClusterUI(t, map[string]connectionProfile{
		"Saved cluster": {Brokers: []string{address}},
	}, "-b", address)

	check := func(verify func()) {
		t.Helper()
		if !u.post(verify) {
			t.Fatal("UI stopped during navigation")
		}
	}
	waitFor := func(workspace, modalDepth int) {
		t.Helper()
		var gotWorkspace, gotDepth int
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			check(func() { gotWorkspace, gotDepth = u.workspace, u.modalDepth })
			if gotWorkspace == workspace && gotDepth == modalDepth {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("got workspace %d with %d dialogs, want workspace %d with %d dialogs", gotWorkspace, gotDepth, workspace, modalDepth)
	}
	const tabs = "1 Topics  2 Consumer Groups  3 Messages  4 Clusters"
	waitFor(0, 0)
	expectWorkspaceBar(t, u, screen, tabs, "Topics")
	for i, key := range "123" {
		clusterKey(t, screen, tcell.KeyRune, '4', 0)
		waitFor(clustersWorkspace, 0)
		check(func() {
			_, tabsY, _, tabsHeight := u.tabs.GetRect()
			_, pickerY, _, _ := u.clusters.root.GetRect()
			if pickerY < tabsY+tabsHeight {
				t.Error("cluster list overlaps the workspace bar")
			}
		})
		expectWorkspaceBar(t, u, screen, tabs, "Clusters")
		clusterKey(t, screen, tcell.KeyRune, key, 0)
		waitFor(i, 0)
		expectWorkspaceBar(t, u, screen, tabs, []string{"Topics", "Consumer Groups", "Messages"}[i])
		check(func() {
			if u.clusters != nil {
				t.Errorf("%c left the cluster list open", key)
			}
		})
	}
	clusterKey(t, screen, tcell.KeyRune, '4', 0)
	clusterKey(t, screen, tcell.KeyRune, 'e', 0)
	waitFor(clustersWorkspace, 1)
	expectWorkspaceBar(t, u, screen, tabs, "Clusters")
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitFor(clustersWorkspace, 0)
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitFor(2, 0)
}

func TestSavedClusterQuickConnect(t *testing.T) {
	cluster := kfake.MustCluster(kfake.NumBrokers(1))
	defer cluster.Close()
	second := cluster.ListenAddrs()[0]
	for _, cc := range []struct {
		name string
		key  tcell.Key
		char rune
	}{
		{"Enter", tcell.KeyEnter, 0},
		{"c", tcell.KeyRune, 'c'},
	} {
		t.Run(cc.name, func(t *testing.T) {
			u, screen, done := startClusterUI(t, map[string]connectionProfile{
				"Alpha": {Brokers: []string{"first.example:9092"}},
				"Beta":  {Brokers: []string{second}},
			})
			if !u.post(func() {
				if _, ok := u.app.GetFocus().(*tview.List); !ok {
					t.Error("startup did not focus the saved cluster list")
				}
				cells, width, height := screen.GetContents()
				for row := range height {
					var line strings.Builder
					for _, cell := range cells[row*width : (row+1)*width] {
						if len(cell.Runes) > 0 {
							line.WriteRune(cell.Runes[0])
						} else {
							line.WriteByte(' ')
						}
					}
					x := strings.Index(line.String(), "Connect Enter")
					if x >= 0 {
						x = utf8.RuneCountInString(line.String()[:x])
						labelColor, _, _ := cells[row*width+x].Style.Decompose()
						keyColor, keyBackground, _ := cells[row*width+x+len("Connect ")].Style.Decompose()
						if labelColor == keyColor || keyColor != tcell.GetColor(shortcutForeground) || keyBackground != tcell.GetColor(shortcutBackground) {
							t.Errorf("Connect text color %s and key colors %s on %s did not match the shortcut style", labelColor, keyColor, keyBackground)
						}
						return
					}
				}
				t.Error("the 80-column terminal did not show the Connect shortcut")
			}) {
				t.Fatal("startup did not render")
			}
			clusterKey(t, screen, tcell.KeyRune, 'e', 0)
			clusterKey(t, screen, tcell.KeyRune, 'n', tcell.ModAlt)
			waitKeyboardState(t, u, "e and Alt+N did not focus the saved profile name", func() bool {
				input, ok := u.app.GetFocus().(*tview.InputField)
				return ok && input.GetLabel() == buttonLabel("Profile name", "Alt+N") && input.GetText() == "Alpha"
			})
			clusterKey(t, screen, tcell.KeyEscape, 0, 0)
			clusterKey(t, screen, tcell.KeyDown, 0, 0)
			clusterKey(t, screen, cc.key, cc.char, 0)
			waitClusterSelection(t, u, done, "Beta", second)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := u.nextBroker.client.Ping(ctx); err != nil {
				t.Fatalf("the selected profile did not reach its broker: %v", err)
			}
		})
	}
}

func TestSavedClusterPromptsOnlyForSessionPassword(t *testing.T) {
	for _, named := range []bool{false, true} {
		name := "choose with c"
		var args []string
		if named {
			name = "named at startup"
			args = []string{"--cluster", "Ask"}
		}
		t.Run(name, func(t *testing.T) {
			security := &securityConfig{}
			security.SASL.Mechanism = "SCRAM-SHA-256"
			security.SASL.Username = "editor"
			const addr = "example.net:9093"
			u, screen, done := startClusterUI(t, map[string]connectionProfile{
				"Ask": {Brokers: []string{addr}, Security: security, PromptPassword: true},
			}, args...)
			path := connectionProfilesPath("api")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !named {
				clusterKey(t, screen, tcell.KeyRune, 'c', 0)
			}
			waitKeyboardState(t, u, "quick connect did not show just the missing password prompt", func() bool {
				focus, ok := u.app.GetFocus().(*tview.InputField)
				return ok && focus.GetLabel() == "Password " && focus.GetText() == "" && u.modalDepth == 2
			})
			const password = "session"
			for _, char := range password {
				clusterKey(t, screen, tcell.KeyRune, char, 0)
			}
			clusterKey(t, screen, tcell.KeyEnter, 0, 0)
			waitClusterSelection(t, u, done, "Ask", addr)
			if u.nextBroker.security == nil || u.nextBroker.security.SASL.Password != password {
				t.Fatal("session password did not reach the Kafka client")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("quick connect changed the saved profile")
			}
		})
	}
}
