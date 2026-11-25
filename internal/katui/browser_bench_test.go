package katui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/twmb/franz-go/pkg/kadm"
)

// Refresh a full live buffer with only a terminal viewport's worth of rows visible.
func BenchmarkMessagesRefresh(b *testing.B) {
	for _, filter := range []string{"", "event"} {
		b.Run("filter="+filter, func(b *testing.B) {
			u := newUI(b.Context(), &broker{options: options{noCache: true}})
			u.workspace = 2
			u.current().filter = filter
			u.table = tview.NewTable().SetFixed(1, 0).SetSelectable(true, false)
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				b.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(120, 35)
			u.table.SetRect(0, 0, 60, 30)
			list := &messageList{u.table, u}
			value := strings.Repeat("test EVENT abc ", 500)
			appendRecord := func(offset int64) {
				u.buffer(&storedRecord{id: recordID{partKey{"topic", 0}, offset}, value: []byte(value), valueSize: len(value), cost: len(value) + 128})
			}
			for i := range 2000 {
				appendRecord(int64(i))
			}
			render := func() {
				u.renderTable(u.browserRows(u.current()))
				list.Draw(screen)
			}
			render()
			for i := int64(2000); b.Loop(); i++ {
				appendRecord(i)
				render()
			}
		})
	}
}

// Refresh the topic list on a cluster where multiple groups consume every topic.
func BenchmarkTopicBrowserRows(b *testing.B) {
	u := newUI(b.Context(), &broker{options: options{noCache: true, interval: time.Second}})
	u.metadata = make(kadm.TopicDetails)
	for t := range 200 {
		topic := fmt.Sprintf("topic-%d", t)
		u.metadata[topic] = kadm.TopicDetail{Topic: topic}
	}
	for n := range 60 {
		name := fmt.Sprintf("group-%d", n)
		group := groupSample{Name: name}
		for topic := range u.metadata {
			for p := range int32(4) {
				group.Partitions = append(group.Partitions, partition{partKey: partKey{topic, p}, Owner: name})
			}
		}
		u.groups[name] = group
		u.groupNames = append(u.groupNames, name)
	}
	for b.Loop() {
		u.browserRows(u.current())
	}
}
