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
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
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

// wire is the AgentWireResult of a subtask nobody ran (or that left nothing to
// keep): a budget defer (a ceiling stopped it, and the caller's next move is a
// smaller call or another one). The deadline never publishes an infrastructure or
// config class of its own — it says WHEN the call ran out, not that a box broke; a
// run's own verdict rides in the reason (ownVerdict), never in the class.
func (c *callDeadline) wire(where string) core.AgentWireResult {
	return c.stamp(core.AgentWireResult{}, where)
}

// stamp turns the wire a run produced into the call-deadline defer WITHOUT
// discarding what the run measured. The deadline decides what the outcome is
// CALLED (a budget defer whose reason opens "call deadline reached"), never what
// was observed: a local run the deadline cancels after nine steps and thousands of
// generated tokens still reports them, on the published result, the ledger row and
// the corpus row. Those are the longest runs — the ones the deadline exists to cut
// and the ones the wall sizing and the rigger most need measured.
func (c *callDeadline) stamp(w core.AgentWireResult, where string) core.AgentWireResult {
	w.SchemaVersion = core.AgentWireSchemaVersion
	w.Deferred = true
	w.DeferClass = core.DeferClassBudget
	w.Reason = c.reason(where)
	return w
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
//
// The cut keeps the run's own wire (steps, tokens, stop reason, seat rate, trace)
// and says what the outcome itself reported beyond the cancellation the deadline
// caused (ownVerdict), so a stack failure that lands inside the unwind is not
// erased: its class and words ride in the reason. Which outcomes are "produced
// after the deadline" is decided by the clock (reached), not by the context's
// cause: ErrCallDeadline is the cause a subtask's context carries so an error that
// wraps it names the deadline, but an outcome's own text is the only evidence of
// what caused it, and every place that ends because a context ended words that
// differently. The one hard line — a real answer is never rewritten — needs no
// cause at all.
func (r *runner) cutByDeadline(pr PlacedResult) PlacedResult {
	return r.cutOutcome(pr, true)
}

// cutOutcome is cutByDeadline with the quoting switchable. settle cuts the outcomes
// of a WAIT (a capacity wait that ran out of call, a lease wait, a shed): their own
// text describes the wait ("no node had room within 30s"), which after the deadline
// would read as the outcome when the call simply ran out of time. The placement
// narration keeps that history (behind the deadline marker), so they quote nothing.
func (r *runner) cutOutcome(pr PlacedResult, quote bool) PlacedResult {
	if !r.call.reached() || pr.deadlineCut || pr.waitCapacity {
		return pr
	}
	if pr.Err == "" && !pr.Result.Deferred {
		return pr
	}
	own := ""
	if quote {
		own = ownVerdict(pr)
	}
	wire := pr.Result
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
		where = "was still running on the local seat when the call's deadline passed"
		if own == "" {
			// Only an outcome that echoed the cancellation says the run was cancelled;
			// one that failed or deferred on its own is not authored a cancel the code
			// never observed.
			where += "; it was cancelled"
		}
	default:
		where = "had not been placed on a seat when the call's deadline passed"
		pr.Unplaced = true
		// No node ran it, so it names none (exhausted() does the same for "no node
		// took it"): a capacity defer's own Node and Seat are the DECIDING box, on
		// the result and on the wire it carries.
		pr.Node, pr.Seat = "", ""
		wire.NodeID, wire.Seat, wire.Placed = "", "", nil
		// PlacementReason narrates how the placement went. A capacity wait's own
		// text ("no node had room within 30s") would now read as the OUTCOME, when
		// the call simply ran out of time: the marker leads, the history follows.
		if pr.PlacementReason != "" {
			pr.PlacementReason = callDeadlinePrefix + " before a seat took it — " + pr.PlacementReason
		} else {
			pr.PlacementReason = callDeadlinePrefix + " before a seat took it"
		}
	}
	if own != "" {
		where += "; the run itself " + own
	}
	pr.Result = r.call.stamp(wire, where)
	pr.Err = ""
	pr.AcceptanceFailures = nil
	pr.refused, pr.refusalStatus = false, 0
	pr.deadlineCut = true
	return pr
}

// verdictMax bounds what a cut quotes of the outcome it replaced: a poll-deadline
// or refusal-chain reason can run to a paragraph, and the published reason has to
// stay one readable sentence.
const verdictMax = 240

// echoesOfTheCancel are the words an outcome carries when the only thing it says is
// that a context ended — the deadline's own doing, so a cut does not quote them
// back.
var echoesOfTheCancel = []string{
	"context canceled", "context deadline exceeded", callDeadlinePrefix,
	"agent loop: canceled (the parent context ended",
	"(the caller's deadline, not this node's ceiling)",
}

// ownVerdict is what an outcome said for itself before the deadline stamped it:
// "" when it said nothing beyond the cancellation. The result of a subtask that
// failed or deferred for its own reasons a moment after the deadline must keep them
// (an engine that refused the connection is not "a subtask that ran out of time"),
// so they ride in the published reason and in the ledger and corpus rows that carry
// it: "reported <class>: <reason>" for a defer, "failed: <error>" for a failure.
//
// A cancelled poll's own opening clause ("canceled: context deadline exceeded") is
// the deadline speaking and is dropped; anything a give-up appended after it (a
// withdraw that was asked and not confirmed) is kept.
func ownVerdict(pr PlacedResult) string {
	switch {
	case pr.Err != "":
		text := pr.Err
		if strings.HasPrefix(text, "canceled:") {
			text = ""
			if i := strings.Index(pr.Err, "; "); i >= 0 {
				text = strings.TrimSpace(pr.Err[i+2:])
			}
		}
		if text == "" || echoesTheCancel(text) {
			return ""
		}
		return "failed: " + deadlineClip(text, verdictMax)
	case pr.Result.Deferred:
		text := pr.Result.Reason
		if text == "" || echoesTheCancel(text) {
			return ""
		}
		class := pr.Result.DeferClass
		if class == "" {
			class = "defer"
		}
		return "reported " + class + ": " + deadlineClip(text, verdictMax)
	}
	return ""
}

func echoesTheCancel(text string) bool {
	for _, e := range echoesOfTheCancel {
		if strings.Contains(text, e) {
			return true
		}
	}
	return false
}

// deadlineClip cuts s to at most n bytes on a rune boundary, marking the cut.
func deadlineClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
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
		// Dropped by design, but not silently: PAIR's card for this job stays as it was
		// until PAIR's own staleness sweep, and nobody would otherwise know why.
		r.pairLate.Do(func() {
			log.Printf("delegate: a PAIR frame for job %s was dropped: an abandoned subtask reported after the call had returned; its card stays as it was until PAIR's own staleness sweep (results unaffected; further drops in this run are not logged)", ev.JobID)
		})
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

// abandoned is the published result of subtask i, whose goroutine had not returned
// when the unwind allowance ran out — a seat stuck somewhere that never looks at
// its context. Its late answer is dropped by the closed board, but the goroutine
// keeps writing its own rows, and a finished answer there would contradict what the
// caller was told ("discarded") with nothing to reconcile it by. So the result
// carries the job id of the attempt that had not returned (lastJob), the call records
// its own row under that id now, and the goroutine's late row, when it comes, is under
// the same id. It still names no node or seat: the call cannot say where it was
// running.
func (r *runner) abandoned(i int, contract core.AgentContract) PlacedResult {
	pr := PlacedResult{
		Unplaced: true, deadlineCut: true,
		PlacementReason: "call deadline reached before this subtask stopped",
	}
	if id, ok := r.lastJob.Load(i); ok {
		pr.JobID, _ = id.(string)
	}
	if pr.JobID == "" {
		pr.JobID = mintJobID()
	}
	pr.Result = r.call.wire(fmt.Sprintf("did not stop within %s of the call's deadline passing; whatever it answers later is discarded (job %s: its own late row, if it ends, is under that id)",
		r.call.grace.Round(time.Millisecond), pr.JobID))
	r.record(contract, pr)
	return pr
}

// queueLookMax bounds the last look cutQueued takes at the holder. It runs after the
// deadline, outside the unwind the fan-out has, so its wall lands directly on the call's:
// the lesser of this and three quarters of the unwind allowance, like the withdraw.
const queueLookMax = 2 * time.Second

// cutQueued applies the deadline to route=queue's outcome. The queue lane polls its
// subtasks one after another and reports a poll the context cancelled as a failure —
// including every job it never got round to asking about, which returns "canceled:"
// without a request. A claimant may well have finished such a job while an earlier, slower
// one was being polled, so the deadline takes ONE last look at each of them (lookAtHolder)
// before it decides anything, and publishes what the holder said:
//
//   - a job the holder has finished (or failed) is returned as that answer, exactly as the
//     poll would have returned it — the finished result is not lost behind a slower one;
//   - a job the holder still holds is unfinished work: a call-deadline defer whose reason
//     says what the holder said (queued, or claimed and running), never a state the code
//     did not observe;
//   - a job the holder could not be asked about says so ("could not be looked up").
//
// The unfinished count spans only the jobs that really are unfinished.
func (c *callDeadline) cutQueued(cfg config.Config, subtasks []core.AgentContract, results []PlacedResult, sum Summary, err error) ([]PlacedResult, Summary, error) {
	if !c.reached() || err != nil {
		return results, sum, err
	}
	var cand []int
	for i, pr := range results {
		if queueCut(pr) {
			cand = append(cand, i)
		}
	}
	if len(cand) == 0 {
		return results, sum, err
	}
	c.lookAtHolder(cfg, subtasks, results, cand)
	var cut []int
	for _, i := range cand {
		if queueCut(results[i]) || results[i].queueSeen != "" {
			cut = append(cut, i)
		}
	}
	if len(cut) > 0 {
		c.frozen.CompareAndSwap(-1, int64(len(cut)))
	}
	for _, i := range cut {
		pr := &results[i]
		var where string
		switch pr.queueSeen {
		case "accepted":
			where = fmt.Sprintf("was still queued on the holder (job %s) when the call's deadline passed; the job stays on the holder and this call is no longer waiting for it", pr.JobID)
		case "running":
			where = fmt.Sprintf("was claimed by a node and still running (job %s) when the call's deadline passed; the job stays on the holder and this call is no longer waiting for it", pr.JobID)
		case "absent":
			where = "had not been submitted to the holder when the call's deadline passed"
			pr.Unplaced = true
		default:
			where = fmt.Sprintf("could not be looked up on the holder when the call's deadline passed (job %s); if it was submitted it stays there, and this call is no longer waiting for it", pr.JobID)
		}
		pr.Result = c.wire(where)
		pr.Err = ""
		pr.deadlineCut = true
	}
	return results, queueSummary(results), nil
}

// queueCut reports whether a queue result is one the call deadline's cancellation ended
// without an answer: a poll it cancelled, or a submit it interrupted. A transport error
// names the context's CAUSE ("call deadline reached"), a bare cancellation its Err
// ("context deadline exceeded"): both count.
func queueCut(pr PlacedResult) bool {
	if pr.deadlineCut {
		return false
	}
	return strings.HasPrefix(pr.Err, "canceled:") ||
		(strings.HasPrefix(pr.Err, "queue submit:") &&
			(strings.Contains(pr.Err, callDeadlinePrefix) || strings.Contains(pr.Err, context.DeadlineExceeded.Error())))
}

// lookAtHolder asks the queue holder, once per job and all at once, what became of each
// job the deadline cancelled the poll of. It runs on a context of its own (the call's is
// cancelled) bounded by queueLookMax and the unwind allowance, so a holder that has stopped
// answering costs the call at most that. It moves each answer onto the result: a finished
// or failed job becomes that outcome; a job still held records what the holder said in
// queueSeen; a job it could not ask about is "unknown". Each goroutine writes only its own
// element.
func (c *callDeadline) lookAtHolder(cfg config.Config, subtasks []core.AgentContract, results []PlacedResult, cand []int) {
	holder := strings.TrimRight(strings.TrimSpace(cfg.FleetQueueHolder), "/")
	budget := queueLookMax
	if g := c.grace * 3 / 4; g > 0 && g < budget {
		budget = g
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var wg sync.WaitGroup
	for _, i := range cand {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pr := &results[i]
			if pr.JobID == "" {
				pr.queueSeen = "unknown"
				return
			}
			submitCut := strings.HasPrefix(pr.Err, "queue submit:")
			p, perr := pollJobOnceAt(ctx, cfg, holder+"/fleet/queue/jobs/"+pr.JobID)
			switch {
			case perr != nil:
				pr.queueSeen = "unknown"
			case p.Status == http.StatusUnauthorized:
				pr.Err = "queue poll: 401 unauthorized (fleet_auth_token mismatch)"
			case p.Status == http.StatusNotFound && submitCut:
				pr.queueSeen = "absent"
			case p.Status == http.StatusNotFound:
				pr.Err = "holder denies the job (submitted then vanished — holder store reset?)"
			case p.Status == http.StatusOK && (p.State == "done" || p.State == "error"):
				// The holder's answer supersedes the failure the cancelled poll (or the
				// interrupted submit) left behind: a finished job is its result, a failed
				// one is its own failure.
				pr.Err = ""
				queueAnswer(subtasks[i], p, pr)
			case p.Status == http.StatusOK && (p.State == "accepted" || p.State == "running"):
				pr.queueSeen = p.State
			default:
				pr.queueSeen = "unknown"
			}
		}(i)
	}
	wg.Wait()
}
