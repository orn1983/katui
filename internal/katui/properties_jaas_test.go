package katui

import (
	"strings"
	"testing"
)

func TestKafkaJAAS(t *testing.T) {
	const plain = "org.apache.kafka.common.security.plain.PlainLoginModule"
	const scram = "org.apache.kafka.common.security.scram.ScramLoginModule"
	for _, tt := range []struct{ mechanism, value, username, password string }{
		{"SCRAM-SHA-256", scram + ` required username=app-1 password=Pwd_1.2$;`, "app-1", "Pwd_1.2$"},
		{"SCRAM-SHA-512", scram + ` REQUIRED password = 'we\'re "\\ready";' username = "tëst user";`, "tëst user", `we're "\ready";`},
		{"PLAIN", " \t" + plain + "\nReQuIrEd\t" + `username="reader" password="p = \"w\" : 1/2";` + "\n", "reader", `p = "w" : 1/2`},
	} {
		username, password, err := parseKafkaJAAS(tt.value, tt.mechanism)
		if err != nil || username != tt.username || password != tt.password {
			t.Errorf("failed to decode %s login options correctly", tt.mechanism)
		}
	}

	const secret = "fixture-secret"
	for _, value := range []string{
		plain + " required username=u password=" + secret + ";",
		scram + " optional username=u password=" + secret + ";",
		scram + " required username=u;",
		scram + " required username=u password=" + secret,
		scram + " required username=u password=" + secret + "; " + plain + " required;",
		scram + " required username=u password=" + secret + " extra=true;",
		scram + " required username=u username=" + secret + ";",
		scram + " required username=u password='" + secret + ";",
		scram + ` required username=u password="\123";`,
		scram + ` required username=u password=123;`,
		scram + ` required username=u password=p@ss;`,
		scram + ` required username="" password=p;`,
	} {
		username, password, err := parseKafkaJAAS(value, "SCRAM-SHA-256")
		if err == nil || strings.Contains(err.Error(), secret) || username != "" || password != "" {
			t.Fatal("invalid JAAS input should return a value-free error and no credentials")
		}
	}
}
