package katui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
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
	u.watchGroup("a")
	u.watchGroup("b")
	picker := &location{kind: "picker", topic: key.Topic}
	u.stacks[0] = append(u.stacks[0], picker, &location{kind: "group", group: "a", topic: key.Topic})
	u.stacks[1] = append(u.stacks[1], &location{kind: "group", group: "a"}, &location{kind: "group-detail", group: "a", topic: key.Topic})
	want := func(expected string) {
		t.Helper()
		if got := u.relationshipLabel("events"); got != expected {
			t.Fatalf("topic consumers = %q, want %q", got, expected)
		}
	}
	want("2 groups / 2 assigned")
	u.updateGroupList([]string{"b"}, errors.New("one coordinator unavailable"))
	want("2 groups / 2 assigned")
	if u.watchedGroups["a"] == nil || len(u.stacks[0]) != 3 || len(u.stacks[1]) != 3 {
		t.Fatal("partial listing removed an open group")
	}
	u.updateGroupList([]string{"b"}, nil)
	want("1 groups / 2 assigned")
	if _, ok := u.groups["a"]; ok || u.metrics[observationKey{"a", key}] != nil {
		t.Fatal("deleted group still has a snapshot or rate history")
	}
	if u.watchedGroups["a"] != nil || u.watchedGroups["b"] == nil ||
		len(u.stacks[0]) != 2 || u.stacks[0][1] != picker || len(u.stacks[1]) != 1 {
		t.Fatal("deletion did not return the saved views to their respective lists or preserve the other watch")
	}
	b.Partitions = []partition{{partKey: key, Commit: ptr(int64(5))}}
	u.acceptGroup(b)
	want("OFFSETS ONLY")
}

func TestGroupDeletionFromPartitionReturnsToFilteredList(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	b, cluster, producer := migrationBroker(t)
	seedMigrationOffsets(t, ctx, b, producer)
	u, screen, _ := startClusterUI(t, nil, "-b", cluster.ListenAddrs()[0], "-g", migrationGroup, "-t", migrationTopic, "-p", "0")
	waitKeyboardState(t, u, "group detail did not load", func() bool {
		return u.current().kind == "group-detail" && len(u.groups[migrationGroup].Partitions) > 0
	})
	plan, err := b.prepareAdmin(ctx, adminInput{operation: opDeleteGroup, target: migrationGroup})
	if err != nil {
		t.Fatal(err)
	}
	u.post(func() {
		u.stacks[1][0].filter = "review"
		u.adminReview(plan)
		u.app.GetFocus().(*tview.InputField).SetText(migrationGroup)
	})
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	clusterKey(t, screen, tcell.KeyEnter, 0, 0)
	waitKeyboardState(t, u, "review did not report successful group deletion", func() bool {
		return u.modalDepth == 1 && strings.Contains(u.message, "Kafka accepted")
	})
	clusterKey(t, screen, tcell.KeyEscape, 0, 0)
	waitKeyboardState(t, u, "deleting a group left stale detail, filter or focus", func() bool {
		return u.modalDepth == 0 && u.current().kind == "groups" && u.table.HasFocus() && u.search.GetText() == "review"
	})
	u.post(func() {
		_, exists := u.groups[migrationGroup]
		if exists || u.watchedGroups[migrationGroup] != nil || slices.Contains(u.groupNames, migrationGroup) ||
			u.metrics[observationKey{migrationGroup, partKey{migrationTopic, 0}}] != nil {
			t.Error("deleted group remains in the browser or monitoring history")
		}
	})
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

func TestFailedGroupObservationLeavesAGap(t *testing.T) {
	u := newUI(t.Context(), &broker{options: options{noCache: true}})
	key := partKey{"events", 0}
	start := time.Now()
	observe := func(seconds int, offset int64) {
		u.acceptGroup(groupSample{Name: "reader", At: start.Add(time.Duration(seconds) * time.Second),
			Partitions: []partition{{partKey: key, Commit: &offset, End: ptr(int64(100)), Owner: "member"}}})
	}
	observe(0, 1)
	observe(2, 9)
	u.acceptGroup(groupSample{Name: "reader", At: start.Add(4 * time.Second), Error: "request timed out"})
	m := u.metric("reader", key)
	if u.groups["reader"].At != start.Add(2*time.Second) || m.owner != "member" || m.ownerChangeCount != 0 {
		t.Fatal("a failed sample changed the last good snapshot or invented an owner change")
	}
	observe(9, 23)
	observe(12, 26)
	want := []ratePoint{{}, {rate: 4, known: true}, {}, {}, {rate: 1, known: true}}
	if !slices.Equal(m.history, want) {
		t.Errorf("group rates across outage = %v, want %v", m.history, want)
	}
}
