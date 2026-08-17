package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func rowsOf(n int, task, tier string, deferred bool, reason string) []ledger.Entry {
	out := make([]ledger.Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ledger.Entry{Task: task, ModelTier: tier, Deferred: deferred, Reason: reason})
	}
	return out
}

// The property R2-14 exists for: a thin cell must not publish a rate. A 2-sample cell
// rendering "100.0%" is exactly how a report talks somebody into trusting noise, and the
// routing half was stripped precisely because most cells here are thin.
func TestThinCellReportsInsufficientDataNotARate(t *testing.T) {
	rep := buildReliability(rowsOf(3, "summarize", "gemma-4-e2b", false, ""))
	if len(rep.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(rep.Cells))
	}
	c := rep.Cells[0]
	if c.Basis != "insufficient_data" {
		t.Fatalf("basis = %q, want insufficient_data", c.Basis)
	}
	if c.SuccessRate != nil {
		t.Fatalf("SuccessRate = %v, want nil — a thin cell must not publish a rate", *c.SuccessRate)
	}
	if c.N != 3 {
		t.Fatalf("N = %d, want 3 (the count is still reported)", c.N)
	}
	if rep.CellsSuppressed != 1 || rep.CellsMeasured != 0 {
		t.Fatalf("coverage wrong: measured=%d suppressed=%d", rep.CellsMeasured, rep.CellsSuppressed)
	}
}

func TestCellAtTheFloorPublishesAMeasuredRate(t *testing.T) {
	rows := append(rowsOf(minReliableSamples-5, "classify", "gemma-4-e4b", false, ""),
		rowsOf(5, "classify", "gemma-4-e4b", true, "low confidence")...)
	rep := buildReliability(rows)
	c := rep.Cells[0]
	if c.Basis != "measured" || c.SuccessRate == nil {
		t.Fatalf("cell at the floor should be measured, got basis=%q", c.Basis)
	}
	// 15 of 20 succeeded.
	if got := *c.SuccessRate; got < 74.9 || got > 75.1 {
		t.Fatalf("SuccessRate = %v, want 75", got)
	}
	if c.Deferred != 5 {
		t.Fatalf("Deferred = %d, want 5", c.Deferred)
	}
}

// R2-16's exclusion is the entire point of the item. If the obsolete pre-131k
// context-overflow class counted toward the gate it would dominate the histogram and send
// somebody to hand-write anti-patterns for a failure that can no longer occur.
func TestObsoleteContextOverflowIsExcludedFromTheGate(t *testing.T) {
	// The TRUNCATED form the ledger actually stores. The first version of this test used the
	// full sentence, which the classifier matched happily while missing every real row —
	// a test that passed on a string the ledger never contains.
	rows := rowsOf(400, "summarize", "gemma-4-e2b", true,
		"reasoning model call failed: llama-server 400: {\"error\":{\"code\":400,\"message\":\"request (10532 tokens) exceeds the availa")
	a := buildAtlas(rows, 30)
	if a.ObsoleteDefers != 400 {
		t.Fatalf("ObsoleteDefers = %d, want 400", a.ObsoleteDefers)
	}
	if a.LiveDefers != 0 {
		t.Fatalf("LiveDefers = %d, want 0", a.LiveDefers)
	}
	if a.QualifyingCount != 0 {
		t.Fatalf("an obsolete class cleared the gate (%d qualifying) — the exclusion is inert", a.QualifyingCount)
	}
	if a.Classes[0].ClearsGate {
		t.Fatal("obsolete class marked ClearsGate")
	}
	// It must still be REPORTED, so the exclusion is auditable rather than a silent filter.
	if a.Classes[0].Count != 400 || !a.Classes[0].Obsolete {
		t.Fatal("obsolete class was dropped from the report instead of being flagged")
	}
}

func TestLiveClassClearsTheGateOnlyAboveThreshold(t *testing.T) {
	// 4/month — below the gate of 5.
	below := buildAtlas(rowsOf(4, "extract", "gemma-4-e4b", true, "schema had no usable properties"), 30)
	if below.QualifyingCount != 0 {
		t.Fatalf("class below the gate qualified: %+v", below.Classes)
	}
	if below.Verdict == "" || below.Classes[0].ClearsGate {
		t.Fatal("below-gate class should not clear")
	}

	// 10 over 30 days = 10/month — above.
	above := buildAtlas(rowsOf(10, "extract", "gemma-4-e4b", true, "schema had no usable properties"), 30)
	if above.QualifyingCount != 1 || !above.Classes[0].ClearsGate {
		t.Fatalf("class above the gate did not clear: %+v", above.Classes)
	}
}

// Without an observation window a per-month recurrence is undefined. Reporting one anyway
// (by dividing by a guessed span) is how a gate gets closed on a number nobody measured.
func TestZeroSpanReportsInsufficientDataRatherThanGuessing(t *testing.T) {
	a := buildAtlas(rowsOf(50, "triage", "gemma-4-e2b", true, "ungrounded"), 0)
	if a.Basis != "insufficient_data" {
		t.Fatalf("basis = %q, want insufficient_data", a.Basis)
	}
	if a.QualifyingCount != 0 {
		t.Fatal("the gate was evaluated without a window")
	}
	if a.Classes[0].PerMonth != 0 {
		t.Fatalf("PerMonth = %v, want 0 with no span", a.Classes[0].PerMonth)
	}
}

func TestTruncReasonIsRuneSafe(t *testing.T) {
	// 100 multi-byte runes: a byte-slice truncation would split one and emit U+FFFD.
	long := ""
	for i := 0; i < 100; i++ {
		long += "ñ"
	}
	got := truncReason(long)
	for _, r := range got {
		if r == '�' {
			t.Fatal("truncation split a rune")
		}
	}
	if len([]rune(got)) > 84 {
		t.Fatalf("truncated to %d runes, want <= 84", len([]rune(got)))
	}
}

// THE OPPOSITE DIRECTION, and the more dangerous one. `context deadline exceeded` and
// `context canceled` are Go HTTP timeout/cancellation errors, not context-WINDOW overflow.
// A loose "context ..." pattern would classify them obsolete and silently drop a LIVE
// failure class out of the gate — hiding a real problem rather than merely failing to hide
// a dead one. Four such rows exist in the live ledger, on three different tiers.
func TestTimeoutDefersAreNotObsolete(t *testing.T) {
	for _, reason := range []string{
		`reasoning model call failed: Post "http://127.0.0.1:11436/v1/chat/completions": context deadline exceeded (Client.Timeou`,
		`vision model call failed: Post "http://127.0.0.1:11436/v1/chat/completions": context canceled`,
	} {
		if isObsoleteDefer(reason) {
			t.Fatalf("timeout/cancel misclassified as obsolete context-overflow: %s", reason)
		}
	}
}

// The truncated overflow form MUST be caught — this is the bug that made the classifier
// report "0 obsolete" against a ledger holding 12 of them.
func TestTruncatedOverflowIsRecognised(t *testing.T) {
	if !isObsoleteDefer(`reasoning model call failed: llama-server 400: {"error":{"code":400,"message":"request (10532 tokens) exceeds the availa`) {
		t.Fatal("truncated context-overflow was not recognised; the ledger stores this form, not the full sentence")
	}
}
