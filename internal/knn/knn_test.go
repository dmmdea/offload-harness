package knn

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(b, []byte(l+"\n")...)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPreferLargerEntryZeroKGuard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	// All rows are dim-mismatched vs the 2-dim query, so the candidate set is
	// empty; with minNeighbors=0 the thin-substrate guard passes — the k<=0 guard
	// must catch it (no NaN, no panic, fail-open to no-preference).
	writeLines(t, p, []string{
		`{"task":"classify","vec":[1,0,0],"accept":true}`,
		`{"task":"classify","vec":[0,1,0],"accept":false}`,
	})
	ix := Load(p)
	if skip, ok := ix.PreferLargerEntry("classify", []float64{1, 0}, 5, 0, 0.5); skip || ok {
		t.Fatalf("all-dim-mismatch + minNeighbors=0: want (false,false), got (%v,%v)", skip, ok)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	if ix := Load(filepath.Join(t.TempDir(), "nope.jsonl")); ix != nil {
		t.Fatalf("missing file: want nil, got %#v", ix)
	}
}

func TestLoadSkipsMalformedAndParses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	writeLines(t, p, []string{
		`{"task":"classify","vec":[1,0],"accept":true}`,
		`not json`,
		`{"task":"classify","vec":[0,1],"accept":false}`,
	})
	ix := Load(p)
	if ix == nil {
		t.Fatal("want non-nil index")
	}
	// 2 classify rows survived; the malformed line is skipped.
	if _, ok := ix.PreferLargerEntry("classify", []float64{1, 0}, 2, 2, 0.5); !ok {
		t.Fatal("want ok=true with 2 rows and minNeighbors=2")
	}
}

func TestAppendThenLoadRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "k.jsonl") // parent dir does not exist yet
	for i := 0; i < 3; i++ {
		if err := Append(p, Row{Task: "triage", Vec: []float64{float64(i), 1}, Accept: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	ix := Load(p)
	if ix == nil {
		t.Fatal("want non-nil after append")
	}
	if _, ok := ix.PreferLargerEntry("triage", []float64{0, 1}, 3, 3, 0.5); !ok {
		t.Fatal("want ok=true after 3 appends")
	}
}

func TestPreferLargerEntryNilReceiver(t *testing.T) {
	var ix *Index
	if skip, ok := ix.PreferLargerEntry("classify", []float64{1}, 5, 1, 0.5); skip || ok {
		t.Fatalf("nil receiver: want (false,false), got (%v,%v)", skip, ok)
	}
}

func TestPreferLargerEntryTooThin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	writeLines(t, p, []string{`{"task":"classify","vec":[1,0],"accept":true}`})
	ix := Load(p)
	if _, ok := ix.PreferLargerEntry("classify", []float64{1, 0}, 5, 20, 0.5); ok {
		t.Fatal("only 1 row but minNeighbors=20: want ok=false")
	}
}

func TestPreferLargerEntrySkipsWhenNeighborsRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	// Two clusters: accepted near [1,0], rejected near [0,1].
	writeLines(t, p, []string{
		`{"task":"classify","vec":[1,0],"accept":true}`,
		`{"task":"classify","vec":[0.9,0.1],"accept":true}`,
		`{"task":"classify","vec":[0,1],"accept":false}`,
		`{"task":"classify","vec":[0.1,0.9],"accept":false}`,
	})
	ix := Load(p)
	// Query near the REJECT cluster: the 2 nearest both rejected → frac=0 < 0.5 → skip.
	skip, ok := ix.PreferLargerEntry("classify", []float64{0.05, 0.95}, 2, 2, 0.5)
	if !ok || !skip {
		t.Fatalf("near reject cluster: want (skip=true,ok=true), got (%v,%v)", skip, ok)
	}
	// Query near the ACCEPT cluster: the 2 nearest both accepted → frac=1 >= 0.5 → keep.
	skip, ok = ix.PreferLargerEntry("classify", []float64{0.95, 0.05}, 2, 2, 0.5)
	if !ok || skip {
		t.Fatalf("near accept cluster: want (skip=false,ok=true), got (%v,%v)", skip, ok)
	}
}

func TestPreferLargerEntryDimensionMismatchSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	writeLines(t, p, []string{
		`{"task":"classify","vec":[1,0,0],"accept":true}`, // wrong dim vs query
		`{"task":"classify","vec":[0,1],"accept":false}`,
		`{"task":"classify","vec":[0.1,0.9],"accept":false}`,
	})
	ix := Load(p)
	// query is 2-dim; the 3-dim row is dropped, leaving 2 usable reject rows.
	skip, ok := ix.PreferLargerEntry("classify", []float64{0, 1}, 5, 2, 0.5)
	if !ok || !skip {
		t.Fatalf("dim-mismatch row must be dropped: want (true,true), got (%v,%v)", skip, ok)
	}
}

func TestPreferLargerEntryUnknownTask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.jsonl")
	writeLines(t, p, []string{`{"task":"classify","vec":[1,0],"accept":true}`})
	ix := Load(p)
	if _, ok := ix.PreferLargerEntry("summarize", []float64{1, 0}, 5, 1, 0.5); ok {
		t.Fatal("unknown task: want ok=false")
	}
}
