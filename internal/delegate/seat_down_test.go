package delegate

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
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
		{"a per-run stall (a node without ADR 0066)", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "stalled: the seat's engine did no work for 120s"}},
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

// A seat lost in the structured re-pack reports no wait of its own (the loop's
// recovery does not cover the re-pack) and the delegator's rescue ran first, on its
// own clock: what a FAILED rescue spent is credited back with the node's admission and
// wait, inside the same one-contract-wall bound. Uncredited, a rescue that cold-loads
// the delegator's seat and runs a completion to its allowance (minutes) leaves the retry
// floor to refuse the re-placement the defer is promised.
func TestTheSeatDownDeferCreditsBackTheWallOfAFailedRescue(t *testing.T) {
	pr := PlacedResult{Result: seatDownWire("node-a", 0), rescueSpent: 400 * time.Second}
	pr.Result.AdmissionWaitSec = 30
	if got := admissionCredit(pr); got != 430*time.Second {
		t.Fatalf("admissionCredit = %v, want the 30 s of admission plus the 400 s the failed rescue spent", got)
	}
	pr.Result.SeatDownWaitSec = 100
	if got := admissionCredit(pr); got != 530*time.Second {
		t.Fatalf("admissionCredit = %v, want admission, the wait on the dead seat and the rescue's wall added", got)
	}
	// The retry's budget: a 300 s contract whose first attempt took 420 s of the delegator's
	// clock (20 s of work, a 400 s rescue) has nothing left until the rescue is credited.
	start := time.Now().Add(-420 * time.Second)
	pl := newPlacements()
	if got := pl.remaining(start, 300); got >= 0 {
		t.Fatalf("uncredited remaining = %d, want nothing left (premise)", got)
	}
	pl.credit += admissionCredit(PlacedResult{Result: seatDownWire("node-a", 0), rescueSpent: 400 * time.Second})
	if got := pl.remaining(start, 300); got < 279 || got > 281 {
		t.Fatalf("credited remaining = %d, want about 280", got)
	}
	// Bounded, with the rest of the credit, by one contract wall.
	huge := PlacedResult{Result: seatDownWire("node-a", 200), rescueSpent: 1e6 * time.Second}
	if got := admissionCredit(huge); got != time.Duration(core.AgentTimeoutSecCap)*time.Second {
		t.Fatalf("admissionCredit = %v: a slow rescue must never buy more than one contract wall (%d s)", got, core.AgentTimeoutSecCap)
	}
}

// Only the seat-down defer is credited its wait and its rescue: every other result
// keeps what it had (nothing, or the coherence defer's admission).
func TestOnlyTheSeatDownDeferIsCreditedItsWait(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      core.AgentWireResult
		rescueSpent time.Duration
	}{
		{"a clean result", core.AgentWireResult{Output: "42", StopReason: "done", SeatDownWaitSec: 200}, 0},
		{"another infrastructure defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: chat 502", SeatDownWaitSec: 200}, 0},
		{"a coherence defer ignores a wait it did not have", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.IncoherentSeatReason + "x", SeatDownWaitSec: 200}, 0},
		{"a seat-down defer that reports nothing", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.SeatDownReason + "x"}, 0},
		{"a failed rescue's wall on a defer that is not the seat-down one", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "structured re-pack unreachable: stalled: no progress for 120s in repack"}, 400 * time.Second},
		{"a failed rescue's wall on a coherence defer", core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: core.IncoherentSeatReason + "x"}, 400 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := admissionCredit(PlacedResult{Result: tc.result, rescueSpent: tc.rescueSpent}); got != 0 {
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

// The caller sees what the run survived: seat_recoveries and seat_down_wait_sec
// reach the published result wire beside queued_ms and the other wait figures.
func TestResultWireCarriesTheSeatRecoveries(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	w := remoteWire("the answer", `{"answer":"42"}`)
	w.SeatRecoveries, w.SeatDownWaitSec = 1, 42.5
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(n int64) (map[string]any, int) { return doneWire(t, w), 200 },
	}
	srv := node.server()
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got := wireOf(t, results, sum)
	if v, _ := got["seat_recoveries"].(float64); v != 1 {
		t.Fatalf("seat_recoveries = %v on the result wire: %v", got["seat_recoveries"], got)
	}
	if v, _ := got["seat_down_wait_sec"].(float64); v != 42.5 {
		t.Fatalf("seat_down_wait_sec = %v on the result wire", got["seat_down_wait_sec"])
	}
}

// The published seat-down wire carries the dead seat's OWN min_turn_sec (its cold
// load plus a final turn at its measured rate: a flagship that records its ~270 s
// load publishes ~390 s). The first-pass retry floor read that number of the FAILED
// seat, so a seat-down defer on a 300 s contract was refused re-placement although
// the cure is another node whose own floor applies once it is chosen.
func TestSeatDownDeferIsReplacedWhenTheDeadSeatsMinTurnExceedsTheBudget(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		w := seatDownWire("node-a", 6)
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

// The retry seat's own floor still applies once it is chosen: the exemption is the
// FAILED seat's number, not the retry's. node-b publishes that its seat needs 350 s
// and the contract has 300, so the re-placement is refused for that reason.
func TestSeatDownDeferStillHonoursTheRetrySeatsOwnFloor(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, seatDownWire("node-a", 6)), 200 }
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

// The other refusal: the alternative node is at its ceiling. During the
// 2026-09-29 outage the fleet was saturated (44 runs in flight for ~13 slots), so
// this is the normal state, not an edge: busy is a place in line, never a refusal
// (INV-4) — the node's own queue is the line.
func TestSeatDownDeferIsReplacedOnAnAlternativeThatIsAtItsCeiling(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, seatDownWire("node-a", 6)), 200 }
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	nodeB.maxConcurrentJobs, nodeB.jobsRunning, nodeB.queueDepth = 4, 4, 4
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].Result.Deferred {
		t.Fatalf("retried=%d deferred=%v dispatches A=%d B=%d note=%q", sum.Retried, results[0].Result.Deferred, nodeA.dispatches.Load(), nodeB.dispatches.Load(), results[0].RetryNote)
	}
}

// A seat-down defer is promised a second placement; when there is nowhere to put
// it the caller is told it was considered and why it did not happen, never left
// with a retryable defer and an empty note.
func TestSeatDownDeferWithNowhereToGoSaysSo(t *testing.T) {
	t.Run("route local", func(t *testing.T) {
		local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
			return seatDownWire("local", 5), nil
		}
		contract := remoteContract()
		results, sum, err := Run(context.Background(), testCfg(t), local, []core.AgentContract{contract}, "local", nil)
		if err != nil {
			t.Fatal(err)
		}
		if sum.Retried != 0 || !results[0].Result.Deferred || !strings.Contains(results[0].RetryNote, "no other node could take the contract") ||
			!strings.Contains(results[0].RetryNote, "route local") {
			t.Fatalf("retried=%d deferred=%v note=%q, want the refusal named", sum.Retried, results[0].Result.Deferred, results[0].RetryNote)
		}
	})
	t.Run("the only remote node was the one that went down", func(t *testing.T) {
		compressPolls(t, 10*time.Millisecond, 2*time.Second)
		nodeA, urlA := eligibleNode(t, "node-a", "unused")
		nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, seatDownWire("node-a", 6)), 200 }
		contract := remoteContract()
		cfg := testCfg(t)
		cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})
		results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Retried != 0 || results[0].RetryNote == "" {
			t.Fatalf("retried=%d note=%q, want the missing second placement explained", sum.Retried, results[0].RetryNote)
		}
	})
}

// scriptFirstJob scripts a fleet for a test about where a SECOND placement goes: whichever of the
// nodes is handed a job first answers it with first(node), and every other job any of them is handed
// is answered with `later`. Keyed on the job id (a poll repeats its answer) and on no node name, so
// the test does not depend on where the deal or the ranking put the first attempt.
func scriptFirstJob(t *testing.T, first func(node string) core.AgentWireResult, later string, nodes ...*fakeNode) {
	t.Helper()
	var mu sync.Mutex
	firstJob := ""
	for _, n := range nodes {
		id := n.nodeID
		n.pollByJob = func(jobID string, _ int64) (map[string]any, int) {
			mu.Lock()
			if firstJob == "" {
				firstJob = jobID
			}
			isFirst := firstJob == jobID
			mu.Unlock()
			if isFirst {
				return doneWire(t, first(id)), 200
			}
			w := remoteWire(later, `{"answer":"`+later+`"}`)
			w.NodeID = id
			return doneWire(t, w), 200
		}
	}
}

// occupyTheLocalSeat registers one run on the local agent seat: the record the seat's run-cap line
// counts. With fleet_max_concurrent_jobs = 1 the line then has no free slot ahead of a newcomer.
func occupyTheLocalSeat(t *testing.T, cfg config.Config) {
	t.Helper()
	run := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: cfg.AgentPlannerModel(""), Kind: "contract", Goal: "occupies the seat", Phase: gpuactivity.PhaseRunning})
	if run == nil {
		t.Fatal("fixture: could not register a run on the local seat")
	}
	t.Cleanup(run.End)
}

// A seat-down re-placement is exempt from the retry seat's busy check (ADR 0066 decision 3): "the
// node's own queue is the line, and its 503 is re-placed at once". That is true of a REMOTE
// alternative and false of the local seat, which answers no 503: a run forced onto it joins the
// run-cap line and waits there for the run's whole wall before it defers as capacity (the first
// come first served gate of pipeline/agenttask.go), a line the subtask could not leave. So while
// that line has no free slot ahead of a newcomer (the question replacementNode asks, ADR 0063
// decision 2), an untried remote is where the retry goes. A dying seat with several jobs on it is the
// normal case, and without this every one of them landed on the delegator's own card.
func TestSeatDownRetryLeavesAFullLocalLineForAnUntriedRemote(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "unused")
	scriptFirstJob(t, func(node string) core.AgentWireResult { return seatDownWire(node, 6) }, "the answer from the second node", nodeA, nodeB)
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	occupyTheLocalSeat(t, cfg)
	var localCalls atomic.Int64
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}

	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if nodeA.dispatches.Load()+nodeB.dispatches.Load() == 0 {
		t.Fatalf("fixture: the first attempt ran on the local seat (local calls %d), so there was no seat-down defer to re-place", localCalls.Load())
	}
	if localCalls.Load() != 0 {
		t.Fatalf("the retry joined the local seat's full run-cap line (local calls %d) while an untried remote sat idle: a line the subtask cannot leave, waited out for the retry's whole wall; note=%q", localCalls.Load(), results[0].RetryNote)
	}
	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("dispatches A=%d B=%d, want the seat-down attempt on one node and the retry on the other", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	if sum.Retried != 1 || results[0].Result.Deferred {
		t.Fatalf("retried=%d deferred=%v note=%q reason=%q, want the seat-down defer re-placed and answered", sum.Retried, results[0].Result.Deferred, results[0].RetryNote, results[0].Result.Reason)
	}
	if !strings.Contains(results[0].PlacementReason, "run-cap line") {
		t.Fatalf("placement reason = %q, want it to say the retry skipped the local seat because its run-cap line is full", results[0].PlacementReason)
	}
}

// The control the preference must not break: with a free slot on the local seat's line a seat-down
// retry still goes where it always went (local -> the best remote is the verification retry's rule,
// remote -> local is this one's), and the untried remote is not touched.
func TestSeatDownRetryStillTakesTheLocalSeatWhenItsLineHasRoom(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "unused")
	scriptFirstJob(t, func(node string) core.AgentWireResult { return seatDownWire(node, 6) }, "the answer from the second node", nodeA, nodeB)
	var localCalls atomic.Int64
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}

	_, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), []core.AgentContract{contract}, "remote", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeA.dispatches.Load() + nodeB.dispatches.Load(); got != 1 {
		t.Fatalf("remote dispatches = %d, want only the first attempt: a local seat with a free slot keeps the retry", got)
	}
	if localCalls.Load() != 1 || sum.Retried != 1 {
		t.Fatalf("local calls=%d retried=%d, want the retry on the local seat", localCalls.Load(), sum.Retried)
	}
}

// Joining a full line beats losing the job: with no untried remote left the local seat is the only
// place the seat-down retry can go, so it stays there, and the job is not refused for a busy line
// (ADR 0066 decision 3, INV-4).
func TestSeatDownRetryStaysOnAFullLocalLineWhenNoRemoteIsLeft(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, seatDownWire("node-a", 6)), 200 }
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	occupyTheLocalSeat(t, cfg)
	var localCalls atomic.Int64
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}

	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{contract}, "remote", []string{urlA})
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Retried != 1 || results[0].Result.Deferred {
		t.Fatalf("local calls=%d retried=%d deferred=%v note=%q, want the retry on the only other seat there is", localCalls.Load(), sum.Retried, results[0].Result.Deferred, results[0].RetryNote)
	}
}

// The preference is the seat-down defer's, not every retry's: a verification retry has its own busy
// guard for the seat it lands on (retrySeatBusy and awaitRetrySeat, ADR 0063 decision 10) and keeps
// the local seat as before. Widening the rule to it is a separate decision, and this is where it
// would have to be made.
func TestAVerificationRetryStillTakesTheLocalSeatWhoseLineIsFull(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeB, urlB := eligibleNode(t, "node-b", "unused")
	scriptFirstJob(t, func(node string) core.AgentWireResult {
		w := remoteWire("wrong answer", `{"answer":"wrong answer"}`)
		w.NodeID = node
		return w
	}, "qube from the second node", nodeA, nodeB)
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 1
	occupyTheLocalSeat(t, cfg)
	var localCalls atomic.Int64

	_, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "remote", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeA.dispatches.Load() + nodeB.dispatches.Load(); got != 1 {
		t.Fatalf("remote dispatches = %d, want only the first attempt", got)
	}
	if localCalls.Load() != 1 || sum.Retried != 1 {
		t.Fatalf("local calls=%d retried=%d, want the verification retry on the local seat as before", localCalls.Load(), sum.Retried)
	}
}
