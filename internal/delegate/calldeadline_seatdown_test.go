package delegate

// The whole-call deadline (ADR 0065) composed with the seat-down re-placement (ADR 0066).
//
// The two features were built apart. A node whose seat went down under a run files a
// `seat down:` defer, and the delegator re-places the contract on another node with the dead
// seat's wait credited back to the retry's budget (one contract wall at most); the call deadline
// cancels everything still running at one instant below the client's abort. Where they meet:
//
//   - the credit buys the retry time on the contract's clock, never on the call's: a re-placement
//     that is running, or waiting for capacity, when the deadline passes is cut like any other
//     attempt, with the cut's reason and its own row, and the credited budget does not hold the
//     call open;
//   - a retry the deadline cut ran no page to a verdict, so the per-page cap that reads the
//     retry's verdict for a seat-down first attempt (retryRanAndFailed) reads nothing there;
//   - a seat-down defer that is produced after the deadline is the deadline's outcome, and is
//     not re-placed; the same holds for the finished answer a delegator-side rescue was still
//     trying to save when the deadline passed;
//   - a retry whose choice of node the deadline ended says so in its note, before it says that
//     no other node could take the contract;
//   - a node's finished answer the call was still working on when the deadline passed (the
//     rescue above) is cut as an answered job, not as one still on the node, and its intent is
//     closed, not left for the recovery pass.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// firstJobScript scripts a fleet for a test about the SECOND placement of a subtask: the first job
// any of the nodes is handed is answered with the seat-down defer, and every other job with
// `later`. It is keyed on the job id (a poll repeats its answer) and on no node name, so the test
// does not depend on where the deal put the first attempt.
type firstJobScript struct {
	mu    sync.Mutex
	first string
}

// poll is the poll function of the node named id.
func (s *firstJobScript) poll(t *testing.T, id string, down core.AgentWireResult, later func() (map[string]any, int)) func(string, int64) (map[string]any, int) {
	return func(jobID string, _ int64) (map[string]any, int) {
		s.mu.Lock()
		if s.first == "" {
			s.first = jobID
		}
		isFirst := s.first == jobID
		s.mu.Unlock()
		if isFirst {
			w := down
			w.NodeID = id
			return doneWire(t, w), http.StatusOK
		}
		return later()
	}
}

// runningForever is what a node answers for a job it has started and never finishes.
func runningForever() (map[string]any, int) {
	return map[string]any{"state": "running"}, http.StatusOK
}

// fenceTheLocalSeat holds an exclusive lease so the local seat cannot take a retry: the re-placement
// has exactly one place to go, another node. Placement is not these tests' subject.
func fenceTheLocalSeat(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})
}

// finishedRows is the ledger's finished rows (the dispatch markers left out).
func finishedRows(t *testing.T, path string) []ledger.Entry {
	t.Helper()
	rows, err := readFinished(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestASeatDownReplacementThatTheCallDeadlineCutKeepsTheSeatDownDeferAndRecordsTheCut: node X's seat
// went down (6 s of wait on the dead seat, credited back), the contract is placed on node Y, and Y
// is still running it when the call's deadline passes. The re-placement is an attempt like any
// other: the deadline cancels its poll and cuts it, and the cut gets its own row with the
// deadline's reason. What the call publishes is the first attempt, which finished long before the
// deadline (a seat-down defer that was produced in time is what it was, ADR 0065 decision 2), and
// the retry's fate is in its retry_note, exactly as for an abstention whose retry was cut. The
// credit made the retry's own budget longer than the call has left; it must not hold the call open.
func TestASeatDownReplacementThatTheCallDeadlineCutKeepsTheSeatDownDeferAndRecordsTheCut(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	script := &firstJobScript{}
	var mu sync.Mutex
	var dispatched []core.AgentContract
	tune := func(id string) func(*fakeNode) {
		return func(f *fakeNode) {
			f.pollByJob = script.poll(t, id, seatDownWire(id, 6), runningForever)
			f.onDispatch = func(_ string, c core.AgentContract) {
				mu.Lock()
				dispatched = append(dispatched, c)
				mu.Unlock()
			}
		}
	}
	nodeA, urlA := acceptingNode(t, "node-a", "unused", tune("node-a"))
	nodeB, urlB := acceptingNode(t, "node-b", "unused", tune("node-b"))
	cfg := testCfg(t)
	fenceTheLocalSeat(t, &cfg)

	results, sum, elapsed := runWithin(t, 10*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "spread", []string{urlA, urlB}, deadlineIn(time.Second), nil)
	pr := results[0]

	if nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("dispatches A=%d B=%d, want the seat-down attempt on one node and the re-placement on the other", nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	mu.Lock()
	got := append([]core.AgentContract(nil), dispatched...)
	mu.Unlock()
	if len(got) != 2 || got[0].TimeoutSec != 30 || got[1].TimeoutSec <= 30 {
		t.Fatalf("dispatched timeout_sec = %+v, want 30 and then more than 30: the dead seat's wait is credited back to the re-placement (premise)", got)
	}
	// The credit made the re-placement's budget ~35 s; the call has 1 s. The deadline wins.
	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("RunWith returned after %s, want about the 1 s deadline plus a short unwind, not the retry's credited ~35 s budget", elapsed)
	}
	if sum.Deferred != 1 || sum.Retried != 1 || sum.Succeeded != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want one deferred subtask that was retried once", sum)
	}
	if pr.deadlineCut || !SeatDownDefer(pr.Result) {
		t.Fatalf("published = cut %v class %q reason %q, want the seat-down defer the first attempt produced before the deadline, not rewritten", pr.deadlineCut, pr.Result.DeferClass, pr.Result.Reason)
	}
	if pr.RetriedOn == "" || pr.RetriedOn == pr.Node {
		t.Fatalf("retried_on = %q node = %q, want the re-placement on the other node", pr.RetriedOn, pr.Node)
	}
	for _, want := range []string{"retry on " + pr.RetriedOn + " also deferred", deadlinePrefix + "1 unfinished", "this result is the first attempt"} {
		if !strings.Contains(pr.RetryNote, want) {
			t.Fatalf("retry_note = %q, want it to carry %q: the deadline cut the re-placement and the note is where the first attempt says so", pr.RetryNote, want)
		}
	}
	if pageIssueFailed(pr) {
		t.Fatal("a page whose seat-down re-placement the call deadline cut counted against the page: the call ran out of time, no seat ran the page to a verdict")
	}

	rows := finishedRows(t, cfg.LedgerPath)
	var down, cut []ledger.Entry
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.Reason, deadlinePrefix):
			cut = append(cut, row)
		case strings.HasPrefix(row.Reason, core.SeatDownReason):
			down = append(down, row)
		}
	}
	if len(down) != 1 || len(cut) != 1 || len(rows) != 2 {
		t.Fatalf("ledger: %d seat-down row(s) and %d cut row(s) among %d finished rows, want one each and nothing else: %+v", len(down), len(cut), len(rows), rows)
	}
	if down[0].ReasonCode != ledger.ReasonSeatDown || cut[0].ReasonCode != ledger.ReasonBudget {
		t.Fatalf("reason codes = %q and %q, want %q for the dead seat and %q for the cut (the deadline's rows are budget, ADR 0065)", down[0].ReasonCode, cut[0].ReasonCode, ledger.ReasonSeatDown, ledger.ReasonBudget)
	}
	if down[0].JobID != pr.JobID || cut[0].JobID == pr.JobID || !strings.HasPrefix(cut[0].ModelTier, pr.RetriedOn+":") || !cut[0].Deferred {
		t.Fatalf("rows = seat-down %+v cut %+v: the published result is the first attempt's job %q, and the cut is the re-placement's own row on %q", down[0], cut[0], pr.JobID, pr.RetriedOn)
	}
	if !strings.Contains(cut[0].Reason, "was still on "+pr.RetriedOn) {
		t.Fatalf("cut row reason = %q, want it to say the re-placement was still on %s", cut[0].Reason, pr.RetriedOn)
	}
}

// TestASeatDownReplacementWaitingForCapacityIsCutByTheCallDeadlineNotByItsOwnWait: the re-placement's
// node is full (503 with a Retry-After), no other node has room, and the subtask waits for capacity
// (ADR 0063) with the dead seat's wait credited. The wait is bounded by agent_placement_wait_sec
// (30 s here) and by the retry's budget (~35 s); the call has 1 s. The deadline ends the wait, and
// the cut is recorded like any other: a closing row under a job id of its own (the refused
// dispatch's row belongs to that attempt), with the deadline's reason.
func TestASeatDownReplacementWaitingForCapacityIsCutByTheCallDeadlineNotByItsOwnWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	withCallReserve(t, 0) // pins the CUT: with a reserve the wait ends before the deadline (callwait_test.go)
	script := &firstJobScript{}
	var dispatchesAnywhere atomic.Int64
	// The first dispatch anywhere is taken; every later one is refused, full, with a Retry-After.
	hook := func(int64) int {
		if dispatchesAnywhere.Add(1) == 1 {
			return 0
		}
		return http.StatusServiceUnavailable
	}
	tune := func(id string) func(*fakeNode) {
		return func(f *fakeNode) {
			f.pollByJob = script.poll(t, id, seatDownWire(id, 6), runningForever)
			f.dispatchHook = hook
			f.dispatchRetryAfter = "5"
			f.maxQueueDepth = 4
		}
	}
	_, urlA := acceptingNode(t, "node-a", "unused", tune("node-a"))
	_, urlB := acceptingNode(t, "node-b", "unused", tune("node-b"))
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30
	fenceTheLocalSeat(t, &cfg)

	results, sum, elapsed := runWithin(t, 10*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "spread", []string{urlA, urlB}, deadlineIn(time.Second), nil)
	pr := results[0]

	if dispatchesAnywhere.Load() < 2 {
		t.Fatalf("dispatches = %d, want the seat-down attempt and at least the re-placement's refused dispatch (premise)", dispatchesAnywhere.Load())
	}
	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("RunWith returned after %s, want about the 1 s deadline: the credited wait must not run to its own 30 s TTL", elapsed)
	}
	if pr.deadlineCut || !SeatDownDefer(pr.Result) || sum.Deferred != 1 {
		t.Fatalf("published = cut %v reason %q summary %+v, want the seat-down defer the first attempt produced in time", pr.deadlineCut, pr.Result.Reason, sum)
	}
	for _, want := range []string{deadlinePrefix + "1 unfinished", "had not been placed on a seat", "this result is the first attempt"} {
		if !strings.Contains(pr.RetryNote, want) {
			t.Fatalf("retry_note = %q, want it to carry %q", pr.RetryNote, want)
		}
	}
	if strings.Contains(pr.RetryNote, "no node had room") {
		t.Fatalf("retry_note = %q describes the wait's own end for a wait the call's deadline ended", pr.RetryNote)
	}

	var closing, refused, down int
	for _, row := range finishedRows(t, cfg.LedgerPath) {
		switch {
		case strings.HasPrefix(row.Reason, deadlinePrefix):
			closing++
			if row.JobID == pr.JobID || row.ReasonCode != ledger.ReasonBudget || !row.Deferred {
				t.Fatalf("the closing row = job %q code %q deferred %v, want a budget row under a job id of its own (not the first attempt's %q)", row.JobID, row.ReasonCode, row.Deferred, pr.JobID)
			}
		case strings.Contains(row.Reason, "status 503"):
			refused++
			if row.JobID == pr.JobID {
				t.Fatalf("a refused dispatch's row carries the first attempt's job id %q", pr.JobID)
			}
		case strings.HasPrefix(row.Reason, core.SeatDownReason):
			down++
		}
	}
	if closing != 1 || refused < 1 || down != 1 {
		t.Fatalf("ledger: %d closing row(s), %d refused-dispatch row(s), %d seat-down row(s), want 1, at least 1 and 1", closing, refused, down)
	}
}

// TestASeatDownDeferThatArrivesAfterTheCallDeadlineIsTheDeadlinesDeferAndIsNotReplaced: the local seat
// is waiting for its engine when the call's deadline cancels the run, and the run answers with the
// seat-down defer a moment later. That outcome is produced after the deadline, so it is the
// deadline's (a budget defer that quotes what the run itself reported), not an infrastructure defer
// the delegator would re-place on another node while nobody is waiting for the answer. The idle
// remote must see no dispatch.
func TestASeatDownDeferThatArrivesAfterTheCallDeadlineIsTheDeadlinesDeferAndIsNotReplaced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-b", "verified from B", nil)
	var localRuns atomic.Int64
	local := func(ctx context.Context, _ core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		localRuns.Add(1)
		<-ctx.Done() // waiting for the seat's engine: the call's deadline ends the wait
		w := seatDownWire("this-box", 6)
		w.Seat = "local-seat"
		return w, nil
	}
	cfg := testCfg(t)

	results, sum, _ := runWithin(t, 10*time.Second, cfg, local,
		[]core.AgentContract{plainContract()}, "auto", []string{url}, deadlineIn(500*time.Millisecond), nil)
	pr := results[0]

	if localRuns.Load() != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("local runs = %d, remote dispatches = %d, want the one local run and no re-placement after the deadline", localRuns.Load(), node.dispatches.Load())
	}
	if sum.Deferred != 1 || sum.Retried != 0 || sum.Infrastructure != 0 || sum.LostToStack != 0 {
		t.Fatalf("summary = %+v, want one deferred subtask that was not retried and is not counted as a lost one (the deadline is a result shape, never a failure)", sum)
	}
	if !pr.deadlineCut || pr.retried || pr.RetryNote != "" || pr.Result.DeferClass != core.DeferClassBudget || SeatDownDefer(pr.Result) {
		t.Fatalf("published = cut %v retried %v note %q class %q reason %q, want the deadline's budget defer with no retry", pr.deadlineCut, pr.retried, pr.RetryNote, pr.Result.DeferClass, pr.Result.Reason)
	}
	if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(pr.Result.Reason, "reported infrastructure: "+core.SeatDownReason) {
		t.Fatalf("reason = %q, want the deadline's opening and what the run itself reported quoted behind it", pr.Result.Reason)
	}
	// What the run measured is still on the published result: the cut renames the outcome only.
	if pr.Result.SeatDownWaitSec != 6 {
		t.Fatalf("seat_down_wait_sec = %v, want the run's own 6 s kept on the cut", pr.Result.SeatDownWaitSec)
	}
	rows := finishedRows(t, cfg.LedgerPath)
	if len(rows) != 1 || rows[0].ReasonCode != ledger.ReasonBudget || !strings.HasPrefix(rows[0].Reason, deadlinePrefix) {
		t.Fatalf("ledger rows = %+v, want exactly the one budget row that says what the caller was told", rows)
	}
}

// TestTheRescueOfASeatLostInTheRepackIsEndedByTheCallDeadlineAndTheDeferIsNotReplaced: a node's seat
// went down in the structured re-pack, so its defer carries the finished answer and schema_miss and
// the delegator tries to re-pack the answer itself FIRST, on its own clock (the wall of a failed
// rescue is credited to the re-placement, ADR 0066). That rescue can run for minutes (it may
// cold-load the delegator's seat), and it is on the call's clock too: the call's deadline ends it, and
// the defer, which is then produced after the deadline, is the deadline's and is not re-placed.
func TestTheRescueOfASeatLostInTheRepackIsEndedByTheCallDeadlineAndTheDeferIsNotReplaced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	script := &firstJobScript{}
	down := seatDownWire("", 0)
	down.Output, down.StopReason, down.SchemaMiss = "the finished answer, in prose", "done", true
	down.Reason += " (during the structured re-pack)"
	tune := func(id string) func(*fakeNode) {
		return func(f *fakeNode) { f.pollByJob = script.poll(t, id, down, runningForever) }
	}
	nodeA, urlA := acceptingNode(t, "node-a", "unused", tune("node-a"))
	nodeB, urlB := acceptingNode(t, "node-b", "unused", tune("node-b"))
	cfg := testCfg(t)
	fenceTheLocalSeat(t, &cfg)
	var rescues atomic.Int64
	opts := deadlineIn(time.Second)
	opts.Rescue = func(ctx context.Context, _ core.AgentContract, _ string, budget time.Duration) (Rescued, error) {
		rescues.Add(1)
		// What the pipeline's rescue does on a cold delegator seat: it runs for as long as its
		// budget (never under two minutes) allows, and answers to its context.
		if budget < rescueFloor {
			t.Errorf("rescue budget = %s, want at least the rescue floor %s", budget, rescueFloor)
		}
		<-ctx.Done()
		return Rescued{}, ctx.Err()
	}

	results, sum, elapsed := runWithin(t, 10*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "spread", []string{urlA, urlB}, opts, nil)
	pr := results[0]

	if rescues.Load() != 1 {
		t.Fatalf("rescue ran %d times, want once (premise: the seat-down defer carried a finished answer)", rescues.Load())
	}
	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("RunWith returned after %s, want about the 1 s deadline, not the rescue's own two-minute budget", elapsed)
	}
	if got := nodeA.dispatches.Load() + nodeB.dispatches.Load(); got != 1 {
		t.Fatalf("dispatches = %d, want only the first attempt: a defer produced after the deadline is not re-placed", got)
	}
	if !pr.deadlineCut || pr.retried || pr.Result.DeferClass != core.DeferClassBudget || SeatDownDefer(pr.Result) ||
		!strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(pr.Result.Reason, "reported infrastructure: "+core.SeatDownReason) {
		t.Fatalf("published = cut %v retried %v class %q reason %q, want the deadline's budget defer quoting the seat-down the node reported", pr.deadlineCut, pr.retried, pr.Result.DeferClass, pr.Result.Reason)
	}
	if sum.Deferred != 1 || sum.Retried != 0 || sum.LostToStack != 0 {
		t.Fatalf("summary = %+v, want one deferred subtask, not retried and not lost to the stack", sum)
	}
	// The node's job had ended (it reported the seat-down); what was still running was the delegator's
	// own re-pack of the finished answer. The cut says that, and it leaves nothing for the recovery
	// pass: "still on the node, not taken back" is false of a job the node answered, and an intent
	// left open for it makes the recovery pass collect an outcome the call has already published.
	for _, bad := range []string{"was still on", "not taken back", "no longer waiting for it"} {
		if strings.Contains(pr.Result.Reason, bad) {
			t.Fatalf("reason = %q claims the node still holds a job it had already answered (%q)", pr.Result.Reason, bad)
		}
	}
	for _, want := range []string{"had been answered by " + pr.Node, "(job " + pr.JobID + ")", "re-packing the finished answer itself"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Fatalf("reason = %q, want it to say what was still running: %q", pr.Result.Reason, want)
		}
	}
	closed, open := intentNotes(t, cfg.StateDir)
	if pr.orphanable || closed[pr.JobID] != intentNoteTerminal || len(open) != 0 {
		t.Fatalf("orphanable %v, intent closed as %q (open=%v), want the node's terminal answer closed as %q with nothing left open for recovery", pr.orphanable, closed[pr.JobID], open, intentNoteTerminal)
	}
}

// TestTheCutOfAJobTheNodeAlreadyAnsweredDoesNotSayItIsStillThere: the unit under the run-level test
// above, with the contrast it must not move. A cancelled poll, and a delegator-authored defer for a job
// a give-up left on its node, are jobs the node may still hold: they keep the words and the open
// intent. The node's own terminal defer, read before the deadline, is not one.
func TestTheCutOfAJobTheNodeAlreadyAnsweredDoesNotSayItIsStillThere(t *testing.T) {
	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}
	answered := r.cutByDeadline(PlacedResult{
		Node: "node-a", Seat: "remote-seat", ranBase: "http://192.0.2.50:1", JobID: "agd-done", intentRecorded: true,
		Result: seatDownWire("node-a", 6), rescueSpent: 3 * time.Second,
	})
	if !answered.deadlineCut || answered.orphanable || answered.Err != "" {
		t.Fatalf("answered job: cut %v orphanable %v err %q, want a cut that is not orphanable", answered.deadlineCut, answered.orphanable, answered.Err)
	}
	for _, want := range []string{"had been answered by node-a (job agd-done)", "re-packing the finished answer itself", "the run itself reported infrastructure: " + core.SeatDownReason} {
		if !strings.Contains(answered.Result.Reason, want) {
			t.Errorf("answered job: reason = %q, want %q", answered.Result.Reason, want)
		}
	}
	if strings.Contains(answered.Result.Reason, "was still on") || strings.Contains(answered.Result.Reason, "not taken back") {
		t.Errorf("answered job: reason = %q says the job is still on the node", answered.Result.Reason)
	}
	// No rescue behind it (the node's answer was read a moment late): still answered, nothing to re-pack.
	late := r.cutByDeadline(PlacedResult{Node: "node-a", ranBase: "http://192.0.2.50:1", JobID: "agd-late", intentRecorded: true, Result: seatDownWire("node-a", 6)})
	if late.orphanable || strings.Contains(late.Result.Reason, "re-packing") || !strings.Contains(late.Result.Reason, "had been answered by node-a (job agd-late)") {
		t.Errorf("late answer: orphanable %v reason %q, want the answered wording without a re-pack", late.orphanable, late.Result.Reason)
	}
	// What the cut must not move: a job the node may still hold.
	polled := r.cutByDeadline(PlacedResult{Node: "node-a", ranBase: "http://192.0.2.50:1", JobID: "agd-open", intentRecorded: true,
		Err: "canceled: context deadline exceeded; withdraw not confirmed: HTTP 405: the node has no withdraw route", orphanable: true})
	if !polled.orphanable || !strings.Contains(polled.Result.Reason, "was still on node-a (job agd-open)") || !strings.Contains(polled.Result.Reason, "it was not taken back from the node") {
		t.Errorf("cancelled poll: orphanable %v reason %q, want it still on the node and left to the recovery pass", polled.orphanable, polled.Result.Reason)
	}
	gaveUp := r.cutByDeadline(PlacedResult{Node: "node-a", ranBase: "http://192.0.2.50:1", JobID: "agd-poll", intentRecorded: true, orphanable: true,
		Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget, Reason: "poll deadline after 5m0s: the node accepted the job but did not reach a terminal state"}})
	if !gaveUp.orphanable || !strings.Contains(gaveUp.Result.Reason, "was still on node-a (job agd-poll)") {
		t.Errorf("poll-deadline defer: orphanable %v reason %q, want it still on the node (the give-up left it open)", gaveUp.orphanable, gaveUp.Result.Reason)
	}
}

// TestARescueIsNeverStartedOnALiveContextAfterTheCallDeadline: a seat-down defer that carries a
// finished answer reaches the delegator once the deadline has passed. The deadline forbids a seat
// run after it (lookAnswer says so for the queue lane), so the only way a rescue may run is on the
// call's own context, which is over: a rescue that can structure the answer without a seat still
// delivers it, and one that needs the seat stops at once.
func TestARescueIsNeverStartedOnALiveContextAfterTheCallDeadline(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-b", "verified from B", nil)
	local := func(ctx context.Context, _ core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		w := seatDownWire("this-box", 0)
		w.Seat, w.Output, w.StopReason, w.SchemaMiss = "local-seat", "the finished answer, in prose", "done", true
		return w, nil
	}
	var rescues, live atomic.Int64
	opts := deadlineIn(400 * time.Millisecond)
	opts.Rescue = func(ctx context.Context, _ core.AgentContract, _ string, _ time.Duration) (Rescued, error) {
		rescues.Add(1)
		if ctx.Err() == nil {
			live.Add(1)
		}
		return Rescued{}, ctx.Err()
	}

	results, _, _ := runWithin(t, 10*time.Second, testCfg(t), local,
		[]core.AgentContract{plainContract()}, "auto", []string{url}, opts, nil)

	if live.Load() != 0 {
		t.Fatalf("a rescue was started on a live context %d time(s) after the call deadline: a seat run the deadline exists to prevent", live.Load())
	}
	if node.dispatches.Load() != 0 || !results[0].deadlineCut || SeatDownDefer(results[0].Result) {
		t.Fatalf("dispatches = %d cut %v reason %q, want the deadline's defer and no re-placement", node.dispatches.Load(), results[0].deadlineCut, results[0].Result.Reason)
	}
}

// TestASeatDownRetryWhoseNodeSelectionWasCutByTheCallDeadlineNamesTheDeadlineNotTheFleet: the local
// seat's run ends in a seat-down defer in time, so a re-placement on a remote is owed, and the
// deadline ends the fleet read that would have named the node. The note must say so. "No other node
// could take the contract" is a claim about nodes that a read the deadline cut cannot support (ADR
// 0065), and the order of the two notes in runOne is what decides which one the caller gets.
func TestASeatDownRetryWhoseNodeSelectionWasCutByTheCallDeadlineNamesTheDeadlineNotTheFleet(t *testing.T) {
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
	var localRuns atomic.Int64
	local := func(context.Context, core.AgentContract, LocalOptions) (core.AgentWireResult, error) {
		localRuns.Add(1)
		slow.Store(true) // the remote's health turns slow once the first attempt is running
		w := seatDownWire("this-box", 6)
		w.Seat = "local-seat"
		return w, nil
	}

	results, sum, _ := runWithin(t, 15*time.Second, testCfg(t), local,
		[]core.AgentContract{plainContract()}, "auto", []string{url}, deadlineIn(1500*time.Millisecond), nil)
	pr := results[0]

	if localRuns.Load() != 1 || pr.retried || pr.deadlineCut || !SeatDownDefer(pr.Result) || sum.Deferred != 1 {
		t.Fatalf("local runs %d retried %v cut %v summary %+v reason %q, want the seat-down defer published as it was, with no retry",
			localRuns.Load(), pr.retried, pr.deadlineCut, sum, pr.Result.Reason)
	}
	if !strings.HasPrefix(pr.RetryNote, "retry skipped: ") || !strings.Contains(pr.RetryNote, "call deadline reached") {
		t.Fatalf("retry_note = %q, want it to say the call's deadline ended the choice of a retry node", pr.RetryNote)
	}
	if strings.Contains(pr.RetryNote, "no other node could take the contract") {
		t.Fatalf("retry_note = %q accuses nodes the deadline kept the delegator from asking", pr.RetryNote)
	}
}

// TestPageIssueFailedReadsADeadlineCutRetryOfASeatDownFirstAttemptAsNothing: the per-page cap reads
// the retry's own verdict for a first attempt that stands as a seat-down defer (retryRanAndFailed,
// ADR 0066), and a retry the call's deadline cut is class budget with its seat and node named,
// which is what "the seat hit its budget on this page" looks like (ADR 0065). The two rules meet in
// mergeAttempts, and only deadlineCut tells them apart. Built with the real cut, so the fixture
// cannot drift from what the engine publishes.
func TestPageIssueFailedReadsADeadlineCutRetryOfASeatDownFirstAttemptAsNothing(t *testing.T) {
	r := &runner{call: pastDeadline(1)}
	down := PlacedResult{Node: "node-a", Result: seatDownWire("node-a", 6)}
	cuts := map[string]PlacedResult{
		"the retry's remote job was still running": r.cutByDeadline(PlacedResult{
			Node: "node-b", Seat: "remote-seat", ranBase: "http://192.0.2.50:1", intentRecorded: true,
			Err: "canceled: context deadline exceeded",
		}),
		"the retry's local run was still running": r.cutByDeadline(PlacedResult{
			Node: "this-box", Seat: "local-seat", ranLocal: true, Result: cancelledLoop(),
		}),
	}
	for name, cut := range cuts {
		if !cut.deadlineCut || cut.Unplaced || !cut.Result.Deferred || cut.Result.DeferClass != core.DeferClassBudget {
			t.Fatalf("%s: the cut is %+v, want a class-budget defer that is not Unplaced (the shape the page cap would count)", name, cut)
		}
		merged := mergeAttempts(down, cut)
		if merged.retryRanAndFailed || pageIssueFailed(merged) {
			t.Errorf("%s counted against the page: the call ran out of time, no seat ran the page to a verdict", name)
		}
	}
	// What the cut must not move: a retry that ran and hit its own budget, with no deadline behind
	// it, still counts (ADR 0066), and so does one that failed verification.
	own := PlacedResult{Node: "node-b", Seat: "remote-seat", ranBase: "http://192.0.2.50:1",
		Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget, Reason: "agent loop: step budget exhausted"}}
	if !pageIssueFailed(mergeAttempts(down, own)) {
		t.Error("a seat-down first attempt whose retry hit its own budget stopped counting against the page")
	}
	wrong := PlacedResult{Node: "node-b", AcceptanceFailures: []string{"contains:x"}}
	if !pageIssueFailed(mergeAttempts(down, wrong)) {
		t.Error("a seat-down first attempt whose retry failed verification stopped counting against the page")
	}
}

// TestPageCapIgnoresASeatDownRetryTheCallDeadlineKeepsCutting: run level. Five issues of one research
// page, each ending the same way: the local seat's run is a seat-down defer, the re-placement on the
// remote is still running when the call's deadline cuts it. None of them is the page's fault, so the
// page is never backed off: the remote is asked all five times. Without the cut's exemption the
// third issue would count (the retry is class budget with a seat named) and the fourth would be
// refused for fifteen minutes with "none of them produced a verified digest".
func TestPageCapIgnoresASeatDownRetryTheCallDeadlineKeepsCutting(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-b", "unused", func(f *fakeNode) {
		f.pollByJob = func(string, int64) (map[string]any, int) { return runningForever() }
	})
	var localRuns atomic.Int64
	local := func(context.Context, core.AgentContract, LocalOptions) (core.AgentWireResult, error) {
		localRuns.Add(1)
		w := seatDownWire("this-box", 6)
		w.Seat = "local-seat"
		return w, nil
	}
	page := uniquePage("the page whose seat-down retry a call keeps running out of time on")
	for issue := 1; issue <= 5; issue++ {
		results, _, _ := runWithin(t, 10*time.Second, testCfg(t), local,
			[]core.AgentContract{pageContract(page)}, "auto", []string{url}, deadlineIn(700*time.Millisecond), nil)
		pr := results[0]
		if strings.Contains(pr.Result.Reason, "page retry cap") {
			t.Fatalf("issue %d was backed off (%s): a seat-down retry the call's deadline cut was counted against its page", issue, pr.Result.Reason)
		}
		if !SeatDownDefer(pr.Result) || !strings.Contains(pr.RetryNote, deadlinePrefix) {
			t.Fatalf("issue %d: reason %q note %q, want the seat-down defer with the cut retry in its note (fixture)", issue, pr.Result.Reason, pr.RetryNote)
		}
	}
	if localRuns.Load() != 5 || node.dispatches.Load() != 5 {
		t.Fatalf("local runs = %d, remote dispatches = %d, want 5 and 5: every issue reached its retry", localRuns.Load(), node.dispatches.Load())
	}
}
