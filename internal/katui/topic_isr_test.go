package katui

import (
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
)

func TestTopicISR(t *testing.T) {
	rf3 := []int32{1, 2, 3}
	full := kadm.PartitionDetail{Leader: 1, Replicas: rf3, ISR: rf3}
	short := kadm.PartitionDetail{Leader: 1, Replicas: rf3, ISR: rf3[:2]}
	missing := kadm.PartitionDetail{Err: kerr.TopicAuthorizationFailed}
	u := &ui{metadataAt: time.Now()}
	for _, tc := range []struct {
		name string
		md   kadm.TopicDetail
		want isrSummary
	}{
		{"mixed replication factors", kadm.TopicDetail{Partitions: kadm.PartitionDetails{
			0: full, 1: {Leader: 1, Replicas: rf3[:2], ISR: rf3[:2]},
		}}, isrSummary{inSync: 5, assigned: 5, complete: true}},
		{"one missing on each", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: short, 1: short}},
			isrSummary{inSync: 4, assigned: 6, degraded: 2, complete: true}},
		{"two missing on one", kadm.TopicDetail{Partitions: kadm.PartitionDetails{
			0: full, 1: {Leader: 1, Replicas: rf3, ISR: rf3[:1]},
		}}, isrSummary{inSync: 4, assigned: 6, degraded: 1, complete: true, critical: true}},
		{"empty ISR", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: {Leader: 1, Replicas: rf3[:1]}}},
			isrSummary{assigned: 1, degraded: 1, complete: true, critical: true}},
		{"no leader", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: {Leader: -1, Replicas: rf3, ISR: rf3}}},
			isrSummary{inSync: 3, assigned: 3, degraded: 1, complete: true, critical: true}},
		{"partition error", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: full, 1: missing}},
			isrSummary{inSync: 3, assigned: 3}},
		{"leader error", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: {Err: kerr.LeaderNotAvailable}}},
			isrSummary{degraded: 1, critical: true}},
		{"topic error", kadm.TopicDetail{Err: kerr.TopicAuthorizationFailed}, isrSummary{}},
		{"no partitions", kadm.TopicDetail{}, isrSummary{}},
		{"no replica assignments", kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: {}}}, isrSummary{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := u.topicISR(tc.md); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTopicISRFreshnessUsesMetadata(t *testing.T) {
	md := kadm.TopicDetail{Partitions: kadm.PartitionDetails{0: {Leader: 1, Replicas: []int32{1}, ISR: []int32{1}}}}
	u := &ui{metadataAt: time.Now(), errors: map[string]string{"offsets": "timeout"}}
	if u.topicISR(md).stale {
		t.Fatal("an offset error invalidated fresh ISR metadata")
	}
	u.errors["metadata"] = "timeout"
	if s := u.topicISR(md); !s.stale || s.inSync != 1 || s.assigned != 1 {
		t.Fatalf("failed metadata refresh should retain stale counts: %+v", s)
	}
	delete(u.errors, "metadata")
	u.metadataAt = time.Now().Add(-2*topicMetadataInterval - time.Second)
	if !u.topicISR(md).stale {
		t.Fatal("expired metadata treated as fresh")
	}
}
