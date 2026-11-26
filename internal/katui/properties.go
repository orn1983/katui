package katui

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/magiconair/properties"
)

// readKafkaProperties imports a draft only. Certificates are loaded and the
// connection is opened through the same path as a manually entered profile.
func readKafkaProperties(path string) (connectionProfile, error) {
	path, err := filepath.Abs(expandHome(path))
	if err != nil {
		return connectionProfile{}, err
	}
	data, err := readLimited(path, connectionFileLimit)
	if err != nil {
		return connectionProfile{}, err
	}
	loader := properties.Loader{Encoding: properties.ISO_8859_1, DisableExpansion: true}
	// A backslash continues a whole line ending, including both bytes of CRLF.
	props, err := loader.LoadBytes(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
	if err != nil {
		return connectionProfile{}, errors.New("invalid Java properties syntax")
	}
	values := props.Map()
	if len(values) == 0 {
		return connectionProfile{}, errors.New("the file contains no Kafka connection properties")
	}
	protocol := "PLAINTEXT"
	if raw, ok := values["security.protocol"]; ok {
		protocol = strings.ToUpper(strings.TrimSpace(raw))
	}
	var security securityConfig
	switch protocol {
	case "PLAINTEXT", "SASL_PLAINTEXT":
	case "SSL", "SASL_SSL":
		security.TLS = true
	default:
		return connectionProfile{}, errors.New("unsupported security.protocol (use PLAINTEXT, SSL, SASL_PLAINTEXT, or SASL_SSL)")
	}
	usesSASL := strings.HasPrefix(protocol, "SASL_")
	for index, key := range props.Keys() {
		value := values[key]
		// The properties decoder replaces UTF-16 surrogate escapes with U+FFFD.
		// Preserve credentials by refusing a lossy import.
		if strings.ContainsRune(value, unicode.ReplacementChar) {
			return connectionProfile{}, errors.New("a Unicode escape could not be imported exactly; enter the value in the connection form")
		}
		if strings.Contains(value, "${") {
			return connectionProfile{}, errors.New("Kafka config-provider expressions cannot be imported; enter the resolved settings in the form")
		}
		switch key {
		case "bootstrap.servers", "security.protocol":
		case "sasl.mechanism", "sasl.jaas.config", "sasl.username", "sasl.password":
			if !usesSASL {
				return connectionProfile{}, errors.New("SASL settings require security.protocol=SASL_SSL or SASL_PLAINTEXT")
			}
		case "ssl.truststore.type", "ssl.truststore.location", "ssl.keystore.type", "ssl.keystore.location", "ssl.endpoint.identification.algorithm":
			if !security.TLS {
				return connectionProfile{}, errors.New("SSL settings require security.protocol=SSL or SASL_SSL")
			}
		case "ssl.truststore.password", "ssl.keystore.password", "ssl.key.password":
			return connectionProfile{}, errors.New("password-protected stores and keys cannot be imported; use PEM files")
		default:
			// A malformed credential can be parsed as a key. Report its position, not its contents.
			return connectionProfile{}, fmt.Errorf("unsupported property (entry %d); import only Kafka connection settings", index+1)
		}
	}
	var draft connectionProfile
	if brokers, ok := values["bootstrap.servers"]; ok {
		var opts options
		if strings.ContainsRune(brokers, '@') || strings.IndexFunc(brokers, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) >= 0 || opts.setBrokers(brokers) != nil {
			return connectionProfile{}, errors.New("invalid bootstrap.servers; enter comma-separated broker addresses")
		}
		draft.Brokers = opts.brokers
	}
	if security.TLS {
		if alg, ok := values["ssl.endpoint.identification.algorithm"]; ok && !strings.EqualFold(strings.TrimSpace(alg), "https") {
			return connectionProfile{}, errors.New("Katui requires TLS hostname verification (HTTPS)")
		}
		security.CAFile, err = propertyPEMStore(values, "ssl.truststore", filepath.Dir(path))
		if err != nil {
			return connectionProfile{}, err
		}
		security.CAFileOnly = security.CAFile != ""
		security.CertFile, err = propertyPEMStore(values, "ssl.keystore", filepath.Dir(path))
		if err != nil {
			return connectionProfile{}, err
		}
		// Kafka's file-based PEM keystore contains both certificate chain and key.
		security.KeyFile = security.CertFile
	}
	if usesSASL {
		auth := &security.SASL
		auth.Mechanism = strings.ToUpper(strings.TrimSpace(values["sasl.mechanism"]))
		switch auth.Mechanism {
		case "SCRAM-SHA-256", "SCRAM-SHA-512":
		case "PLAIN":
			if !security.TLS {
				return connectionProfile{}, errors.New("SASL PLAIN requires TLS")
			}
		default:
			return connectionProfile{}, errors.New("set sasl.mechanism to SCRAM-SHA-256, SCRAM-SHA-512, or PLAIN")
		}
		username, hasUser := values["sasl.username"]
		password, hasPassword := values["sasl.password"]
		if jaas, ok := values["sasl.jaas.config"]; ok {
			if hasUser || hasPassword {
				return connectionProfile{}, errors.New("choose either sasl.jaas.config or separate SASL credentials")
			}
			username, password, err = parseKafkaJAAS(jaas, auth.Mechanism)
			if err != nil {
				return connectionProfile{}, err
			}
		}
		auth.Username, auth.Password = username, password
	}
	if security.TLS || usesSASL {
		draft.Security = &security
	}
	return draft, nil
}

func propertyPEMStore(values map[string]string, prefix, dir string) (string, error) {
	kind, hasKind := values[prefix+".type"]
	path, hasPath := values[prefix+".location"]
	if !hasKind && !hasPath {
		return "", nil
	}
	if !strings.EqualFold(strings.TrimSpace(kind), "PEM") {
		return "", fmt.Errorf("only PEM files are supported for %s; specify %s.type=PEM", prefix, prefix)
	}
	path = strings.TrimSpace(path)
	if path == "" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("expected a PEM file path in %s.location", prefix)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}
