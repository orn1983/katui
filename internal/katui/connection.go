package katui

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

const connectionFileLimit = 1 << 20

type securityConfig struct {
	TLS        bool   `json:"tls"`
	CAFile     string `json:"ca_file"`
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`
	ServerName string `json:"server_name"`
	SASL       struct {
		Mechanism string `json:"mechanism"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	} `json:"sasl"`
}

func loadSecurityConfig(path string) (*securityConfig, error) {
	data, err := readLimited(expandHome(path), connectionFileLimit)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c *securityConfig
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("connection JSON: %w", err)
	}
	if c == nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("connection JSON must contain a single object")
	}
	return c, nil
}

func (c *securityConfig) tlsConfig() (*tls.Config, error) {
	if !c.TLS && c.CAFile == "" && c.CertFile == "" && c.KeyFile == "" && c.ServerName == "" {
		return nil, nil
	}
	t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := readLimited(expandHome(c.CAFile), connectionFileLimit)
		if err != nil {
			return nil, err
		}
		t.RootCAs, err = x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !t.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no PEM certificates")
		}
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, errors.New("cert_file and key_file must be configured together")
	}
	if c.CertFile != "" {
		certPEM, err := readLimited(expandHome(c.CertFile), connectionFileLimit)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readLimited(expandHome(c.KeyFile), connectionFileLimit)
		if err != nil {
			return nil, err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		t.Certificates = []tls.Certificate{cert}
	}
	return t, nil
}

func (o options) kafkaOptions(path string) ([]kgo.Opt, error) {
	opts := []kgo.Opt{
		kgo.SeedBrokers(o.brokers...),
		kgo.ClientID("katui"),
		kgo.DialTimeout(o.timeout),
		kgo.RequestRetries(0),
		kgo.RequestTimeoutOverhead(o.timeout),
	}
	if path == "" {
		return opts, nil
	}
	c, err := loadSecurityConfig(path)
	if err != nil {
		return nil, err
	}
	t, err := c.tlsConfig()
	if err != nil {
		return nil, err
	}
	if t != nil {
		opts = append(opts, kgo.DialTLSConfig(t))
	}
	switch strings.ToUpper(c.SASL.Mechanism) {
	case "":
		if c.SASL.Username != "" || c.SASL.Password != "" {
			return nil, errors.New("specify a SASL mechanism")
		}
	case "PLAIN":
		if t == nil {
			return nil, errors.New("SASL PLAIN requires TLS")
		}
		opts = append(opts, kgo.SASL(plain.Auth{User: c.SASL.Username, Pass: c.SASL.Password}.AsMechanism()))
	case "SCRAM-SHA-256":
		opts = append(opts, kgo.SASL(scram.Auth{User: c.SASL.Username, Pass: c.SASL.Password}.AsSha256Mechanism()))
	case "SCRAM-SHA-512":
		opts = append(opts, kgo.SASL(scram.Auth{User: c.SASL.Username, Pass: c.SASL.Password}.AsSha512Mechanism()))
	default:
		return nil, errors.New("unsupported SASL mechanism")
	}
	return opts, nil
}

func (o options) kafka(path string) (*kgo.Client, error) {
	opts, err := o.kafkaOptions(path)
	if err != nil {
		return nil, err
	}
	return kgo.NewClient(opts...)
}
