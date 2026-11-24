package katui

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

const connectionFileLimit = 1 << 20

type connectionProfile struct {
	Brokers        []string        `json:"brokers"`
	ClientConfig   string          `json:"client_config,omitempty"`
	CommandConfig  string          `json:"command_config,omitempty"`
	Security       *securityConfig `json:"security,omitempty"`
	PromptPassword bool            `json:"prompt_password,omitempty"`
}

var errProfilePassword = errors.New("this cluster needs a password; open the interactive setup or supply --client-config")

func connectionProfilesPath(backend string) string {
	filename := "connections.json"
	if backend == "cli" {
		filename = "cli-connections.json"
	}
	return defaultPath("XDG_CONFIG_HOME", ".config", filename)
}

func loadConnectionProfiles(backend string) (map[string]connectionProfile, error) {
	data, err := readLimited(connectionProfilesPath(backend), connectionFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]connectionProfile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var profiles map[string]connectionProfile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("saved clusters: %w", err)
	}
	if profiles == nil {
		return nil, errors.New("saved clusters must be a JSON object")
	}
	return profiles, nil
}

func saveConnectionProfiles(backend string, profiles map[string]connectionProfile) error {
	data, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > connectionFileLimit {
		return errors.New("saved clusters exceed the file size limit")
	}
	return atomicFile(connectionProfilesPath(backend), data)
}

func (o *options) selectConnection(p connectionProfile) error {
	if o.backend != "cli" && o.config == "" && p.PromptPassword {
		return errProfilePassword
	}
	if err := o.setBrokers(strings.Join(p.Brokers, ",")); err != nil {
		return err
	}
	if o.backend == "cli" {
		if o.commandConfig == "" {
			o.commandConfig = p.CommandConfig
		}
	} else if o.config == "" {
		o.config, o.security = p.ClientConfig, p.Security
	}
	o.setCachePolicy()
	return nil
}

func (o options) connectionProfile(rememberPassword bool) connectionProfile {
	p := connectionProfile{Brokers: o.brokers, ClientConfig: o.config, CommandConfig: o.commandConfig}
	if o.security != nil {
		saved := *o.security
		if saved.SASL.Mechanism != "" && !rememberPassword {
			p.PromptPassword = true
			saved.SASL.Password = ""
		}
		p.Security = &saved
	}
	return p
}

func (o *options) setCachePolicy() {
	// Cache files do not record the identity used to obtain the snapshot.
	o.noCache = o.noCacheFlag || o.config != "" || o.security != nil || o.sizeConfig != "" || o.commandConfig != ""
}

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
	c := o.security
	if path != "" {
		var err error
		c, err = loadSecurityConfig(path)
		if err != nil {
			return nil, err
		}
	}
	if c == nil {
		return opts, nil
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
