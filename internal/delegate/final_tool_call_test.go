package delegate

// A run whose forced final step was answered with a tool call, even after the loop re-asked once (internal/agent), is
// a fact about THAT seat: one seat family did it in 35 of 106 forced finals and the seats of every other family in none
// of 241. The contract is sound and the run had read what it needed (43 s of 600 s spent in the row that shows it), so
// the cure is another seat, and the shape used to be filed `budget`, which retryable() leaves alone as "not something
// another seat fixes". It is the sixth defer worth a second placement, matched on the constant the loop writes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// finalToolCallWire is what the executing node files for the shape (pipeline.runAgentTask, the budget stop).
func finalToolCallWire(node string) core.AgentWireResult {
	w := remoteWire("", "")
	w.NodeID = node
	w.Output, w.Structured, w.StopReason = "", nil, "budget"
	w.Deferred, w.DeferClass = true, core.DeferClassBudget
	w.Steps = 12
	w.Reason = "step budget exhausted (12 steps): " + core.ForcedFinalToolCallReason + `, and the seat wrote a tool call as text ("<tool_call>") - nothing executed`
	return w
}

func TestRetryableFinalToolCallDefer(t *testing.T) {
	ftc := PlacedResult{Result: finalToolCallWire("node-a")}
	if !FinalToolCallDefer(ftc.Result) || !retryable(ftc) {
		t.Fatalf("a forced-final tool call must be recognised by its constant and be retry-eligible: %+v", ftc.Result)
	}
	plain := PlacedResult{Result: finalToolCallWire("node-a")}
	plain.Result.Reason = "step budget exhausted (12 steps)"
	if FinalToolCallDefer(plain.Result) || retryable(plain) {
		t.Fatal("a plain step-budget defer is not worth a second seat: the exception is the one shape")
	}
	other := PlacedResult{Result: finalToolCallWire("node-a")}
	other.Result.DeferClass = core.DeferClassInfrastructure
	if FinalToolCallDefer(other.Result) {
		t.Fatal("the same words in another defer class are not the shape")
	}
	if ftc.Result.Deferred = false; FinalToolCallDefer(ftc.Result) {
		t.Fatal("a finished result is not the shape")
	}
}

// The first seat writes the tool call; the second seat, another node, answers.
func TestAFinalToolCallDeferIsReplacedOnAnotherSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	scriptFirstJob(t, finalToolCallWire, "the answer from B", nodeA, nodeB)
	contract := remoteContract()
	contract.TimeoutSec = 300
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || sum.RetryRecovered != 1 || results[0].Result.Deferred || results[0].RetriedOn == "" {
		t.Fatalf("summary %+v result deferred=%v retried_on=%q note=%q dispatches A=%d B=%d, want the contract recovered on the other seat",
			sum, results[0].Result.Deferred, results[0].RetriedOn, results[0].RetryNote, nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// With nowhere to put the second placement the caller is told it was considered, and why it did not happen.
func TestAFinalToolCallDeferWithNowhereToGoSaysSo(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	scriptFirstJob(t, finalToolCallWire, "unused", nodeA)
	results, sum, err := Run(context.Background(), seatlessCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "spread", []string{urlA})
	if err != nil {
		t.Fatal(err)
	}
	note := results[0].RetryNote
	if sum.Retried != 0 || !results[0].Result.Deferred || !strings.Contains(note, "retry skipped") || !strings.Contains(note, "tool call") || !strings.Contains(note, "no other node") {
		t.Fatalf("retried=%d deferred=%v note=%q, want the refusal named: the seat wrote a tool call as its answer and no other node could take it", sum.Retried, results[0].Result.Deferred, note)
	}
}
