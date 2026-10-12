package delegate

// A verification retry goes to a DIFFERENT node, so the first attempt's seat says nothing about what the retry needs. The
// first-pass floor (taken before a retry node is chosen) used to read the first seat's own min_turn_sec for every retryable
// shape but two (the seat-down and lost-root defers, exempted for exactly this reason): a research digest that failed
// acceptance on a seat whose cold load plus two 8,192-token turns is measured at 1,112 s was refused its retry for a floor of
// 1,112 s although the node it would have gone to needs a fraction of that, and the same seat took 20 to 325 s to give the
// answer. 7 of the 14 retries skipped in one day were refused that way (the rest fell to other rules: the chosen node's own floor, a
// busy retry seat, a fenced local seat).
// The retry seat's own floor still applies once it is chosen, and so does the configured one.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// wrongAnswerWire is a finished answer that fails the contract's acceptance, from a seat whose own floor is minTurn.
func wrongAnswerWire(node string, minTurn int) core.AgentWireResult {
	w := remoteWire("no idea", `{"answer":"no idea"}`)
	w.NodeID = node
	w.MinTurnSec = minTurn
	return w
}

func publishSmallSeat(n *fakeNode) {
	n.seatRate = map[string]any{"tok_s": 50.0, "cold_load_sec": 28.0, "samples": 6, "min_turn_sec": 70}
	n.seatBudget = map[string]any{"step_tokens": 1024, "final_tokens": 1024, "thinking": "auto"}
}

func TestAVerificationRetryIsNotRefusedByTheFirstSeatsMinTurn(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	publishSmallSeat(nodeA)
	publishSmallSeat(nodeB)
	scriptFirstJob(t, func(node string) core.AgentWireResult { return wrongAnswerWire(node, 1112) }, "zorblax from B", nodeA, nodeB)
	contract := remoteContract()
	contract.TimeoutSec = 300
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Retried != 1 || sum.RetryRecovered != 1 || pr.RetriedOn == "" || len(pr.AcceptanceFailures) != 0 {
		t.Fatalf("summary %+v retried_on %q note %q failures %v dispatches A=%d B=%d, want the wrong answer retried and recovered on the other node",
			sum, pr.RetriedOn, pr.RetryNote, pr.AcceptanceFailures, nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// The floor of the node the retry will actually run on still applies once it is chosen.
func TestTheRetrySeatsOwnFloorStillRefusesAVerificationRetry(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	for _, n := range []*fakeNode{nodeA, nodeB} {
		n.seatRate = map[string]any{"tok_s": 5.0, "cold_load_sec": 350.0, "samples": 6, "min_turn_sec": 555}
		n.seatBudget = map[string]any{"step_tokens": 1024, "final_tokens": 1024, "thinking": "auto"}
	}
	scriptFirstJob(t, func(node string) core.AgentWireResult { return wrongAnswerWire(node, 1112) }, "zorblax from B", nodeA, nodeB)
	contract := remoteContract()
	contract.TimeoutSec = 300
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	note := results[0].RetryNote
	if sum.Retried != 0 || !strings.Contains(note, "after choosing a retry node") || !strings.Contains(note, "min_turn_sec published by") {
		t.Fatalf("retried=%d note=%q, want the re-placement refused on the chosen node's own floor, after the node was chosen", sum.Retried, note)
	}
}

// The configured floor (agent_retry_min_sec) is still the cheap refusal before a node is chosen.
func TestTheConfiguredFloorStillRefusesBeforeANodeIsChosen(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	scriptFirstJob(t, func(node string) core.AgentWireResult { return wrongAnswerWire(node, 0) }, "zorblax from B", nodeA, nodeB)
	contract := remoteContract()
	contract.TimeoutSec = 300
	cfg := testCfg(t)
	cfg.AgentRetryMinSec = 400
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	note := results[0].RetryNote
	if sum.Retried != 0 || !strings.Contains(note, "after the first attempt") || !strings.Contains(note, "agent_retry_min_sec") {
		t.Fatalf("retried=%d note=%q, want the configured floor to refuse before a retry node is chosen", sum.Retried, note)
	}
}
