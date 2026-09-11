package seatrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRateIgnoresToolCallCompletions: the 27B's tool steps generate 25–60
// tokens over calls dominated by prefill; counting them would report a
// "decode" rate a fraction of the real one (measured 2026-09-10: 42 tokens in
// ~8 s = 5 tok/s beside a 9,100-token final at 30 tok/s).
func TestRateIgnoresToolCallCompletions(t *testing.T) {
	calls := []Call{
		{CompletionTokens: 42, Ms: 8000},
		{CompletionTokens: 54, Ms: 7000},
		{CompletionTokens: 9100, Ms: 300000},
		{CompletionTokens: 2114, Ms: 70000},
	}
	tokS, n := Rate(calls)
	if n != 2 {
		t.Fatalf("samples = %d, want 2 (only the ≥ %d-token completions)", n, minSampleTokens)
	}
	// (9100+2114) tokens / 370 s = 30.3 tok/s
	if tokS < 30.2 || tokS > 30.4 {
		t.Fatalf("tok/s = %.2f, want ≈ 30.3", tokS)
	}
	if got, n := Rate([]Call{{CompletionTokens: 40, Ms: 5000}}); got != 0 || n != 0 {
		t.Fatalf("a run of tool calls only must report no rate, got %.1f (%d samples)", got, n)
	}
	if got, n := Rate([]Call{{CompletionTokens: 5000, Ms: 0}}); got != 0 || n != 0 {
		t.Fatalf("a completion without a measured wall must not count, got %.1f (%d samples)", got, n)
	}
}

// TestComputeMatchesTheMeasuredQube27B: the 2026-09-10 ledger-01 legs on the
// 27B — cold load 210 s, 12 steps, 4,096-token steps, 8,192 final, 30 tok/s —
// took 381 s (off) / 516 s (auto) warm, plus a 210 s cold load. The estimate
// must land in that band and say "BELOW" for the 600 s box default.
func TestComputeMatchesTheMeasuredQube27B(t *testing.T) {
	in := Input{Seat: "agent-pool", TokS: 30, RateSamples: 5, ColdLoadSec: 210, MaxSteps: 12, StepBudget: 4096, FinalBudget: 8192, TimeoutSec: 600}
	off := Compute(in)
	// 210 + 11×(128/30 + 6) = 113 + 8192/30 = 273 → 596
	if off.TotalSec < 590 || off.TotalSec > 600 {
		t.Fatalf("thinking-off estimate = %d s, want ≈ 596", off.TotalSec)
	}
	if off.MinTurnSec != 484 { // 210 + 273.07 → ceil 484
		t.Fatalf("min_turn = %d s, want 484 (cold load + one turn at the final budget)", off.MinTurnSec)
	}
	in.ThinkingAuto = true
	auto := Compute(in)
	if auto.TotalSec-off.TotalSec < 130 || auto.TotalSec-off.TotalSec > 140 {
		t.Fatalf("auto adds %d s, want one 4,096-token think block ≈ 137 s", auto.TotalSec-off.TotalSec)
	}
	if !auto.Below || !strings.Contains(auto.Note, "BELOW") || !strings.Contains(auto.Note, "wall 600 s") {
		t.Fatalf("a 600 s wall under a %d s estimate must be flagged: below=%v note=%q", auto.TotalSec, auto.Below, auto.Note)
	}
	for _, want := range []string{"cold load 210 s", "think block 4096 tok", "11 tool steps", "final 8192 tok", "30.0 tok/s", "5 samples", "min_turn 484 s"} {
		if !strings.Contains(auto.Note, want) {
			t.Errorf("note lacks %q: %q", want, auto.Note)
		}
	}
	in.TimeoutSec = 900
	if e := Compute(in); e.Below || strings.Contains(e.Note, "BELOW") {
		t.Fatalf("a 900 s wall over a %d s estimate must not be flagged: %q", e.TotalSec, e.Note)
	}
}

// TestComputeWithoutARateOnlyExplains: no sample = no numbers to publish; the
// note says how one gets recorded and never invents a rate.
func TestComputeWithoutARateOnlyExplains(t *testing.T) {
	e := Compute(Input{Seat: "qwen3.5-4b-vllm", ColdLoadSec: 34, MaxSteps: 12, TimeoutSec: 300})
	if e.TotalSec != 0 || e.MinTurnSec != 0 || e.Below {
		t.Fatalf("no rate must publish no estimate: %+v", e)
	}
	if !strings.Contains(e.Note, "no decode-rate sample") || !strings.Contains(e.Note, "cold load 34 s known") {
		t.Fatalf("note = %q", e.Note)
	}
}

// TestStoreRoundTripAndCorruptFile: observe → save → load keeps the EMA, the
// cold-load window (max of the last five) and the sample count; a corrupt
// file loads EMPTY with an error to log, never fails.
func TestStoreRoundTripAndCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := Path(filepath.Join(dir, "nested"))
	s, err := Load(path)
	if err != nil || len(s.Seats) != 0 {
		t.Fatalf("missing store: err=%v seats=%d", err, len(s.Seats))
	}
	now := time.Date(2026, 9, 10, 20, 30, 0, 0, time.UTC)
	if !s.Observe("agent-pool", 30, 210, now) {
		t.Fatal("first observation must register")
	}
	s.Observe("agent-pool", 40, 0, now) // rate only: EMA 0.3×40 + 0.7×30 = 33
	for _, c := range []float64{100, 250, 180, 190, 170, 160} {
		s.Observe("agent-pool", 0, c, now) // 6 loads: the window drops the first (100) and the 210
	}
	if s.Observe("", 30, 0, now) || s.Observe("x", 0, 0, now) {
		t.Fatal("an empty seat or an empty observation must not register")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Get("agent-pool")
	if got.Samples != 2 || got.TokS < 32.9 || got.TokS > 33.1 {
		t.Fatalf("rate = %.2f over %d samples, want 33.0 over 2", got.TokS, got.Samples)
	}
	if got.ColdLoadSec != 250 || len(got.ColdLoads) != coldLoadWindow {
		t.Fatalf("cold load = %.0f over %d loads, want the max 250 of the last %d", got.ColdLoadSec, len(got.ColdLoads), coldLoadWindow)
	}
	if got.Updated.IsZero() {
		t.Fatal("updated must be stamped")
	}
	if names := back.Names(); len(names) != 1 || names[0] != "agent-pool" {
		t.Fatalf("names = %v", names)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp file left behind: %d entries", len(entries))
	}
	// Corrupt store: empty + an error for the log, never a failure.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, cerr := Load(path)
	if cerr == nil || len(c.Seats) != 0 {
		t.Fatalf("corrupt store: err=%v seats=%d, want an error and an empty store", cerr, len(c.Seats))
	}
	if c.Get("agent-pool").TokS != 0 {
		t.Fatal("a corrupt store must not hand back stale numbers")
	}
}
