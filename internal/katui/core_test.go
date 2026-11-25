package katui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestRatesUseElapsedTimeAndPairedLag(t *testing.T) {
	start := time.Now()
	var m measurement
	m.observe(start, partition{Commit: ptr(int64(0)), End: ptr(int64(1200000))})
	m.observe(start.Add(19*time.Second), partition{Commit: ptr(int64(11400)), End: ptr(int64(1200950))})
	m.observe(start.Add(70*time.Second), partition{Commit: ptr(int64(42000)), End: ptr(int64(1203500))})
	rate, coverage, known := m.commit.average()
	if !known || coverage != 60 || rate != 600 {
		t.Fatalf("rate=%v coverage=%v known=%v", rate, coverage, known)
	}
	if eta := m.eta(ptr(int64(1100000)), true); eta != "33m20s" {
		t.Fatalf("ETA=%s", eta)
	}
	m.observe(start.Add(75*time.Second), partition{Commit: nil, End: ptr(int64(1203900))})
	m.observe(start.Add(80*time.Second), partition{Commit: ptr(int64(48000)), End: ptr(int64(1204000))})
	if _, ok := m.commit.instant(); ok {
		t.Fatal("averaged across unknown commit")
	}
	m.observe(start.Add(90*time.Second), partition{Commit: ptr(int64(1)), End: ptr(int64(1204500))})
	if _, ok := m.commit.instant(); ok || m.discontinuities != 1 {
		t.Fatal("commit decrease failed to reset consumption history")
	}
	if progress, ok := m.progress(ptr(int64(1204499))); !ok || progress != 0 {
		t.Fatalf("baseline did not reset: %f, %v", progress, ok)
	}
}

func TestRateChartFramesSamplesAndShowsGaps(t *testing.T) {
	values := []ratePoint{{rate: 0, known: true}, {}, {rate: 0.01, known: true}, {rate: 1, known: true}}
	got := chart(values, 7)
	want := "Scale 1.0/s  Peak 1.0/s\n┌─────┐\n│    █│\n│    █│\n│    █│\n│   ▁█│\n└─━───┘\n"
	if got != want {
		t.Fatalf("rate chart:\n%s\nwant:\n%s", got, want)
	}
	waiting := "Waiting for rate samples\n┌───┐\n│   │\n│   │\n│   │\n│   │\n└───┘\n"
	for _, history := range [][]ratePoint{nil, {{}, {}}} {
		if got := chart(history, 5); got != waiting {
			t.Fatalf("waiting chart for %v:\n%s", history, got)
		}
	}
}

func TestConfigDurationExact(t *testing.T) {
	for input, want := range map[string]string{"1h30m": "5400000", "1.5s": "1500", "1w2d": "777600000", "-1": "-1", "9223372036854775807ms": "9223372036854775807", "0.5ms0.5ms": "1"} {
		got, err := configValue("retention.ms", input)
		if err != nil || got != want {
			t.Errorf("%q → %q (%v), want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"1.1ms", "9223372036854775808ms", "-2", "."} {
		if got, err := configValue("retention.ms", input); err == nil {
			t.Errorf("%q accepted as %q", input, got)
		}
	}
}

func TestCSVAndFullTree(t *testing.T) {
	d, warnings, err := parseCSV(context.Background(), "id;値;empty\n1;\"a\nb\";\n2;b\n", profile{delimiter: ";", header: true}, false, 200, maxPreviewOutput)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("CSV warnings=%v err=%v", warnings, err)
	}
	if d.rows[0][1] != "a\\nb" || csvField(d.rows[0], 2) != "" || csvField(d.rows[1], 2) != "∅" {
		t.Fatalf("CSV fields: %#v", d.rows)
	}
	if len(d.names) != 3 || d.names[1] != "値" {
		t.Fatal(d.names)
	}
	values := make([]any, 10101)
	values[len(values)-1] = "needle"
	tree, _ := buildTree(context.Background(), "root", values, true)
	var verify func(*payloadNode) bool
	verify = func(n *payloadNode) bool {
		if len(n.children) > 100 {
			t.Fatal("unbounded tree fanout")
		}
		found := strings.Contains(n.search, "needle")
		for _, child := range n.children {
			if child.parent != n {
				t.Fatal("lost search path")
			}
			if verify(child) {
				found = true
			}
		}
		return found
	}
	if !verify(tree) {
		t.Fatal("nested value could not be searched")
	}
}

func TestKafkaIntegration(t *testing.T) {
	address := os.Getenv("KATUI_TEST_BROKER")
	if address == "" {
		t.Skip("set KATUI_TEST_BROKER to a disposable Kafka broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	opts := options{brokers: []string{address}, cluster: address, backend: "api", timeout: 5 * time.Second}
	client, err := opts.kafka("")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	b := &broker{options: opts, client: client, admin: kadm.NewClient(client), disk: kadm.NewClient(client)}
	topic := fmt.Sprintf("katui-test-%d", time.Now().UnixNano())
	group := topic + "-group"
	key := partKey{topic, 0}
	plan, err := b.prepareAdmin(ctx, adminInput{operation: opCreate, target: topic, partitions: 1, replicas: 1})
	if err != nil {
		t.Fatalf("review create: %v", err)
	}
	md, err := b.metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := md[topic]; ok {
		t.Fatal("validate-only create created a topic")
	}
	if sent, err := b.applyAdmin(ctx, plan); err != nil || !sent {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, e := opts.kafka("")
		if e != nil {
			t.Error(e)
			return
		}
		defer conn.Close()
		admin := kadm.NewClient(conn)
		_, _ = admin.DeleteGroup(cleanup, group)
		_, _ = admin.DeleteTopic(cleanup, topic)
	})
	payload := strings.Repeat("a", 70000) + "MATCH"
	records := []*kgo.Record{{Topic: topic, Value: []byte("filtered")}, {Topic: topic, Value: []byte(payload)}, {Topic: topic, Value: nil}}
	if err := client.ProduceSync(ctx, records...).FirstErr(); err != nil {
		t.Fatal(err)
	}
	var offsets kadm.Offsets
	offsets.Add(kadm.Offset{Topic: topic, Partition: 0, At: 1, LeaderEpoch: -1})
	seedTestGroup(t, ctx, b.admin, group, offsets)
	g := b.groupSnapshots(ctx, []string{group})[group]
	if g.Error != "" || len(g.Partitions) != 1 || g.Partitions[0].lag() == nil || *g.Partitions[0].lag() != 2 {
		t.Fatalf("bad group snapshot %#v", g)
	}
	store := newPayloadStore(t.TempDir(), payloadQuota)
	var updates []readerUpdate
	readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	b.readRecords(readCtx, &readerSession{wake: make(chan struct{}, 1)}, topic, consumeOptions{partitions: []int32{0}, start: 2, value: "match"}, store, func(u readerUpdate) { updates = append(updates, u) })
	stop()
	if len(updates) == 0 {
		t.Fatal("reader did not emit any state")
	}
	last := updates[len(updates)-1]
	if last.state != "COMPLETE" || last.scanned != 3 || last.matched != 1 {
		t.Fatalf("reader final state %#v", last)
	}
	for _, event := range updates {
		for _, r := range event.records {
			value, err := r.full()
			if err != nil || string(value) != payload || len(r.value) != 65536 {
				t.Fatalf("retained value length=%d error=%v", len(value), err)
			}
		}
	}
	old, err := b.storedOffsets(ctx, group)
	if err != nil || old[key] != 1 {
		t.Fatalf("reader changed group offsets: %v %v", old, err)
	}
	reset := adminInput{operation: opReset, target: group, selected: []partKey{key}, mode: 0, value: "2099-01-01T00:00:00Z"}
	plan, err = b.prepareAdmin(ctx, reset)
	if err != nil || len(plan.changes) != 1 || !plan.changes[0].fallback || plan.changes[0].target != 3 {
		t.Fatalf("review offset reset: %+v %v", plan.changes, err)
	}
	if sent, err := b.applyAdmin(ctx, plan); err != nil || !sent {
		t.Fatalf("reset: %v", err)
	}
	if sent, err := b.applyAdmin(ctx, plan); err == nil || sent {
		t.Fatal("changed commits passed stale review")
	}
	plan, err = b.prepareAdmin(ctx, adminInput{operation: opConfig, target: topic, settings: map[string]*string{"retention.ms": ptr("5400000")}})
	if err != nil {
		t.Fatalf("review config: %v", err)
	}
	if sent, err := b.applyAdmin(ctx, plan); err != nil || !sent {
		t.Fatalf("alter config: %v", err)
	}
	configs, err := b.settings(ctx, topic)
	if err != nil || configs["retention.ms"].value != "5400000" {
		t.Fatalf("config result: %v %v", configs["retention.ms"], err)
	}
	t.Run("classic rebalance", func(t *testing.T) {
		opts, err := b.kafkaOptions("")
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := kgo.NewClient(append(opts, kgo.ConsumerGroup(group), kgo.ConsumeTopics(topic), kgo.DisableAutoCommit())...)
		if err != nil {
			t.Fatal(err)
		}
		defer consumer.Close()
		joinCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		pollDone := make(chan struct{})
		go func() { defer close(pollDone); consumer.PollFetches(joinCtx) }()
		defer func() { stop(); <-pollDone }()
		var review adminPlan
		for joinCtx.Err() == nil {
			review, err = b.prepareAdmin(joinCtx, adminInput{operation: opRebalance, target: group})
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("review rebalance: %v", err)
		}
		if _, err := b.prepareAdmin(ctx, reset); err == nil {
			t.Fatal("active group passed reset review")
		}
		if sent, err := b.applyAdmin(ctx, review); err != nil || !sent {
			t.Fatalf("remove reviewed member: %v", err)
		}
		stored, err := b.storedOffsets(ctx, group)
		if err != nil || stored[key] != 3 {
			t.Fatalf("rebalance changed committed offsets: %v, %v", stored, err)
		}
	})

	plan, err = b.prepareAdmin(ctx, adminInput{operation: opDeleteGroup, target: group})
	if err != nil {
		t.Fatalf("review group delete: %v", err)
	}
	if sent, err := b.applyAdmin(ctx, plan); err != nil || !sent {
		t.Fatalf("group delete: %v", err)
	}
	plan, err = b.prepareAdmin(ctx, adminInput{operation: opDeleteTopic, target: topic})
	if err != nil {
		t.Fatalf("review topic delete: %v", err)
	}
	if sent, err := b.applyAdmin(ctx, plan); err != nil || !sent {
		t.Fatalf("topic delete: %v", err)
	}
}

func seedTestGroup(t *testing.T, ctx context.Context, admin *kadm.Client, group string, offsets kadm.Offsets) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		err := admin.CommitAllOffsets(ctx, group, offsets)
		if err == nil {
			return
		}
		if !errors.Is(err, kerr.CoordinatorNotAvailable) && !errors.Is(err, kerr.CoordinatorLoadInProgress) && !errors.Is(err, kerr.NotCoordinator) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(errors.Join(err, ctx.Err()))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestLocalProtobufNestedTypeAndConfluentFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "root.proto")
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(path, `syntax="proto3"; package demo; import "inside.proto"; message Outer { message Value { int64 count=1; } demo.Helper helper=1; }`)
	imports := filepath.Join(dir, "imports")
	if err := os.Mkdir(imports, 0700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(imports, "inside.proto"), `syntax="proto3"; package demo; message Helper { string name=1; }`)
	p := protoProfile{Path: path, Include: []string{imports}, Message: "demo.Outer.Value", Framing: "confluent"}
	schema, err := loadSchema(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0, 0, 0, 0, 9, 4, 0, 0, 8, 150, 1}
	data, warning, err := schema.decode(&p, frame)
	if err != nil || warning != "" || !strings.Contains(string(data), `"150"`) {
		t.Fatalf("decode result %s, %q, %v", data, warning, err)
	}
	if _, _, err := schema.decode(&p, []byte{0, 0, 0, 0, 9, 0, 8, 150, 1}); err == nil {
		t.Fatal("incorrect Confluent message path accepted")
	}
	write(filepath.Join(dir, "inside.proto"), `syntax="proto3"; package demo; message Helper { uint64 other=1; }`)
	if _, err := loadSchema(context.Background(), p); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting import not reported: %v", err)
	}
}
