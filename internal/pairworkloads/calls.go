package pairworkloads

import (
	"fmt"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Running cards for long tool calls (0.140.5).
//
// A tool call reaches PAIR through its ledger row, which is written when the
// call ENDS: a ten-minute render showed nothing in the Jobs list until it was
// over (2026-09-23, an animate_character run on the OptiPlex held its card at
// 100 % for minutes with no card at all). Begin opens a running card when the
// call starts; the call's ledger row, or End when no row came, closes the SAME
// card.
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

// openCall is one running card awaiting its terminal frame.
type openCall struct {
	jobID     string
	task      string
	engine    string
	requester string
	created   int64
	closed    bool
}

// Begin opens a running card for task and returns the function that closes
// it. The returned func is nil when nothing was opened (emitter disabled, or a
// task whose engine is not fixed by the task). It is safe to call after the
// call's ledger row already closed the card: it then does nothing.
func (e *Emitter) Begin(task string) func(deferred bool, reason string) {
	if !e.Enabled() || !longCallTasks[task] {
		return nil
	}
	c := &openCall{
		jobID:     fmt.Sprintf("call-%d-%d", e.now().UnixMilli(), e.seq.Add(1)),
		task:      task,
		engine:    EngineFor(task, ""),
		requester: Requester(ledger.ProcessOrigin().Session),
		created:   e.now().UnixMilli(),
	}
	e.callMu.Lock()
	if e.calls == nil {
		e.calls = map[string][]*openCall{}
	}
	e.calls[task] = append(e.calls[task], c)
	e.callMu.Unlock()
	e.Emit(Event{JobID: c.jobID, Model: task, Engine: c.engine, State: "running",
		Requester: c.requester, CreatedAt: c.created, StartedAt: c.created})
	return func(deferred bool, reason string) {
		if !e.release(c) {
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
			Requester: c.requester, CreatedAt: c.created, StartedAt: c.created,
			CompletedAt: e.now().UnixMilli()})
	}
}

// claim takes the oldest open card for task, for its ledger row to close.
// Concurrent calls of one task are matched first-in first-out; a row for a
// task with no open card (a batch's second image) gets its own card as before.
func (e *Emitter) claim(task string) *openCall {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	q := e.calls[task]
	if len(q) == 0 {
		return nil
	}
	c := q[0]
	c.closed = true
	e.dropLocked(c)
	return c
}

// release removes c if its ledger row has not closed it yet; true = the
// caller must send the terminal frame.
func (e *Emitter) release(c *openCall) bool {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	e.dropLocked(c)
	return true
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
// same id, engine and start as the running frame; the row's model, outcome
// and completion time.
func closeWith(ev Event, c *openCall) Event {
	ev.JobID = c.jobID
	ev.Engine = c.engine
	ev.Node = ""
	ev.CreatedAt = c.created
	ev.StartedAt = c.created
	if ev.Requester == "" || ev.Requester == "offload-harness" {
		ev.Requester = c.requester
	}
	return ev
}
