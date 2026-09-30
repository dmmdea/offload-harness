package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// warmFailedWire is what a node sends when its admission warm-up was refused and
// the seat never came up (register C-76, R-05a): an infrastructure defer, before
// any wall, with the admission it spent on the wire.
func warmFailedWire(node string) core.AgentWireResult {
	return core.AgentWireResult{
		SchemaVersion:    core.AgentWireSchemaVersion,
		NodeID:           node,
		Seat:             "seat-x",
		Deferred:         true,
		DeferClass:       core.DeferClassInfrastructure,
		Reason:           core.SeatWarmFailedReason + "warm request answered HTTP 500 after 3s but /running never listed seat-x ready: the seat's process did not start",
		AdmissionWaitSec: 180,
	}
}

// A seat that failed to start is a fact about THAT seat, caught before the wall
// started: the same contract on another node is the cure, exactly as for the
// admission-time coherence defer. It was terminal, counted as lost work, and the
// docs said another node may take it.
func TestSeatWarmFailedDeferIsRetryableOnAnotherNode(t *testing.T) {
	pr := PlacedResult{Result: warmFailedWire("node-a")}
	if !retryable(pr) {
		t.Fatal("a seat that failed to start must be retry-eligible on another node")
	}
}

// The credit is the same one the coherence defer earns: what the node's admission
// spent before it gave up (a cold load is minutes) is not the subtask's to pay,
// or the retry floor would refuse the retry on the very path that produces it.
func TestSeatWarmFailedDeferCreditsBackTheNodesAdmission(t *testing.T) {
	if got := admissionCredit(PlacedResult{Result: warmFailedWire("node-a")}); got != 180*time.Second {
		t.Fatalf("admissionCredit = %v, want the node's 180 s of admission", got)
	}
	w := warmFailedWire("node-a")
	w.AdmissionWaitSec = 0
	if got := admissionCredit(PlacedResult{Result: w}); got != 0 {
		t.Fatalf("admissionCredit = %v, want 0 when the node reported no admission", got)
	}
}

// The exception stays narrow: only the warm-up prefix on an infrastructure defer.
func TestOnlyTheWarmUpDeferIsTheSeatWarmFailedRetry(t *testing.T) {
	for name, w := range map[string]core.AgentWireResult{
		"another infrastructure defer":   {Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: chat 502: bad gateway", AdmissionWaitSec: 180},
		"a config defer with the prefix": {Deferred: true, DeferClass: core.DeferClassConfig, Reason: core.SeatWarmFailedReason + "x", AdmissionWaitSec: 180},
		"a budget defer":                 {Deferred: true, DeferClass: core.DeferClassBudget, Reason: "wall timeout after 300s", AdmissionWaitSec: 180},
	} {
		t.Run(name, func(t *testing.T) {
			pr := PlacedResult{Result: w}
			if retryable(pr) {
				t.Fatalf("%s must not be retried", name)
			}
			if got := admissionCredit(pr); got != 0 {
				t.Fatalf("admissionCredit = %v, want 0 for %s", got, name)
			}
		})
	}
}

// Through the engine: the first node's seat did not start, the second answers, and
// the subtask is recovered on it instead of counted as lost work.
func TestSeatWarmFailedDeferIsRecoveredOnAnotherNode(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	a := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-a"}
	a.pollByJob = func(string, int64) (map[string]any, int) { return doneWire(t, warmFailedWire("node-a")), 200 }
	_, urlB := eligibleNode(t, "node-b", "the answer from B")
	c := researchRetryContract()
	c.Door = "agent_delegate"
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{c}, "spread", []string{a.server().URL, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || sum.RetryRecovered != 1 || results[0].Result.Deferred {
		t.Fatalf("retried=%d recovered=%d deferred=%v reason=%q note=%q, want the subtask re-run on the other node and recovered",
			sum.Retried, sum.RetryRecovered, results[0].Result.Deferred, results[0].Result.Reason, results[0].RetryNote)
	}
}
