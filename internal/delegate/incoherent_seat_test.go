package delegate

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestIncoherentSeatDeferIsTheOneRetryableInfrastructureDefer (register D-118).
//
// The retry gate deliberately refuses infrastructure defers: nothing about the
// task was learned and no other seat fixes a broken box. The admission-time
// coherence defer is the exception and has to be, because the fault is a
// property of THIS seat, it was caught before the wall started, and the whole
// timeout_sec budget is still on the table — which is exactly the case another
// node fixes. Without this, a NaN seat would eat every contract placed on it.
func TestIncoherentSeatDeferIsTheOneRetryableInfrastructureDefer(t *testing.T) {
	incoherent := PlacedResult{Result: core.AgentWireResult{
		Deferred:      true,
		DeferClass:    core.DeferClassInfrastructure,
		Reason:        core.IncoherentSeatReason + `the completion repeats "!" 40 times (finish_reason "length", 51 chars)`,
		CoherenceNote: core.IncoherentSeatReason + "…",
	}}
	if !IncoherentSeatDefer(incoherent.Result) {
		t.Fatal("the coherence defer must be recognised by its reason prefix")
	}
	if !retryable(incoherent) {
		t.Fatal("an incoherent seat must be retry-eligible on another node")
	}
}

// TestOrdinaryInfrastructureDefersStayUnretried: the narrow exception above must
// not widen. A transport death, a failed build and a contended wall are still
// terminal — a second seat handed the same broken fleet repeats the shape.
func TestOrdinaryInfrastructureDefersStayUnretried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.AgentWireResult
	}{
		{"a transport death", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: chat 502: bad gateway"}},
		{"a failed build", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "building agent: read root does not exist"}},
		{"a contended wall", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "seat contended: 90s of wait budget spent"}},
		{"a config defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassConfig, Reason: core.IncoherentSeatReason + "wrong class"}},
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := PlacedResult{Result: tc.result}
			if IncoherentSeatDefer(tc.result) {
				t.Fatalf("IncoherentSeatDefer must not claim %q (class %q)", tc.result.Reason, tc.result.DeferClass)
			}
			if retryable(pr) {
				t.Fatalf("%s must not be retried", tc.name)
			}
		})
	}
}

// TestIncoherentSeatDeferStillCountsAsABrokenStack: it IS a broken stack — an
// operator has to fix that box — so `--route remote` exits non-zero and the
// corpus counts it. Retry-eligible and broken-stack are two different questions
// and this pins that they are answered separately.
func TestIncoherentSeatDeferStillCountsAsABrokenStack(t *testing.T) {
	r := core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.IncoherentSeatReason + "x"}
	if !BrokenStackDefer(r.DeferClass) {
		t.Fatal("an incoherent seat is a broken stack: the box needs an operator")
	}
}

// TestAWireResultCarriesTheCoherenceNoteToTheCaller: the delegator's published
// wire must repeat the node's note, or a run that deferred before its wall
// started reaches the caller with nothing saying the seat itself was tested.
func TestAWireResultCarriesTheCoherenceNoteToTheCaller(t *testing.T) {
	note := "coherence probe: tool call parsed in 1.2s"
	wire := WireResponse([]PlacedResult{{
		Node:   "local",
		Seat:   "agent-pool",
		Result: core.AgentWireResult{Output: "42", StopReason: "done", CoherenceNote: note},
	}}, Summary{Succeeded: 1}, nil)
	if len(wire.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(wire.Results))
	}
	if wire.Results[0].CoherenceNote != note {
		t.Fatalf("coherence_note = %q, want %q", wire.Results[0].CoherenceNote, note)
	}
}

// TestTheCoherenceDeferCreditsBackTheNodesAdmission (reviewer finding, D-118).
// The retry this defer is retryable FOR is budgeted in delegator wall clock
// since the subtask started, and the default trigger is a COLD LOAD — 125–250 s
// of a vLLM seat against a 300 s default contract. Uncredited, the retry floor
// (the alternate seat's min_turn, ≈ 484 s for the 27B) refuses the retry on the
// very path that produces the defer, and the promise "the budget is still on
// the table" is false. The node reports what its admission spent; the subtask's
// ledger credits exactly that back, the same way the capacity wait is credited.
func TestTheCoherenceDeferCreditsBackTheNodesAdmission(t *testing.T) {
	pr := PlacedResult{Result: core.AgentWireResult{
		Deferred:         true,
		DeferClass:       core.DeferClassInfrastructure,
		Reason:           core.IncoherentSeatReason + "the completion is empty at the 96-token cap",
		AdmissionWaitSec: 180,
	}}
	if got := admissionCredit(pr); got != 180*time.Second {
		t.Fatalf("admissionCredit = %v, want the node's 180 s of admission", got)
	}
	// The arithmetic the retry floor actually reads: a subtask 200 s old on a
	// 300 s budget has 100 s left uncredited — under any real retry floor — and
	// 280 s once the node's 180 s cold load is credited back.
	start := time.Now().Add(-200 * time.Second)
	pl := newPlacements()
	if got := pl.remaining(start, 300); got > 101 || got < 99 {
		t.Fatalf("uncredited remaining = %d, want ≈100", got)
	}
	pl.credit += admissionCredit(pr)
	if got := pl.remaining(start, 300); got > 281 || got < 279 {
		t.Fatalf("credited remaining = %d, want ≈280", got)
	}
}

// TestOnlyTheCoherenceDeferIsCredited: the credit is not a general rebate. A
// result that RAN spent its admission on work the contract received, and a node
// that reports no admission at all is credited nothing rather than guessed at.
func TestOnlyTheCoherenceDeferIsCredited(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.AgentWireResult
	}{
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done", AdmissionWaitSec: 180}},
		{"a wall timeout", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget, Reason: "wall timeout after 300s", AdmissionWaitSec: 180}},
		{"another infrastructure defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: chat 502: bad gateway", AdmissionWaitSec: 180}},
		{"a coherence defer with no admission reported", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.IncoherentSeatReason + "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := admissionCredit(PlacedResult{Result: tc.result}); got != 0 {
				t.Fatalf("admissionCredit = %v, want 0 for %s", got, tc.name)
			}
		})
	}
}
