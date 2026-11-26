package katui

import (
	"errors"
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
	}
	for _, tt := range tests {
		o, err := arguments(tt.args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if o.noCache != tt.noCache {
			t.Errorf("%v: noCache = %v, want %v", tt.args, o.noCache, tt.noCache)
		}
		if len(o.brokers) != 0 {
			t.Errorf("%v: unexpected implicit brokers %v", tt.args, o.brokers)
		}
	}
}

func TestSavedClusterCredentials(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	options, err := arguments([]string{"-b", "b.example.net:9093,a.example.net:9093"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	options.security = &securityConfig{TLS: true}
	options.security.SASL.Mechanism = "SCRAM-SHA-256"
	options.security.SASL.Username = "test-user"
	options.security.SASL.Password = "fixture-password"
	profiles := map[string]connectionProfile{
		"ask":        options.connectionProfile(false),
		"remembered": options.connectionProfile(true),
	}
	if err := saveConnectionProfiles(profiles); err != nil {
		t.Fatal(err)
	}
	saved, err := loadConnectionProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if pass := saved["ask"].Security.SASL.Password; pass != "" || !saved["ask"].PromptPassword {
		t.Fatal("a session-only password was saved")
	}
	if pass := saved["remembered"].Security.SASL.Password; pass != options.security.SASL.Password {
		t.Fatal("the opted-in password was not saved")
	}
	fresh, err := arguments(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.selectConnection(saved["ask"]); !errors.Is(err, errProfilePassword) {
		t.Fatalf("password prompt error = %v", err)
	}
	if err := fresh.selectConnection(saved["remembered"]); err != nil {
		t.Fatal(err)
	}
	if fresh.cluster != "a.example.net:9093,b.example.net:9093" || !fresh.noCache {
		t.Fatalf("profile did not preserve inspector key or authenticated cache policy: %q, %t", fresh.cluster, fresh.noCache)
	}
	if err := run(t.Context(), []string{"--once", "--cluster", "ask"}); !errors.Is(err, errProfilePassword) {
		t.Fatalf("noninteractive lookup = %v", err)
	}
}
