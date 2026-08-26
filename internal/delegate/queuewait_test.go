package delegate

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// This file pins the delegator half of the 0.100.0 node-queue change.
//
// Before that change `Accept` on the node WAS `start`, so dispatch → running
// was microseconds and the whole poll budget went to execution. Now a job can
// legitimately sit in the node's backlog in state `accepted`, and that wait
// would otherwise eat the contract's own timeout — converting a loud, immediate
// 503 ("queue full") into a slow timeout that burns the budget and then hands
// back a manufactured "node accepted the job but did not reach a terminal
// state" defer. Same lost work, less signal, more wall clock.
//
// The rule these tests hold: queued time does not consume execution budget, the
// total queued wait is bounded, and giving up while QUEUED is distinguishable
// from giving up while RUNNING.

// scriptedNode builds a fake node whose job state is driven by WALL CLOCK
// rather than poll ordinal: it answers `accepted` for queuedFor, then `running`
// for runFor, then the given terminal body. Wall clock is the right driver
// here because the property under test is about elapsed time versus a deadline,
// and a poll-ordinal script would silently change meaning if the cadence moved.
func scriptedNode(t *testing.T, queuedFor, runFor time.Duration, terminal func() (map[string]any, int), sawAccepted, sawRunning *atomic.Int64) *fakeNode {
	t.Helper()
	start := time.Now()
	return &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			switch elapsed := time.Since(start); {
			case elapsed < queuedFor:
				sawAccepted.Add(1)
				return map[string]any{"state": "accepted"}, http.StatusOK
			case elapsed < queuedFor+runFor:
				sawRunning.Add(1)
				return map[string]any{"state": "running"}, http.StatusOK
			default:
				return terminal()
			}
		},
	}
}

// TestRunQueuedTimeDoesNotConsumeExecutionBudget is the regression test for the
// defect the node-side queue introduced. The node holds the job in `accepted`
// for most of the contract budget, then runs it — and the run finishes at a
// point PAST where the uncredited deadline would have fired. If queued time
// counts against the deadline, the job is abandoned mid-run with a poll-deadline
// defer; it must instead succeed, because the contract's timeout is a budget for
// WORK and the backlog wait was not work.
func TestRunQueuedTimeDoesNotConsumeExecutionBudget(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)

	var sawAccepted, sawRunning atomic.Int64
	// Execution budget = TimeoutSec(1s) + grace(20ms) = 1.02s, and the
	// queued-wait bound is min(that, the absolute ceiling) = the same 1.02s.
	// The fixture must sit inside BOTH constraints to test the credit rather
	// than the bound:
	//   queuedFor (800ms) < the queued-wait bound  → it is not a give-up, and
	//   queuedFor + runFor (1.3s) > the budget     → without the credit it dies
	//                                                mid-run at 1.02s.
	// So this test can only pass if queued time was credited back.
	node := scriptedNode(t, 800*time.Millisecond, 500*time.Millisecond,
		func() (map[string]any, int) {
			return doneWire(t, remoteWire("the answer is qube", `{"answer":"qube"}`)), http.StatusOK
		}, &sawAccepted, &sawRunning)
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0]
	if r.Err != "" {
		t.Fatalf("err = %q, want none — the job ran and finished; only its WAIT was long", r.Err)
	}
	if r.Result.Deferred {
		t.Fatalf("deferred %q/%q — queued time was charged to the execution budget",
			r.Result.DeferClass, r.Result.Reason)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want one success", sum)
	}
	if r.Result.Output != "the answer is qube" {
		t.Fatalf("output = %q", r.Result.Output)
	}
	// Proof the test actually exercised a backlog: without queued polls it
	// would be asserting nothing at all.
	if sawAccepted.Load() < 5 {
		t.Fatalf("only %d queued poll(s) observed — this test never exercised a backlog", sawAccepted.Load())
	}
	if sawRunning.Load() < 1 {
		t.Fatalf("the job never reported running; this test must cover queue-THEN-run, not queue-forever")
	}
}

// TestRunQueuedWaitIsBoundedAndDistinct: a job that is admitted and then never
// given a worker must not wait forever. The delegator gives up on a bounded
// wait, and what the caller sees must say the job NEVER STARTED — distinctly
// from the give-up shape for a job that was running and ran out of clock
// (which is a `deferred` result whose reason starts "poll deadline").
func TestRunQueuedWaitIsBoundedAndDistinct(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)

	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "accepted"}, http.StatusOK // queued forever
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1 // queued-wait bound = min(budget 1.02s, the absolute ceiling)

	began := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(began)
	r := results[0]

	// 1) It gave up, and said the job never started.
	if r.Err == "" {
		t.Fatalf("no error; result = %+v — a job that never started must be reported, not silently deferred as if it ran", r.Result)
	}
	if !strings.HasPrefix(r.Err, "queue deadline") {
		t.Fatalf("err = %q, want the stable %q prefix", r.Err, "queue deadline")
	}
	for _, want := range []string{"never started", "backlog"} {
		if !strings.Contains(r.Err, want) {
			t.Fatalf("err = %q, want it to mention %q", r.Err, want)
		}
	}
	// 2) DISTINCT from the running give-up, which is a defer whose reason
	//    begins "poll deadline".
	if strings.Contains(r.Err, "poll deadline") {
		t.Fatalf("err = %q reuses the RUNNING give-up wording; the two must be tellable apart", r.Err)
	}
	if r.Result.Deferred {
		t.Fatalf("a never-started job must not be reported as a node-side defer: %+v", r.Result)
	}
	if sum.Failed != 1 {
		t.Fatalf("summary = %+v, want one failure", sum)
	}

	// 3) BOUNDED, and bounded by the QUEUED-WAIT bound specifically. The
	//    ceiling is deliberately just above the 1.02s bound rather than a loose
	//    "not forever": with the bound removed, the run instead survives to the
	//    CREDITED execution deadline (budget + credit ≈ 2.04s), so a 3s ceiling
	//    would let that mutant through on timing and leave the whole property
	//    resting on the shape assertions above.
	if elapsed > 1600*time.Millisecond {
		t.Fatalf("gave up after %v — that is past the queued-wait bound and into the credited execution deadline; the bound is not binding", elapsed)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("gave up after only %v — it abandoned the backlog before the bound; a queued job must actually get its wait", elapsed)
	}

	// 4) And it STOPPED polling.
	polled := node.polls.Load()
	time.Sleep(80 * time.Millisecond)
	if node.polls.Load() != polled {
		t.Error("polling continued after the queue deadline; the delegator must STOP")
	}
}

// TestRunQueuedThenRunningStillHonoursTheExecutionDeadline is the control arm
// for the credit: extending the deadline by queued time must not turn the
// execution budget off. A job that waits, starts, and then runs forever still
// gets abandoned — with the RUNNING give-up shape (a "poll deadline" defer),
// because by then it really was executing.
func TestRunQueuedThenRunningStillHonoursTheExecutionDeadline(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)

	var sawAccepted, sawRunning atomic.Int64
	// 600ms of backlog, not 300: the queued window starts when the fake node
	// starts, but the delegator only begins polling after a health fetch,
	// placement and a dispatch. A 300ms window left only a sliver of observed
	// queue time, which is both the tightest timing margin in the suite and the
	// thing this arm needs to be generous about — it is asserting that credit
	// happened at all, not how little of it is enough.
	node := scriptedNode(t, 600*time.Millisecond, 24*time.Hour, // runs "forever"
		func() (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
		&sawAccepted, &sawRunning)
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1

	began := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(began)
	r := results[0]
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer — this job DID run, it just never finished", sum)
	}
	if !r.Result.Deferred || !strings.HasPrefix(r.Result.Reason, "poll deadline") {
		t.Fatalf("deferred/reason = %v/%q, want the RUNNING give-up shape", r.Result.Deferred, r.Result.Reason)
	}
	if strings.HasPrefix(r.Result.Reason, "queue deadline") {
		t.Fatalf("reason = %q: a job that reached running must not report the queued give-up", r.Result.Reason)
	}
	// The execution budget still applies — it is simply measured from when the
	// work could start. It waited ~300ms and then got its ~1.02s of run time,
	// so the total is meaningfully more than the budget alone but still bounded.
	// Budget alone is 1.02s; with ~600ms of backlog credited the give-up lands
	// near 1.6s. The floor sits at 1.2s — comfortably above the uncredited
	// deadline, comfortably below the expected one — so it detects "the credit
	// was not applied" without depending on exactly how much was banked.
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("gave up after %v — the queued wait was charged to the execution budget after all", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("gave up after %v — the credited deadline is not bounded", elapsed)
	}
	if sawAccepted.Load() < 3 {
		t.Fatalf("only %d queued poll(s) — this control arm never exercised a backlog", sawAccepted.Load())
	}
}

// TestRunQueuedCreditExcludesNonQueuedIntervals is the over-credit test. The
// credit is meant to bank time the job PROVABLY spent in the node's backlog,
// and the endpoints test ("both ends of this interval answered `accepted`") is
// not sufficient on its own: an interval can be bracketed by two `accepted`
// answers and still contain a long stretch during which the node said
// something else entirely.
//
// The node here answers `accepted`, then 503 for half a second, then `accepted`
// again. Nothing about that 503 window is backlog wait — the node was not
// telling us the job was queued, it was failing to answer about it — and
// banking it does BOTH of the harms this mechanism exists to prevent: it hands
// the job more execution budget than the contract granted, and it makes the
// give-up message assert the job "waited in the node's backlog" across time the
// node spent returning 5xx. (The 404 arm is worse in kind: crediting an
// interval in which the node positively DENIED ever holding the job.)
//
// The assertion is on the give-up TIMING, because the credit is not otherwise
// observable: with the gap excluded the queued-wait bound is reached only by
// real queued time, so the run must outlast the gap it refused to count.
func TestRunQueuedCreditExcludesNonQueuedIntervals(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)

	const gapStart = 200 * time.Millisecond
	const gapEnd = 700 * time.Millisecond // a 500ms window of 5xx in the middle

	start := time.Now()
	var sawGap atomic.Int64
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			if e := time.Since(start); e >= gapStart && e < gapEnd {
				sawGap.Add(1)
				return map[string]any{"status": "error", "error": "scripted 503"}, http.StatusServiceUnavailable
			}
			return map[string]any{"state": "accepted"}, http.StatusOK
		},
	}
	srv := node.server()

	contract := remoteContract()
	contract.TimeoutSec = 1 // queued-wait bound = 1.02s of REAL queued time

	began := time.Now()
	results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(began)
	r := results[0]

	if sawGap.Load() < 5 {
		t.Fatalf("only %d poll(s) hit the 5xx window — the fixture never produced a gap, so this test proved nothing", sawGap.Load())
	}
	if !strings.HasPrefix(r.Err, "queue deadline") {
		t.Fatalf("err = %q, want the queued give-up (the node never started the job)", r.Err)
	}
	// The bound is 1.02s of QUEUED time. Crediting the 500ms gap as backlog
	// would reach it after ~1.02s of wall clock; excluding it cannot get there
	// before ~1.52s. The floor sits between the two.
	if elapsed < 1350*time.Millisecond {
		t.Fatalf("gave up after %v — a window the node spent answering 5xx was banked as backlog wait", elapsed)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("gave up after %v — the wait is no longer bounded", elapsed)
	}
}
