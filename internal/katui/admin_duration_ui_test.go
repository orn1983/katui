package katui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kfake"
)

func TestTopicDurationEditor(t *testing.T) {
	const topic, setting = "durations", "retention.ms"
	cluster := kfake.MustCluster(kfake.NumBrokers(1))
	t.Cleanup(cluster.Close)
	u, screen, _ := startClusterUI(t, nil, "-b", cluster.ListenAddrs()[0])
	u.post(func() { screen.SetSize(80, 32) })
	key := func(r rune) { screen.InjectKey(tcell.KeyRune, r, 0) }
	alt := func(r rune) { screen.InjectKey(tcell.KeyRune, r, tcell.ModAlt) }

	setField := func(label, text string) {
		t.Helper()
		waitKeyboardState(t, u, "did not focus "+label, func() bool {
			field, ok := u.app.GetFocus().(*tview.InputField)
			if !ok || !strings.HasPrefix(field.GetLabel(), label) {
				return false
			}
			field.SetText(text)
			return true
		})
	}
	visible := func(fragment string, want bool) {
		t.Helper()
		waitKeyboardState(t, u, "unexpected screen visibility of "+fragment, func() bool {
			cells, width, _ := screen.GetContents()
			var out strings.Builder
			for i, cell := range cells {
				if i > 0 && i%width == 0 {
					out.WriteByte('\n')
				}
				if len(cell.Runes) != 0 {
					out.WriteRune(cell.Runes[0])
				} else {
					out.WriteByte(' ')
				}
			}
			return strings.Contains(out.String(), fragment) == want
		})
	}
	reviewAndApply := func(operation, raw string, descriptions ...string) {
		t.Helper()
		alt('s')
		visible("Staged "+setting+". Alt+R to review.", true)
		if info := cluster.TopicInfo(topic); (operation == opCreate && info != nil) ||
			(operation == opConfig && (info == nil || info.Configs[setting] == nil || *info.Configs[setting] == raw)) {
			t.Fatal("staging changed the topic before review")
		}
		alt('r')
		visible("Review · "+operation, true)
		visible("Apply Alt+A", true)
		for _, description := range descriptions {
			visible(description, true)
		}
		setField("Exact target name", strings.TrimSuffix(topic, "s"))
		alt('a') // An incomplete target must leave the confirmation editable.
		key('s')
		waitKeyboardState(t, u, "confirmation lost focus or swallowed the final letter", func() bool {
			field, ok := u.app.GetFocus().(*tview.InputField)
			return ok && field.GetText() == topic
		})
		alt('a')
		visible("Kafka accepted "+strings.ToLower(operation), true)
		if info := cluster.TopicInfo(topic); info == nil || info.Configs[setting] == nil || *info.Configs[setting] != raw {
			t.Fatalf("%s did not write %s=%s to Kafka: %+v", operation, setting, raw, info)
		}
		clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	}

	key('n')
	visible("Stage Alt+S", true)
	visible("Unstage Alt+U", true)
	visible("Review Alt+R", true)
	visible("Alt+D", false)
	for _, letter := range topic {
		key(letter)
	}
	waitKeyboardState(t, u, "typing the topic name activated a shortcut", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		return ok && field.GetText() == topic
	})
	alt('f')
	setField("Replicas", "1")
	alt('k')
	setField("Setting key", setting)
	visible("1mo = 30d", true)
	alt('v')
	setField("Setting value", "0.1ms")
	visible("resolve to whole", true)
	alt('k')
	setField("Setting key", "cleanup.policy")
	visible("1mo = 30d", false)
	alt('k')
	setField("Setting key", setting)
	alt('v')
	setField("Setting value", "6.5w")
	visible("3931200000 ms", true)
	reviewAndApply(opCreate, "3931200000", "mo", "3931200000 ms")

	waitKeyboardState(t, u, "create review did not close", func() bool { return u.modalDepth == 0 })
	// Open the same editor directly so the test does not depend on the
	// independently cached topic list picking up the new topic.
	u.post(func() { u.topicForm(topic, opConfig) })
	visible("Alt+D", true)
	visible("Filter settings Alt+F", true)
	alt('v')
	setField("Setting value", "draft")
	alt('f')
	for _, letter := range setting {
		key(letter)
	}
	waitKeyboardState(t, u, "typed filter did not reach the settings search", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		return ok && strings.HasPrefix(field.GetLabel(), "Filter settings") && field.GetText() == setting
	})
	alt('v')
	waitKeyboardState(t, u, "filtering changed the draft setting value", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		return ok && field.GetText() == "draft"
	})
	alt('b')
	waitKeyboardState(t, u, "saved duration or raw value missing from the settings table", func() bool {
		table, ok := u.app.GetFocus().(*tview.Table)
		if ok {
			for row := 1; row < table.GetRowCount(); row++ {
				if strings.TrimSpace(table.GetCell(row, 0).Text) == setting {
					table.Select(row, 0)
					return strings.Contains(table.GetCell(row, 1).Text, "mo") && strings.TrimSpace(table.GetCell(row, 2).Text) == "3931200000"
				}
			}
		}
		return false
	})
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	waitKeyboardState(t, u, "editing the rounded duration changed its millisecond value", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		if !ok {
			return false
		}
		raw, err := configValue(setting, field.GetText())
		return err == nil && raw == "3931200000"
	})
	alt('d')
	visible(setting+" → inherit broker default", true)
	alt('b')
	key('u') // Browsing settings must not discard a staged change.
	alt('v')
	waitKeyboardState(t, u, "Alt+V did not return to the value field", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		return ok && strings.HasPrefix(field.GetLabel(), "Setting value")
	})
	visible(setting+" → inherit broker default", true)
	alt('u')
	visible("No staged changes", true)
	setField("Setting value", "1.2d")
	visible("103680000 ms", true)
	reviewAndApply(opConfig, "103680000", "3931200000 ms", "1.2d", "103680000 ms")
}
