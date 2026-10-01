package delegate

// The withdraw of a job the whole-call deadline cut (ADR 0065, register C-67), through the
// delegator's ONE withdraw path (ADR 0064).
//
// Cancelling the delegator's poll leaves the job on its node, where it may start later on a
// seat nobody is waiting for (a ghost job). The deadline cancels the context runRemote polls
// under, and runRemote's cancel arms then give the job up like any other cancel: giveUp asks the
// node to take back a job that can still be unstarted (DELETE /fleet/jobs/{id} with the fleet
// bearer), on a context the cancel did not touch and inside the unwind allowance. It is a
// request, not a claim: a node that has not shipped the route answers 404 or 405, a job that has
// already started is not the delegator's to cancel, and what the call publishes depends on the
// answer only for the words that say what it was. The cut itself sends nothing: it used to make a
// request of its own, and a job last seen `accepted` was asked twice.
//
// Every scripted node here keeps its slow job `accepted`. A job last seen `running` has started
// and is never asked (TestARunningJobIsNotAskedToWithdrawAtTheCut).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// withdrawnBody is what a node that ships the route answers for a job it took back.
const withdrawnBody = `{"state":"withdrawn","withdrawn":true}`

// withdrawCall is one DELETE the fake node received.
type withdrawCall struct{ jobID, auth string }

// withdrawLog records the withdraws a node was asked for.
type withdrawLog struct {
	mu    sync.Mutex
	calls []withdrawCall
}

func (l *withdrawLog) snapshot() []withdrawCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]withdrawCall(nil), l.calls...)
}

// withWithdrawRoute fronts a scripted fleet node with DELETE /fleet/jobs/{id},
// which records what it is asked and answers status. Every other request reaches
// the fake node untouched, so the fake itself needs no new route.
func withWithdrawRoute(t *testing.T, inner *httptest.Server, status int) (string, *withdrawLog) {
	t.Helper()
	return withWithdrawAnswer(t, inner, status, "")
}

// withWithdrawAnswer is withWithdrawRoute with a body: the JSON a node that ships the route
// answers with (`{"state":"withdrawn","withdrawn":true}` for a job it took back).
func withWithdrawAnswer(t *testing.T, inner *httptest.Server, status int, body string) (string, *withdrawLog) {
	t.Helper()
	log := &withdrawLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.calls = append(log.calls, withdrawCall{jobID: r.PathValue("id"), auth: r.Header.Get("Authorization")})
		log.mu.Unlock()
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	})
	mux.Handle("/", inner.Config.Handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, log
}

// TestRunWithDeadlineAsksTheNodeToWithdrawAnOutstandingJob: the job still queued on the
// node at the deadline is the one withdrawn — ONCE (the give-up's request; the cut makes none
// of its own), with the fleet bearer — and the job that finished is not. The reason says the
// node took it back.
func TestRunWithDeadlineAsksTheNodeToWithdrawAnOutstandingJob(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	url, log := withWithdrawAnswer(t, inner, http.StatusOK, withdrawnBody)
	cfg := testCfg(t)
	cfg.FleetAuthToken = "sekrit"

	results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteGoal("fast one"), remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)

	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want the finished job and one call-deadline defer", sum)
	}
	calls := log.snapshot()
	if len(calls) != 1 {
		t.Fatalf("the node was asked to withdraw %d job(s) %+v, want exactly the one still outstanding, once", len(calls), calls)
	}
	if calls[0].jobID != results[1].JobID {
		t.Fatalf("withdrew %q, want the outstanding job %q (never the finished %q)", calls[0].jobID, results[1].JobID, results[0].JobID)
	}
	if calls[0].auth != "Bearer sekrit" {
		t.Fatalf("withdraw carried Authorization %q, want the fleet bearer", calls[0].auth)
	}
	if !strings.Contains(results[1].Result.Reason, "the node confirmed it took the job back") {
		t.Fatalf("reason = %q, want it to say the node took the job back", results[1].Result.Reason)
	}
}

// TestRunWithDeadlineWithdrawNeverChangesTheResult: what the node answers is
// irrelevant to what the call publishes. A node that has not shipped the route
// (404 / 405), one that refuses (409: the job already started) and one that errors
// (500) all leave the same result — the call-deadline defer with the job named — and the
// job stays the recovery pass's.
func TestRunWithDeadlineWithdrawNeverChangesTheResult(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			widenUnwind(t, 2*time.Second)
			compressPolls(t, 5*time.Millisecond, time.Second)
			_, inner := remoteQueuedForeverServer(t)
			url, log := withWithdrawRoute(t, inner, status)

			results, sum, _ := runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
				[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)

			if len(log.snapshot()) != 1 {
				t.Fatalf("withdraw requests = %d, want the one best-effort ask", len(log.snapshot()))
			}
			r := results[0]
			if sum != (Summary{Deferred: 1}) || r.Err != "" || !r.Result.Deferred || r.Result.DeferClass != core.DeferClassBudget ||
				!strings.HasPrefix(r.Result.Reason, deadlinePrefix+"1 unfinished") || !r.orphanable || r.withdrawn {
				t.Fatalf("summary %+v result %+v orphanable %v, want the ordinary call-deadline defer whatever the node said", sum, r, r.orphanable)
			}
		})
	}
}

// TestRunWithDeadlineWithdrawsNothingWhenNothingIsOutstanding: a call whose jobs
// all finish before the deadline sends no withdraw at all.
func TestRunWithDeadlineWithdrawsNothingWhenNothingIsOutstanding(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	url, log := withWithdrawRoute(t, inner, http.StatusOK)

	_, sum, _ := runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("fast one"), remoteGoal("fast two")}, "remote", []string{url}, deadlineIn(5*time.Second), nil)

	if sum != (Summary{Succeeded: 2}) {
		t.Fatalf("summary = %+v, want both jobs finished", sum)
	}
	if calls := log.snapshot(); len(calls) != 0 {
		t.Fatalf("%d withdraw request(s) %+v for a call that finished inside its deadline", len(calls), calls)
	}
}

// TestRunWithDeadlineWithdrawNeverHoldsTheCallPastTheUnwind: a node that never
// answers the withdraw (a blackholed box) must cost the call no more than the
// unwind allowance. The withdraw's own bound (withdrawBound) is shorter than that allowance
// once the deadline has passed, so the goroutine finishes inside it and the published result
// stays the TRUTHFUL cut (the node and the job), not an abandoned "did not stop" that names neither.
func TestRunWithDeadlineWithdrawNeverHoldsTheCallPastTheUnwind(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		select { // never answers: the delegator's own timeout closes the connection
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	mux.Handle("/", inner.Config.Handler)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)

	results, sum, elapsed := runWithin(t, 6*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{front.URL}, deadlineIn(300*time.Millisecond), nil)

	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s: a node that never answers the withdraw held the call", elapsed)
	}
	r := results[0]
	if sum != (Summary{Deferred: 1}) || strings.Contains(r.Result.Reason, "did not stop") ||
		r.JobID == "" || !strings.Contains(r.Result.Reason, r.JobID) || r.Node != "node-a" {
		t.Fatalf("summary %+v result %+v: want the truthful cut (the node and the job named), not an abandoned subtask", sum, r)
	}
}

// TestTheWithdrawBoundShrinksOnlyOnceTheCallDeadlineHasPassed: a give-up that happens because
// the call's deadline passed asks within three quarters of the unwind allowance, so a node that
// does not answer cannot turn the truthful cut into an abandoned subtask (ADR 0065). Every other
// give-up — a caller that cancels, a call with no deadline, a deadline still ahead — keeps the
// flat bound of ADR 0064.
func TestTheWithdrawBoundShrinksOnlyOnceTheCallDeadlineHasPassed(t *testing.T) {
	ahead := &callDeadline{at: time.Now().Add(time.Hour), grace: 4 * time.Second, total: 1}
	ahead.frozen.Store(-1)
	for _, tc := range []struct {
		name string
		call *callDeadline
		want time.Duration
	}{
		{"no deadline on the call", nil, withdrawTimeout},
		{"a deadline still ahead", ahead, withdrawTimeout},
		{"a deadline that has passed, a long unwind", &callDeadline{at: time.Now().Add(-time.Second), grace: 10 * time.Second, total: 1}, withdrawTimeout},
		{"a deadline that has passed, a short unwind", &callDeadline{at: time.Now().Add(-time.Second), grace: 400 * time.Millisecond, total: 1}, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{cfg: testCfg(t), call: tc.call}
			if got := r.withdrawBound(); got != tc.want {
				t.Fatalf("withdrawBound = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestTheCutSaysWhatTheNodeAnsweredToTheWithdraw: the withdraw is a request, and the published
// reason carries the node's answer in words — a node with no route, one that refused a started
// job and one that dropped the connection do not read alike, and a ghost job that survived the
// request leaves a trace on the row. On the result and in the corpus row.
func TestTheCutSaysWhatTheNodeAnsweredToTheWithdraw(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"an older node with no route", http.StatusMethodNotAllowed, "", "HTTP 405: the node has no withdraw route"},
		{"a node that does not hold the job", http.StatusNotFound, "", "HTTP 404"},
		{"a job that had already started", http.StatusConflict, "", "HTTP 409: the node said the job had already started"},
		{"a refused bearer", http.StatusUnauthorized, "", "HTTP 401"},
		{"a node that errors", http.StatusInternalServerError, "", "HTTP 500"},
		{"a 200 that does not say it took the job back", http.StatusOK, `{"state":"running"}`, "HTTP 200, but the answer did not say the job was taken back"},
		{"a job the node took back", http.StatusOK, withdrawnBody, "the node confirmed it took the job back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			widenUnwind(t, 2*time.Second)
			compressPolls(t, 5*time.Millisecond, time.Second)
			_, inner := remoteQueuedForeverServer(t)
			url, log := withWithdrawAnswer(t, inner, tc.status, tc.body)
			cfg := testCfg(t)
			results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
				[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)
			r := results[0].Result
			if sum != (Summary{Deferred: 1}) || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
				t.Fatalf("summary %+v reason %q, want the ordinary call-deadline defer", sum, r.Reason)
			}
			if calls := log.snapshot(); len(calls) != 1 || calls[0].jobID != results[0].JobID {
				t.Fatalf("the node was asked %+v, want exactly one request, for the cut job %q", calls, results[0].JobID)
			}
			if !strings.Contains(r.Reason, tc.want) {
				t.Fatalf("reason = %q, want it to carry the node's answer (%q)", r.Reason, tc.want)
			}
			if lines := corpusLines(t, cfg); len(lines) != 1 || lines[0].Result == nil || !strings.Contains(lines[0].Result.Reason, tc.want) {
				t.Fatalf("the corpus row does not carry the answer either: %+v", lines)
			}
		})
	}
}

// TestTheCutSaysWhenTheNodeNeverAnsweredTheWithdraw: no answer inside the bound is said as
// such — a blackholed node is not the same as one with no route.
func TestTheCutSaysWhenTheNodeNeverAnsweredTheWithdraw(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	mux.Handle("/", inner.Config.Handler)
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	results, _, _ := runWithin(t, 6*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{front.URL}, deadlineIn(300*time.Millisecond), nil)
	if r := results[0].Result.Reason; !strings.Contains(r, "no answer within") || strings.Contains(r, "did not stop") {
		t.Fatalf("reason = %q, want it to say the node did not answer within the bound (and still be the truthful cut, not an abandoned subtask)", r)
	}
}

// TestTheWithdrawSendsNoBearerWithoutAToken: a delegator with no fleet_auth_token sends no
// Authorization header at all — never an empty "Bearer " a node would read as a bad credential.
func TestTheWithdrawSendsNoBearerWithoutAToken(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	url, log := withWithdrawRoute(t, inner, http.StatusOK)
	runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)
	calls := log.snapshot()
	if len(calls) != 1 || calls[0].auth != "" {
		t.Fatalf("withdraw requests %+v, want exactly one with no Authorization header", calls)
	}
}

// TestACutJobTheNodeTookBackIsClosedAsWithdrawn: the node confirmed the withdraw, so the job
// will never run there and there is nothing for the recovery pass to collect. The cut must keep
// that outcome (withdrawn, not orphanable) so the attempt's close-out files the intent as
// withdrawn; a cut that marked the job orphanable unconditionally would leave the intent open
// and the recovery pass re-polling a job the node already dropped (ADR 0064 decisions 3 and 6).
func TestACutJobTheNodeTookBackIsClosedAsWithdrawn(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteQueuedForeverServer(t)
	url, _ := withWithdrawAnswer(t, inner, http.StatusOK, withdrawnBody)
	cfg := testCfg(t)

	results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)

	r := results[0]
	if sum != (Summary{Deferred: 1}) || !r.deadlineCut || !r.Result.Deferred {
		t.Fatalf("summary %+v result %+v, want the call-deadline defer", sum, r)
	}
	if !r.withdrawn || r.orphanable {
		t.Fatalf("withdrawn %v orphanable %v, want the node's confirmation kept (the job will never run there)", r.withdrawn, r.orphanable)
	}
	closed, open := intentNotes(t, cfg.StateDir)
	if closed[r.JobID] != intentNoteWithdrawn || len(open) != 0 {
		t.Fatalf("intent closed as %q (open=%v), want %q and nothing left open for recovery", closed[r.JobID], open, intentNoteWithdrawn)
	}
}

// TestARunningJobIsNotAskedToWithdrawAtTheCut: a job the node last reported `running` has
// started, and the request could only be refused, so the delegator does not make it (ADR
// 0064: the give-up's rule, which the cut no longer bypasses with a request of its own). The
// job stays the recovery pass's, and the reason does not claim a withdraw.
func TestARunningJobIsNotAskedToWithdrawAtTheCut(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
	url, log := withWithdrawAnswer(t, inner, http.StatusOK, withdrawnBody)
	cfg := testCfg(t)

	results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)

	if calls := log.snapshot(); len(calls) != 0 {
		t.Fatalf("%d withdraw request(s) %+v for a job last seen running", len(calls), calls)
	}
	r := results[0]
	if sum != (Summary{Deferred: 1}) || !r.orphanable || r.withdrawn {
		t.Fatalf("summary %+v orphanable %v withdrawn %v, want the ordinary cut, the job left for recovery", sum, r.orphanable, r.withdrawn)
	}
	if !strings.Contains(r.Result.Reason, "it was not taken back from the node") || strings.Contains(r.Result.Reason, "withdraw") {
		t.Fatalf("reason = %q, want it to say the job was not taken back and claim no withdraw", r.Result.Reason)
	}
	if _, open := intentNotes(t, cfg.StateDir); !open[r.JobID] {
		t.Fatal("the intent of a job that may still finish was closed")
	}
}

// TestAJobTheQueueDeadlineAskedAboutIsNotAskedAgainAtTheCut: the queue deadline asked the node
// to take the job back and the node said it had already started (409), so the delegator kept
// polling it, and the call's deadline passed while the node was still answering. The give-up
// the deadline then causes has last polled the job `accepted`, but the node's answer is newer,
// and one job is never asked twice (ADR 0065 decision 5; ADR 0064 decision 3). The node is
// asked once, the job stays the recovery pass's, and the published reason says what it answered.
func TestAJobTheQueueDeadlineAskedAboutIsNotAskedAgainAtTheCut(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressQueueBudget(t) // a 30 s contract queues for about 1.2 s before the queue deadline asks
	stuck := stuckNode(t, "node-stuck")
	opts := deadlineIn(3 * time.Second)
	probe := &withdrawProbe{answer: func(n int64, id string) (int, map[string]any) {
		if n == 1 {
			if time.Now().After(opts.Deadline) {
				t.Errorf("the first withdraw was sent after the call deadline, so it was a give-up's own and not the queue deadline's: this run never reached the state it pins")
			}
			time.Sleep(time.Until(opts.Deadline) + 20*time.Millisecond) // the call ends while the node is answering
		}
		return http.StatusConflict, map[string]any{"job_id": id, "state": "running", "withdrawn": false}
	}}
	url := probe.front(t, stuck.server()).URL
	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 30

	results, sum, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url}, opts, nil)

	if got := probe.deletes.Load(); got != 1 {
		t.Fatalf("withdraw attempts = %d, want exactly 1: the give-up at the call deadline asked again about a job the node had just said was running", got)
	}
	r := results[0]
	if sum != (Summary{Deferred: 1}) || !r.orphanable || r.withdrawn {
		t.Fatalf("summary %+v orphanable %v withdrawn %v, want the ordinary cut, the job left for recovery", sum, r.orphanable, r.withdrawn)
	}
	if !strings.HasPrefix(r.Result.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(r.Result.Reason, "it was not taken back from the node") || !strings.Contains(r.Result.Reason, "HTTP 409") {
		t.Fatalf("reason = %q, want the call-deadline defer saying the job was not taken back and what the node answered", r.Result.Reason)
	}
	if _, open := intentNotes(t, cfg.StateDir); !open[r.JobID] {
		t.Fatal("the intent of a job that is running was closed")
	}
}

// TestTheCutKeepsWhatTheNodeSaidAboutTheJob: the node's own word that a job will never run —
// a confirmed withdrawal, or its record of a job it never ran (reaped, withdrawn) — is what the
// give-up recorded by clearing orphanable, and the cut must not overwrite it. Any other outcome
// of a job the node acked stays the recovery pass's. The cut makes no request of its own.
func TestTheCutKeepsWhatTheNodeSaidAboutTheJob(t *testing.T) {
	var hits atomic.Int64
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(withdrawnBody))
	}))
	t.Cleanup(node.Close)
	r := &runner{cfg: testCfg(t), call: pastDeadline(1)}

	for _, tc := range []struct {
		name       string
		set        func(*PlacedResult)
		wantOrphan bool
		wantSays   string
	}{
		{"a confirmed withdrawal", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded"
			p.withdrawn = true
		}, false, "the node confirmed it took the job back"},
		{"the node's own record of a job it never ran", func(p *PlacedResult) {
			p.Err = "remote job error: reaped: nobody polled it within the lease"
			p.nodeNeverRan = "reaped: nobody polled it within the lease"
		}, false, "the node's own record says it never ran the job"},
		{"a withdraw the node did not confirm", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded; withdraw not confirmed: HTTP 405: the node has no withdraw route"
			p.orphanable = true
		}, true, "it was not taken back from the node"},
		{"a cancel no give-up has marked", func(p *PlacedResult) {
			p.Err = "canceled: context deadline exceeded"
		}, true, "it was not taken back from the node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := PlacedResult{intentRecorded: true, ranBase: node.URL, JobID: "agd-x", Node: "node-a"}
			tc.set(&pr)
			wasWithdrawn, wasNeverRan := pr.withdrawn, pr.nodeNeverRan
			got := r.cutByDeadline(pr)
			if !got.deadlineCut || got.Err != "" || !got.Result.Deferred {
				t.Fatalf("cut = %+v, want the call-deadline defer", got)
			}
			if got.orphanable != tc.wantOrphan || got.withdrawn != wasWithdrawn || got.nodeNeverRan != wasNeverRan {
				t.Fatalf("orphanable %v withdrawn %v nodeNeverRan %q, want orphanable %v and the node's word kept (withdrawn %v, never ran %q)",
					got.orphanable, got.withdrawn, got.nodeNeverRan, tc.wantOrphan, wasWithdrawn, wasNeverRan)
			}
			if !strings.Contains(got.Result.Reason, tc.wantSays) || !strings.Contains(got.Result.Reason, "agd-x") {
				t.Fatalf("reason = %q, want it to say %q and name the job", got.Result.Reason, tc.wantSays)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the cut made %d request(s) to the node: taking a job back is the give-up's, never the cut's (a second request for one job is a duplicate)", n)
	}
}
