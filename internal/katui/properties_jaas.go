package katui

import (
	"errors"
	"regexp"
	"strings"
)

var (
	jaasHeader = regexp.MustCompile(`^\s*(\S+)\s+(?i:required)\s+`)
	// Bare values use a subset of Kafka's StreamTokenizer word syntax. In quoted
	// values, support escaped quotes and backslashes; reject other escape forms.
	jaasOption   = regexp.MustCompile(`^(username|password)\s*=\s*("(?:[^"\\\r\n]|\\['"\\])*"|'(?:[^'\\\r\n]|\\['"\\])*'|[A-Za-z_$][A-Za-z0-9_$.-]*)`)
	jaasUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\'`, `'`)
	errKafkaJAAS = errors.New("sasl.jaas.config requires one matching login module marked required with only username and password options. Quote values containing spaces or punctuation")
)

// parseKafkaJAAS takes the value after Java properties escaping has been decoded.
func parseKafkaJAAS(value, mechanism string) (string, string, error) {
	var module string
	switch strings.ToUpper(mechanism) {
	case "PLAIN":
		module = "org.apache.kafka.common.security.plain.PlainLoginModule"
	case "SCRAM-SHA-256", "SCRAM-SHA-512":
		module = "org.apache.kafka.common.security.scram.ScramLoginModule"
	default:
		return "", "", errKafkaJAAS
	}
	header := jaasHeader.FindStringSubmatch(value)
	if header == nil || header[1] != module {
		return "", "", errKafkaJAAS
	}
	rest := value[len(header[0]):]
	credentials := make(map[string]string, 2)
	for range 2 {
		option := jaasOption.FindStringSubmatch(rest)
		if option == nil {
			return "", "", errKafkaJAAS
		}
		key, word := option[1], option[2]
		if word[0] == '"' || word[0] == '\'' {
			word = jaasUnescape.Replace(word[1 : len(word)-1])
		}
		if word == "" || credentials[key] != "" {
			return "", "", errKafkaJAAS
		}
		credentials[key] = word
		rest = strings.TrimLeft(rest[len(option[0]):], " \t\r\n\f")
	}
	if strings.TrimRight(rest, " \t\r\n\f") != ";" {
		return "", "", errKafkaJAAS
	}
	return credentials["username"], credentials["password"], nil
}
