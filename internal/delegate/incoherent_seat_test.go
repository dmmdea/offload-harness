package delegate

import (
	"testing"

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
