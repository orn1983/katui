package katui

import (
	"fmt"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
)

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
