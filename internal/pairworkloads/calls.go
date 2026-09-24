package pairworkloads

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Cards for long tool calls (0.140.5; queued-then-running 0.140.6).
//
// A tool call reaches PAIR through its ledger row, which is written when the
// call ENDS: a ten-minute render showed nothing in the Jobs list until it was
// over (2026-09-23, an animate_character run on the OptiPlex held its card at
// 100 % for minutes with no card at all). Begin opens a card when the call
// starts; the call's ledger row, or End when no row came, closes the SAME card.
//
// The card opens "queued" and turns "running" only when the lane marks the
// work started (core.MarkWorking — a media lane does it once it holds the
// GPU). 0.140.5 opened it "running": a transcription waiting minutes for
// whisper to load behind a 3-card seat read "Running on Qube" throughout
// (2026-09-23). A lane that cannot tell when its engine starts (transcribe:
// the wait is inside llama-swap) never marks, so its card stays queued until
// it ends, which is the honest reading.
//
// PAIR keys a card by (origin, engine, runId, id), so the closing frame must
// carry the engine the opening frame named. That is only knowable up front for
// tasks whose engine the task alone decides; a text or vision call learns its
// seat, and so llamacpp vs vllm, only as it runs, and it is short. Those keep
// the single terminal card.
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

// openCall is one card awaiting its terminal frame. started and closed are
// guarded by Emitter.callMu.
type openCall struct {
	jobID     string
	task      string
	engine    string
	requester string
	created   int64
	started   int64
	closed    bool
}

// Begin opens a queued card for a call of task admitted through door, and
// returns the two functions that move it on: working (the lane holds its
// engine: the card turns running, once) and end (the call returned: the card
// closes with its outcome, unless the call's ledger row already closed it).
// Both are nil when nothing was opened: emitter disabled, a task whose engine
// is not fixed by the task, or work this box serves for another (FleetDoor).
func (e *Emitter) Begin(task, door string) (working func(), end func(deferred bool, reason string)) {
	if !e.Enabled() || !longCallTasks[task] || door == FleetDoor {
		return nil, nil
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
	end = func(deferred bool, reason string) {
		started, ok := e.release(c)
		if !ok {
			return
		}
		state, errText := "completed", ""
		if deferred {
			state, errText = "failed", reason
			if errText == "" {
				errText = "deferred"
			}
		}
		e.Emit(Event{JobID: c.jobID, Model: task, Engine: c.engine, State: state, Error: errText,
			Requester: c.requester, CreatedAt: c.created, StartedAt: started,
			CompletedAt: e.now().UnixMilli()})
	}
	return working, end
}

// claim takes the oldest open card for task, for its ledger row to close, and
// reports when that card started (0 = never marked working). Concurrent calls
// of one task are matched first-in first-out; a row for a task with no open
// card (a batch's second image) gets its own card as before.
func (e *Emitter) claim(task string) (*openCall, int64) {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	q := e.calls[task]
	if len(q) == 0 {
		return nil, 0
	}
	c := q[0]
	c.closed = true
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
