package katui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestLiveReaderDeliversLargeBatchWhileAnotherPartitionStaysBusy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	b, cluster, _ := migrationBroker(t)
	producer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...),
		kgo.RecordPartitioner(kgo.ManualPartitioner()), kgo.ProducerBatchMaxBytes(4<<20),
		kgo.ProducerBatchCompression(kgo.NoCompression()))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	configs, err := b.admin.AlterTopicConfigs(ctx, []kadm.AlterConfig{
		{Op: kadm.SetConfig, Name: "max.message.bytes", Value: ptr("4194304")},
	}, migrationTopic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configs.On(migrationTopic, func(r *kadm.AlterConfigsResponse) error { return r.Err }); err != nil {
		t.Fatal(err)
	}

	busy := func() *kgo.Record { return &kgo.Record{Topic: migrationTopic, Partition: 0, Value: []byte("busy")} }
	large := bytes.Repeat([]byte{'x'}, (2<<20)+1)
	if err := producer.ProduceSync(ctx, busy(), &kgo.Record{Topic: migrationTopic, Partition: 1, Value: large}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	store := newPayloadStore(t.TempDir(), payloadQuota)
	defer store.close()
	var delivered bool
	opts := consumeOptions{partitions: []int32{0, 1}, start: startBeginning, follow: true, limit: 10}
	b.readRecords(ctx, &readerSession{}, migrationTopic, opts, store, func(update readerUpdate) {
		for _, record := range update.records {
			if record.id.Partition == 1 {
				full, err := record.full()
				if err != nil || !bytes.Equal(large, full) || record.id.offset != 0 {
					t.Fatal("large batch was not delivered intact at offset 0", err)
				}
				delivered = true
				cancel()
				return
			}
		}
		if len(update.records) > 0 && update.state == "READING" {
			if err := producer.ProduceSync(ctx, busy()).FirstErr(); err != nil {
				t.Fatal(err)
			}
		}
	})
	if !delivered {
		t.Fatal("partition 0 consumed the available fetch budget every time, starving partition 1")
	}
}

func TestReaderTimestampRange(t *testing.T) {
	if _, err := timestamp("1969-12-31T23:59:59.999Z"); err == nil {
		t.Fatal("timestamp -1 ms would be interpreted as Kafka's latest-offset sentinel")
	}
	if at, err := timestamp("1970-01-01T00:00:00Z"); err != nil || at.UnixMilli() != 0 {
		t.Fatalf("epoch timestamp: got %v, err=%v", at, err)
	}
}

func TestPayloadStoreQuota(t *testing.T) {
	dir := t.TempDir()
	value := bytes.Repeat([]byte{'x'}, 65_537)
	store := newPayloadStore(dir, int64(len(value)))
	record := &kgo.Record{Topic: "topic", Value: value}
	first, err := saveRecord(record, store)
	if err != nil {
		t.Fatal(err)
	}
	if full, err := first.full(); err != nil || !bytes.Equal(full, value) {
		t.Fatalf("full payload: got %d bytes, err=%v", len(full), err)
	}
	second, err := saveRecord(record, store)
	if err != nil || !second.truncated() || !bytes.Equal(second.value, value[:65_536]) {
		t.Fatalf("quota should retain a preview: got %v, err=%v", second != nil, err)
	}
	if full, err := second.full(); !errors.Is(err, errPayloadQuota) || full != nil {
		t.Fatalf("missing original should report the quota: got %d bytes, err=%v", len(full), err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("expected one spill file: got %d, err=%v", len(entries), err)
	}
	first.releasePayload()
	first.releasePayload()
	third, err := saveRecord(record, store)
	if err != nil || third.fullPath == "" {
		t.Fatalf("deleting a spill file should make space for the next record: got %v, err=%v", third != nil, err)
	}
	store.close()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload store directory was not removed: %v", err)
	}
	if _, err := saveRecord(record, store); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("payload store must reject writes after shutdown: %v", err)
	}
}

func TestKafkaFiniteReaderIntegration(t *testing.T) {
	address := os.Getenv("KATUI_TEST_BROKER")
	if address == "" {
		t.Skip("set KATUI_TEST_BROKER to a disposable Kafka broker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o := options{brokers: []string{address}, cluster: address, backend: "api", timeout: 5 * time.Second}
	opts, err := o.kafkaOptions("")
	if err != nil {
		t.Fatal(err)
	}
	client, err := kgo.NewClient(append(opts,
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.ProducerBatchMaxBytes(4<<20),
		kgo.ProducerBatchCompression(kgo.NoCompression()),
	)...)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	admin := kadm.NewClient(client)
	b := &broker{options: o, admin: admin}
	topic := fmt.Sprintf("katui-test-finite-%d", time.Now().UnixNano())
	if _, err := admin.CreateTopic(ctx, 2, 1, map[string]*string{"max.message.bytes": ptr("4194304")}, topic); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.DeleteTopic(cleanup, topic); err != nil {
			t.Error(err)
		}
	}()
	// The second partition's batch exceeds the per-partition fetch limit.
	// Kafka only guarantees returning such a batch if it is the first
	// nonempty partition in the fetch, so both partitions need a turn.
	value := bytes.Repeat([]byte{'x'}, (2<<20)+1)
	if err := client.ProduceSync(ctx,
		&kgo.Record{Topic: topic, Partition: 0, Value: []byte("small")},
		&kgo.Record{Topic: topic, Partition: 1, Value: value},
	).FirstErr(); err != nil {
		t.Fatal(err)
	}
	var last readerUpdate
	var records []*storedRecord
	store := newPayloadStore(t.TempDir(), payloadQuota)
	b.readRecords(ctx, &readerSession{wake: make(chan struct{}, 1)}, topic, consumeOptions{partitions: []int32{0, 1}, start: 2}, store, func(update readerUpdate) {
		last = update
		records = append(records, update.records...)
	})
	if last.state != "COMPLETE" || last.scanned != 2 || last.matched != 2 || len(records) != 2 {
		t.Fatalf("finite reader final state=%#v, retained records=%d", last, len(records))
	}
	for _, record := range records {
		if record.id.Partition == 1 {
			if got, err := record.full(); err != nil || !bytes.Equal(got, value) {
				t.Fatalf("large batch record: got %d bytes, err=%v", len(got), err)
			}
			return
		}
	}
	t.Fatal("second partition record was unavailable")
}

func TestExactReaderOffsetIsInclusiveAndValidatesBounds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	b, _, producer := migrationBroker(t)
	seedMigrationOffsets(t, ctx, b, producer)
	store := newPayloadStore(t.TempDir(), payloadQuota)
	defer store.close()
	read := func(offset int64) (readerUpdate, []recordID) {
		var last readerUpdate
		var records []recordID
		opts := consumeOptions{start: startOffset, offset: offset, partitions: []int32{0, 1}}
		b.readRecords(ctx, &readerSession{wake: make(chan struct{}, 1)}, migrationTopic, opts, store, func(update readerUpdate) {
			last = update
			for _, r := range update.records {
				records = append(records, r.id)
			}
		})
		return last, records
	}
	last, records := read(1)
	want := []recordID{{partKey{migrationTopic, 0}, 1}, {partKey{migrationTopic, 1}, 1}}
	if last.state != "COMPLETE" || !reflect.DeepEqual(records, want) {
		t.Fatalf("exact reader state=%+v, records=%v, want %v", last, records, want)
	}
	if last, records := read(2); last.state != "COMPLETE" || len(records) != 0 {
		t.Fatalf("end is a valid empty snapshot: state=%+v, records=%v", last, records)
	}
	var offsets kadm.Offsets
	offsets.AddOffset(migrationTopic, 0, 1, -1)
	res, err := b.admin.DeleteRecords(ctx, offsets)
	if err := errors.Join(err, res.Error()); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{0, 3} {
		last, records := read(offset)
		if last.state != "ERROR" || !strings.Contains(last.info, errReaderOffset.Error()) || !strings.Contains(last.info, "events:0") || len(records) != 0 {
			t.Fatalf("out-of-range start %d: state=%+v, records=%v", offset, last, records)
		}
	}
}
