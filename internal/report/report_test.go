package report

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func b(v bool) *bool { return &v }

func TestSummarize(t *testing.T) {
	es := []ledger.Entry{
		{Task: "extract", Grounded: b(true), Escalations: 0, Margin: 0.9, TokensOut: 100},
		{Task: "extract", Grounded: b(false), Escalations: 0, Margin: 0.4, TokensOut: 100},
		{Task: "extract", Grounded: b(true), Escalations: 1, Margin: 0.5, TokensOut: 200}, // escalation-resolved
		{Task: "extract", Deferred: true, TokensOut: 0},
	}
	rep := Summarize(es, 0.35)["extract"]
	if rep.N != 4 || rep.Deferred != 1 || rep.EscalationResolved != 1 {
		t.Fatalf("bad counts: %+v", rep)
	}
	if rep.GroundedRate < 0.66 || rep.GroundedRate > 0.67 { // 2 grounded of 3 labeled
		t.Fatalf("grounded rate: %+v", rep)
	}
}

// TestSummarizePerTaskReasoningReclaimed: the per-task view counts a reasoning
// reclaim only when the flagged entry was completed (not deferred). A reclaim
// carries Escalations>0 (the cascade climbed before deferring to the reasoning
// tier), but the escalation tier did NOT produce the answer — so a reclaim must
// be attributed to reasoning_reclaimed, NOT double-counted as escalation_resolved.
func TestSummarizePerTaskReasoningReclaimed(t *testing.T) {
	es := []ledger.Entry{
		{Task: "classify", Deferred: false},                                  // normal accept
		{Task: "classify", Deferred: false, Reasoning: true, Escalations: 2}, // reclaim (climbed 2 tiers, then reasoning answered)
		{Task: "classify", Deferred: true, Reasoning: true},                  // reasoning attempt that deferred -> not a reclaim
	}
	rep := Summarize(es, 0)["classify"]
	if rep.ReasoningReclaimed != 1 {
		t.Fatalf("want 1 reasoning_reclaimed for classify, got %d", rep.ReasoningReclaimed)
	}
	if rep.EscalationResolved != 0 {
		t.Fatalf("a reclaim must NOT also count as escalation_resolved (escalation tier deferred); got %d", rep.EscalationResolved)
	}
}
