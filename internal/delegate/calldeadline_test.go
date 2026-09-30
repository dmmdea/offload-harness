package delegate

// The whole-call deadline (ADR 0065, register C-67), engine side.
//
// The defect: RunWith returned only when EVERY subtask ended, and the MCP client
// aborts a tool call at its own limit (1,800 s) and drops the response with it. A
// producing job is polled to its node ceiling (up to 14,400 s), so one slow
// subtask held a call past the abort and took the finished ones down with it (the
// 2026-09-27 call that ran 2,103 s and lost a finished 423 s answer).
//
// RunOptions.Deadline is the fix's engine half: at the deadline the finished
// subtasks' results are returned, every unfinished one is published as a budget
// defer "call deadline reached; N unfinished", the outstanding work is cancelled,
// and nothing further is started. Every clock here is compressed to milliseconds.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// deadlineIn is an absolute deadline d from now.
func deadlineIn(d time.Duration) *RunOptions { return &RunOptions{Deadline: time.Now().Add(d)} }

// runWithin runs RunWith in a goroutine and FAILS the test when it has not
// returned inside limit: the shape of the bug is "returns only when every
// subtask ends", which must fail fast rather than hang the suite. unblock (may
// be nil) releases runners that ignore their context, and the goroutine is
// always waited for, so nothing writes the temp dir after the test ends.
func runWithin(t *testing.T, limit time.Duration, cfg config.Config, local LocalRunner, contracts []core.AgentContract, route string, remotes []string, opts *RunOptions, unblock func()) ([]PlacedResult, Summary, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		res []PlacedResult
		sum Summary
		err error
	}
	ch := make(chan outcome, 1)
	start := time.Now()
	go func() {
		r, s, e := RunWith(ctx, cfg, local, contracts, route, remotes, opts)
		ch <- outcome{r, s, e}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("RunWith: %v", o.err)
		}
		return o.res, o.sum, time.Since(start)
	case <-time.After(limit):
		cancel()
		if unblock != nil {
			unblock()
		}
		<-ch
		t.Fatalf("RunWith had not returned after %s: it blocks until every subtask ends instead of returning at the call deadline", limit)
		return nil, Summary{}, 0
	}
}

// localOK is a finished local answer.
func localOK() core.AgentWireResult {
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "local-seat", Output: "done", StopReason: "done"}
}

// cancelledLoop is what the real agent loop returns when its context ends: a
// budget defer naming the cancellation (pipeline/agenttask.go).
func cancelledLoop() core.AgentWireResult {
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "local-seat", Deferred: true,
		DeferClass: core.DeferClassBudget, Reason: "agent loop: canceled (the parent context ended — the caller gave up, or this box is draining)"}
}

// isSlow marks the contracts a test wants to block.
func isSlow(c core.AgentContract) bool { return strings.Contains(c.Goal, "slow") }

const deadlinePrefix = "call deadline reached; "

// TestRunWithDeadlineReturnsFinishedAndDefersUnfinished is the defect itself,
// at the engine: one subtask finishes at once, the other runs until its context
// ends. The call must return AT the deadline with the finished result intact and
// the other one published as a budget defer that says why — and the blocked seat
// must have been told to stop.
func TestRunWithDeadlineReturnsFinishedAndDefersUnfinished(t *testing.T) {
	cfg := testCfg(t)
	var cancelled atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-ctx.Done()
			cancelled.Add(1)
			return cancelledLoop(), nil
		}
		return localOK(), nil
	}
	results, sum, elapsed := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)

	if elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("RunWith returned after %s, want ~300ms (the deadline) plus a short unwind", elapsed)
	}
	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want one success and one defer (a deadline is never a failure)", sum)
	}
	fast, slow := results[0], results[1]
	if fast.Err != "" || fast.Result.Deferred || fast.Result.Output != "done" {
		t.Fatalf("the finished subtask = %+v, want its own result untouched", fast)
	}
	r := slow.Result
	if slow.Err != "" || !r.Deferred || r.DeferClass != core.DeferClassBudget {
		t.Fatalf("the unfinished subtask = err %q deferred %v class %q, want a budget defer and no error", slow.Err, r.Deferred, r.DeferClass)
	}
	if !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("reason = %q, want it to open %q", r.Reason, deadlinePrefix+"1 unfinished")
	}
	if BrokenStackDefer(r.DeferClass) {
		t.Fatal("a call-deadline defer must not read as a broken stack")
	}
	if cancelled.Load() != 1 {
		t.Fatalf("the blocked seat saw the cancellation %d time(s), want 1 — the deadline must CANCEL the outstanding work, not just stop waiting for it", cancelled.Load())
	}
}

// TestRunWithDeadlineDoesNotWaitForARunnerThatIgnoresItsContext: a seat stuck in
// something that never looks at its context must not hold the call. The call
// still returns within the unwind allowance, the stuck subtask is published as
// unfinished, and the late answer (released after the fact) is dropped — not
// written into a result slice nobody owns any more.
func TestRunWithDeadlineDoesNotWaitForARunnerThatIgnoresItsContext(t *testing.T) {
	cfg := testCfg(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		// The stuck goroutine records its own rows when it finally returns; let
		// it finish before the temp dir is removed (a Windows handle would block).
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if rows, err := ledger.ReadAll(cfg.LedgerPath); err == nil && len(rows) >= 2 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
	})
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-release // deaf to ctx
			return localOK(), nil
		}
		return localOK(), nil
	}
	results, sum, elapsed := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), unblock)

	if elapsed > 2*time.Second {
		t.Fatalf("RunWith returned after %s: it waited for a runner that never looks at its context", elapsed)
	}
	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want one success and one defer", sum)
	}
	r := results[1].Result
	if !r.Deferred || r.DeferClass != core.DeferClassBudget || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("stuck subtask = %+v, want the call-deadline budget defer", r)
	}
	if !strings.Contains(r.Reason, "did not stop") {
		t.Fatalf("reason = %q, want it to say the subtask did not stop when the deadline ended", r.Reason)
	}
}

// TestRunWithDeadlineStartsNothingPastTheSlots: eight subtasks, four run at a
// time, all four running ones block. At the deadline the four that never got a
// slot must not START (a call that is over cannot begin work) and are published
// as unfinished; the count in every reason is the WHOLE call's.
func TestRunWithDeadlineStartsNothingPastTheSlots(t *testing.T) {
	cfg := testCfg(t)
	var started, cancelled atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		started.Add(1)
		<-ctx.Done()
		cancelled.Add(1)
		return cancelledLoop(), nil
	}
	contracts := make([]core.AgentContract, 8)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "slow one"}
	}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, local, contracts, "local", nil, deadlineIn(300*time.Millisecond), nil)

	if got := started.Load(); got != int64(runConcurrency) {
		t.Fatalf("the seat was started %d times, want %d: subtasks past the run slots must not start once the call is over", got, runConcurrency)
	}
	if sum != (Summary{Deferred: 8}) {
		t.Fatalf("summary = %+v, want all eight deferred", sum)
	}
	never := 0
	for i, pr := range results {
		if pr.Err != "" || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassBudget {
			t.Fatalf("result %d = %+v, want a budget defer", i, pr)
		}
		if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"8 unfinished") {
			t.Fatalf("result %d reason = %q, want every reason to carry the whole call's count (8)", i, pr.Result.Reason)
		}
		if strings.Contains(pr.Result.Reason, "never started") {
			never++
			if !pr.Unplaced {
				t.Errorf("result %d never started but is not marked Unplaced", i)
			}
		}
	}
	if never != 8-runConcurrency {
		t.Fatalf("%d results say they never started, want %d", never, 8-runConcurrency)
	}
}

// TestRunWithDeadlineKeepsAResultThatFinishesInTheUnwind: a subtask that
// completes as the cancellation reaches it is a real answer, and a real answer is
// never rewritten into a defer. Only work that was NOT finished is deferred.
func TestRunWithDeadlineKeepsAResultThatFinishesInTheUnwind(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-ctx.Done()
			return localOK(), nil // finished anyway
		}
		return localOK(), nil
	}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)
	if sum != (Summary{Succeeded: 2}) {
		t.Fatalf("summary = %+v, want both answers kept", sum)
	}
	if results[1].Result.Deferred || results[1].Result.Output != "done" {
		t.Fatalf("a finished answer was rewritten: %+v", results[1])
	}
}

// TestRunWithDeadlineHonoursAnEarlierContextDeadline: a caller that already
// bounded the context tighter keeps that bound — the deadline can only shorten a
// call, never extend it — and the cut is still published as the call deadline.
func TestRunWithDeadlineHonoursAnEarlierContextDeadline(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		return cancelledLoop(), nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	results, sum, err := RunWith(ctx, cfg, local, []core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(time.Hour))
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want the caller's 200ms bound", elapsed)
	}
	if sum != (Summary{Deferred: 1}) || !strings.HasPrefix(results[0].Result.Reason, deadlinePrefix) {
		t.Fatalf("summary %+v, reason %q, want the cut published as the call deadline", sum, results[0].Result.Reason)
	}
}

// asString is a nil-safe string assertion for sync.Map values.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// remoteRunningForever builds a fleet node whose "fast" jobs finish at once and
// whose every other job answers `running` for as long as it is asked.
func remoteRunningForever(t *testing.T) (*fakeNode, string) {
	t.Helper()
	var goals sync.Map
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-a"}
	f.onDispatch = func(jobID string, c core.AgentContract) { goals.Store(jobID, c.Goal) }
	f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		if g, _ := goals.Load(jobID); strings.Contains(asString(g), "fast") {
			w := remoteWire("the qube answer", `{"answer":"qube"}`)
			w.NodeID = "node-a"
			return doneWire(t, w), http.StatusOK
		}
		return map[string]any{"state": "running"}, http.StatusOK
	}
	return f, f.server().URL
}

func remoteGoal(goal string) core.AgentContract {
	c := remoteContract()
	c.Goal = goal
	return c
}

// TestRunWithDeadlineCancelsAnOutstandingRemoteJob: a job a node is still
// running at the deadline stops being polled (the delegator's poll loop is the
// thing that used to hold the call open), the result names the node and the job
// so the caller can reconcile it, and the intent stays open for the recovery
// pass — the node may still finish it.
func TestRunWithDeadlineCancelsAnOutstandingRemoteJob(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := remoteRunningForever(t)
	cfg := testCfg(t)

	results, sum, elapsed := runWithin(t, 4*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteGoal("fast one"), remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)

	if elapsed > 2*time.Second {
		t.Fatalf("RunWith returned after %s, want ~400ms plus a short unwind", elapsed)
	}
	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want the finished job and one call-deadline defer", sum)
	}
	if results[0].Err != "" || results[0].Result.Deferred || results[0].Node != "node-a" {
		t.Fatalf("the finished job = %+v, want its own result", results[0])
	}
	cut := results[1]
	if cut.Err != "" || !cut.Result.Deferred || cut.Result.DeferClass != core.DeferClassBudget {
		t.Fatalf("the cut job = err %q %+v, want a budget defer", cut.Err, cut.Result)
	}
	for _, s := range []string{deadlinePrefix + "1 unfinished", "node-a", cut.JobID} {
		if !strings.Contains(cut.Result.Reason, s) {
			t.Errorf("reason = %q, want it to contain %q", cut.Result.Reason, s)
		}
	}
	if !strings.HasPrefix(cut.JobID, "agd-") || cut.Node != "node-a" || cut.Unplaced {
		t.Errorf("cut job id %q node %q unplaced %v, want the placed job named", cut.JobID, cut.Node, cut.Unplaced)
	}
	if !cut.orphanable {
		t.Error("the cut job is not orphanable: the node may still finish it, so its intent must stay open for the recovery pass")
	}

	// The cancellation reached the poll loop: polling stops.
	before := node.polls.Load()
	time.Sleep(200 * time.Millisecond)
	if after := node.polls.Load(); after-before > 1 {
		t.Fatalf("the node was polled %d more time(s) after RunWith returned: the outstanding job is still being polled", after-before)
	}

	// Telemetry says the same thing the wire does: the cut subtask's ledger row
	// carries the deadline wording, not "canceled: context deadline exceeded".
	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var row *ledger.Entry
	for i := range rows {
		if rows[i].JobID == cut.JobID {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("no ledger row for the cut job %s among %d rows", cut.JobID, len(rows))
	}
	if !row.Deferred || !strings.HasPrefix(row.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("ledger row = deferred %v reason %q, want the same call-deadline wording the caller sees", row.Deferred, row.Reason)
	}
}

// TestRunWithDeadlineEndsTheCapacityWaitAsACallDeadlineDefer: a subtask waiting
// for a node to have room is over when the call is. Its outcome is the call
// deadline — not "capacity wait: no node had room", which would say the fleet
// was full for the whole wait when the call simply ran out of time.
func TestRunWithDeadlineEndsTheCapacityWaitAsACallDeadlineDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30 // would hold the subtask for half a minute

	results, sum, elapsed := runWithin(t, 5*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)

	if elapsed > 3*time.Second {
		t.Fatalf("RunWith returned after %s: the capacity wait outlived the call deadline", elapsed)
	}
	if sum.Deferred != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want one defer and no failure", sum)
	}
	r := results[0].Result
	if !r.Deferred || r.DeferClass != core.DeferClassBudget || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("result = %+v, want the call-deadline budget defer (not a capacity defer)", r)
	}
	if strings.Contains(r.Reason, "no node had room") {
		t.Fatalf("reason = %q: the call ran out of time, the fleet was not proven full", r.Reason)
	}
	// The placement narration leads with the deadline too: the capacity wait's own
	// text ("no node had room within 30s") would read as the outcome.
	if pl := results[0].PlacementReason; !strings.HasPrefix(pl, "call deadline reached") || !results[0].Unplaced {
		t.Fatalf("placement = %q unplaced = %v, want the deadline marker first and the subtask marked unplaced", pl, results[0].Unplaced)
	}
}

// TestRunQueueRouteHonoursTheCallDeadline: route=queue polls its holder one job
// after another and reports a poll its context cancelled as a FAILURE. When the
// call deadline is what cancelled it that is unfinished work — the job stays on
// the holder — so it is published as the call-deadline defer and the summary
// moves from failed to deferred.
func TestRunQueueRouteHonoursTheCallDeadline(t *testing.T) {
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.Config{FleetQueueHolder: srv.URL, StateDir: t.TempDir()} // no claimant ever takes the job
	contract := core.AgentContract{
		Goal:         "which shipment is refrigerated?",
		OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`),
		TimeoutSec:   30,
	}
	results, sum, elapsed := runWithin(t, 4*time.Second, cfg, nil, []core.AgentContract{contract}, "queue", nil, deadlineIn(300*time.Millisecond), nil)

	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s, want ~300ms", elapsed)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want the cancelled poll counted as a defer, not a failure", sum)
	}
	pr := results[0]
	if pr.Err != "" || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassBudget {
		t.Fatalf("result = err %q %+v, want the call-deadline budget defer", pr.Err, pr.Result)
	}
	if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(pr.Result.Reason, pr.JobID) {
		t.Fatalf("reason = %q, want the deadline wording naming the job that stays on the holder (%s)", pr.Result.Reason, pr.JobID)
	}
}

// TestRunWithDeadlineRecordsACapacityWaitCutBeforeAnyAttempt: the one subtask
// never reaches a node — the only one is full, so it waits for capacity from the
// start and the deadline ends the wait with no dispatch ever made. Nothing else
// records it, so the deadline defer must record ITS OWN row, and that row must
// carry the deadline wording, not the capacity wait's.
func TestRunWithDeadlineRecordsACapacityWaitCutBeforeAnyAttempt(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.maxConcurrentJobs, f.jobsRunning = 1, 1 // no headroom: the deal sends it straight to the wait
		f.maxQueueDepth, f.queueDepth = 2, 2      // and the wait sees it saturated on every tick
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30

	results, sum, _ := runWithin(t, 5*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)

	if sum.Deferred != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want one call-deadline defer", sum)
	}
	if !strings.HasPrefix(results[0].Result.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("reason = %q, want the call-deadline wording", results[0].Result.Reason)
	}
	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger rows = %d (%v), want exactly the one row the deadline defer records (no attempt ever wrote one)", len(rows), err)
	}
	if !rows[0].Deferred || !strings.HasPrefix(rows[0].Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("ledger row = deferred %v reason %q, want the call-deadline wording", rows[0].Deferred, rows[0].Reason)
	}
}

// pastDeadline is a call state whose deadline has already passed.
func pastDeadline(owed int) *callDeadline {
	c := &callDeadline{at: time.Now().Add(-time.Second), grace: time.Second, total: owed}
	c.frozen.Store(-1)
	return c
}

// TestAttemptStartsNothingOnceTheCallDeadlineHasPassed: no placement, probe,
// dispatch or local run may BEGIN after the call is over — a subtask whose
// verification retry, re-placement or capacity wait reaches attempt() late must
// not start a seat nobody is waiting for. The outcome is the call-deadline defer,
// and its telemetry row says so.
func TestAttemptStartsNothingOnceTheCallDeadlineHasPassed(t *testing.T) {
	var ran atomic.Int64
	local := func(context.Context, core.AgentContract, LocalOptions) (core.AgentWireResult, error) {
		ran.Add(1)
		return localOK(), nil
	}
	cfg := testCfg(t)
	r := &runner{cfg: cfg, local: local, route: "local", call: pastDeadline(1)}

	pr := r.attempt(t.Context(), 0, core.AgentContract{Goal: "say done"}, nil)

	if ran.Load() != 0 {
		t.Fatalf("the local seat was started %d time(s) after the call deadline", ran.Load())
	}
	if pr.Err != "" || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassBudget || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("outcome = err %q %+v, want the call-deadline budget defer", pr.Err, pr.Result)
	}
	if !pr.Unplaced || !pr.deadlineCut || pr.JobID == "" {
		t.Fatalf("outcome unplaced %v cut %v job %q, want an unplaced, marked, identified result", pr.Unplaced, pr.deadlineCut, pr.JobID)
	}
}

// TestCutByDeadlineNeverRewritesAFinishedAnswer: only outcomes that are NOT
// answers are rewritten. A result that finished — even one that failed its
// acceptance checks, which is a wrong answer and not a missing one — keeps
// everything it had.
func TestCutByDeadlineNeverRewritesAFinishedAnswer(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(3)}
	answer := PlacedResult{Node: "n", Seat: "s", Result: localOK()}
	wrong := PlacedResult{Node: "n", Seat: "s", Result: localOK(), AcceptanceFailures: []string{"contains:qube"}}
	for name, pr := range map[string]PlacedResult{"a finished answer": answer, "a finished answer that failed acceptance": wrong} {
		got := r.cutByDeadline(pr)
		if got.deadlineCut || got.Result.Deferred || got.Result.Output != "done" || len(got.AcceptanceFailures) != len(pr.AcceptanceFailures) {
			t.Errorf("%s was rewritten: %+v", name, got)
		}
	}
	// And the other side of the line: a failure and a defer, arriving after the
	// deadline, ARE the call deadline.
	for name, pr := range map[string]PlacedResult{
		"a cancelled poll":  {Err: "canceled: context deadline exceeded"},
		"a cancelled seat":  {Result: cancelledLoop()},
		"a refusal chain":   {Err: "placement refused: 1 node(s) refused this subtask and none of them ran it"},
		"a capacity defer":  {Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassCapacity, Reason: "capacity wait: no node had room"}},
		"an infrastructure": {Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: llama-server 500"}},
	} {
		got := r.cutByDeadline(pr)
		if !got.deadlineCut || got.Err != "" || got.Result.DeferClass != core.DeferClassBudget || !strings.HasPrefix(got.Result.Reason, deadlinePrefix) {
			t.Errorf("%s was not published as the call deadline: %+v", name, got)
		}
	}
}

// TestRunBatchedDefersLaterChunksAtTheDeadline: offload_research runs its pages
// in consecutive chunks of eight. The deadline covers the WHOLE call: chunk two
// must not start after it, the pages it held are published as unfinished (not
// skipped, not an error — the finished digests keep their order), and the count
// in every reason spans both chunks.
func TestRunBatchedDefersLaterChunksAtTheDeadline(t *testing.T) {
	cfg := testCfg(t)
	var ran atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		ran.Add(1)
		if isSlow(c) {
			<-ctx.Done()
			return cancelledLoop(), nil
		}
		return localOK(), nil
	}
	contracts := make([]core.AgentContract, 9)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "fast one"}
	}
	contracts[7] = core.AgentContract{Goal: "slow one"} // the last page of chunk one

	type outcome struct {
		res []PlacedResult
		sum Summary
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		r, s, e := RunBatched(t.Context(), cfg, local, contracts, "local", nil, deadlineIn(400*time.Millisecond))
		ch <- outcome{r, s, e}
	}()
	var o outcome
	select {
	case o = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("RunBatched had not returned after 5s: it does not honour the call deadline")
	}
	if o.err != nil {
		t.Fatalf("RunBatched: %v", o.err)
	}
	if len(o.res) != 9 {
		t.Fatalf("got %d results, want one per page (9)", len(o.res))
	}
	if o.sum.Succeeded != 7 || o.sum.Deferred != 2 || o.sum.Skipped != 0 || o.sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 7 digests and 2 call-deadline defers, nothing skipped or failed", o.sum)
	}
	if got := ran.Load(); got != 8 {
		t.Fatalf("the seat ran %d subtasks, want 8: chunk two must not start once the call is over", got)
	}
	for _, i := range []int{7, 8} {
		r := o.res[i].Result
		if !r.Deferred || r.DeferClass != core.DeferClassBudget || !strings.HasPrefix(r.Reason, deadlinePrefix+"2 unfinished") {
			t.Fatalf("result %d = %+v, want the call-deadline defer counting BOTH chunks (2 unfinished)", i, r)
		}
	}
	if !strings.Contains(o.res[8].Result.Reason, "never started") {
		t.Fatalf("result 8 reason = %q, want it to say the page never started", o.res[8].Result.Reason)
	}
	// Every outcome has a row, the never-started page included (the precedent
	// settle() set: a subtask that produced no attempt is still recorded).
	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil || len(rows) != 9 {
		t.Fatalf("ledger rows = %d (%v), want one per page (9)", len(rows), err)
	}
	for _, row := range rows {
		if row.JobID == o.res[8].JobID && !strings.HasPrefix(row.Reason, deadlinePrefix+"2 unfinished") {
			t.Fatalf("the never-started page's row = %q, want the call-deadline wording", row.Reason)
		}
	}
}

// TestRunWithAnExpiredDeadlineStartsNothing: a deadline that is already over when
// RunWith begins (a later chunk of a batched call) must not dispatch or run a
// single subtask, and must still answer every one of them — each as a
// call-deadline defer with its own row.
func TestRunWithAnExpiredDeadlineStartsNothing(t *testing.T) {
	node, url := remoteRunningForever(t)
	cfg := testCfg(t)
	for _, route := range []string{"spread", "auto", "remote"} {
		t.Run(route, func(t *testing.T) {
			results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
				[]core.AgentContract{remoteGoal("fast one"), remoteGoal("slow one")}, route, []string{url}, deadlineIn(-time.Second))
			if err != nil {
				t.Fatalf("RunWith: %v", err)
			}
			if node.dispatches.Load() != 0 {
				t.Fatalf("%d dispatch(es) after the deadline had passed, want none", node.dispatches.Load())
			}
			if sum != (Summary{Deferred: 2}) {
				t.Fatalf("summary = %+v, want both subtasks deferred", sum)
			}
			for i, pr := range results {
				if pr.Err != "" || !pr.Unplaced || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"2 unfinished") || !strings.Contains(pr.Result.Reason, "never started") {
					t.Fatalf("result %d = err %q unplaced %v reason %q, want the never-started call-deadline defer", i, pr.Err, pr.Unplaced, pr.Result.Reason)
				}
			}
		})
	}
}
