package delegate

// Register C-79, the delegator's half. A node whose run lost its workspace root
// reports an `infrastructure` defer whose reason opens core.RootGoneReason. Before
// the node classified it, the seat's "I could not read the file" came back as a
// failed verification, which the delegator re-places on another node; a plain
// infrastructure defer is not re-placed ("a broken box does not get better on
// another seat"), so without this predicate the better classification would have
// lost the recovery. The fault is the run's own materialized root, not the seat or
// the contract: the same contract on another node materializes a root of its own.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// rootGoneWire is what a node reports for a run whose root vanished: an
// infrastructure defer whose reason opens core.RootGoneReason and names the root.
func rootGoneWire(node string) core.AgentWireResult {
	w := remoteWire("", "")
	w.NodeID = node
	w.Output, w.Structured, w.StopReason = "", nil, "error"
	w.Deferred, w.DeferClass = true, core.DeferClassInfrastructure
	w.Reason = core.RootGoneReason + "/jobs/agent-local-1/context: no such file or directory - the run's directory could not be opened, so the run was stopped, or its result withheld, instead of going on without it"
	return w
}

func TestRetryableRootGoneDefer(t *testing.T) {
	gone := PlacedResult{Result: rootGoneWire("node-a")}
	if !RootGoneDefer(gone.Result) {
		t.Fatal("the lost-root defer must be recognised by its reason prefix")
	}
	if !retryable(gone) {
		t.Fatal("a lost-root defer must be retry-eligible on another node")
	}
	if !BrokenStackDefer(gone.Result.DeferClass) {
		t.Fatal("it is still a broken stack for the exit code and the corpus: the box lost a directory it made")
	}
}

// The exception is narrow: the same words in another class, another infrastructure
// defer, the words in the middle of a reason, and a clean result stay as they were.
func TestRootGoneDeferDoesNotWiden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.AgentWireResult
	}{
		{"the prefix in the wrong class", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassConfig, Reason: core.RootGoneReason + "x"}},
		{"the prefix on a result that did not defer", core.AgentWireResult{DeferClass: core.DeferClassInfrastructure, Reason: core.RootGoneReason + "x"}},
		{"a per-run stall", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "stalled: the seat's engine did no work for 120s"}},
		{"another infrastructure defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "building agent: bad ReadRoot"}},
		{"the prefix in the middle of a reason", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "structured re-pack unreachable: " + core.RootGoneReason + "x"}},
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if RootGoneDefer(tc.result) {
				t.Fatalf("RootGoneDefer must not claim %q (class %q)", tc.result.Reason, tc.result.DeferClass)
			}
			if retryable(PlacedResult{Result: tc.result}) {
				t.Fatalf("%s must not be retried", tc.name)
			}
		})
	}
}

// TestRunOneReplacesARootGoneDeferOnAnotherNode: node A's run lost its root and
// said so; the contract is placed once more on node B, which answers. The result
// the caller gets is B's answer, carrying the retry, not A's defer.
func TestRunOneReplacesARootGoneDeferOnAnotherNode(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		return doneWire(t, rootGoneWire("node-a")), 200
	}
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	var seenA, seenB atomic.Value
	nodeA.onDispatch = func(_ string, c core.AgentContract) { seenA.Store(c) }
	nodeB.onDispatch = func(_ string, c core.AgentContract) { seenB.Store(c) }
	contract := remoteContract() // timeout_sec 30
	contract.Acceptance = []string{"nonempty:answer"}
	// The local seat is fenced so the retry has exactly one place to go: node-b.
	// Placement is not this test's subject.
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].RetriedOn != "node-b" || results[0].Result.Deferred {
		t.Fatalf("the lost-root defer must be re-placed on node-b: retried=%d retried_on=%q deferred=%v note=%q reason=%q",
			sum.Retried, results[0].RetriedOn, results[0].Result.Deferred, results[0].RetryNote, results[0].Result.Reason)
	}
	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("dispatches A=%d B=%d, want one each", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	if sum.LostToStack != 0 {
		t.Errorf("lost_to_stack = %d, want 0: the retry delivered the contracted output", sum.LostToStack)
	}
}
