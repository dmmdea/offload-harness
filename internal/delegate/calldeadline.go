// calldeadline.go is the whole-call deadline of the delegation engine (ADR 0065,
// register C-67).
//
// The MCP client aborts a tool call at its own limit (1,800 s in the reference
// setup) and drops the response with it. RunWith used to return only when EVERY
// subtask ended, and a producing job is polled to its node ceiling (up to 14,400
// s), so one slow subtask held a call past the abort and took the finished ones
// down with it — a call that ran 2,103 s lost a finished 423 s answer.
//
// RunOptions.Deadline is the door's answer: an absolute instant, set below the
// client's abort. At that instant
//
//   - every subtask still running is CANCELLED (the deadline is the context every
//     placement, poll and local run already honours, so the seat is told to stop);
//   - nothing further is STARTED (a subtask still waiting for a run slot, and the
//     later chunks of a batched call, never begin);
//   - the call RETURNS what has finished, and every subtask that had not is
//     published as a budget defer "call deadline reached; N unfinished" — a result
//     shape, never a failure, so the finished digests are not lost behind an error.
//
// It is not a per-subtask wall: timeout_sec still bounds each subtask's own
// execution. The deadline bounds the CALL as the client experiences it.

package delegate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// ErrCallDeadline is the cause a context carries when the whole-call deadline
// passed (context.Cause). A caller that cancels for any other reason — the client
// going away, a test tearing down — is NOT the call deadline: its subtasks report
// their own "canceled" outcomes exactly as before.
var ErrCallDeadline = errors.New("call deadline reached")

// callDeadlinePrefix opens every reason the deadline publishes. It is a stable
// grep key: the ledger, the corpus and the wire all carry it.
const callDeadlinePrefix = "call deadline reached"

// Grace bounds the unwind allowance after the deadline: the time cooperating
// goroutines get to write their own telemetry rows and hand back a truthful
// result before the call returns without them. It scales with the deadline so a
// compressed test clock stays fast, and is capped so the live check "call wall <=
// deadline + 30 s" holds with room to spare.
const (
	callGraceMin = 250 * time.Millisecond
	callGraceMax = 10 * time.Second
)

// callDeadline is ONE call's deadline state. It is shared by every chunk of a
// batched call, so the unfinished count in each published reason spans the whole
// call, not the chunk that happened to hold the subtask.
type callDeadline struct {
	at    time.Time     // the instant the call must be over
	grace time.Duration // how long cooperating subtasks get to unwind after it

	total    int          // subtasks the whole call owes an answer for
	answered atomic.Int64 // subtasks that produced a REAL result (never a deadline defer)
	frozen   atomic.Int64 // unfinished count at the instant the deadline was first observed; -1 = not yet
}

// newCallDeadline builds the state for a call whose deadline is opts.Deadline, or
// returns nil when there is none. A context that already ends earlier keeps that
// bound: the deadline can only shorten a call, never extend it. owed is how many
// subtasks the whole call carries.
func newCallDeadline(ctx context.Context, opts *RunOptions, owed int) *callDeadline {
	if opts == nil {
		return nil
	}
	if opts.call != nil {
		return opts.call
	}
	if opts.Deadline.IsZero() {
		return nil
	}
	at := opts.Deadline
	if d, ok := ctx.Deadline(); ok && d.Before(at) {
		at = d
	}
	grace := time.Until(at) / 20
	grace = min(max(grace, callGraceMin), callGraceMax)
	c := &callDeadline{at: at, grace: grace, total: owed}
	c.frozen.Store(-1)
	return c
}

// reached reports whether the call deadline has passed. Nil-safe: a call with no
// deadline never reaches it.
func (c *callDeadline) reached() bool {
	return c != nil && !time.Now().Before(c.at)
}

// unfinished is how many subtasks of the whole call had no result when the
// deadline was first observed. It is frozen at that first look, so every reason a
// call publishes states the same number even though the subtasks unwind at
// slightly different moments.
func (c *callDeadline) unfinished() int {
	if v := c.frozen.Load(); v >= 0 {
		return int(v)
	}
	n := max(int64(c.total)-c.answered.Load(), 1)
	c.frozen.CompareAndSwap(-1, n)
	return int(c.frozen.Load())
}

// answer records that one subtask delivered a real result; nil-safe.
func (c *callDeadline) answer() {
	if c != nil {
		c.answered.Add(1)
	}
}

// reason builds the published sentence: the stable opening, the whole call's
// unfinished count, and what this subtask was doing when the deadline passed.
func (c *callDeadline) reason(where string) string {
	return fmt.Sprintf("%s; %d unfinished — this subtask %s", callDeadlinePrefix, c.unfinished(), where)
}

// wire is the AgentWireResult of a subtask the deadline cut off: a budget defer
// (a ceiling stopped it, and the caller's next move is a smaller call or another
// one), never an infrastructure or config class — nothing about the stack broke.
func (c *callDeadline) wire(where string) core.AgentWireResult {
	return core.AgentWireResult{
		SchemaVersion: core.AgentWireSchemaVersion,
		Deferred:      true,
		DeferClass:    core.DeferClassBudget,
		Reason:        c.reason(where),
	}
}

// await is RunWith's wait for its subtask goroutines. With no deadline it is
// wg.Wait(), exactly as before. With one it returns when they are all done, or —
// once the deadline has passed — after the unwind allowance, whichever is first.
// abandoned says goroutines were still running; drained closes when they finally
// are, so the caller can keep the ledger open for them.
//
// A context that ended for any OTHER reason (the client cancelled) is not the
// deadline: every subtask reports its own cancelled outcome, so every goroutine is
// waited for, as it always was.
func (c *callDeadline) await(ctx context.Context, wg *sync.WaitGroup) (drained <-chan struct{}, abandoned bool) {
	if c == nil {
		wg.Wait()
		return nil, false
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return done, false
	case <-ctx.Done():
	}
	if !c.reached() {
		<-done
		return done, false
	}
	c.unfinished() // freeze the whole call's count at the instant the deadline passed
	t := time.NewTimer(c.grace)
	defer t.Stop()
	select {
	case <-done:
		return done, false
	case <-t.C:
		return done, true
	}
}

// resultBoard collects the subtask results. The goroutines write through put and
// RunWith reads once, after closing the board, so a goroutine that is still
// running when the call returns can never write into a slice the caller owns — its
// late answer is dropped, not raced.
type resultBoard struct {
	mu      sync.Mutex
	results []PlacedResult
	done    []bool
	closed  bool
}

func newResultBoard(n int) *resultBoard {
	return &resultBoard{results: make([]PlacedResult, n), done: make([]bool, n)}
}

// put stores subtask i's result; false means the board was already closed and the
// result was dropped.
func (b *resultBoard) put(i int, pr PlacedResult) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.results[i], b.done[i] = pr, true
	return true
}

// close freezes the board and hands back what it holds.
func (b *resultBoard) close() ([]PlacedResult, []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.results, b.done
}

// cutByDeadline turns a subtask outcome that was NOT finished when the deadline
// passed into the published call-deadline defer. It is applied at the two moments
// an outcome is PRODUCED — finish (an attempt's end) and settle (an outcome no
// attempt produced) — so the wire result, the ledger row and the corpus row say
// the same thing. It is not a filter to run over a result later: "produced after
// the deadline" can only be answered when the outcome is produced, and a result
// that finished earlier (an abstention whose retry was cut, published with the
// retry's fate in its retry_note) must keep what it was.
//
// It rewrites only outcomes that are not answers: a failure or a defer produced
// once the deadline had passed (a cancelled poll, a cancelled local run, a capacity
// wait that ran out of call). A result that finished — including one that failed
// its acceptance checks — is a real answer and is never rewritten.
func (r *runner) cutByDeadline(pr PlacedResult) PlacedResult {
	if !r.call.reached() || pr.deadlineCut || pr.waitCapacity {
		return pr
	}
	if pr.Err == "" && !pr.Result.Deferred {
		return pr
	}
	var where string
	switch {
	case pr.intentRecorded:
		// Cancelling the poll leaves the job on its node, where it could start later
		// on a seat nobody is waiting for. Ask the node to drop it (a request, not a
		// claim: see withdrawCut).
		r.withdrawCut(pr.ranBase, pr.JobID)
		where = fmt.Sprintf("was still on %s (job %s) when the call's deadline passed; the node was asked to withdraw it if it had not started (a job that had started may still finish there), and this call is no longer waiting for it",
			nodeOrBase(pr), pr.JobID)
		// The node acked the job and the delegator walked away: it may finish it, so
		// the intent stays open for the recovery pass (the cancel arms in runRemote
		// set this too; setting it here makes it hold for every exit).
		pr.orphanable = true
	case pr.ranLocal:
		where = "was still running on the local seat when the call's deadline passed; it was cancelled"
	default:
		where = "had not been placed on a seat when the call's deadline passed"
		pr.Unplaced = true
		// No node ran it, so it names none (exhausted() does the same for "no node
		// took it"): a capacity defer's own Node and Seat are the DECIDING box.
		pr.Node, pr.Seat = "", ""
		// PlacementReason narrates how the placement went. A capacity wait's own
		// text ("no node had room within 30s") would now read as the OUTCOME, when
		// the call simply ran out of time: the marker leads, the history follows.
		if pr.PlacementReason != "" {
			pr.PlacementReason = callDeadlinePrefix + " before a seat took it — " + pr.PlacementReason
		} else {
			pr.PlacementReason = callDeadlinePrefix + " before a seat took it"
		}
	}
	pr.Result = r.call.wire(where)
	pr.Err = ""
	pr.AcceptanceFailures = nil
	pr.refused, pr.refusalStatus = false, 0
	pr.deadlineCut = true
	return pr
}

// deadlineWithdrawTimeout bounds the one best-effort withdraw sent for a job the
// call deadline cut. It runs detached from the (already cancelled) call context,
// and never longer than three quarters of the unwind allowance: a node that does
// not answer must not turn the truthful cut result (its node and job) into an
// abandoned one that says neither.
const deadlineWithdrawTimeout = 5 * time.Second

// withdrawCut asks the node to withdraw a job the call deadline walked away from:
// DELETE /fleet/jobs/{id} with the fleet bearer. Best effort, and deliberately a
// REQUEST rather than a claim — nothing the call publishes depends on the answer:
//
//   - a node that has not shipped the route answers 404 or 405 (its behaviour
//     today: the job stays, as it always did);
//   - a job the node has already started is not the delegator's to cancel, so a
//     node that only ever withdraws never-started jobs refuses it, and the intent
//     stays open for the recovery pass either way (cutByDeadline marks it
//     orphanable);
//   - a transport failure is logged and dropped.
//
// Blocking (bounded by deadlineWithdrawTimeout) on purpose: the goroutine that
// calls it is inside the unwind allowance, and returning before the request is out
// would let the call return with the ask still unsent.
func (r *runner) withdrawCut(base, jobID string) {
	if base == "" || jobID == "" {
		return
	}
	timeout := deadlineWithdrawTimeout
	if g := r.call.grace * 3 / 4; g > 0 && g < timeout {
		timeout = g
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + url.PathEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return
	}
	if r.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		log.Printf("delegate: call deadline: the withdraw of job %s at %s failed (best effort): %v", jobID, base, err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxFleetBody))
	resp.Body.Close()
	log.Printf("delegate: call deadline: asked %s to withdraw job %s: status %d (best effort; a node that has not shipped the route answers 404 or 405)", base, jobID, resp.StatusCode)
}

// emitPair sends one PAIR frame unless the run has shut the emitter. Before the
// call deadline no goroutine could outlive RunWith, so RunWith's deferred
// pair.Wait() saw every frame. A seat that ignores its context is abandoned and
// may report later; Emit does WaitGroup.Add, and an Add racing the final Done of a
// Wait in progress makes that Done panic ("WaitGroup misuse") — a crash for a
// frame nobody is waiting for. Emission therefore holds the read lock, and
// shutPair (below) takes the write lock before Wait, so a late frame is dropped.
func (r *runner) emitPair(ev pairworkloads.Event) {
	r.pairMu.RLock()
	defer r.pairMu.RUnlock()
	if r.pairShut {
		return
	}
	r.pair.Emit(ev)
}

// shutPair ends PAIR emission for this run; RunWith defers it so it runs before
// pair.Wait(). It waits for any Emit in progress (bounded: building and queueing a
// frame), so after it returns no Add can race the Wait.
func (r *runner) shutPair() {
	r.pairMu.Lock()
	r.pairShut = true
	r.pairMu.Unlock()
}

// nodeOrBase names the node a job was placed on: its advertised id, else its dial
// base — never an empty string mid-sentence.
func nodeOrBase(pr PlacedResult) string {
	if pr.Node != "" {
		return pr.Node
	}
	if pr.ranBase != "" {
		return pr.ranBase
	}
	return "its node"
}

// unlaunched is the result of a subtask that was never started because the call
// ended first (or, for a caller cancellation, because the context ended). Its row
// is recorded here — no attempt ever will — under a freshly minted job id.
func (r *runner) unlaunched(contract core.AgentContract) PlacedResult {
	// No node ran it, so it names no node and no seat.
	pr := PlacedResult{
		Unplaced: true, deadlineCut: r.call.reached(),
		PlacementReason: "not started: the call ended first",
	}
	if r.call.reached() {
		pr.Result = r.call.wire("never started: the call's deadline passed first")
	} else {
		pr.Err = "canceled: the call's context ended before this subtask started"
	}
	pr.JobID = mintJobID()
	r.record(contract, pr)
	return pr
}

// abandoned is the published result of a subtask whose goroutine had not returned
// when the unwind allowance ran out — a seat stuck somewhere that never looks at
// its context. The goroutine keeps its own telemetry and its late answer is
// dropped by the closed board.
func (r *runner) abandoned() PlacedResult {
	return PlacedResult{
		Unplaced: true, deadlineCut: true,
		PlacementReason: "call deadline reached before this subtask stopped",
		Result: r.call.wire(fmt.Sprintf("did not stop within %s of the call's deadline passing; whatever it answers later is discarded",
			r.call.grace.Round(time.Millisecond))),
	}
}

// cutQueued applies the deadline to route=queue's outcome. The queue lane polls
// its subtasks one after another and reports a poll the context cancelled as a
// failure; when the CALL DEADLINE is what cancelled it, those are unfinished work
// (the job stays on the holder), not failures, and the summary follows.
func (c *callDeadline) cutQueued(results []PlacedResult, sum Summary, err error) ([]PlacedResult, Summary, error) {
	if !c.reached() || err != nil {
		return results, sum, err
	}
	var cut []int
	for i, pr := range results {
		// A transport error names the context's CAUSE ("call deadline reached"), a
		// bare cancellation its Err ("context deadline exceeded"): accept both.
		if strings.HasPrefix(pr.Err, "canceled:") ||
			(strings.HasPrefix(pr.Err, "queue submit:") &&
				(strings.Contains(pr.Err, callDeadlinePrefix) || strings.Contains(pr.Err, context.DeadlineExceeded.Error()))) {
			cut = append(cut, i)
		}
	}
	if len(cut) == 0 {
		return results, sum, err
	}
	c.frozen.CompareAndSwap(-1, int64(len(cut)))
	for _, i := range cut {
		pr := &results[i]
		where := fmt.Sprintf("was still queued on the holder (job %s) when the call's deadline passed; the job stays on the holder and this call is no longer waiting for it", pr.JobID)
		if strings.HasPrefix(pr.Err, "queue submit:") {
			where = "had not been submitted to the holder when the call's deadline passed"
			pr.Unplaced = true
		}
		pr.Result = c.wire(where)
		pr.Err = ""
		pr.deadlineCut = true
		sum.Failed--
		sum.Deferred++
	}
	return results, sum, nil
}
