package delegate

// Register C-79, the retry of a lost-root defer: the second placement it is promised.
//
// A lost-root defer is a property of THAT run's directory (the contract is sound, the seat
// served it), and the retry goes to another node, so it is budgeted the way the seat-down
// defer's is: the first seat's own min_turn is not the retry seat's floor, the node's
// admission is time the contract's wall never ran, and a retry with nowhere to go is told so
// instead of being published with an empty note.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The lost-root wire carries the min_turn_sec of the seat that served the run (its cold load
// plus a final turn at its measured rate: a flagship that records a ~270 s load publishes
// ~390 s). The first-pass retry floor read that number of a seat the retry will never use, so
// a lost-root defer on a 300 s contract was refused its re-placement although the cure is
// another node whose own floor applies once it is chosen.
func TestRootGoneDeferIsReplacedWhenTheFirstSeatsMinTurnExceedsTheBudget(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		w := rootGoneWire("node-a")
		w.MinTurnSec = 387 // 270 s cold load + a 4,096-token final at 35 tok/s
		return doneWire(t, w), 200
	}
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	contract := remoteContract()
	contract.TimeoutSec = 300
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].RetriedOn != "node-b" || results[0].Result.Deferred {
		t.Fatalf("retried=%d retried_on=%q deferred=%v dispatches A=%d B=%d note=%q", sum.Retried, results[0].RetriedOn, results[0].Result.Deferred,
			nodeA.dispatches.Load(), nodeB.dispatches.Load(), results[0].RetryNote)
	}
}

// The retry seat's own floor still applies once it is chosen: the exemption is the FIRST
// seat's number, not the retry's. node-b publishes that its seat needs 555 s and the contract
// has 300, so the re-placement is refused for that reason.
func TestRootGoneDeferStillHonoursTheRetrySeatsOwnFloor(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, rootGoneWire("node-a")), 200 }
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	nodeB.seatRate = map[string]any{"tok_s": 5.0, "cold_load_sec": 350.0, "samples": 6, "min_turn_sec": 555}
	nodeB.seatBudget = map[string]any{"step_tokens": 1024, "final_tokens": 1024, "thinking": "auto"}
	contract := remoteContract()
	contract.TimeoutSec = 300
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 0 || nodeB.dispatches.Load() != 0 || !strings.Contains(results[0].RetryNote, "min_turn_sec published by node-b") {
		t.Fatalf("retried=%d B dispatches=%d note=%q, want the re-placement refused on the retry seat's own floor", sum.Retried, nodeB.dispatches.Load(), results[0].RetryNote)
	}
}

// The credit: the retry is budgeted in delegator wall clock since the subtask started, and a
// run that waited out a cold load before its wall began has spent most of a default contract
// on time the contract never ran. The node reports what its admission spent; the subtask's
// ledger credits exactly that back, bounded by one contract wall. Nothing else is credited to
// a lost-root defer: the wall the run did spend working before its directory went was work.
func TestTheRootGoneDeferCreditsBackTheNodesAdmission(t *testing.T) {
	pr := PlacedResult{Result: rootGoneWire("node-a")}
	pr.Result.AdmissionWaitSec = 180
	if got := admissionCredit(pr); got != 180*time.Second {
		t.Fatalf("admissionCredit = %v, want the node's 180 s of admission", got)
	}
	// A subtask 200 s old on a 300 s budget has 100 s left uncredited, under any real retry
	// floor, and 280 s once the cold load is credited back.
	start := time.Now().Add(-200 * time.Second)
	pl := newPlacements()
	if got := pl.remaining(start, 300); got > 101 || got < 99 {
		t.Fatalf("uncredited remaining = %d, want about 100", got)
	}
	pl.credit += admissionCredit(pr)
	if got := pl.remaining(start, 300); got > 281 || got < 279 {
		t.Fatalf("credited remaining = %d, want about 280", got)
	}
	// Only the admission: the wait on a seat that went down earlier in the run and the wall of
	// a rescue are the seat-down defer's own credits.
	pr.Result.SeatDownWaitSec = 200
	pr.rescueSpent = 400 * time.Second
	if got := admissionCredit(pr); got != 180*time.Second {
		t.Fatalf("admissionCredit = %v, want 180 s: a lost-root defer is credited its admission and nothing else", got)
	}
	// And bounded by one contract wall, whatever a node reports.
	huge := PlacedResult{Result: rootGoneWire("node-a")}
	huge.Result.AdmissionWaitSec = 1e6
	if got := admissionCredit(huge); got != time.Duration(core.AgentTimeoutSecCap)*time.Second {
		t.Fatalf("admissionCredit = %v: a node's number must never buy more than one contract wall (%d s)", got, core.AgentTimeoutSecCap)
	}
	// A lost-root defer that reports no admission is credited nothing rather than guessed at.
	if got := admissionCredit(PlacedResult{Result: rootGoneWire("node-a")}); got != 0 {
		t.Fatalf("admissionCredit = %v, want 0 for a defer that reports no admission", got)
	}
}

// A lost-root defer is promised a second placement; when there is nowhere to put it the
// caller is told it was considered and why it did not happen, never left with a retryable
// defer and an empty note.
func TestRootGoneDeferWithNowhereToGoSaysSo(t *testing.T) {
	t.Run("route local", func(t *testing.T) {
		local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
			return rootGoneWire("local"), nil
		}
		results, sum, err := Run(context.Background(), testCfg(t), local, []core.AgentContract{remoteContract()}, "local", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := "retry skipped: the run on local lost its workspace root and no other node could take the contract (route local places nothing on another node)"
		if sum.Retried != 0 || !results[0].Result.Deferred || results[0].RetryNote != want {
			t.Fatalf("retried=%d deferred=%v note=%q, want the refusal named: %q", sum.Retried, results[0].Result.Deferred, results[0].RetryNote, want)
		}
	})
	t.Run("the only remote node was the one whose directory went", func(t *testing.T) {
		compressPolls(t, 10*time.Millisecond, 2*time.Second)
		nodeA, urlA := eligibleNode(t, "node-a", "unused")
		scriptFirstJob(t, func(node string) core.AgentWireResult { return rootGoneWire(node) }, "unused", nodeA)
		// A seatless box has no local seat to retry on and no lease to name, so the only
		// thing left to say is the node's own.
		results, sum, err := Run(context.Background(), seatlessCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "spread", []string{urlA})
		if err != nil {
			t.Fatal(err)
		}
		want := "retry skipped: the run on node-a lost its workspace root and no other node could take the contract (every eligible node was already tried or is not eligible)"
		if sum.Retried != 0 || !results[0].Result.Deferred || results[0].RetryNote != want {
			t.Fatalf("retried=%d deferred=%v note=%q, want the missing second placement explained: %q", sum.Retried, results[0].Result.Deferred, results[0].RetryNote, want)
		}
	})
}
