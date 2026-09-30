package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// This file pins the delegator half of ADR 0064: where the delegator gives an
// acked job up it asks the node to take an unstarted job back (DELETE
// /fleet/jobs/{id}, 5 s, on a context that outlives the caller's), and only a
// node's CONFIRMATION lets the result be re-placed and closes the intent as
// withdrawn. A node that answers anything else — an old node's 404/405, a 401, a
// 500, a dropped connection — leaves today's behaviour exactly as it was.

// withdrawProbe fronts a fakeNode's server and answers DELETE /fleet/jobs/{id}
// itself, recording what the delegator sent. answer == nil behaves as a node
// without the route: the request falls through to the fakeNode's mux, which
// answers 405 because the pattern is GET-only — the real compatibility signal.
type withdrawProbe struct {
	deletes atomic.Int64
	mu      sync.Mutex
	ids     []string
	auth    []string
	// answer scripts the reply for the nth DELETE; status -1 drops the connection.
	answer func(n int64, jobID string) (int, map[string]any)
	// hold, when non-zero, makes the node sit on the request before answering.
	hold time.Duration
}

func (p *withdrawProbe) front(t *testing.T, inner *httptest.Server) *httptest.Server {
	t.Helper()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasPrefix(r.URL.Path, "/fleet/jobs/") {
			inner.Config.Handler.ServeHTTP(w, r)
			return
		}
		n := p.deletes.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/fleet/jobs/")
		p.mu.Lock()
		p.ids = append(p.ids, id)
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.mu.Unlock()
		if p.hold > 0 {
			select {
			case <-time.After(p.hold):
			case <-r.Context().Done():
				return
			}
		}
		if p.answer == nil {
			inner.Config.Handler.ServeHTTP(w, r) // an old node: 405
			return
		}
		status, body := p.answer(n, id)
		if status == -1 {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(outer.Close)
	return outer
}

func (p *withdrawProbe) sent() (ids, auth []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...), append([]string(nil), p.auth...)
}

// confirmsWithdrawal is the answer a node gives for a job it took back.
func confirmsWithdrawal(_ int64, id string) (int, map[string]any) {
	return http.StatusOK, map[string]any{"job_id": id, "state": "withdrawn", "withdrawn": true}
}

// stuckNode is a fleet node that accepts every job and never starts it.
func stuckNode(t *testing.T, id string) *fakeNode {
	t.Helper()
	return &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id,
		pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "accepted"}, http.StatusOK },
	}
}

// intentNotes reads the delegator's intent ledger for a test's config: the close
// note per job (absent = still open) and the set of open jobs.
func intentNotes(t *testing.T, stateDir string) (closed map[string]string, open map[string]bool) {
	t.Helper()
	root, err := gpulease.ResolveStateRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "delegate-intent.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	closed = map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev intentEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.E == "ok" {
			closed[ev.Job] = ev.Note
		}
	}
	openMap, _, err := readOpenIntents(path)
	if err != nil {
		t.Fatal(err)
	}
	open = map[string]bool{}
	for id := range openMap {
		open[id] = true
	}
	return closed, open
}

const withdrawToken = "sekrit"

// withdrawContract is remoteContract() with its acceptance cut to what these tests
// need — a non-empty answer — so a node's canned output can be any text.
func withdrawContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{"nonempty:answer"}
	return c
}

// queueBudgetUnit is the real time one second of a contract's budget takes in the
// queue-deadline tests: a 30 s contract polls for 30 x this.
const queueBudgetUnit = 40 * time.Millisecond

// compressQueueBudget compresses the clocks for a test whose subject is the QUEUE
// deadline: a 30 s contract polling a node that keeps its job `accepted`.
//
// The queue arm fires when the backlog credit banked between consecutive
// `accepted` polls reaches the poll budget; the poll-deadline arm fires at anchor +
// budget + credit. Time the loop cannot credit (the first poll's round trip, and any
// stall between one observation and the next deadline check) counts against the
// budget alone, so the two arms are separated by ONE budget of slack. A stall longer
// than that (a first poll that answers late, a timer delivered late on a loaded
// box) hands the run to the poll-deadline arm and the test to a failure that has
// nothing to do with what it pins: it took 1 run in 84, under twelve parallel
// copies of the test binary, with a 300 ms budget. The unit is sized for a slack of
// over a second; TestQueueDeadlineSurvivesAStalledFirstPoll pins that margin.
func compressQueueBudget(t *testing.T) {
	t.Helper()
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, queueBudgetUnit)
}

// TestQueueDeadlineSurvivesAStalledFirstPoll: a node whose first answer takes 400 ms
// (a scheduling stall, an antivirus scan, a loaded box) must still end as the queue
// deadline it is, not turn into a poll deadline because the stall was longer than a
// compressed budget. The stall is beyond a 300 ms budget and inside the shared one.
func TestQueueDeadlineSurvivesAStalledFirstPoll(t *testing.T) {
	compressQueueBudget(t)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-slow",
		pollState: func(n int64) (map[string]any, int) {
			if n == 1 {
				time.Sleep(400 * time.Millisecond)
			}
			return map[string]any{"state": "accepted"}, http.StatusOK
		},
	}
	url := (&withdrawProbe{}).front(t, node.server()).URL // no DELETE route: an old node, so the deadline stays a plain failure
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("err = %q (deferred %v: %s), want the queue deadline: a slow first poll is not a poll deadline", results[0].Err, results[0].Result.Deferred, results[0].Result.Reason)
	}
}

// TestRunQueueDeadlineWithdrawsAndReplaces is the fix for the ghost jobs: a node
// accepts a job and never starts it; at the queue deadline the delegator takes it
// back, the node CONFIRMS, and only then is the subtask offered to another node —
// with the intent closed as withdrawn, so nothing is left behind to run later.
func TestRunQueueDeadlineWithdrawsAndReplaces(t *testing.T) {
	compressQueueBudget(t) // a 30 s contract polls for 1.2 s + the grace

	stuck := stuckNode(t, "node-stuck")
	stuck.token = withdrawToken
	probe := &withdrawProbe{answer: confirmsWithdrawal}
	stuckURL := probe.front(t, stuck.server()).URL
	idle, idleURL := acceptingNode(t, "node-idle", "answer from the idle node", func(f *fakeNode) { f.token = withdrawToken })

	cfg := testCfg(t)
	cfg.FleetAuthToken = withdrawToken
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{stuckURL, idleURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0]
	if r.Err != "" {
		t.Fatalf("err = %q: a job the node confirmed it took back must be re-placed, not reported as a queue-deadline failure", r.Err)
	}
	if r.Node != "node-idle" {
		t.Fatalf("node = %q, want the work re-placed on node-idle", r.Node)
	}
	if r.Replacements != 1 || !strings.Contains(r.ReplacementNote, "node-stuck") || !strings.Contains(r.ReplacementNote, "queue deadline") {
		t.Fatalf("replacements = %d, note = %q, want one, naming node-stuck's queue deadline", r.Replacements, r.ReplacementNote)
	}

	stuckJob, _ := stuck.lastJobID.Load().(string)
	ids, auth := probe.sent()
	if len(ids) != 1 || ids[0] != stuckJob {
		t.Fatalf("withdraws sent = %v, want exactly one, for the job dispatched to node-stuck (%q)", ids, stuckJob)
	}
	if auth[0] != "Bearer "+withdrawToken {
		t.Fatalf("withdraw Authorization = %q, want the fleet bearer token", auth[0])
	}
	if got := stuck.dispatches.Load(); got != 1 {
		t.Fatalf("node-stuck saw %d dispatches, want 1", got)
	}
	if got := idle.dispatches.Load(); got != 1 {
		t.Fatalf("node-idle saw %d dispatches, want 1", got)
	}

	closed, open := intentNotes(t, cfg.StateDir)
	if closed[stuckJob] != intentNoteWithdrawn {
		t.Fatalf("intent for the withdrawn job closed as %q, want %q (open=%v)", closed[stuckJob], intentNoteWithdrawn, open)
	}
	if len(open) != 0 {
		t.Fatalf("intents still open after the run: %v — a confirmed withdrawal leaves nothing for recovery", open)
	}
}

// TestRunQueueDeadlineOnANodeWithoutWithdrawKeepsTodaysBehaviour: every answer
// that is not a confirmation — an old node's 405, a 404, a 401, a 500, a dropped
// connection — leaves the queue deadline exactly as it was: a failure naming the
// deadline, NOT re-placed (the node may still run the job), the intent left OPEN
// for the recovery pass.
func TestRunQueueDeadlineOnANodeWithoutWithdrawKeepsTodaysBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(n int64, id string) (int, map[string]any)
	}{
		{"an old node (405 from its GET-only route)", nil},
		{"404", func(int64, string) (int, map[string]any) {
			return http.StatusNotFound, map[string]any{"error": "unknown job"}
		}},
		{"401", func(int64, string) (int, map[string]any) {
			return http.StatusUnauthorized, map[string]any{"error": "unauthorized"}
		}},
		{"500", func(int64, string) (int, map[string]any) {
			return http.StatusInternalServerError, map[string]any{"error": "boom"}
		}},
		{"200 without a withdrawn verdict", func(int64, string) (int, map[string]any) { return http.StatusOK, map[string]any{"state": "accepted"} }},
		{"a dropped connection", func(int64, string) (int, map[string]any) { return -1, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressQueueBudget(t)
			stuck := stuckNode(t, "node-stuck")
			probe := &withdrawProbe{answer: tc.answer}
			stuckURL := probe.front(t, stuck.server()).URL
			idle, idleURL := acceptingNode(t, "node-idle", "must never be asked", nil)

			cfg := testCfg(t)
			contract := withdrawContract()
			contract.TimeoutSec = 30
			results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{stuckURL, idleURL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			r := results[0]
			if !strings.HasPrefix(r.Err, "queue deadline") {
				t.Fatalf("err = %q, want today's %q failure — an unconfirmed withdrawal proves nothing about the node", r.Err, "queue deadline")
			}
			if strings.Contains(r.Err, "withdrawn") {
				t.Fatalf("err = %q claims a withdrawal nobody confirmed", r.Err)
			}
			if idle.dispatches.Load() != 0 {
				t.Fatalf("node-idle was asked %d time(s): an unconfirmed give-up must never be re-placed (a double run)", idle.dispatches.Load())
			}
			if got := probe.deletes.Load(); got != 1 {
				t.Fatalf("withdraw attempts = %d, want exactly one best-effort try", got)
			}
			stuckJob, _ := stuck.lastJobID.Load().(string)
			if _, open := intentNotes(t, cfg.StateDir); !open[stuckJob] {
				t.Fatalf("the intent for %s was closed without a confirmed withdrawal: recovery could no longer find the job", stuckJob)
			}
		})
	}
}

// TestRunCancelWithdrawsAQueuedJob: a caller that walks away from a queued job
// takes it back on its way out — through a context the cancel did not touch, or
// the request would die before it left. The intent closes as withdrawn.
func TestRunCancelWithdrawsAQueuedJob(t *testing.T) {
	compressPolls(t, 2*time.Second, 20*time.Millisecond) // the delegator is asleep between polls when the cancel lands
	stuck := stuckNode(t, "node-stuck")
	probe := &withdrawProbe{answer: confirmsWithdrawal}
	stuckURL := probe.front(t, stuck.server()).URL

	cfg := testCfg(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for stuck.polls.Load() < 1 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
	}()
	results, _, err := Run(ctx, cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{stuckURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(results[0].Err, "canceled") {
		t.Fatalf("err = %q, want a canceled result", results[0].Err)
	}
	stuckJob, _ := stuck.lastJobID.Load().(string)
	if ids, _ := probe.sent(); len(ids) != 1 || ids[0] != stuckJob {
		t.Fatalf("withdraws sent = %v, want one for %s — the cancel must reach the node on a context of its own", ids, stuckJob)
	}
	closed, open := intentNotes(t, cfg.StateDir)
	if closed[stuckJob] != intentNoteWithdrawn || len(open) != 0 {
		t.Fatalf("intent closed as %q (open=%v), want %q and nothing left open", closed[stuckJob], open, intentNoteWithdrawn)
	}
}

// TestRunCancelWithdrawsOnEveryCancelExit: the cancel is observed at two places
// in the poll loop — while it sleeps between polls and at the top of the next
// pass — and a 1 µs cadence lands it on both. Every run must withdraw and settle
// the intent, whichever exit it took; a cancel exit that forgot to would leave a
// ghost only some cancels leave.
func TestRunCancelWithdrawsOnEveryCancelExit(t *testing.T) {
	compressPolls(t, time.Microsecond, 20*time.Millisecond)
	for i := 0; i < 24; i++ {
		stuck := stuckNode(t, "node-stuck")
		probe := &withdrawProbe{answer: confirmsWithdrawal}
		stuckURL := probe.front(t, stuck.server()).URL
		cfg := testCfg(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			for stuck.polls.Load() < 3 {
				time.Sleep(time.Millisecond)
			}
			cancel()
		}()
		results, _, err := Run(ctx, cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{stuckURL})
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: Run: %v", i, err)
		}
		if !strings.HasPrefix(results[0].Err, "canceled") {
			t.Fatalf("iteration %d: err = %q, want a canceled result", i, results[0].Err)
		}
		if got := probe.deletes.Load(); got != 1 {
			t.Fatalf("iteration %d: %d withdraws sent, want 1 — this cancel exit did not take the job back", i, got)
		}
		stuckJob, _ := stuck.lastJobID.Load().(string)
		if closed, open := intentNotes(t, cfg.StateDir); closed[stuckJob] != intentNoteWithdrawn || len(open) != 0 {
			t.Fatalf("iteration %d: intent closed as %q (open=%v), want %q — this cancel exit left the intent as it found it (or closed it as observed)",
				i, closed[stuckJob], open, intentNoteWithdrawn)
		}
	}
}

// TestRunWithdrawAnswerRunningKeepsPolling: the job started between the last poll
// and the withdraw. The node says so (409) and the delegator keeps polling: the
// job is running now, not abandoned, and its result is still wanted.
func TestRunWithdrawAnswerRunningKeepsPolling(t *testing.T) {
	compressQueueBudget(t)

	var started atomic.Bool
	var runningPolls atomic.Int64
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-late-start",
		pollState: func(int64) (map[string]any, int) {
			switch {
			case !started.Load():
				return map[string]any{"state": "accepted"}, http.StatusOK
			case runningPolls.Add(1) <= 2:
				return map[string]any{"state": "running"}, http.StatusOK
			}
			return doneWire(t, remoteWire("the answer", `{"answer":"ok"}`)), http.StatusOK
		},
	}
	probe := &withdrawProbe{answer: func(_ int64, id string) (int, map[string]any) {
		started.Store(true) // a slot took the job just before the withdraw arrived
		return http.StatusConflict, map[string]any{"job_id": id, "state": "running", "withdrawn": false}
	}}
	url := probe.front(t, node.server()).URL

	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r := results[0]; r.Err != "" || r.Result.Deferred || r.Result.Output != "the answer" {
		t.Fatalf("result = err %q deferred %v output %q: a job that turned out to be running must be polled to its result, not abandoned", r.Err, r.Result.Deferred, r.Result.Output)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want one success", sum)
	}
	if got := probe.deletes.Load(); got != 1 {
		t.Fatalf("withdraw attempts = %d, want exactly 1 (a 'running' answer is final for that job)", got)
	}
	jobID, _ := node.lastJobID.Load().(string)
	closed, open := intentNotes(t, cfg.StateDir)
	if closed[jobID] != intentNoteTerminal || len(open) != 0 {
		t.Fatalf("intent closed as %q (open=%v), want %q: the delegator saw the job finish", closed[jobID], open, intentNoteTerminal)
	}
}

// TestRunPollDeadlineWithdrawsAJobThatNeverStarted: an owned job the node still
// holds as `accepted` when the poll deadline lands (its queue credit never built
// up, because the node kept interleaving 503s) is taken back like any other
// unstarted job — and one the delegator last saw RUNNING is not, since it has
// certainly started and the request could only be refused.
func TestRunPollDeadlineWithdrawsAJobThatNeverStarted(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 10*time.Millisecond)

	t.Run("accepted at the deadline", func(t *testing.T) {
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-flaky",
			pollState: func(n int64) (map[string]any, int) {
				if n%2 == 0 {
					return map[string]any{"error": "proxy hiccup"}, http.StatusServiceUnavailable
				}
				return map[string]any{"state": "accepted"}, http.StatusOK
			},
		}
		probe := &withdrawProbe{answer: confirmsWithdrawal}
		url := probe.front(t, node.server()).URL
		cfg := testCfg(t)
		contract := withdrawContract()
		contract.TimeoutSec = 3 // 30 ms of poll budget: the deadline lands before any queue credit banks
		results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		r := results[0]
		if !r.Result.Deferred || !strings.HasPrefix(r.Result.Reason, "poll deadline") {
			t.Fatalf("result = deferred %v reason %q, want the owned-job poll deadline defer", r.Result.Deferred, r.Result.Reason)
		}
		// The reason says what became of the job (docs/FLEET-NODE.md promises it): a
		// reader of the defer alone learns it never started and is not left running.
		if !strings.HasSuffix(r.Result.Reason, "; the job never started and was withdrawn from the node") {
			t.Fatalf("reason = %q, want it to end by saying the job never started and was withdrawn from the node", r.Result.Reason)
		}
		if probe.deletes.Load() != 1 {
			t.Fatalf("withdraw attempts = %d, want 1: the job was still accepted at the deadline", probe.deletes.Load())
		}
		jobID, _ := node.lastJobID.Load().(string)
		if closed, open := intentNotes(t, cfg.StateDir); closed[jobID] != intentNoteWithdrawn || len(open) != 0 {
			t.Fatalf("intent closed as %q (open=%v), want %q", closed[jobID], open, intentNoteWithdrawn)
		}
	})

	t.Run("running at the deadline", func(t *testing.T) {
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-busy",
			pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
		}
		probe := &withdrawProbe{answer: confirmsWithdrawal}
		url := probe.front(t, node.server()).URL
		cfg := testCfg(t)
		contract := withdrawContract()
		contract.TimeoutSec = 3
		results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !results[0].Result.Deferred || !strings.HasPrefix(results[0].Result.Reason, "poll deadline") {
			t.Fatalf("result = %+v, want the poll deadline defer", results[0].Result)
		}
		if strings.Contains(results[0].Result.Reason, "withdrawn") {
			t.Fatalf("reason = %q claims a withdrawal for a job that was running", results[0].Result.Reason)
		}
		if probe.deletes.Load() != 0 {
			t.Fatalf("withdraw attempts = %d for a job last seen RUNNING, want 0: it has started, so the request could only be refused", probe.deletes.Load())
		}
		jobID, _ := node.lastJobID.Load().(string)
		if _, open := intentNotes(t, cfg.StateDir); !open[jobID] {
			t.Fatalf("the running job's intent was closed: it is exactly the work recovery exists to collect")
		}
	})
}

// TestRunWithdrawIsBoundedAndBestEffort: a node that sits on the DELETE cannot
// hold the delegator up beyond the bound (5 s in production), and a withdrawal
// that timed out is unconfirmed — today's behaviour, the intent left open.
func TestRunWithdrawIsBoundedAndBestEffort(t *testing.T) {
	compressQueueBudget(t)
	old := withdrawTimeout
	withdrawTimeout = 150 * time.Millisecond
	t.Cleanup(func() { withdrawTimeout = old })

	stuck := stuckNode(t, "node-stuck")
	probe := &withdrawProbe{answer: confirmsWithdrawal, hold: 5 * time.Second}
	url := probe.front(t, stuck.server()).URL
	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 30

	began := time.Now()
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A healthy run is the queue budget (1.2 s) plus the 150 ms bound; one that
	// waited out the node's 5 s hold would take over 6 s.
	if elapsed := time.Since(began); elapsed > 4*time.Second {
		t.Fatalf("the run took %v: a hung node held the give-up past the withdraw bound", elapsed)
	}
	if !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("err = %q, want today's queue-deadline failure after an unconfirmed withdraw", results[0].Err)
	}
	stuckJob, _ := stuck.lastJobID.Load().(string)
	if _, open := intentNotes(t, cfg.StateDir); !open[stuckJob] {
		t.Fatal("the intent was closed although the node never confirmed the withdrawal")
	}
}

// TestRunWithdrawAnswerRunningButNodeStaysAcceptedGivesUpOnce: a node that says
// "running" to the withdraw and then keeps answering `accepted` is contradicting
// itself. The delegator asks ONCE — a `running` answer is final for the job — and
// then gives up exactly as it always did, with the intent left open: it must not
// send a DELETE on every poll until the deadline.
func TestRunWithdrawAnswerRunningButNodeStaysAcceptedGivesUpOnce(t *testing.T) {
	compressQueueBudget(t)
	stuck := stuckNode(t, "node-liar")
	probe := &withdrawProbe{answer: func(_ int64, id string) (int, map[string]any) {
		return http.StatusConflict, map[string]any{"job_id": id, "state": "running", "withdrawn": false}
	}}
	url := probe.front(t, stuck.server()).URL
	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 30
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("err = %q, want today's queue-deadline failure once the node's own answers contradict each other", results[0].Err)
	}
	if got := probe.deletes.Load(); got != 1 {
		t.Fatalf("withdraw attempts = %d, want exactly 1", got)
	}
	jobID, _ := stuck.lastJobID.Load().(string)
	if _, open := intentNotes(t, cfg.StateDir); !open[jobID] {
		t.Fatal("the intent was closed although the job may still run")
	}
}

// TestRunWithdrawnQueueWaitIsCreditedBackToTheReplacement: a job that waited out
// most of its contract's budget in a node's backlog and was then taken back has
// spent that time QUEUED, not working — the same rule as the credit inside one
// node (0.100.0). Without the credit the re-placement finds under the 10 s floor
// left and the withdrawal buys nothing: "re-placeable" would be inert exactly when
// a queue deadline is at its longest.
func TestRunWithdrawnQueueWaitIsCreditedBackToTheReplacement(t *testing.T) {
	compressPolls(t, 20*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 250*time.Millisecond) // a 12 s contract now queues for ~3 s of REAL time

	stuck := stuckNode(t, "node-stuck")
	probe := &withdrawProbe{answer: confirmsWithdrawal}
	stuckURL := probe.front(t, stuck.server()).URL
	idle, idleURL := acceptingNode(t, "node-idle", "answer from the idle node", nil)

	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 12 // budget 12 s: after ~3 s queued, 9 s remain uncredited — under the 10 s floor
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{stuckURL, idleURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0]
	if r.Err != "" || r.Node != "node-idle" {
		t.Fatalf("err = %q node = %q: the queue wait was charged to the contract's budget, leaving too little for the re-placement", r.Err, r.Node)
	}
	if idle.dispatches.Load() != 1 {
		t.Fatalf("node-idle saw %d dispatches, want 1", idle.dispatches.Load())
	}
}
