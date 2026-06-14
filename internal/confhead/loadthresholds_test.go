package confhead

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadThresholdsValid reads a Task-3 confhead-thresholds.json and returns the map.
func TestLoadThresholdsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "confhead-thresholds.json")
	if err := os.WriteFile(path, []byte(`{"summarize":0.69,"extract":0}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := LoadThresholds(path)
	if m == nil {
		t.Fatal("want non-nil map for a valid file")
	}
	if m["summarize"] != 0.69 {
		t.Fatalf("summarize: want 0.69, got %v", m["summarize"])
	}
	if v, ok := m["extract"]; !ok || v != 0 {
		t.Fatalf("extract: want 0 present, got %v ok=%v", v, ok)
	}
}

// TestLoadThresholdsMissing: a missing/empty/unparseable path yields an empty
// map that is nil-safe to index (never panics).
func TestLoadThresholdsMissing(t *testing.T) {
	// empty path
	if m := LoadThresholds(""); len(m) != 0 {
		t.Fatalf("empty path: want empty map, got %v", m)
	}
	if LoadThresholds("")["summarize"] != 0 { // nil-safe index
		t.Fatal("indexing the empty-path result must not panic and yields 0")
	}
	// missing file
	m := LoadThresholds(filepath.Join(t.TempDir(), "nope.json"))
	if len(m) != 0 {
		t.Fatalf("missing file: want empty map, got %v", m)
	}
	if m["summarize"] != 0 { // nil-safe index
		t.Fatal("indexing a missing-file result must not panic and yields 0")
	}
	// unparseable file
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m := LoadThresholds(bad); len(m) != 0 {
		t.Fatalf("unparseable: want empty map, got %v", m)
	}
}
