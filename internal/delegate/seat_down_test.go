package delegate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// seatDownWire is what a node whose seat went down under the run reports: an
// infrastructure defer whose reason opens core.SeatDownReason, plus the wait it
// spent on the dead seat.
func seatDownWire(node string, waitSec float64) core.AgentWireResult {
	w := remoteWire("", "")
	w.NodeID = node
	w.Output, w.Structured, w.StopReason = "", nil, "error"
	w.Deferred, w.DeferClass = true, core.DeferClassInfrastructure
	w.Reason = core.SeatDownReason + "the seat's engine did no work for 120s while this request waited in decoding (allowed 120s; engine: vllm-metrics: 5 running, 0 waiting; 0 tok so far); the seat did not come back (waited 200s)"
	w.SeatDownWaitSec = waitSec
	return w
}

// TestRetryableSeatDownDefer (ADR 0066, register C-72): a node whose seat went
// down under the run reports a sound contract it could not finish, and the fault
// is a property of THAT seat. The general infrastructure rule says another seat
// does not fix a broken stack; this is the second exception to it (after the
// coherence defer), because here the cure IS another node.
func TestRetryableSeatDownDefer(t *testing.T) {
	down := PlacedResult{Result: seatDownWire("node-a", 200)}
	if !SeatDownDefer(down.Result) {
		t.Fatal("the seat-down defer must be recognised by its reason prefix")
	}
	if !retryable(down) {
		t.Fatal("a seat-down defer must be retry-eligible on another node")
	}
	if !BrokenStackDefer(down.Result.DeferClass) {
		t.Fatal("it is still a broken stack for the exit code and the corpus: the box needs an operator")
	}
}

// The exception is narrow: the same words in another class, another
// infrastructure defer, a contended seat and a plain stall stay terminal.
func TestSeatDownDeferDoesNotWiden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.AgentWireResult
	}{
		{"the prefix in the wrong class", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassConfig, Reason: core.SeatDownReason + "x"}},
		{"a per-run stall (a node older than 0.144)", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "stalled: the seat's engine did no work for 120s"}},
		{"a contended seat", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "seat contended: 90s of wait budget spent"}},
		{"a seat that is not serving, from the status-aware wording", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "seat not serving: llama-swap answered HTTP 500"}},
		{"the prefix in the middle of a reason", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "structured re-pack unreachable: " + core.SeatDownReason + "x"}},
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if SeatDownDefer(tc.result) {
				t.Fatalf("SeatDownDefer must not claim %q (class %q)", tc.result.Reason, tc.result.DeferClass)
			}
			if retryable(PlacedResult{Result: tc.result}) {
				t.Fatalf("%s must not be retried", tc.name)
			}
		})
	}
}

// The credit (ADR 0066): the retry is budgeted in delegator wall clock since the
// subtask started, and a seat that hung for minutes and then waited for a restart
// has spent most of a default contract on a seat that could not serve it. The
// node reports what its admission and its wait on the dead seat spent; the
// subtask's ledger credits exactly that back, bounded by one contract wall.
func TestTheSeatDownDeferCreditsBackAdmissionAndTheWait(t *testing.T) {
	pr := PlacedResult{Result: seatDownWire("node-a", 200)}
	pr.Result.AdmissionWaitSec = 30
	if got := admissionCredit(pr); got != 230*time.Second {
		t.Fatalf("admissionCredit = %v, want the 30 s of admission plus the 200 s waited", got)
	}
	start := time.Now().Add(-240 * time.Second)
	pl := newPlacements()
	if got := pl.remaining(start, 300); got > 61 || got < 59 {
		t.Fatalf("uncredited remaining = %d, want ≈60", got)
	}
	pl.credit += admissionCredit(pr)
	if got := pl.remaining(start, 300); got > 291 || got < 289 {
		t.Fatalf("credited remaining = %d, want ≈290", got)
	}
	huge := PlacedResult{Result: seatDownWire("node-a", 1e6)}
	if got := admissionCredit(huge); got != time.Duration(core.AgentTimeoutSecCap)*time.Second {
		t.Fatalf("admissionCredit = %v: a node's number must never buy more than one contract wall (%d s)", got, core.AgentTimeoutSecCap)
	}
}

// Only the seat-down defer is credited its wait: every other result keeps what it
// had (nothing, or the coherence defer's admission).
func TestOnlyTheSeatDownDeferIsCreditedItsWait(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result core.AgentWireResult
	}{
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done", SeatDownWaitSec: 200}},
		{"another infrastructure defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: chat 502", SeatDownWaitSec: 200}},
		{"a coherence defer ignores a wait it did not have", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.IncoherentSeatReason + "x", SeatDownWaitSec: 200}},
		{"a seat-down defer that reports nothing", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.SeatDownReason + "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := admissionCredit(PlacedResult{Result: tc.result}); got != 0 {
				t.Fatalf("admissionCredit = %v, want 0 for %s", got, tc.name)
			}
		})
	}
}

// TestRunOneReplacesASeatDownDeferOnAnotherNodeWithCredit: node A's seat went
// down under the run (its defer carries 6 s of wait on the dead seat); the
// contract is placed once more on node B, which answers, and the retry is
// budgeted with the wait credited back — more than the first attempt's whole
// budget, because the first attempt took ~nothing of it on the delegator's
// clock and the node's wait was provably not work.
func TestRunOneReplacesASeatDownDeferOnAnotherNodeWithCredit(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		return doneWire(t, seatDownWire("node-a", 6)), 200
	}
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	var seenA, seenB atomic.Value
	nodeA.onDispatch = func(_ string, c core.AgentContract) { seenA.Store(c) }
	nodeB.onDispatch = func(_ string, c core.AgentContract) { seenB.Store(c) }
	contract := remoteContract() // timeout_sec 30
	contract.Acceptance = []string{"nonempty:answer"}
	// The local seat is fenced (as in the coherence-defer tests) so the retry has
	// exactly one place to go: node-b. Placement is not this test's subject.
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].RetriedOn != "node-b" || results[0].Result.Deferred {
		t.Fatalf("the seat-down defer must be re-placed on node-b: retried=%d retried_on=%q deferred=%v note=%q reason=%q",
			sum.Retried, results[0].RetriedOn, results[0].Result.Deferred, results[0].RetryNote, results[0].Result.Reason)
	}
	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("dispatches A=%d B=%d, want one each", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	first, _ := seenA.Load().(core.AgentContract)
	retry, _ := seenB.Load().(core.AgentContract)
	if first.TimeoutSec != 30 {
		t.Fatalf("first attempt timeout = %d, want 30", first.TimeoutSec)
	}
	if retry.TimeoutSec < 32 {
		t.Fatalf("retry timeout = %d s: the 6 s the node spent waiting on its dead seat were not credited back (want ≈ 34 = the 30 s budget plus the credit, less the first attempt's poll; anything at or under 30 is uncredited)", retry.TimeoutSec)
	}
}
