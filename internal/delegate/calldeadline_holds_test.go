package delegate

// The whole-call deadline (ADR 0065) composed with placement holds (ADR 0063).
//
// Each of these seams sat between two features that were built apart and each passed its
// own suite: the per-page retry cap counts a class-budget defer as the seat's own failure,
// and the call deadline publishes exactly that class for a run it cut; a capacity-wait tick
// cut by the wait's own deadline keeps the previous tick's state, and the call's deadline is
// a second way a tick gets cut; a retry that waits in line for a busy seat says the caller
// canceled when it was the call's deadline that ended the wait.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestPageIssueFailedIgnoresACallDeadlineCut: the cut keeps the run's seat and node named and
// is class budget, which is what "the seat hit its step or wall budget on this page" looks
// like. Only deadlineCut tells them apart, and the call running out of time is the caller's
// clock, not evidence that no seat can digest the page. Both cut shapes are built by the real
// cut so this test cannot drift from what the engine publishes.
func TestPageIssueFailedIgnoresACallDeadlineCut(t *testing.T) {
	r := &runner{call: pastDeadline(1)}
	cuts := map[string]PlacedResult{
		"remote job cut while running": r.cutByDeadline(PlacedResult{
			Node: "node-a", Seat: "remote-seat", ranBase: "http://192.0.2.50:1", intentRecorded: true,
			Err: "canceled: context deadline exceeded",
		}),
		"local run cut while running": r.cutByDeadline(PlacedResult{
			Node: "this-box", Seat: "local-seat", ranLocal: true, Result: cancelledLoop(),
		}),
	}
	for name, pr := range cuts {
		if !pr.deadlineCut || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassBudget || pr.Unplaced {
			t.Fatalf("%s: the cut is %+v, want a class-budget defer that is not Unplaced (fixture: the shape the budget line would count)", name, pr)
		}
		if pageIssueFailed(pr) {
			t.Errorf("%s counted against the page: the call ran out of time, no seat was shown unable to digest it", name)
		}
	}
	// What the cut must not move: the seat's own budget, with no deadline behind it, still counts.
	own := PlacedResult{Node: "this-box", Seat: "local-seat", ranLocal: true,
		Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget, Reason: "agent loop: step budget exhausted"}}
	if !pageIssueFailed(own) {
		t.Error("a seat's own budget defer no longer counts against the page")
	}
}

// Run level: five calls, each cut by its own deadline while a seat is still working the same
// research page. None of them is the page's fault, so none may back it off: the seat must be
// asked all five times. Before the fix the third cut backed the page off for fifteen minutes
// with "none of them produced a verified digest".
func TestPageCapIgnoresCallDeadlineCuts(t *testing.T) {
	cfg := testCfg(t)
	var runs atomic.Int64
	local := func(ctx context.Context, _ core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		runs.Add(1)
		<-ctx.Done() // still working the page when the call's deadline cancels it
		return cancelledLoop(), nil
	}
	page := uniquePage("the page a call keeps running out of time on")
	cut := 0
	for issue := 1; issue <= 5; issue++ {
		results, _, _ := runWithin(t, 4*time.Second, cfg, local,
			[]core.AgentContract{pageContract(page)}, "local", nil, deadlineIn(100*time.Millisecond), nil)
		if results[0].deadlineCut {
			cut++
		}
	}
	if got := runs.Load(); got != 5 || cut != 5 {
		t.Fatalf("the seat ran %d time(s) for 5 issues and %d of them were cut by the call deadline, want 5 and 5 - a cut was counted against the page", got, cut)
	}
}

// TestRetrySeatWaitEndedByTheCallDeadlineNamesTheDeadline: the retry stood in line for a busy
// seat and the CALL's deadline ended the wait, not the caller. The note says so, under the
// deadline's stable opening, and still does not claim the seat stayed busy.
func TestRetrySeatWaitEndedByTheCallDeadlineNamesTheDeadline(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.jobsRunning, node.queueDepth, node.maxConcurrentJobs = 1, 1, 1 // busy for good
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	var localCalls atomic.Int64
	results, _, _ := runWithin(t, 4*time.Second, cfg, failingLocal(&localCalls),
		[]core.AgentContract{verifiedContract()}, "spread", []string{url}, deadlineIn(300*time.Millisecond), nil)
	note := results[0].RetryNote
	if !strings.Contains(note, "call deadline reached") {
		t.Fatalf("retry_note = %q, want it to say the call's deadline ended the retry's wait", note)
	}
	if strings.Contains(note, "caller canceled") || strings.Contains(note, "a shared seat would only slow both") {
		t.Fatalf("retry_note = %q blames the caller, or claims the seat stayed busy for a wait the call's deadline cut short", note)
	}
}

// A node that answered earlier in the wait and whose last probe is cut off by the CALL's
// deadline is described by what it last said (cooling down after its refusal), never as a
// node whose probe failed: the deadline ended that probe, not the node. The same shape as
// the wait's own deadline cutting the probe (ADR 0063), with the wait's TTL far away so
// only the call's deadline can end it. With a 300 ms health answer the reads are the deal's,
// the re-placement's, and then one per tick, so the first tick has answered by ~0.9 s and the
// fourth straddles 1.7 s. If a slow host lands the deadline between two probes instead, the
// last answer is preserved trivially and the test still holds.
func TestCapacityWaitCutByTheCallDeadlineMidProbeKeepsTheLastAnswer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "5"
		f.maxQueueDepth = 4
		f.healthDelay = 300 * time.Millisecond
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30 // the wait's own TTL is nowhere near: only the call's deadline ends it
	results, sum, _ := runWithin(t, 10*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "remote", []string{url}, deadlineIn(1700*time.Millisecond), nil)
	pr := results[0]
	if sum.Deferred != 1 || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || node.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v reason = %q dispatches = %d, want one call-deadline defer after ONE ask (the cooldown outlasts the call)", sum, pr.Result.Reason, node.dispatches.Load())
	}
	if !strings.Contains(pr.PlacementReason, "cooling down after its own refusal") {
		t.Errorf("placement = %q: the call's deadline cut the last probe of a node that had answered, and the narration lost what the node said", pr.PlacementReason)
	}
	if strings.Contains(pr.PlacementReason, "probe(s) failed during the wait") {
		t.Errorf("placement = %q names a probe failure that the call's deadline caused", pr.PlacementReason)
	}
}
