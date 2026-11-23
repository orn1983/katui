package katui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestReaderFetches(t *testing.T) {
	leaders := map[int32][]int32{7: {3, 9}}
	wantFinite := []readerFetch{{7, []int32{3}}, {7, []int32{9}}}
	if got := readerFetches(leaders, false); !reflect.DeepEqual(got, wantFinite) {
		t.Fatalf("finite fetching must give each partition its own byte budget: got %v, want %v", got, wantFinite)
	}
	wantFollowing := []readerFetch{{7, []int32{3, 9}}}
	if got := readerFetches(leaders, true); !reflect.DeepEqual(got, wantFollowing) {
		t.Fatalf("following can fetch multiple partitions per broker: got %v, want %v", got, wantFollowing)
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
