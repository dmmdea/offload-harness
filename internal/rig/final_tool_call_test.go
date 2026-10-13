package rig

// A run whose forced final step (the last step: no tools offered, the answer asked for) was answered with a tool call,
// even after the loop re-asked once with it quoted, ended on stop_reason "budget" and landed on the budget axis, whose
// remedy reads "a run at the cap with no loop axis is a task too large for one contract: split it". It is not: in the
// rows that day the seat had read the whole document and used 43 s of 600 s; the same seat wrote a tool call there in a
// third of its forced finals and no other seat ever did. The note carries a stable phrase (core.ForcedFinalToolCallReason),
// and the shape has an axis of its own ahead of budget.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func finalToolCallRow(id string) Row {
	return row(id, func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassBudget
		r.Result.StopReason = "budget"
		r.Result.Steps = 12
		r.Result.Reason = "step budget exhausted (12 steps): " + core.ForcedFinalToolCallReason + `, and the seat wrote a tool call as text ("<tool_call>\n<function=read_file>") - nothing executed`
	})
}

func TestAForcedFinalToolCallIsItsOwnAxisNotATaskTooLarge(t *testing.T) {
	v := Classify(finalToolCallRow("ftc"))
	if v.Axis != Axis("final-tool-call") {
		t.Fatalf("classified %q (evidence %q), want final-tool-call", v.Axis, v.Evidence)
	}
	plain := row("plain", func(r *Row) {
		r.Deferred = true
		r.DeferClass = core.DeferClassBudget
		r.Result.StopReason = "budget"
		r.Result.Reason = "step budget exhausted (12 steps)"
	})
	if v := Classify(plain); v.Axis != AxisBudget {
		t.Errorf("a plain step-budget row classified %q, want budget: the new axis is for the one shape", v.Axis)
	}
	rem, ok := Remedies[Axis("final-tool-call")]
	if !ok {
		t.Fatalf("no remedy for the final-tool-call axis")
	}
	if all := rem.Key + rem.Value + rem.Rationale; strings.Contains(all, "split it") {
		t.Errorf("remedy %+v tells the reader to split a contract that had all it needed", rem)
	}
}

func TestTheFinalToolCallAxisSitsAheadOfBudget(t *testing.T) {
	idx := func(a Axis) int {
		for i, x := range Precedence {
			if x == a {
				return i
			}
		}
		return -1
	}
	f, b := idx(Axis("final-tool-call")), idx(AxisBudget)
	if f < 0 || f > b {
		t.Fatalf("precedence %v: final-tool-call must come before budget, whose stop_reason it shares", Precedence)
	}
}
