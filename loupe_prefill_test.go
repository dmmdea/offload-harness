package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func prefillRows(n, steps int, cache, prompt int64) []ledger.Entry {
	out := make([]ledger.Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ledger.Entry{
			Task: "agent", PrefillSteps: steps,
			CacheTokens: cache, PrefillTokens: prompt, PrefillMS: 10,
		})
	}
	return out
}

// The property the whole view turns on: an unobserved workload must never read as a
// measured zero. A 0% reuse rate would be the ONLY result that justifies building
// prefix-stability work, so fabricating one is the most expensive possible mistake here.
func TestUnobservedIsNotAMeasuredZero(t *testing.T) {
	// Rows exist, but none carried a measurement (PrefillSteps == 0).
	rows := []ledger.Entry{{Task: "agent"}, {Task: "agent_delegate"}, {Task: "summarize"}}
	r := buildPrefill(rows)

	if r.MeasuredRows != 0 {
		t.Fatalf("MeasuredRows = %d, want 0 — rows without PrefillSteps are not observations", r.MeasuredRows)
	}
	if r.Basis != "insufficient_data" {
		t.Fatalf("Basis = %q, want insufficient_data", r.Basis)
	}
	if r.KVReusePct != nil {
		t.Fatalf("KVReusePct = %v, want nil — a zero here would wrongly justify the build", *r.KVReusePct)
	}
	if !strings.Contains(r.Verdict, "unobserved") {
		t.Fatalf("verdict should say unobserved, got %q", r.Verdict)
	}
}

// Counts are still useful below the floor; a VERDICT is not. Quoting a reuse rate off a
// handful of runs is how an untrustworthy number reaches a decision.
func TestBelowSampleFloorShowsCountsButNoVerdict(t *testing.T) {
	r := buildPrefill(prefillRows(prefillMinRows-1, 4, 900, 100))
	if r.MeasuredRows != prefillMinRows-1 {
		t.Fatalf("MeasuredRows = %d", r.MeasuredRows)
	}
	if r.Basis == "measured" {
		t.Fatal("Basis went measured below the sample floor")
	}
	if r.KVReusePct == nil {
		t.Fatal("the rate should still be computed for display")
	}
	if !strings.Contains(r.Verdict, "sample floor") {
		t.Fatalf("verdict = %q, want a sample-floor note", r.Verdict)
	}
}

// The denominator is the thing most likely to be got wrong, and getting it wrong yields a
// plausible-looking number above 100%.
func TestKVReuseDenominatorIsCachePlusPrefill(t *testing.T) {
	r := buildPrefill(prefillRows(prefillMinRows, 1, 900, 100))
	if r.KVReusePct == nil {
		t.Fatal("expected a rate")
	}
	// 900/(900+100) = 90%. The wrong formula, CacheN/PromptN, gives 900%.
	if got := *r.KVReusePct; got < 89.9 || got > 90.1 {
		t.Fatalf("KVReusePct = %v, want 90 — must be CacheN/(CacheN+PromptN)", got)
	}
}

// Both verdicts must be reachable from realistic inputs, or the gate is decoration.
func TestVerdictReachableBothWays(t *testing.T) {
	high := buildPrefill(prefillRows(prefillMinRows, 3, 950, 50))
	if !strings.HasPrefix(high.Verdict, "HIGH REUSE") {
		t.Fatalf("high-reuse verdict = %q", high.Verdict)
	}
	if high.Basis != "measured" {
		t.Fatalf("Basis = %q, want measured", high.Basis)
	}

	low := buildPrefill(prefillRows(prefillMinRows, 3, 50, 950))
	if !strings.HasPrefix(low.Verdict, "LOW REUSE") {
		t.Fatalf("low-reuse verdict = %q", low.Verdict)
	}
}

func TestMixedRowsCountOnlyMeasuredOnes(t *testing.T) {
	rows := append(prefillRows(prefillMinRows, 2, 100, 100), ledger.Entry{Task: "agent"})
	r := buildPrefill(rows)
	if r.MeasuredRows != prefillMinRows {
		t.Fatalf("MeasuredRows = %d, want %d — the unmeasured row must not dilute", r.MeasuredRows, prefillMinRows)
	}
	if r.ObservedSteps != prefillMinRows*2 {
		t.Fatalf("ObservedSteps = %d", r.ObservedSteps)
	}
}
