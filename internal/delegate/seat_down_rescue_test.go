package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A seat lost DURING the structured re-pack (ADR 0066) files the `seat down:`
// prefix with the finished answer in output and, like every failure of the re-pack,
// the schema_miss flag (register C-66, PR-4). The two decisions compose: the
// delegator holds the finished answer, so it re-packs it itself first (the lossless
// reading, or one completion on its own seat), and the contract is re-placed on
// another node only when that cannot produce a validated object — re-placing a
// finished answer runs the whole loop again.

// seatDownDuringTheRepack is the wire a node files for it: the loop's answer intact,
// stop_reason done, the seat-down reason with the re-pack suffix, and the wait the
// node spent on the dead seat.
func seatDownDuringTheRepack(node string) core.AgentWireResult {
	w := legacyRepackStall()
	w.NodeID = node
	w.SchemaMiss = true
	w.Reason = core.SeatDownReason + "the seat's engine did no work for 120s while this request waited in repack (allowed 120s; engine: vllm-metrics: 5 running, 0 waiting; 900 tok so far); the seat did not come back (waited 200s) (during the structured re-pack)"
	w.SeatDownWaitSec = 6
	return w
}

// TestASeatDownDuringTheRepackIsBothRescuableAndReplaceable is the premise of the
// composition: the one wire is the seat-down defer the delegator re-places AND a
// schema miss it may rescue, so the order in which the two run decides what the
// caller gets.
func TestASeatDownDuringTheRepackIsBothRescuableAndReplaceable(t *testing.T) {
	w := seatDownDuringTheRepack("node-a")
	if !SeatDownDefer(w) || !SchemaMissRescuable(w) {
		t.Fatalf("seat-down defer = %v, rescuable = %v: want both for %q", SeatDownDefer(w), SchemaMissRescuable(w), w.Reason)
	}
	if !retryable(PlacedResult{Result: w}) {
		t.Fatal("a seat-down defer that nobody rescued must stay retry-eligible")
	}
}

// The rescue comes first and, when it structures the answer, the subtask is a
// success: nothing is re-placed and the second node never hears of it.
func TestASeatDownDuringTheRepackIsRescuedBeforeItIsReplaced(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(string, int64) (map[string]any, int) {
		return doneWire(t, seatDownDuringTheRepack("node-a")), 200
	}
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	rs := &rescuer{structured: `{"answer":"42"}`}
	// The local seat is fenced, as in the coherence-defer tests, so the contract has
	// exactly two places to go: node-a first, node-b for a re-placement.
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := RunWith(context.Background(), cfg, neverLocal(t), []core.AgentContract{rescueContract()}, "spread", []string{urlA, urlB}, &RunOptions{Rescue: rs.fn()})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if rs.calls.Load() != 1 || sum.Succeeded != 1 || r.Result.Deferred || r.RetriedOn != "" || sum.Retried != 0 {
		t.Fatalf("rescue calls=%d summary=%+v retried_on=%q deferred=%v reason=%q: want the finished answer rescued and nothing re-placed",
			rs.calls.Load(), sum, r.RetriedOn, r.Result.Deferred, r.Result.Reason)
	}
	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 0 {
		t.Fatalf("dispatches A=%d B=%d, want the contract to stay with its first node", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// When the rescue cannot produce an object, the node's defer stands and the
// contract is re-placed on another node with the wait credited back, exactly as a
// seat-down defer of the loop is.
func TestASeatDownDuringTheRepackIsReplacedWhenTheRescueFails(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(string, int64) (map[string]any, int) {
		return doneWire(t, seatDownDuringTheRepack("node-a")), 200
	}
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	rs := &rescuer{err: errors.New("the local seat is not serving")}
	contract := rescueContract()
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := RunWith(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB}, &RunOptions{Rescue: rs.fn()})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if rs.calls.Load() != 1 || sum.Retried != 1 || r.RetriedOn != "node-b" || r.Result.Deferred {
		t.Fatalf("rescue calls=%d retried=%d retried_on=%q deferred=%v note=%q reason=%q: want the failed rescue followed by a re-placement on node-b",
			rs.calls.Load(), sum.Retried, r.RetriedOn, r.Result.Deferred, r.RetryNote, r.Result.Reason)
	}
	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("dispatches A=%d B=%d, want one each", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}
