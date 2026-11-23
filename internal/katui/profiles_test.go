package katui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPreferencesSaveAfterRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inspectors.json")
	invalid := `{"version":2,"clusters":{}}`
	if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	prefs := loadPreferences(path, "cluster")
	if prefs.err == nil {
		t.Fatal("loading a future version succeeded")
	}
	value := profile{Mode: "pretty", Columns: []string{"first", "second"}}
	if err := prefs.save("topic", value, "columns"); err == nil {
		t.Fatal("saving over a future version succeeded")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != invalid {
		t.Fatalf("saving over a future version changed it: %q, %v", data, err)
	}

	repaired := `{
		"version":1,
		"clusters":{
			"cluster":{"topics":{"topic":{"mode":"raw"}}},
			"other":{"topics":{"topic":{"mode":"pretty"}}}
		}
	}`
	if err := os.WriteFile(path, []byte(repaired), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prefs.save("topic", value, "columns"); err != nil {
		t.Fatal(err)
	}
	loaded := loadPreferences(path, "cluster")
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	got := loaded.file.Clusters["cluster"].Topics["topic"]
	if got.Mode != "raw" || !slices.Equal(got.Columns, value.Columns) {
		t.Fatalf("saved profile = %+v", got)
	}
	other := loaded.file.Clusters["other"].Topics["topic"]
	if other.Mode != "pretty" || len(other.Columns) != 0 {
		t.Fatalf("unrelated profile = %+v", other)
	}
}
