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

Run `./katui` to choose a saved cluster. Use ↑ and ↓ to select a profile, then press Enter or `c` to connect directly. Press `e` to edit the selected profile or `n` to open **Connect to Kafka** for a new one. If a saved profile needs a session password, Katui prompts for the password and Enter connects. Press `4` or choose **Kafka clusters** from `:` to open the Clusters workspace. From there, press `1`, `2`, or `3` to switch directly, or Esc to return to the previous workspace. Connecting to another cluster stops the current reader and opens a fresh view.

With no saved profiles, **Connect to Kafka** opens automatically. Press Alt+A to focus broker addresses or Alt+S for security. Use `c` to connect when outside an input, Alt+C while typing, or Enter on the Connect button. Choose plaintext, TLS, SASL with TLS, SASL plaintext with SCRAM, or an existing client-config JSON file.

Choose **Save profile** and enter a name to keep the settings. With SASL, the profile asks for the password at the next connection unless you also select **Remember password**. A remembered password is written to the local profile JSON with private file permissions. Imported client configuration and TLS files are referenced by path. Profiles live in `$XDG_CONFIG_HOME/katui/connections.json`, falling back to `~/.config/katui/connections.json`. The CLI backend uses `cli-connections.json`.

Use `-b` to supply broker addresses directly or `--cluster NAME` to select a saved profile. `--once` requires one of these options and a complete configuration. While a connection is pending, Esc cancels it, `r` retries from the main view, and `q` opens quit confirmation. Kafka clients need network access to the advertised broker addresses as well as the bootstrap address.

Inspector preferences are keyed by the set of bootstrap addresses and the topic. When moving from an earlier Katui build, pass the same broker addresses with `-b` to reuse its saved profiles.

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

- `1` opens Topics, `2` opens Consumer Groups, `3` opens Messages, and `4` opens Clusters. `/` searches, Enter opens or activates the focused item, and Esc goes back or cancels. Tab and Shift+Tab move focus. `?` shows shortcuts. `:` or Ctrl+P opens Commands.
- Lists, viewers, and dialog buttons use the highlighted lowercase action letters. While typing in a dialog field, use Alt plus the action letter or Tab to the button and press Enter. Field shortcuts labeled Alt jump to fields or toggle options.
- In a topic or partition, `c` opens the reader form. Space checks partitions, `a` selects all, and `n` clears the selection. Choose a start position and whether to follow new records. Press `c` from the partition list or use the Start button.
- In Messages, Enter on a record, `i`, or Tab enters the payload inspector on its text or tree. `b` returns to records. Press `m` to open the format picker. Space checks the selected record, `w` compares checked records, and F4 opens Full view. **Clear message buffer** is in Commands.
- The record reader uses partition fetches without group membership or offset commits. Kafka offsets are positions, so retained-count and rate displays are estimates.
- Topic administration is under `a`, `n`, and `d`. Group administration is under `a`. `r` opens Review. Type the exact target name, then press Enter to focus Apply and Enter again to apply. Alt+A also activates Apply after the target matches. Kafka writes make one request after revalidation. A timeout or partial response may have changed state and requires a fresh review.
- Preferences live under `$XDG_CONFIG_HOME/katui/inspectors.json`, or `~/.config/katui/inspectors.json`. Use `--inspector-config` to select a preferences file. Local monitoring snapshots are disabled by `--no-cache` and whenever a connection configuration file is supplied.

### Inspect, copy, and export payloads

- Full view defaults to 2 MiB of structured input and 8 MiB of rendered text. Set `--structured-input-mib` and `--rendered-output-mib` to choose limits from 1 to 128 MiB. When the larger limits can reveal more, press `l` and then Enter to confirm in place. This raises both limits to 128 MiB for JSON, CSV, Protobuf, or raw text. Esc cancels the prompt or a running load and keeps Full view open. Some decoder calls finish in the background after cancellation.
- Press `/` in the inspector or comparison to open payload search in Full view. From Full view, `/` focuses search, and `n` or `p` move through matches. Full view keeps at most 10,000 matches and one million text lines. CSV displays at most 50,000 rows and 256 columns. Trees display at most 50,000 nodes and 128 levels per record. The view reports when a limit is reached.
- Messages shows temporary storage usage and marks each record **Complete** or **Preview only**. Set `--payload-storage-mib` to change the default 128 MiB quota per instance. `0` disables temporary storage while keeping inline values and previews. A warning appears when a value cannot fit. Katui cleans up temporary payloads on normal exit.
- Drag or hold Shift while using the arrow keys to select text, Ctrl+A to select all displayed text, and Ctrl+C to copy the selection. In a tree, use Tab or click to focus the selected node’s detail pane. The inspector, checked-record comparison, and Full view all support selection.
- In payload text, Left/Right move the cursor one character and Shift+Left/Right extend or shrink the selection. Alt+Left/Right pan eight columns and Ctrl+Left/Right pan half the visible width, with a minimum of eight columns. Panning preserves the cursor and selection. Left/Right still collapse and expand nodes when the tree itself has focus.
- `y` copies the complete retained text value and `v` copies the rendered view. An incomplete full-value copy or rendered-view copy reports an error. `s` saves a complete retained value byte for byte, including binary values. From a comparison, choose a record first. Type a new path on the machine running Katui and press Enter to save.
- Local macOS uses `pbcopy` when available. SSH and the fallback use terminal clipboard transfer (OSC 52), which requires terminal or multiplexer support and has a 64 KiB limit in Katui. Use Export for larger copies over SSH or binary payloads.

## Verification

```sh
go test -race ./...
go vet ./...
govulncheck ./...
```

The default suite covers large payloads, temporary storage, clipboard paths, changed administration reviews, partial replies, timeouts, and snapshot recovery after reconnecting. The Kafka integration test is opt-in. Use a disposable cluster and credentials with permissions to create, configure, produce to, and delete temporary topics and manage temporary test groups.

```sh
KATUI_TEST_BROKER=localhost:9092 go test -race ./...
```

The test checks validate-only topic creation, finite filtered reading with a retained large payload, unchanged group commits after reading, timestamp reset with a missing-timestamp fallback, rejection of a stale reset review, staged configuration, classic group rebalance, and topic and group deletion.
