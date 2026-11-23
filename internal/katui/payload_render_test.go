package katui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestJSONDisplayDepth(t *testing.T) {
	n := maxFullTreeDepth
	nested := func(n int) []byte {
		return []byte(strings.Repeat("[", n) + `"A quoted [bracket], a \" quote and a \\ slash"` + strings.Repeat("]", n))
	}
	if _, err := prettyJSON(nested(n)); err != nil {
		t.Fatalf("formatting at the depth limit: %v", err)
	}
	if _, err := prettyJSON(nested(n + 1)); err == nil {
		t.Fatal("expected an error above the display depth limit")
	}
}

func TestRenderTextKeepsUTF8AndHonorsBudget(t *testing.T) {
	var text renderText
	_, _ = text.WriteString(strings.Repeat("a", maxRenderedPayload-1))
	_, _ = text.WriteString("☃")
	_, _ = text.WriteString("x")
	if !text.limited || len(text.String()) != maxRenderedPayload-1 || !utf8.ValidString(text.String()) {
		t.Fatal("render output exceeded its budget or produced invalid UTF-8")
	}
}

func TestBoundedDisplayKeepsOriginalPayload(t *testing.T) {
	const value = "abc☃def"
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	record := &storedRecord{valueSize: len(value), fullPath: path}
	prefix, limited, err := renderValue(record, 4)
	if err != nil || !limited || string(prefix) != "abc" {
		t.Fatalf("display prefix %q, limited %t, error %v", prefix, limited, err)
	}
	full, err := record.full()
	if err != nil || string(full) != value {
		t.Fatalf("retained payload %q, error %v", full, err)
	}
}
