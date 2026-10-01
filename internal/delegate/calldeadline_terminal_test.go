package delegate

// A node's terminal FAILURE, read as the call's deadline passes (ADR 0065, decision 3).
//
// The call's context ends a moment after the clock passes the deadline (a timer trails the clock),
// so for that moment a poll can still be answered and read. When the answer is the node's last word
// on the job and it is not an answer (the job ended in error, its result cannot be decoded, or the
// node has lost the job), the outcome is produced after the deadline and is the deadline's. What it
// describes is a job that is OVER, though, and the cut used to word it like a cancelled poll: "was
// still on <node>, not taken back, no longer waiting for it", with the intent left open for the
// recovery pass, which would then file the node's error as a recovered orphan: an outcome this call
// had already published.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// terminalShape is one way the poll loop ends on the node's last word on a job that is not an answer.
type terminalShape struct {
	name string
	// poll is what the node answers every poll with.
	poll func(int64) (map[string]any, int)
	// err is the opening of the failure the poll loop publishes for it.
	err string
}

func terminalShapes() []terminalShape {
	return []terminalShape{
		{"the job ended in error", func(int64) (map[string]any, int) {
			return map[string]any{"state": "error", "error": "engine exited"}, http.StatusOK
		}, "remote job error: engine exited"},
		{"the result cannot be decoded", func(int64) (map[string]any, int) {
			return map[string]any{"state": "done", "data": "not a result"}, http.StatusOK
		}, "job done but data is not an AgentWireResult"},
		{"the node lost the job", func(int64) (map[string]any, int) {
			return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
		}, "node lost job agd-term"},
	}
}

// pollNode is a fleet node that acks the job and answers every poll with poll.
func pollNode(t *testing.T, poll func(int64) (map[string]any, int), tune func(*fakeNode)) (*fakeNode, string) {
	t.Helper()
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-a", pollState: poll}
	if tune != nil {
		tune(f)
	}
	return f, f.server().URL
}

// runTerminal runs the real poll loop (runRemote) against the node and completes the result the way
// attempt does, so the fixtures a cut is tested with are what the engine produces and cannot drift.
func runTerminal(t *testing.T, ctx context.Context, url string) (*runner, PlacedResult) {
	t.Helper()
	cfg := testCfg(t)
	r := &runner{cfg: cfg, intent: openIntentLedger(cfg)}
	pr := r.runRemote(ctx, url, "agd-term", remoteContract(), NodeView{NodeID: "node-a"}, "")
	pr.ranBase, pr.Node, pr.JobID = url, "node-a", "agd-term"
	return r, pr
}

// TestARemoteRunMarksANodesLastWordAsTerminalAndNothingElse: the poll loop says which failures are the
// node's last word on a job. A cut cannot infer it from the error alone: a cancelled poll and a refused
// re-dispatch are failures too, and the node may still hold the job in both.
func TestARemoteRunMarksANodesLastWordAsTerminalAndNothingElse(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	for _, s := range terminalShapes() {
		_, url := pollNode(t, s.poll, nil)
		_, pr := runTerminal(t, t.Context(), url)
		if !strings.HasPrefix(pr.Err, s.err) || !pr.intentRecorded || pr.orphanable {
			t.Fatalf("%s: err %q recorded %v orphanable %v, want %q on an acked job that is not orphanable (fixture)", s.name, pr.Err, pr.intentRecorded, pr.orphanable, s.err)
		}
		if !pr.nodeTerminal {
			t.Errorf("%s: the failure is the node's last word on the job and was not marked as such", s.name)
		}
	}

	// A re-dispatch the node refuses: it denied holding the job, and whether the second POST landed is
	// not known. That is not the node's last word on the job.
	_, url := pollNode(t, func(int64) (map[string]any, int) {
		return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
	}, func(f *fakeNode) {
		f.dispatchHook = func(n int64) int {
			if n >= 2 {
				return http.StatusServiceUnavailable
			}
			return 0
		}
	})
	_, refused := runTerminal(t, t.Context(), url)
	if refused.Err == "" || !refused.intentRecorded || strings.HasPrefix(refused.Err, "node lost job") {
		t.Fatalf("err %q recorded %v, want the failure of the second dispatch on an acked job (fixture)", refused.Err, refused.intentRecorded)
	}
	if refused.nodeTerminal {
		t.Errorf("a refused re-dispatch (%q) was marked as the node's last word: its POST may have landed", refused.Err)
	}

	// A cancelled poll is the give-up's: the node may still hold a job it was last seen running.
	_, url = pollNode(t, func(int64) (map[string]any, int) { return runningForever() }, nil)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(60*time.Millisecond, cancel)
	_, cancelled := runTerminal(t, ctx, url)
	if !strings.HasPrefix(cancelled.Err, "canceled:") || !cancelled.orphanable {
		t.Fatalf("err %q orphanable %v, want a cancelled poll the give-up left on its node (fixture)", cancelled.Err, cancelled.orphanable)
	}
	if cancelled.nodeTerminal {
		t.Errorf("a cancelled poll (%q) was marked as the node's last word", cancelled.Err)
	}
}

// TestTheCutOfANodesTerminalFailureDoesNotSayItIsStillThere: the unit under the run-level test below. A
// failure the node ended the job with, produced after the deadline, is cut as a job that ended and
// leaves nothing for the recovery pass; every other cut keeps its words and its open intent.
func TestTheCutOfANodesTerminalFailureDoesNotSayItIsStillThere(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	for _, s := range terminalShapes() {
		_, url := pollNode(t, s.poll, nil)
		r, pr := runTerminal(t, t.Context(), url)
		r.call = pastDeadline(1)
		cut := r.cutByDeadline(pr)

		if !cut.deadlineCut || cut.Err != "" || cut.orphanable || cut.Result.DeferClass != core.DeferClassBudget {
			t.Errorf("%s: cut %v err %q orphanable %v class %q, want the deadline's budget defer with nothing left for the recovery pass", s.name, cut.deadlineCut, cut.Err, cut.orphanable, cut.Result.DeferClass)
		}
		reason := cut.Result.Reason
		for _, want := range []string{deadlinePrefix + "1 unfinished", "had ended on node-a (job agd-term)", "the run itself failed: " + s.err} {
			if !strings.Contains(reason, want) {
				t.Errorf("%s: reason = %q, want it to carry %q", s.name, reason, want)
			}
		}
		for _, bad := range []string{"was still on", "not taken back", "no longer waiting for it"} {
			if strings.Contains(reason, bad) {
				t.Errorf("%s: reason = %q says the node still holds a job it had ended (%q)", s.name, reason, bad)
			}
		}
	}

	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}

	// What the cut must not move. A refused re-dispatch: the node denied holding the job and the second
	// POST may have landed, so it is still the node's as far as this call can tell.
	_, url := pollNode(t, func(int64) (map[string]any, int) {
		return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
	}, func(f *fakeNode) {
		f.dispatchHook = func(n int64) int {
			if n >= 2 {
				return http.StatusServiceUnavailable
			}
			return 0
		}
	})
	_, refused := runTerminal(t, t.Context(), url)
	got := r.cutByDeadline(refused)
	if !got.orphanable || !strings.Contains(got.Result.Reason, "was still on node-a (job agd-term)") || !strings.Contains(got.Result.Reason, "it was not taken back from the node") {
		t.Errorf("refused re-dispatch: orphanable %v reason %q, want it still on the node and left to the recovery pass", got.orphanable, got.Result.Reason)
	}

	// A job the node says it never ran keeps ADR 0064's words: its intent closes as never started.
	_, url = pollNode(t, func(int64) (map[string]any, int) {
		return map[string]any{"state": "error", "error": "reaped: nobody polled it within the poll lease"}, http.StatusOK
	}, nil)
	_, reaped := runTerminal(t, t.Context(), url)
	got = r.cutByDeadline(reaped)
	if got.orphanable || got.nodeNeverRan == "" || !strings.Contains(got.Result.Reason, "the node's own record says it never ran the job") {
		t.Errorf("never-ran terminal read: orphanable %v never-ran %q reason %q, want ADR 0064's words and an intent that closes as never started", got.orphanable, got.nodeNeverRan, got.Result.Reason)
	}
}

// TestANodesTerminalErrorReadAfterTheDeadlineClosesItsIntent: run level. The node answers `running` until
// the clock passes the call's deadline and `error` after it, and the poll's context is the caller's own
// (it does not end at the deadline: the window the context's timer leaves between the clock and the
// cancellation is what this stands for). The poll reads the error, finish cuts it, and the intent is
// closed as the terminal observation, with nothing open for the recovery pass to "recover".
func TestANodesTerminalErrorReadAfterTheDeadlineClosesItsIntent(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	at := time.Now().Add(time.Second)
	var posts atomic.Int64
	node, url := pollNode(t, func(int64) (map[string]any, int) {
		if time.Now().Before(at) {
			return runningForever()
		}
		return map[string]any{"state": "error", "error": "engine exited"}, http.StatusOK
	}, func(f *fakeNode) {
		f.onDispatch = func(string, core.AgentContract) { posts.Add(1) }
	})
	cfg := testCfg(t)
	call := &callDeadline{at: at, grace: time.Second, total: 1}
	call.frozen.Store(-1)
	r := &runner{cfg: cfg, intent: openIntentLedger(cfg), call: call, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	forced := &placement{view: NodeView{NodeID: "node-a", AgentSeat: "remote-seat"}, base: url, reason: "test placement"}

	pr := r.attempt(t.Context(), 0, remoteContract(), forced)

	if posts.Load() != 1 || node.polls.Load() < 2 {
		t.Fatalf("dispatches %d polls %d, want one dispatch and the poll loop running to the deadline (premise)", posts.Load(), node.polls.Load())
	}
	if !pr.deadlineCut || pr.Err != "" || pr.Result.DeferClass != core.DeferClassBudget {
		t.Fatalf("published = cut %v err %q class %q, want the deadline's budget defer", pr.deadlineCut, pr.Err, pr.Result.DeferClass)
	}
	if !strings.Contains(pr.Result.Reason, "had ended on node-a (job "+pr.JobID+")") || !strings.Contains(pr.Result.Reason, "the run itself failed: remote job error: engine exited") {
		t.Fatalf("reason = %q, want a job that had ended on the node, with what it ended with", pr.Result.Reason)
	}
	closed, open := intentNotes(t, cfg.StateDir)
	if pr.orphanable || closed[pr.JobID] != intentNoteTerminal || len(open) != 0 {
		t.Fatalf("orphanable %v, intent closed as %q (open=%v), want the node's terminal answer closed as %q with nothing left open for recovery", pr.orphanable, closed[pr.JobID], open, intentNoteTerminal)
	}
}
