# Katui

Katui is a Go terminal application for browsing Kafka topics, monitoring consumer groups, reading records, inspecting payloads, and reviewing Kafka administration changes.

## Build

Use Go 1.26.11 or newer. From the repository root:

```sh
go build -o katui .
./katui -b localhost:9092
```

The executable entry point is `main.go`. Kafka operations, the terminal UI, and their tests live in `internal/katui`. Kafka requests and record decoding use franz-go, the terminal UI uses tview and tcell, and local Protobuf support uses protocompile and the Go Protobuf runtime.

## Connect

Supply the brokers for your cluster with `-b`. The default is `localhost:9092`. Kafka clients need network access to the advertised broker addresses as well as the bootstrap address.

```sh
./katui -b broker.example.net:9092 -i 2
./katui -b broker.example.net:9092 -t orders -p 1
./katui -b broker.example.net:9092 -g billing -t orders
./katui -b broker.example.net:9092 --once -g billing
./katui -b broker.example.net:9092 --once -t orders
```

`--help` prints the available options. Interactive mode uses terminal stdin and stdout. `--once` writes JSON and can be redirected. Without a group or topic, its output is a list of group names.

### TLS and SASL

Pass a JSON file with `--client-config`. Omit fields you do not use. TLS uses system trust roots plus any PEM certificates in `ca_file`. Optional `cert_file` and `key_file` enable client authentication. `server_name` can set the TLS name explicitly. The supported SASL mechanisms are `PLAIN`, `SCRAM-SHA-256`, and `SCRAM-SHA-512`. `PLAIN` requires TLS. Each connection JSON or PEM file can be up to 1 MiB.

```json
{
  "tls": true,
  "ca_file": "/path/to/ca.pem",
  "sasl": {
    "mechanism": "SCRAM-SHA-512",
    "username": "username",
    "password": "password"
  }
}
```

`--size-client-config` accepts the same format. If supplied, it configures the separate log-directory client. Keep connection JSON files private.

### Kafka CLI backend

```sh
./katui --backend cli -b localhost:9092 \
  --kafka-cli /opt/kafka/bin/kafka-consumer-groups.sh \
  --command-config /path/to/connection.properties
```

The CLI backend supports consumer-group monitoring. The executable can also be on PATH or under `$KAFKA_HOME/bin`. Topics, the record reader, and administration require the API backend.

## Use

- `1`, `2`, and `3` switch workspaces. `/` searches, Enter opens or returns focus to results, and Esc goes back. `?` shows shortcuts and Ctrl+P opens application commands.
- In a topic or partition, `c` opens the reader form. Choose partitions, a start position, and whether to continue following new records. Alt+G starts the reader.
- In Messages, `i` focuses the inspector, `b` returns to records, and `m` changes payload format. Space checks the selected record and `w` compares checked records. F4 opens complete retained payloads. In Full view, `/` searches and `n` and `p` move between matches. Full view renders up to 8 MiB, with a 2 MiB input limit for structured formats and up to 10,000 search matches.
- The record reader uses partition fetches without group membership or offset commits. Kafka offsets are positions, so retained-count and rate displays are estimates.
- Topic administration is under `a`, `n`, and `d`. Group administration is under `a`. Review displays the proposed effect. Type the exact target name and use Alt+A or the Apply button. Kafka writes make one request after revalidation. A timeout or partial response may have changed state and requires a fresh review.
- Preferences live under `$XDG_CONFIG_HOME/katui/inspectors.json`, or `~/.config/katui/inspectors.json`. A preference file can be selected with `--inspector-config`. Local monitoring snapshots are disabled by `--no-cache` and whenever a connection configuration file is supplied. Large retained values use up to 128 MiB of temporary disk space per Katui instance. After that, the reader keeps previews and marks complete payloads that could not be stored. Temporary files are cleaned up on normal exit.
- Copy actions request clipboard transfer through the terminal. The terminal or multiplexer must support clipboard requests. The app reports the request without claiming delivery.

## Verification

```sh
go test -race ./...
go vet ./...
govulncheck ./...
```

The Kafka integration test is opt-in. Use a disposable cluster and credentials with permissions to create, configure, produce to, and delete temporary topics and manage temporary test groups.

```sh
KATUI_TEST_BROKER=localhost:9092 go test -race ./...
```

The test checks validate-only topic creation, finite filtered reading with a retained large payload, unchanged group commits after reading, timestamp reset with a missing-timestamp fallback, rejection of a stale reset review, staged configuration, classic group rebalance, and topic and group deletion.

The implementation uses a keyboard-first terminal layout. The topic-to-group picker occupies the browser pane and retains its location on Back. Copy Value uses the complete retained value. It does not implement a separate mouse text-selection model.
