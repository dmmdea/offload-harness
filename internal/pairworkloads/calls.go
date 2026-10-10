package pairworkloads

import (
	"fmt"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Cards for long tool calls (0.140.5; queued-then-running 0.140.6).
//
// A tool call reaches PAIR through its ledger row, which is written when the
// call ENDS: a ten-minute render showed nothing in the Jobs list until it was
// over (2026-09-23, an animate_character run on <node-e> held its card at
// 100 % for minutes with no card at all). Begin opens a card when the call
// starts; the call's ledger row, or End when no row came, closes the SAME card.
//
// The card opens "queued" and turns "running" only when the lane marks the
// work started (core.MarkWorking — a media lane does it once it holds the
// GPU). 0.140.5 opened it "running": a transcription waiting minutes for
// whisper to load behind a 3-card seat read "Running on <node-b>" throughout
// (2026-09-23). A lane that cannot tell when its engine starts (transcribe:
// the wait is inside llama-swap) never marks, so its card stays queued until
// it ends, which is the honest reading.
//
// PAIR keys a card by (origin, engine, runId, id), so the closing frame must
// carry the engine the opening frame named. That is only knowable up front for
// tasks whose engine the task alone decides; a text or vision call learns its
// seat, and so llamacpp vs vllm, only as it runs, and it is short. Those keep
// the single terminal card.
//
// The close is on the wire before the call returns (2026-10-09). A door answers the moment its
// call returns, and a client that opens a fresh stdio door per attempt closes stdin and kills the
// process right after the reply: a close left on a background goroutine died with the process, the
// card's queued marker was still in the register, and the orphan sweep closed the card "Failed:
// harness process exited before the job finished" for a call that had answered cleanly. So end posts
// its own close inline (EmitSync), and when the call's ledger row claimed the card instead (the
// row's frame is posted by the observer's goroutine) end waits for that frame to land.
var longCallTasks = map[string]bool{
	"generate_image":        true,
	"inpaint_image":         true,
	"edit_image_generative": true,
	"upscale_image":         true,
	"generate_video":        true,
	"animate_character":     true,
	"generate_audio":        true,
	"run_graph":             true,
	"compose_video":         true,
	"transcribe":            true,
}

// FleetDoor is the door a fleet node stamps on work it serves FOR another box
// (fleetnode.dispatchDoor, register A-102). The box that asked reports that
// work; the serving node reporting it too would show one job as two cards.
// Skipping it is what lets every box, not only the delegators, run with
// pair_workloads_enabled (0.140.6).
const FleetDoor = "fleet"

// openCall is one card awaiting its terminal frame. started, closed and sent are
// guarded by Emitter.callMu.
type openCall struct {
	jobID     string
	task      string
	engine    string
	requester string
	created   int64
	started   int64
	closed    bool
	// sent exists once the call's ledger row has claimed the card (claim) and is closed when the frame
	// that closes it has been posted or parked: end waits on it, so the call does not return, and its
	// door does not answer, before the card is closed on the wire. nil = no row claimed the card.
	sent chan struct{}
}

// callID is the card's own job id: the one id a ledger row needs to name to
// close exactly this card.
func (c *openCall) callID() string { return c.jobID }

// Begin opens a queued card for a call of task admitted through door, and
// returns the call id (the card's job id: the caller stamps it on the call's
// ledger row so that row closes THIS card) and the two functions that move the
// card on: working (the lane holds its engine: the card turns running, once)
// and end (the call returned with res: the card closes with its outcome, unless
// the call's ledger row already closed it, in which case end waits for that
// frame). end returns after the close is on the wire, so the caller may answer
// and be killed. The id is "" and both functions are nil when nothing was
// opened: emitter disabled, a task whose engine is not fixed by the task, or
// work this box serves for another (FleetDoor).
func (e *Emitter) Begin(task, door string) (callID string, working func(), end func(res core.Result)) {
	if !e.Enabled() || !longCallTasks[task] || door == FleetDoor {
		return "", nil, nil
	}
	now := e.now().UnixMilli()
	c := &openCall{
		jobID:     fmt.Sprintf("call-%d-%d", now, e.seq.Add(1)),
		task:      task,
		engine:    EngineFor(task, ""),
		requester: Requester(ledger.ProcessOrigin().Session),
		created:   now,
	}
	e.callMu.Lock()
	if e.calls == nil {
		e.calls = map[string][]*openCall{}
	}
	e.calls[task] = append(e.calls[task], c)
	e.callMu.Unlock()
	e.Emit(Event{JobID: c.jobID, Model: task, Engine: c.engine, State: "queued",
		Requester: c.requester, CreatedAt: c.created})
	working = func() {
		e.callMu.Lock()
		if c.closed || c.started != 0 {
			e.callMu.Unlock()
			return
		}
		c.started = e.now().UnixMilli()
		started := c.started
		e.callMu.Unlock()
		e.Emit(Event{JobID: c.jobID, Model: task, Engine: c.engine, State: "running",
			Requester: c.requester, CreatedAt: c.created, StartedAt: started})
	}
	end = func(res core.Result) {
		started, ok := e.release(c)
		if !ok {
			// The call's ledger row claimed the card and its frame is on the observer's goroutine:
			// the caller is about to answer, so wait until it has landed (or been parked).
			e.awaitRowClose(c)
			return
		}
		state, errText := CardOutcome(res, started != 0)
		e.EmitSync(Event{JobID: c.jobID, Model: task, Engine: c.engine, State: state, Error: errText,
			Requester: c.requester, CreatedAt: c.created, StartedAt: started,
			CompletedAt: e.now().UnixMilli()})
	}
	return c.callID(), working, end
}

// closeWait bounds end's wait for the frame its ledger row's goroutine posts: the post itself is
// bounded by sendTimeout, and planning it (a relay's health probe, a cold identity read) by a
// little more. A hung PAIR therefore costs a call at most this once, never its answer.
const closeWait = sendTimeout + 2*time.Second

// awaitRowClose waits until the frame that closes c on the strength of its ledger row has been
// posted or parked. It returns at once when no row claimed the card (end closed it itself, or an
// earlier end already waited).
func (e *Emitter) awaitRowClose(c *openCall) {
	e.callMu.Lock()
	sent := c.sent
	e.callMu.Unlock()
	if sent == nil {
		return
	}
	select {
	case <-sent:
	case <-time.After(closeWait):
	}
}

// claim takes the open card a ledger row closes, and reports when that card
// started (0 = never marked working).
//
// A row that names its call (callID, stamped from Begin's return) claims exactly
// that card. Matching the oldest open card instead let one call's row close
// another's under overlapping calls of the same task (the shorter call's row
// closed the older call's card, its own End then closed its own card empty, and
// the older row found no card at all: 3 cards for 2 calls). A named row whose
// card is not open (already closed, or another process's) claims nothing and
// gets its own card: it must never close someone else's.
//
// Only a row that carries no call id (a writer that does not stamp one) is
// matched first-in first-out. A row for a task with no open card (a batch's
// second image) gets its own card as before.
//
// The claimant owes the card's sent channel a close once its frame has been posted
// or parked; end waits on it (awaitRowClose).
func (e *Emitter) claim(task, callID string) (*openCall, int64) {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	q := e.calls[task]
	var c *openCall
	switch {
	case callID != "":
		for _, x := range q {
			if x.callID() == callID {
				c = x
				break
			}
		}
	case len(q) > 0:
		c = q[0]
	}
	if c == nil {
		return nil, 0
	}
	c.closed = true
	c.sent = make(chan struct{}) // the observer's goroutine closes it once the row's frame has landed
	e.dropLocked(c)
	return c, c.started
}

// release removes c if its ledger row has not closed it yet; ok = the caller
// must send the terminal frame, started = when the card turned running.
func (e *Emitter) release(c *openCall) (started int64, ok bool) {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	if c.closed {
		return 0, false
	}
	c.closed = true
	e.dropLocked(c)
	return c.started, true
}

func (e *Emitter) dropLocked(c *openCall) {
	q := e.calls[c.task]
	for i, x := range q {
		if x == c {
			e.calls[c.task] = append(q[:i:i], q[i+1:]...)
			break
		}
	}
	if len(e.calls[c.task]) == 0 {
		delete(e.calls, c.task)
	}
}

// closeWith turns a ledger row's terminal event into the frame that closes c:
// same id, engine and creation as the opening frame, the start the working
// mark recorded (none if the work never started), the row's model, outcome
// and completion time.
func closeWith(ev Event, c *openCall, started int64) Event {
	ev.JobID = c.jobID
	ev.Engine = c.engine
	ev.Node = ""
	ev.CreatedAt = c.created
	ev.StartedAt = started
	if ev.Requester == "" || ev.Requester == "offload-harness" {
		ev.Requester = c.requester
	}
	return ev
}

// CardOutcome is the terminal state and error text of the PAIR card of a call that ended with res;
// started says whether its lane ever held the engine (the card ran). Every writer of a call's closing
// frame maps its outcome through here (Begin's end, the ledger row, RemoteCall.Finish, the node's
// card), so a call is closed the same way whichever of them gets there first.
func CardOutcome(res core.Result, started bool) (state, errText string) {
	return cardOutcome(res.Deferred || !res.OK, res.Meta.ErrClass, res.Reason, started)
}

// cardOutcome folds how a call ended into the card's terminal state: failed says it did not succeed
// (deferred, or not OK), errClass is its err_class, reason its defer reason, started whether its lane
// ever held the engine.
//
// A call HELD BACK by another job's hold on the card it needed (core.CardHeld: gpu_queued, which
// leaves a place in line, and gpu_busy) did not fail. Nothing ran, the lease queue kept its place or
// the caller was told to come back, and "a busy card is a place in line" is the design working
// (register C-89, plan P13). But PAIR's lifecycle has two terminal states, `completed` and `failed`
// (workload:completed / workload:errored; its workload manager defines no cancelled or skipped one),
// its desktop paints the second red and prints `error` only on it, and `workloads:remove`, the one
// other thing a producer can say, is outside the lifecycle the card relay carries (ParseRelay refuses
// it). So such a call closes COMPLETED with its start left null (never started) and the reason in
// `error`: the Jobs list shows a quiet card instead of a red one, and PAIR's history keeps why it did
// not run. Anything else that did not succeed, and a held call whose lane had already started,
// closes failed with the reason ("deferred" when there is none).
func cardOutcome(failed bool, errClass, reason string, started bool) (state, errText string) {
	if !failed {
		return "completed", ""
	}
	errText = reason
	if errText == "" {
		errText = "deferred"
	}
	if core.CardHeld(errClass) && !started {
		return "completed", errText
	}
	return "failed", errText
}
