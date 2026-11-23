package katui

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKafkaConnectionConfig(t *testing.T) {
	tests := []struct {
		name, config, wantError string
	}{
		{name: "plain without TLS", config: `{"sasl":{"mechanism":"PLAIN"}}`, wantError: "SASL PLAIN requires TLS"},
		{name: "plain with TLS", config: `{"tls":true,"sasl":{"mechanism":"PLAIN"}}`},
		{name: "plain with implicit TLS", config: `{"server_name":"broker.example.com","sasl":{"mechanism":"PLAIN"}}`},
		{name: "scram without TLS", config: `{"sasl":{"mechanism":"SCRAM-SHA-256"}}`},
		{name: "null", config: `null`, wantError: "single object"},
		{name: "unknown field", config: `{"tlss":true}`, wantError: "unknown field"},
		{name: "trailing object", config: `{} {}`, wantError: "single object"},
		{name: "over limit", config: `{}` + strings.Repeat(" ", connectionFileLimit), wantError: "exceeds"},
		{name: "incomplete key pair", config: `{"cert_file":"client.pem"}`, wantError: "configured together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "connection.json")
			if err := os.WriteFile(path, []byte(tt.config), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := (options{}).kafkaOptions(path)
			if tt.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestCachePolicy(t *testing.T) {
	tests := []struct {
		args    []string
		noCache bool
	}{
		{},
		{[]string{"--no-cache"}, true},
		{[]string{"--client-config", "client.json"}, true},
		{[]string{"--size-client-config", "client.json"}, true},
		{[]string{"--backend", "cli", "--command-config", "client.properties"}, true},
	}
	for _, tt := range tests {
		o, err := arguments(tt.args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.noCache != tt.noCache {
			t.Errorf("%v: noCache = %v, want %v", tt.args, o.noCache, tt.noCache)
		}
		if len(o.brokers) != 1 || o.brokers[0] != "localhost:9092" {
			t.Errorf("%v: default brokers = %v", tt.args, o.brokers)
		}
	}
}
