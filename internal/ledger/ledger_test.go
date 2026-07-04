package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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

// TestSummarizeCountsReasoningReclaims: ReasoningReclaims counts ONLY completed
// entries flagged reasoning (a reclaimed deferral). A reasoning attempt that still
// deferred is not a reclaim, and pre-field ledger lines (no "reasoning" key) are
// back-compat non-reasoning.
func TestSummarizeCountsReasoningReclaims(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	lines := []string{
		`{"ts":1,"task":"triage","deferred":false}`,                    // pre-field completed -> not a reclaim
		`{"ts":2,"task":"classify","deferred":false,"reasoning":true}`, // reclaim (completed + flagged)
		`{"ts":3,"task":"triage","deferred":true,"reasoning":true}`,    // reasoning attempt that DEFERRED -> not a reclaim
		`{"ts":4,"task":"extract","deferred":false,"reasoning":true}`,  // reclaim
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := SummarizeFile(p, 0, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ReasoningReclaims != 2 {
		t.Fatalf("want 2 reasoning reclaims (completed+flagged only), got %d", s.ReasoningReclaims)
	}
	if s.Completed != 3 { // entries 1,2,4 completed; entry 3 deferred
		t.Fatalf("want 3 completed, got %d", s.Completed)
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

// TestRecordReasonTruncatedAndRoundTrips (LO-8): a defer entry's reason is
// persisted, truncated to 120 bytes on a RUNE boundary, and old lines without
// the field still parse (backward compatible).
func TestRecordReasonTruncatedAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 119) + "ñññ" // first 2-byte ñ straddles the 120-byte cut
	if err := l.Record(Entry{Task: "triage", Deferred: true, Reason: long}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(Entry{Task: "triage", Deferred: true, Reason: "model call failed: timeout"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	// An old pre-reason line must still parse.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"ts":1,"task":"classify","deferred":true}` + "\n")
	f.Close()

	got, err := ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	r0 := got[0].Reason
	if len(r0) > 120 {
		t.Fatalf("reason not truncated: %d bytes", len(r0))
	}
	if len(r0) != 119 { // byte 120 is mid-ñ, so the cut backs off to the 119 x's
		t.Fatalf("rune-boundary back-off expected 119 bytes, got %d (%q)", len(r0), r0[110:])
	}
	if !utf8.ValidString(r0) {
		t.Fatalf("truncated reason is not valid UTF-8: %q", r0)
	}
	if got[1].Reason != "model call failed: timeout" {
		t.Fatalf("short reason must round-trip, got %q", got[1].Reason)
	}
	if got[2].Reason != "" {
		t.Fatalf("pre-reason line must parse with empty Reason, got %q", got[2].Reason)
	}
}

// TestTopDeferReasons (LO-8): aggregation counts only deferred entries in the
// window, groups legacy blank reasons under (unrecorded), sorts by count then
// alphabetically, and caps at topN.
func TestTopDeferReasons(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	lines := []string{
		`{"ts":100,"task":"triage","deferred":true,"reason":"model call failed: timeout"}`,
		`{"ts":101,"task":"triage","deferred":true,"reason":"model call failed: timeout"}`,
		`{"ts":102,"task":"classify","deferred":true,"reason":"low confidence 0.30"}`,
		`{"ts":103,"task":"extract","deferred":true}`,                      // legacy: no reason
		`{"ts":104,"task":"summarize","deferred":false,"reason":"noise"}`,  // completed: excluded
		`{"ts":50,"task":"triage","deferred":true,"reason":"stale defer"}`, // before window
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TopDeferReasons(p, 99, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReasonCount{
		{Reason: "model call failed: timeout", Count: 2},
		{Reason: "(unrecorded)", Count: 1},
		{Reason: "low confidence 0.30", Count: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %v, want %v", i, got[i], want[i])
		}
	}
	// topN cap
	capped, _ := TopDeferReasons(p, 99, 1)
	if len(capped) != 1 || capped[0].Reason != "model call failed: timeout" {
		t.Fatalf("topN cap failed: %v", capped)
	}
	// missing file: nil, no error
	if r, err := TopDeferReasons(filepath.Join(t.TempDir(), "nope.jsonl"), 0, 5); err != nil || r != nil {
		t.Fatalf("missing file: %v %v", r, err)
	}
}

// TestSummaryHonestValueLabel (LO-12): the summary carries the honest
// est_value_kept_local key; the deprecated est_dollar_saved key is still
// emitted with the SAME value for one release (math unchanged — only the
// claim changed).
func TestSummaryHonestValueLabel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, _ := Open(p)
	_ = l.Record(Entry{Task: "summarize", TokensIn: 2_000_000}) // completed
	l.Close()
	s, err := SummarizeFile(p, 0, 15.0)
	if err != nil {
		t.Fatal(err)
	}
	if s.EstValueKeptLocal != 30.0 { // 2M tokens x $15/MTok
		t.Fatalf("EstValueKeptLocal = %v, want 30.0", s.EstValueKeptLocal)
	}
	if s.EstDollarSaved != s.EstValueKeptLocal {
		t.Fatalf("deprecated alias must carry the same value: %v != %v", s.EstDollarSaved, s.EstValueKeptLocal)
	}
	b, _ := json.Marshal(s)
	for _, key := range []string{`"est_value_kept_local":30`, `"est_dollar_saved":30`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("summary JSON missing %s: %s", key, b)
		}
	}
}
