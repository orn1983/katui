package katui

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestTopicOperationSelectionAndReview(t *testing.T) {
	const topic = "audit-events"
	cluster := kfake.MustCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topic))
	t.Cleanup(cluster.Close)
	u, screen, _ := startClusterUI(t, nil, "-b", cluster.ListenAddrs()[0])
	u.post(func() { u.topicForm(topic, opConfig) })
	key := func(k tcell.Key) { clusterKey(t, screen, k, 0, 0) }
	lineWith := func(words ...string) {
		t.Helper()
		waitKeyboardState(t, u, "screen missing line with "+strings.Join(words, ", "), func() bool {
			cells, width, height := screen.GetContents()
			for y := range height {
				var line strings.Builder
				for _, cell := range cells[y*width : (y+1)*width] {
					if len(cell.Runes) == 0 {
						line.WriteByte(' ')
					} else {
						line.WriteRune(cell.Runes[0])
					}
				}
				found := true
				for _, word := range words {
					found = found && strings.Contains(line.String(), word)
				}
				if found {
					return true
				}
			}
			return false
		})
	}
	cursor := func(want int) {
		t.Helper()
		waitKeyboardState(t, u, "wrong operation highlighted", func() bool {
			list, ok := u.app.GetFocus().(*tview.List)
			if !ok || list.GetCurrentItem() != want {
				return false
			}
			x, y, _, _ := list.GetInnerRect()
			_, _, selected, _ := screen.GetContent(x, y+want)
			_, _, other, _ := screen.GetContent(x, y+1-want)
			_, _, selectedAttrs := selected.Decompose()
			_, _, otherAttrs := other.Decompose()
			return selectedAttrs&^otherAttrs != 0 // Still distinguishable without colors.
		})
	}

	lineWith(opConfig, topic)
	key(tcell.KeyEnter)
	key(tcell.KeyDown)
	cursor(1)
	lineWith(opConfig, topic) // Navigating the list hasn't changed the active operation.
	lineWith(opDeleteTopic, "permanent")
	key(tcell.KeyEscape)
	key(tcell.KeyEnter)
	cursor(0) // Canceling a deletion must discard the pending highlight.
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	lineWith(opDeleteTopic, topic)
	lineWith("permanently removes its records")

	// Hold the delete validation while the user switches back to Configure.
	validating, proceed := make(chan struct{}), make(chan struct{})
	resume := sync.OnceFunc(func() { close(proceed) })
	defer resume()
	cluster.ControlKey(int16(kmsg.ApiVersions), func(req kmsg.Request) (kmsg.Response, error, bool) {
		if req.(*kmsg.ApiVersionsRequest).ClientSoftwareName == "katui" {
			cluster.DropControl()
			close(validating)
			<-proceed
		}
		return nil, nil, false
	})
	clusterKey(t, screen, tcell.KeyRune, 'r', tcell.ModAlt)
	select {
	case <-validating:
	case <-time.After(3 * time.Second):
		t.Fatal("delete review did not reach broker validation")
	}
	key(tcell.KeyEnter)
	key(tcell.KeyUp)
	key(tcell.KeyEnter)
	lineWith(opConfig, topic)
	resume()
	lineWith("Choose Review again") // A stale response must not open a deletion review.

	key(tcell.KeyEnter)
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	clusterKey(t, screen, tcell.KeyRune, 'r', tcell.ModAlt)
	lineWith("Review · ", opDeleteTopic)
	lineWith(opDeleteTopic, "Alt+A")
	clusterKey(t, screen, tcell.KeyRune, 'a', tcell.ModAlt)
	waitKeyboardState(t, u, "empty confirmation should keep the review open", func() bool {
		field, ok := u.app.GetFocus().(*tview.InputField)
		return ok && strings.Contains(field.GetLabel(), "Exact target name") && field.GetText() == ""
	})
	key(tcell.KeyEscape)
	waitKeyboardState(t, u, "review did not close", func() bool { return u.modalDepth == 0 })
	if cluster.TopicInfo(topic) == nil {
		t.Fatal("reviewing or canceling an operation deleted the topic")
	}
}
