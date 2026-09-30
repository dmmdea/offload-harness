// progress.go reports a running call's state changes to whoever asked
// (RunOptions.OnProgress). The MCP doors use it to send progress notifications: a
// delegation is silent for minutes, and a client that resets its own timeout on
// progress then keeps waiting instead of aborting the call (ADR 0065).
//
// It reports FACTS about subtasks — one started, one finished and how — and never
// decides anything: no placement, retry or deadline reads it, and a caller that
// sets no callback pays for one nil check.

package delegate

import "sync/atomic"

// ProgressEvent is one state change of a running call.
type ProgressEvent struct {
	// Kind is "started" (a subtask began) or "finished" (one ended, any way).
	Kind string
	// Index is the subtask's position in the WHOLE call (0-based), across every
	// chunk of a batched call.
	Index int
	// Done is how many subtasks of the whole call have ended so far, this one
	// included when Kind is "finished".
	Done int
	// Total is how many subtasks the whole call owes an answer for.
	Total int
	// Node names where a finished subtask ran ("" when it ran nowhere or the
	// node is unknown). Outcome is set only on "finished": succeeded, failed,
	// failed verification, or deferred (with the defer class).
	Node    string
	Outcome string
}

// progressor turns a chunk's goroutine transitions into ProgressEvents. nil = no
// callback; every method is nil-safe.
type progressor struct {
	fn    func(ProgressEvent)
	total int
	done  atomic.Int64
}

// newProgressor returns nil when the caller asked for no progress.
func newProgressor(opts *RunOptions, total int) *progressor {
	if opts == nil || opts.OnProgress == nil {
		return nil
	}
	return &progressor{fn: opts.OnProgress, total: total}
}

func (p *progressor) started(i int) {
	if p == nil {
		return
	}
	p.fn(ProgressEvent{Kind: "started", Index: i, Done: int(p.done.Load()), Total: p.total})
}

func (p *progressor) finished(i int, pr PlacedResult) {
	if p == nil {
		return
	}
	p.fn(ProgressEvent{Kind: "finished", Index: i, Done: int(p.done.Add(1)), Total: p.total, Node: pr.Node, Outcome: outcomeWord(pr)})
}

// outcomeWord is the one-phrase verdict of a placed result, in the vocabulary the
// summary buckets use.
func outcomeWord(pr PlacedResult) string {
	switch {
	case pr.Err != "":
		return "failed"
	case pr.Result.Deferred:
		if pr.Result.DeferClass != "" {
			return "deferred (" + pr.Result.DeferClass + ")"
		}
		return "deferred"
	case len(pr.AcceptanceFailures) > 0:
		return "failed verification"
	}
	return "succeeded"
}

// shiftedProgress rebases a chunk's events onto the whole call: RunBatched runs
// its subtasks in consecutive chunks, and a client counting "n of 9" must not see
// each chunk start over at "n of 8".
func shiftedProgress(fn func(ProgressEvent), offset, total int) func(ProgressEvent) {
	return func(ev ProgressEvent) {
		ev.Index += offset
		ev.Done += offset
		ev.Total = total
		fn(ev)
	}
}
