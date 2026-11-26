package katui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
)

func writePropertiesTestCA(t *testing.T, dir string) (string, tls.Certificate) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return path, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func importTestProperties(t *testing.T, dir, contents string) (connectionProfile, error) {
	t.Helper()
	path := filepath.Join(dir, "client.properties")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return readKafkaProperties(path)
}

func TestImportKafkaProperties(t *testing.T) {
	dir := t.TempDir()
	ca, _ := writePropertiesTestCA(t, dir)
	cfg := "# Demo\r\nbootstrap.servers: broker1.test:9093,\\\r\n  broker2.test:9093\r\n" +
		"security.protocol=SASL_SSL\nsasl.mechanism SCRAM-SHA-512\n" +
		"sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required \\\n username=\"test user\" password=\"s\\u00e9cret;=!\";\n" +
		"ssl.truststore.type=PEM\nssl.truststore.location=ca.pem\nssl.endpoint.identification.algorithm=https\n"
	profile, err := importTestProperties(t, dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := profile.Security
	if !slices.Equal(profile.Brokers, []string{"broker1.test:9093", "broker2.test:9093"}) || s == nil || !s.TLS || !s.CAFileOnly || s.CAFile != ca || s.SASL.Mechanism != "SCRAM-SHA-512" || s.SASL.Username != "test user" || s.SASL.Password != "sécret;=!" {
		t.Fatal("incomplete Kafka settings import")
	}
	tls, err := s.tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	cert, err := os.ReadFile(ca)
	if err != nil || !roots.AppendCertsFromPEM(cert) || !tls.RootCAs.Equal(roots) || tls.InsecureSkipVerify {
		t.Fatal("import changed the trust roots or hostname verification")
	}
	for _, tc := range []struct {
		input     string
		tls       bool
		mechanism string
	}{
		{"bootstrap.servers=localhost:9092", false, ""},
		{"security.protocol=SSL", true, ""},
		{"security.protocol=SASL_SSL\nsasl.mechanism=PLAIN\nsasl.username=test", true, "PLAIN"},
		{"security.protocol=SASL_PLAINTEXT\nsasl.mechanism=SCRAM-SHA-256", false, "SCRAM-SHA-256"},
	} {
		p, err := importTestProperties(t, dir, tc.input)
		if err != nil {
			t.Fatal(err)
		}
		if p.Security == nil {
			if tc.tls || tc.mechanism != "" {
				t.Fatal("missing security settings")
			}
			continue
		}
		if p.Security.TLS != tc.tls || p.Security.SASL.Mechanism != tc.mechanism {
			t.Fatal("incorrect security mode")
		}
		if len(p.Brokers) != 0 {
			t.Fatal("missing bootstrap.servers supplied a broker")
		}
	}
}

func TestKafkaPropertiesRejectUnsupportedSettings(t *testing.T) {
	for name, data := range map[string]string{
		"empty":                         "# no settings\n",
		"unknown protocol":              "security.protocol=sensitive.value",
		"unknown key":                   "bootstrap.servers=broker\nsensitive.value=test",
		"malformed":                     "security.protocol=SSL\nsasl.password=\\uZZZZsensitive.value",
		"invalid broker":                "bootstrap.servers=sensitive.value@example.test",
		"unresolved provider":           "bootstrap.servers=${env:sensitive.value}",
		"surrogate in password":         "security.protocol=SASL_SSL\nsasl.mechanism=PLAIN\nsasl.password=\\uD83D\\uDE80",
		"TLS downgrade":                 "ssl.truststore.type=PEM\nssl.truststore.location=sensitive.value",
		"SASL downgrade":                "sasl.password=sensitive.value",
		"plain credentials without TLS": "security.protocol=SASL_PLAINTEXT\nsasl.mechanism=PLAIN",
		"unsupported mechanism":         "security.protocol=SASL_SSL\nsasl.mechanism=GSSAPI",
		"default JKS":                   "security.protocol=SSL\nssl.truststore.location=client.jks",
		"PKCS12":                        "security.protocol=SSL\nssl.keystore.type=PKCS12\nssl.keystore.location=sensitive.value",
		"encrypted key":                 "security.protocol=SSL\nssl.key.password=sensitive.value",
		"missing PEM path":              "security.protocol=SSL\nssl.truststore.type=PEM",
		"no hostname verification":      "security.protocol=SSL\nssl.endpoint.identification.algorithm=",
		"ambiguous credentials":         "security.protocol=SASL_SSL\nsasl.mechanism=PLAIN\nsasl.username=sensitive.value\nsasl.jaas.config=x",
	} {
		t.Run(name, func(t *testing.T) {
			profile, err := importTestProperties(t, t.TempDir(), data)
			if err == nil || strings.Contains(err.Error(), "sensitive.value") || profile.Security != nil || len(profile.Brokers) != 0 {
				t.Fatal("expected an empty draft and a value-free error")
			}
		})
	}
}

func TestImportedPropertiesAuthenticateToKafka(t *testing.T) {
	dir := t.TempDir()
	_, serverCert := writePropertiesTestCA(t, dir)
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1), kfake.SeedTopics(2, "parcels"),
		kfake.TLS(&tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}),
		kfake.EnableSASL(), kfake.Superuser("SCRAM-SHA-256", "reader", "fixture"),
	)
	t.Cleanup(cluster.Close)
	p, err := importTestProperties(t, dir, "bootstrap.servers="+cluster.ListenAddrs()[0]+`
security.protocol=SASL_SSL
sasl.mechanism=SCRAM-SHA-256
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="reader" password="fixture";
ssl.truststore.type=PEM
ssl.truststore.location=ca.pem
`)
	if err != nil {
		t.Fatal(err)
	}
	b := &broker{options: options{timeout: 5 * time.Second}}
	if err := b.selectConnection(p); err != nil {
		t.Fatal(err)
	}
	if err := b.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	topics, err := b.metadata(ctx, "parcels")
	if err != nil {
		t.Fatal(err)
	}
	if len(topics["parcels"].Partitions) != 2 {
		t.Fatal("authenticated client did not read the seeded topic")
	}
}
