package katui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func cliFixture(t *testing.T, script string) *broker {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Kafka CLI fallback uses the shell wrapper")
	}
	path := filepath.Join(t.TempDir(), "kafka-consumer-groups.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return &broker{options: options{backend: "cli", cli: path, brokers: []string{"localhost:9092"}}}
}

func TestCLIGroupNamesAndOffsets(t *testing.T) {
	const group = "-monitoring group "
	b := cliFixture(t, `
for arg do
	case "$arg" in
		--list) printf '%s\n' '-monitoring group '; exit 0 ;;
		--state) state=yes ;;
	esac
	last=$arg
done
if [ "$state" = yes ]; then
	printf '%s    localhost:9092  (1)   range   Stable   1\n' "$last"
else
	printf '%s    topic    0    4    9    5    member    /localhost    \n' "$last"
fi
`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	names, err := b.groupNames(ctx)
	if err != nil || !slices.Equal(names, []string{group}) {
		t.Fatalf("group names=%q, err=%v", names, err)
	}
	snapshot := b.cliGroup(ctx, group)
	if snapshot.Error != "" || snapshot.State != "Stable" || len(snapshot.Partitions) != 1 {
		t.Fatalf("group snapshot=%#v", snapshot)
	}
	partition := snapshot.Partitions[0]
	if partition.Topic != "topic" || partition.lag() == nil || *partition.lag() != 5 || partition.Owner != "member" || partition.Client != "" {
		t.Fatalf("group partition=%#v", partition)
	}
}

func TestCLISuccessExitWithKafkaError(t *testing.T) {
	const detail = "group observation unavailable"
	b := cliFixture(t, "printf '\\nError: %s\\n' '"+detail+"'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if names, err := b.groupNames(ctx); err == nil || !strings.Contains(err.Error(), detail) || len(names) != 0 {
		t.Fatalf("Kafka error was not reported: names=%q, err=%v", names, err)
	}
	b.commandConfig = "connection.properties"
	if _, err := b.groupNames(ctx); err == nil || strings.Contains(err.Error(), detail) {
		t.Fatalf("diagnostic should be omitted with command config: %v", err)
	}
}

func TestCLIOutputBuffer(t *testing.T) {
	output := cliOutputBuffer{limit: 5}
	_, _ = output.Write([]byte("abc"))
	if n, err := output.Write([]byte("defg")); n != 4 || err != nil || !output.truncated || output.buf.String() != "abcde" {
		t.Fatalf("buffer was not capped: bytes accepted=%d, err=%v, buf=%q", n, err, output.buf.String())
	}
}
