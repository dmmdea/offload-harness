package delegate

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The reason_code classifier keys on text the node writes, so its table has to be
// built from what the node REALLY writes. A hand-typed fixture that drops a prefix
// the node adds tests a string production never emits: every stall row filed
// during the structured re-pack was coded `infrastructure` while the table said
// `stall_repack`, and 44 mutants of the classifier died without touching it.

// repackUnreachable is the stable opening the node puts in front of a stall it saw
// DURING the structured re-pack (internal/pipeline/agenttask.go): the arm that
// files stallOf(live).Error() behind it. The loop arm files the bare text.
const repackUnreachable = "structured re-pack unreachable: "

func stallResult(reason string) PlacedResult {
	return PlacedResult{Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: reason}}
}

// TestStallReasonCodesHoldForTheTextsTheNodeReallyFiles builds every stall shape
// from agent.StallError itself and files it both ways the node does.
func TestStallReasonCodesHoldForTheTextsTheNodeReallyFiles(t *testing.T) {
	secs := func(n int) time.Duration { return time.Duration(n) * time.Second }
	stalls := []struct {
		name string
		err  *agent.StallError
		want string
	}{
		{"admission", &agent.StallError{Phase: agent.PhaseAdmission, Silent: secs(300), Allowed: secs(300)}, ledger.ReasonStallAdmission},
		{"prefill", &agent.StallError{Phase: agent.PhasePrefill, Silent: secs(300), Allowed: secs(240)}, ledger.ReasonStallPrefill},
		{"decoding", &agent.StallError{Phase: agent.PhaseDecoding, Silent: secs(90), Allowed: secs(60), Tokens: 812}, ledger.ReasonStallDecode},
		{"tool", &agent.StallError{Phase: agent.PhaseTool, Silent: secs(120), Allowed: secs(120), Tokens: 5}, ledger.ReasonStallTool},
		{"repack", &agent.StallError{Phase: agent.PhaseRepack, Silent: secs(120), Allowed: secs(120), Tokens: 900}, ledger.ReasonStallRepack},
		{"cold-load, still loading", &agent.StallError{Phase: agent.PhaseColdLoad, Silent: secs(400), Allowed: secs(300)}, ledger.ReasonStallColdLoad},
		{"cold-load, after ready", &agent.StallError{Phase: agent.PhaseColdLoad, PostReady: true, ReadySeen: true, Silent: secs(200), Allowed: secs(150)}, ledger.ReasonStallColdLoad},
		{"engine did no work", &agent.StallError{EngineFlat: true, EngineSilent: secs(120), Waited: agent.PhasePrefill, Allowed: secs(60), Engine: "idle"}, ledger.ReasonStallEngine},
		{"engine thrash", &agent.StallError{EngineThrash: true, EngineSilent: secs(360), Waited: agent.PhaseDecoding, Allowed: secs(60), Engine: "stepping"}, ledger.ReasonStallEngine},
	}
	for _, tc := range stalls {
		for _, prefix := range []string{"", repackUnreachable} {
			reason := prefix + tc.err.Error()
			got := reasonCodeFor(stallResult(reason))
			if got != tc.want {
				t.Errorf("%s, filed as %q: reason_code = %q, want %q", tc.name, reason, got, tc.want)
			}
		}
	}

	// The prefix is not a stall by itself: a re-pack that could not reach the seat,
	// or ran out of wall, is still the infrastructure it always was.
	for _, reason := range []string{
		repackUnreachable + "llama-server 500",
		repackUnreachable + "the wall expired during the wait",
	} {
		if got := reasonCodeFor(stallResult(reason)); got != ledger.ReasonInfrastructure {
			t.Errorf("%q: reason_code = %q, want %q", reason, got, ledger.ReasonInfrastructure)
		}
	}
}

// TestSeatUnreachableHintsAreEachLoadBearing: the seat-down hint is six phrases, and
// one fixture that carried two of them made every phrase individually deletable.
// One row per phrase, each containing ONLY that phrase, so dropping any one turns
// its row from seat_down into infrastructure.
func TestSeatUnreachableHintsAreEachLoadBearing(t *testing.T) {
	for _, tc := range []struct{ hint, reason string }{
		{"connection refused", "agent loop: seat: connect: connection refused"},
		{"actively refused", "agent loop: the target machine actively refused it"},
		{"no such host", "agent loop: lookup seat.example.invalid: no such host"},
		{"dial tcp", "agent loop: Post \"http://192.0.2.7:9/v1/chat/completions\": dial tcp 192.0.2.7:9: i/o timeout"},
		{"seat down", "the seat down for maintenance"},
		{"seat_down", "phase=seat_down"},
		{"case-insensitive", "AGENT LOOP: CONNECTION REFUSED"},
	} {
		if got := reasonCodeFor(stallResult(tc.reason)); got != ledger.ReasonSeatDown {
			t.Errorf("%s: %q coded %q, want %q", tc.hint, tc.reason, got, ledger.ReasonSeatDown)
		}
	}
	for _, reason := range []string{"building agent: boom", "a refusal of another kind: refused by policy", "no host reachable"} {
		if got := reasonCodeFor(stallResult(reason)); got != ledger.ReasonInfrastructure {
			t.Errorf("%q coded %q, want %q: not a dial failure", reason, got, ledger.ReasonInfrastructure)
		}
	}
}
