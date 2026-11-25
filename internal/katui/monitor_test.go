package katui

import (
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
)

func TestGroupListKeepsPartialResultsAndForgetsDeletedGroups(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	u.relationshipAt = time.Now()
	u.updateGroupList([]string{"a", "b"}, nil)
	key := partKey{"events", 0}
	a := groupSample{Name: "a", At: time.Now(), Partitions: []partition{{partKey: key, Owner: "a"}}}
	b := groupSample{Name: "b", At: time.Now(), Partitions: []partition{
		{partKey: key, Owner: "b"}, {partKey: partKey{"events", 1}, Owner: "b"},
	}}
	u.acceptGroup(a)
	u.acceptGroup(b)
	want := func(expected string) {
		t.Helper()
		if got := u.relationshipLabel("events"); got != expected {
			t.Fatalf("topic consumers = %q, want %q", got, expected)
		}
	}
	want("2 groups / 2 assigned")
	u.updateGroupList([]string{"b"}, errors.New("one coordinator unavailable"))
	want("2 groups / 2 assigned")
	u.updateGroupList([]string{"b"}, nil)
	want("1 groups / 2 assigned")
	if _, ok := u.groups["a"]; ok || u.metrics[observationKey{"a", key}] != nil {
		t.Fatal("deleted group still has a snapshot or rate history")
	}
	b.Partitions = []partition{{partKey: key, Commit: ptr(int64(5))}}
	u.acceptGroup(b)
	want("OFFSETS ONLY")
}

func TestMissingTopicEndLeavesChartGapAndBreaksRate(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	md := kadm.TopicDetail{Topic: "events"}
	u.metadata = kadm.TopicDetails{md.Topic: md}
	start := time.Now()
	key := partKey{md.Topic, 0}
	send := func(i int, end *int64, err string) {
		u.acceptTopics(map[string]topicSample{md.Topic: {Name: md.Topic, ID: md.ID.String(), At: start.Add(time.Duration(i) * time.Second),
			Partitions: []partition{{partKey: key, End: end, Error: err}},
		}})
	}
	send(0, ptr(int64(2)), "")
	send(1, ptr(int64(5)), "")
	send(2, nil, "offset unavailable")
	if last := u.topics[md.Topic].Partitions[0].End; last == nil || *last != 5 {
		t.Fatalf("lost last displayed end on failure: %v", last)
	}
	send(3, ptr(int64(20)), "")
	for _, id := range []int32{0, -1} {
		m := u.metric("", partKey{md.Topic, id})
		if len(m.history) != 4 || m.history[1] != (ratePoint{3, true}) || m.history[2].known || m.history[3].known {
			t.Errorf("partition %d rates across outage: %+v", id, m.history)
		}
	}
}
