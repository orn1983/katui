package katui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrettyJSONKeepsStructureAndLiteralsWithinBudget(t *testing.T) {
	for _, raw := range []string{
		" \n" + `{"x":[], "x":[{}, 1e002, "escaped \" [{:}] \\ \u00fe", true, null]}` + "\t\r\n",
		strings.Repeat("[", 200) + `"leaf"` + strings.Repeat("]", 200),
	} {
		var want bytes.Buffer
		if err := json.Indent(&want, []byte(raw), "", "  "); err != nil {
			t.Fatal(err)
		}
		got, err := prettyJSON(t.Context(), []byte(raw), want.Len())
		if err != nil || got != want.String() {
			t.Fatalf("formatted JSON differs from json.Indent (error %v)", err)
		}
		if _, err := prettyJSON(t.Context(), []byte(raw), want.Len()-1); !errors.Is(err, errRenderLimit) {
			t.Fatalf("formatting over the display budget: %v", err)
		}
	}
}

func TestPrettyJSONBeyondStandardNesting(t *testing.T) {
	const depth = 10_001
	raw := []byte(strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth))
	// Use a small display budget so the test doesn't need 200 MB of padding.
	if _, err := prettyJSON(t.Context(), raw, len(raw)*2); !errors.Is(err, errRenderLimit) {
		t.Fatalf("deep valid JSON should reach the output budget: %v", err)
	}
	if _, err := prettyJSON(t.Context(), raw[:len(raw)-1], math.MaxInt); err == nil || errors.Is(err, errRenderLimit) {
		t.Fatalf("incomplete JSON accepted as valid: %v", err)
	}
}

func TestRenderTextKeepsUTF8AndHonorsBudget(t *testing.T) {
	const limit = 256
	text := renderText{limit: limit}
	_, _ = text.WriteString(strings.Repeat("a", limit-1))
	_, _ = text.WriteString("☃")
	_, _ = text.WriteString("x")
	if !text.limited || len(text.String()) != limit-1 || !utf8.ValidString(text.String()) {
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
	prefix, limited, err := renderValue(context.Background(), record, 4)
	if err != nil || !limited || string(prefix) != "abc" {
		t.Fatalf("display prefix %q, limited %t, error %v", prefix, limited, err)
	}
	display, limited, err := renderValue(t.Context(), record, math.MaxInt)
	if err != nil || limited || string(display) != value {
		t.Fatalf("full display %q, limited %t, error %v", display, limited, err)
	}
	full, err := record.full()
	if err != nil || string(full) != value {
		t.Fatalf("retained payload %q, error %v", full, err)
	}
}
