package katui

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const migrationTopic = "events"
const migrationGroup = "migration-review"

func migrationBroker(t *testing.T) (*broker, *kfake.Cluster, *kgo.Client) {
	t.Helper()
	c := kfake.MustCluster(kfake.NumBrokers(1), kfake.SeedTopics(2, migrationTopic))
	t.Cleanup(c.Close)
	b := &broker{options: options{brokers: c.ListenAddrs(), timeout: 2 * time.Second}}
	if err := b.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.close)
	producer, err := kgo.NewClient(kgo.SeedBrokers(c.ListenAddrs()...), kgo.RecordPartitioner(kgo.ManualPartitioner()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	return b, c, producer
}

func seedMigrationOffsets(t *testing.T, ctx context.Context, b *broker, producer *kgo.Client) {
	t.Helper()
	var offsets kadm.Offsets
	for p := range int32(2) {
		for _, value := range []string{"first", "second"} {
			if err := producer.ProduceSync(ctx, &kgo.Record{Topic: migrationTopic, Partition: p, Value: []byte(value)}).FirstErr(); err != nil {
				t.Fatal(err)
			}
		}
		offsets.AddOffset(migrationTopic, p, 1, -1)
	}
	if err := b.admin.CommitAllOffsets(ctx, migrationGroup, offsets); err != nil {
		t.Fatal(err)
	}
}

func migrationReset() adminInput {
	return adminInput{operation: opReset, target: migrationGroup, mode: 2, selected: []partKey{{migrationTopic, 0}, {migrationTopic, 1}}}
}

func TestAdminRequiresClusterIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c := kfake.MustCluster(kfake.NumBrokers(1), kfake.ClusterID(""))
	t.Cleanup(c.Close)
	b := &broker{options: options{brokers: c.ListenAddrs(), timeout: 2 * time.Second}}
	if err := b.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.close)
	var creates atomic.Int32
	c.ControlKey(int16(kmsg.CreateTopics), func(kmsg.Request) (kmsg.Response, error, bool) {
		creates.Add(1)
		return nil, nil, false
	})
	in := adminInput{operation: opCreate, target: "reviewed", partitions: 1, replicas: 1}
	if _, err := b.prepareAdmin(ctx, in); err == nil {
		t.Error("review succeeded without a cluster ID")
	}
	other, _, _ := migrationBroker(t)
	plan, err := other.prepareAdmin(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := b.applyAdmin(ctx, plan); sent || err == nil {
		t.Errorf("applied review on an unidentified cluster: sent=%v, err=%v", sent, err)
	}
	if n := creates.Load(); n != 0 {
		t.Errorf("unidentified broker received %d CreateTopics requests", n)
	}
}

func TestApplyRevalidatesReviewedState(t *testing.T) {
	for _, change := range []struct{ name, message string }{
		{"membership", "membership changed"},
		{"committed offset", "committed offset changed"},
		{"retention bound", "outside current"},
		{"topic identity", "identity or partition layout changed"},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			b, c, producer := migrationBroker(t)
			seedMigrationOffsets(t, ctx, b, producer)
			input := migrationReset()
			var member atomic.Pointer[string]
			member.Store(ptr("reviewed-member"))
			if change.name == "membership" {
				input = adminInput{operation: opRebalance, target: migrationGroup}
				// Keep the group type and assignor stable and change only its membership.
				c.ControlKey(int16(kmsg.DescribeGroups), func(req kmsg.Request) (kmsg.Response, error, bool) {
					c.KeepControl()
					res := req.ResponseKind().(*kmsg.DescribeGroupsResponse)
					res.Groups = []kmsg.DescribeGroupsResponseGroup{{
						Group: migrationGroup, State: "Stable", ProtocolType: "consumer", Protocol: "range",
						Members: []kmsg.DescribeGroupsResponseGroupMember{{MemberID: *member.Load()}},
					}}
					return res, nil, true
				})
			}
			if change.name == "topic identity" {
				input = adminInput{operation: opDeleteTopic, target: migrationTopic}
			}
			plan, err := b.prepareAdmin(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			var offsets kadm.Offsets
			switch change.name {
			case "membership":
				member.Store(ptr("replacement-member"))
			case "committed offset":
				offsets.AddOffset(migrationTopic, 0, 2, -1)
				if err := b.admin.CommitAllOffsets(ctx, migrationGroup, offsets); err != nil {
					t.Fatal(err)
				}
			case "retention bound":
				offsets.AddOffset(migrationTopic, 0, 1, -1)
				res, err := b.admin.DeleteRecords(ctx, offsets)
				if err := errors.Join(err, res.Error()); err != nil {
					t.Fatal(err)
				}
			case "topic identity":
				if _, err := b.admin.DeleteTopic(ctx, migrationTopic); err != nil {
					t.Fatal(err)
				}
				if _, err := b.admin.CreateTopic(ctx, 2, 1, nil, migrationTopic); err != nil {
					t.Fatal(err)
				}
			}
			key := kmsg.OffsetCommit
			if input.operation == opRebalance {
				key = kmsg.LeaveGroup
			} else if input.operation == opDeleteTopic {
				key = kmsg.DeleteTopics
			}
			var attempts atomic.Int32
			c.ControlKey(int16(key), func(kmsg.Request) (kmsg.Response, error, bool) {
				attempts.Add(1)
				return nil, nil, false
			})
			sent, err := b.applyAdmin(ctx, plan)
			if sent || attempts.Load() != 0 || err == nil || !strings.Contains(err.Error(), change.message) {
				t.Fatalf("mutation sent=%t, requests=%d, error=%v, want %q", sent, attempts.Load(), err, change.message)
			}
		})
	}
}

func TestApplyReportsPartialFailureOrTimeoutWithoutRetrying(t *testing.T) {
	for _, scenario := range []string{"partial", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			b, c, producer := migrationBroker(t)
			seedMigrationOffsets(t, ctx, b, producer)
			plan, err := b.prepareAdmin(ctx, migrationReset())
			if err != nil {
				t.Fatal(err)
			}
			var attempts atomic.Int32
			c.ControlKey(int16(kmsg.OffsetCommit), func(req kmsg.Request) (kmsg.Response, error, bool) {
				c.KeepControl()
				attempts.Add(1)
				if scenario == "timeout" {
					return nil, nil, true // Simulate a mutation whose reply is lost.
				}
				r := req.(*kmsg.OffsetCommitRequest)
				topic := r.Topics[0]
				res := req.ResponseKind().(*kmsg.OffsetCommitResponse)
				res.Topics = []kmsg.OffsetCommitResponseTopic{{
					Topic: topic.Topic, TopicID: topic.TopicID,
					Partitions: []kmsg.OffsetCommitResponseTopicPartition{
						{Partition: 0}, {Partition: 1, ErrorCode: kerr.CoordinatorLoadInProgress.Code},
					},
				}}
				return res, nil, true
			})
			applyCtx, stop := context.WithTimeout(ctx, time.Second)
			defer stop()
			sent, err := b.applyAdmin(applyCtx, plan)
			if !sent || attempts.Load() != 1 {
				t.Fatalf("mutation sent=%t, requests=%d, error=%v", sent, attempts.Load(), err)
			}
			if scenario == "timeout" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected a deadline error, got %v", err)
				}
			} else if !errors.Is(err, kerr.CoordinatorLoadInProgress) || !strings.Contains(err.Error(), "events:1") || strings.Contains(err.Error(), "events:0") {
				t.Fatalf("expected an error for only the failed partition, got %v", err)
			}
		})
	}
}

func TestReaderReconnectPreservesSnapshotAndPosition(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	b, c, producer := migrationBroker(t)
	seedMigrationOffsets(t, ctx, b, producer)
	var fetches atomic.Int32
	c.ControlKey(int16(kmsg.Fetch), func(kmsg.Request) (kmsg.Response, error, bool) {
		if fetches.Add(1) == 2 {
			return nil, io.EOF, true
		}
		return nil, nil, false
	})
	store := newPayloadStore(t.TempDir(), 1<<20)
	defer store.close()
	session := &readerSession{wake: make(chan struct{}, 1)}
	var records []recordID
	var state string
	reconnected := false
	b.readRecords(ctx, session, migrationTopic, consumeOptions{start: 2, partitions: []int32{0, 1}}, store, func(update readerUpdate) {
		state = update.state
		for _, r := range update.records {
			records = append(records, r.id)
		}
		if state == "RECONNECTING" && !reconnected {
			reconnected = true
			for p := range int32(2) {
				if err := producer.ProduceSync(ctx, &kgo.Record{Topic: migrationTopic, Partition: p, Value: []byte("after snapshot")}).FirstErr(); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	want := []recordID{
		{partKey{migrationTopic, 0}, 0}, {partKey{migrationTopic, 0}, 1},
		{partKey{migrationTopic, 1}, 0}, {partKey{migrationTopic, 1}, 1},
	}
	if !reconnected || state != "COMPLETE" || !slices.Equal(records, want) {
		t.Fatalf("reconnected=%t, state=%s, records=%v, want %v", reconnected, state, records, want)
	}
}
