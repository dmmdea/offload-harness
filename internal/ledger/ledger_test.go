package ledger

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLedgerRoundTripAndSummary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	rec := func(e Entry) {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	rec(Entry{Task: "summarize", TokensIn: 100, TokensOut: 20}) // completed
	rec(Entry{Task: "classify", TokensIn: 50, CacheHit: true})  // cache hit
	rec(Entry{Task: "triage", TokensIn: 70, Deferred: true})    // deferred
	l.Close()

	s, err := SummarizeFile(p, 0, 15.0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 3 || s.Completed != 1 || s.CacheHits != 1 || s.Deferred != 1 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.TokensSaved != 150 { // completed 100 + cache 50; deferred (70) not counted
		t.Fatalf("TokensSaved=%d want 150", s.TokensSaved)
	}
	if s.ByTask["summarize"] != 1 || s.ByTask["triage"] != 1 {
		t.Fatalf("ByTask=%v", s.ByTask)
	}
}

func TestSummarizeMissingFileIsEmpty(t *testing.T) {
	s, err := SummarizeFile(filepath.Join(t.TempDir(), "nope.jsonl"), 0, 1.0)
	if err != nil || s.Calls != 0 {
		t.Fatalf("missing file: err=%v calls=%d", err, s.Calls)
	}
}

func TestSummarizeSkipsMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, _ := Open(p)
	_ = l.Record(Entry{Task: "summarize", TokensIn: 10})
	l.Close()
	// a not-yet-complete trailing line, as a concurrent writer mid-flush might leave
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"task":"classify","tokens_in":5`)
	f.Close()
	s, err := SummarizeFile(p, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 1 {
		t.Fatalf("expected 1 valid entry (malformed skipped), got %d", s.Calls)
	}
}

func TestAppendLabelRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "labels", "confhead-labels.jsonl") // nested: dir must be created
	agreed := true
	e := Entry{Task: "classify", InputChars: 200, Margin: 0.4, Retries: 1,
		Feat: map[string]float64{"len_chars": 200, "n_words": 30}, EscalatedAgreed: &agreed}
	if err := AppendLabel(p, e); err != nil {
		t.Fatalf("AppendLabel: %v", err)
	}
	got, err := ReadLabelFile(p)
	if err != nil {
		t.Fatalf("ReadLabelFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	r := got[0]
	if r.Task != "classify" || r.InputChars != 200 || r.Margin != 0.4 || r.Retries != 1 {
		t.Fatalf("round-trip mismatch: %+v", r)
	}
	if r.EscalatedAgreed == nil || *r.EscalatedAgreed != true {
		t.Fatalf("EscalatedAgreed not preserved: %+v", r.EscalatedAgreed)
	}
	if r.Feat["len_chars"] != 200 {
		t.Fatalf("Feat not preserved: %v", r.Feat)
	}
}

func TestReadLabelFileMissingIsNil(t *testing.T) {
	got, err := ReadLabelFile(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("missing file should return nil slice, got %v", got)
	}
}

func TestConcurrentAppend(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, _ := Open(p)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = l.Record(Entry{Task: "triage", TokensIn: 1}) }()
	}
	wg.Wait()
	l.Close()
	s, _ := SummarizeFile(p, 0, 0)
	if s.Calls != 20 {
		t.Fatalf("concurrent appends: got %d want 20", s.Calls)
	}
}
