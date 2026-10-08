package delegate

// The whole-call deadline (ADR 0065) composed with placement holds (ADR 0063).
//
// Each of these seams sat between two features that were built apart and each passed its
// own suite: the per-page retry cap counts a class-budget defer as the seat's own failure,
// and the call deadline publishes exactly that class for a run it cut; a capacity-wait tick
// cut by the wait's own deadline keeps the previous tick's state, and the call's deadline is
// a second way a tick gets cut; a retry that waits in line for a busy seat says the caller
// canceled when it was the call's deadline that ended the wait; a refusal chain whose
// re-placement read the deadline ended was published as "placement refused" (a failure that
// accuses nodes never asked, in a call that ran out of time), and a retry whose node
// selection it ended left no note at all.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
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
			[]core.AgentContract{pageContract(page)}, "local", nil, deadlineIn(400*time.Millisecond), nil)
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
		[]core.AgentContract{verifiedContract()}, "spread", []string{url}, deadlineIn(600*time.Millisecond), nil)
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
	withCallReserve(t, 0) // pins the CUT: with a reserve the wait ends before the deadline (callwait_test.go)
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

// A node refuses the dispatch at its own address (404: not a capacity refusal, so the subtask is
// re-placed rather than waited), a second node is healthy and would take it, and the call's
// deadline passes while the re-placement is still reading the fleet. The read is cut, the second
// node is never seen, and the chain used to be published as "placement refused ... no further
// eligible remote was available": a failure that accuses a node nobody asked, in a call that ran
// out of time, which also flagged a one-subtask call as an error. The outcome is the deadline's:
// a budget defer, no node or seat named, the refusal chain quoted behind the deadline marker, and
// one closing row under a job id of its own (the refused attempt's row belongs to that attempt).
func TestAPlacementRefusedAfterTheCallDeadlineIsTheDeadlinesDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	a, aURL := refusingNode(t, "node-a", http.StatusNotFound, nil)
	b, bURL := acceptingNode(t, "node-b", "zorblax from B", func(f *fakeNode) {
		// B answers health at once until A has been asked, then takes longer than the call has.
		f.healthDelayFn = func() time.Duration {
			if a.dispatches.Load() > 0 {
				return 30 * time.Second
			}
			return 0
		}
	})
	cfg := testCfg(t)
	results, sum, elapsed := runWithin(t, 15*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{aURL, bURL}, deadlineIn(2*time.Second), nil)
	pr := results[0]
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v err %q, want one call-deadline defer: the deadline ended the read that would have found node-b, so no refusal chain is the outcome", sum, pr.Err)
	}
	r := pr.Result
	if !pr.deadlineCut || !pr.Unplaced || r.DeferClass != core.DeferClassBudget || pr.Err != "" {
		t.Fatalf("result cut %v unplaced %v class %q err %q, want an unplaced budget defer marked as the deadline's", pr.deadlineCut, pr.Unplaced, r.DeferClass, pr.Err)
	}
	if !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(r.Reason, "had not been placed on a seat") || !strings.Contains(r.Reason, "placement refused") {
		t.Fatalf("reason = %q, want the deadline's opening, that nothing was placed, and the refusal chain quoted", r.Reason)
	}
	if pr.Node != "" || pr.Seat != "" {
		t.Fatalf("the result names node %q seat %q: no node ran it, and the one that refused it is not its node", pr.Node, pr.Seat)
	}
	if a.dispatches.Load() != 1 || b.dispatches.Load() != 0 {
		t.Fatalf("dispatches a=%d b=%d, want node-a asked once and node-b never (the read that would have found it was the one cut)", a.dispatches.Load(), b.dispatches.Load())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the call returned after %s: it waited out the slow health read instead of ending at its deadline", elapsed)
	}

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var closing, refused int
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.Reason, deadlinePrefix):
			closing++
			if row.JobID != pr.JobID || !row.Deferred {
				t.Fatalf("the closing row = job %q deferred %v, want the job id the caller was given (%q)", row.JobID, row.Deferred, pr.JobID)
			}
			if row.ReasonCode != ledger.ReasonBudget {
				t.Fatalf("the closing row's reason_code = %q, want %q: every call-deadline row carries it (ADR 0065)", row.ReasonCode, ledger.ReasonBudget)
			}
		case strings.Contains(row.Reason, "404"):
			refused++
			if row.JobID == pr.JobID {
				t.Fatalf("the refused attempt's row carries the job id the caller was given (%q): two rows would double-count one id", pr.JobID)
			}
		}
	}
	if closing != 1 || refused != 1 {
		t.Fatalf("ledger: %d closing row(s) with the deadline wording and %d refused-attempt row(s) among %d, want exactly 1 and 1", closing, refused, len(rows))
	}
	var corpus int
	for _, line := range corpusLines(t, cfg) {
		if line.JobID == pr.JobID {
			corpus++
			if line.Result == nil || !strings.HasPrefix(line.Result.Reason, deadlinePrefix+"1 unfinished") || !line.Deferred {
				t.Fatalf("the corpus row for the closing job = %+v, want the call-deadline defer", line)
			}
		}
	}
	if corpus != 1 {
		t.Fatalf("%d corpus row(s) carry the job id the caller was given, want exactly 1", corpus)
	}
}

// The same seam on the path the capacity wait owns when it is switched off: the only node refuses
// for capacity (503), the wait is off (testCfg), so the chain is closed at once, and the deadline
// ends the re-read that produced it. Here the refusal is a capacity one, which is the other way a
// chain reaches exhausted().
func TestAnExhaustedChainWithTheWaitOffIsCutByTheCallDeadlineToo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.healthDelayFn = func() time.Duration {
			if f.dispatches.Load() > 0 {
				return 30 * time.Second
			}
			return 0
		}
	})
	results, sum, _ := runWithin(t, 15*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(2*time.Second), nil)
	pr := results[0]
	if sum != (Summary{Deferred: 1}) || !pr.deadlineCut || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("summary = %+v cut %v err %q reason %q, want one call-deadline defer", sum, pr.deadlineCut, pr.Err, pr.Result.Reason)
	}
	if node.dispatches.Load() != 1 {
		t.Fatalf("the node was asked %d times, want once", node.dispatches.Load())
	}
}

// The first attempt ran on the local seat and failed verification, so a retry on a remote is owed,
// and the call's deadline ends the retry's node selection (the fleet read that would have found
// one). The first attempt finished before the deadline: it is an answer, published as it was. What
// the retry did is carried in retry_note (ADR 0065 decision 2), and it used to be silent: an empty
// note reads as "there was nowhere else to go".
func TestARetryWhoseNodeSelectionWasCutByTheCallDeadlineSaysSo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var slow atomic.Bool
	_, url := acceptingNode(t, "node-b", "verified from B", func(f *fakeNode) {
		f.healthDelayFn = func() time.Duration {
			if slow.Load() {
				return 30 * time.Second
			}
			return 0
		}
	})
	var localCalls atomic.Int64
	inner := failingLocal(&localCalls)
	local := func(ctx context.Context, c core.AgentContract, o LocalOptions) (core.AgentWireResult, error) {
		slow.Store(true) // the remote's health turns slow once the first attempt is running
		return inner(ctx, c, o)
	}
	results, sum, _ := runWithin(t, 15*time.Second, testCfg(t), local,
		[]core.AgentContract{verifiedContract()}, "auto", []string{url}, deadlineIn(1500*time.Millisecond), nil)
	pr := results[0]
	if sum != (Summary{FailedVerification: 1}) || pr.deadlineCut || pr.retried || localCalls.Load() != 1 {
		t.Fatalf("summary = %+v cut %v retried %v local runs %d, want the first attempt's failed verification published as it was, with no retry", sum, pr.deadlineCut, pr.retried, localCalls.Load())
	}
	if !strings.HasPrefix(pr.RetryNote, "retry skipped: ") || !strings.Contains(pr.RetryNote, "call deadline reached") {
		t.Fatalf("retry_note = %q, want it to say the call's deadline ended the choice of a retry node", pr.RetryNote)
	}
}
