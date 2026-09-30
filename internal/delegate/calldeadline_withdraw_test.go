package delegate

// The best-effort withdraw of a job the whole-call deadline cut (ADR 0065,
// register C-67).
//
// Cancelling the delegator's poll leaves the job on its node, where it may start
// later on a seat nobody is waiting for (a ghost job). At the deadline the
// delegator therefore ASKS the node to withdraw each job it is walking away from:
// DELETE /fleet/jobs/{id}, five seconds, detached from the cancelled context. It
// is a request, not a claim: a node that has not shipped the route answers 404 or
// 405, a job that has already started is not the delegator's to cancel, and
// nothing about the published result depends on the answer.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

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

// TestRunWithDeadlineAsksTheNodeToWithdrawAnOutstandingJob: the job still on the
// node at the deadline is the one withdrawn — once, with the fleet bearer — and the
// job that finished is not.
func TestRunWithDeadlineAsksTheNodeToWithdrawAnOutstandingJob(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
	url, log := withWithdrawRoute(t, inner, http.StatusOK)
	cfg := testCfg(t)
	cfg.FleetAuthToken = "sekrit"

	results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteGoal("fast one"), remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)

	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want the finished job and one call-deadline defer", sum)
	}
	calls := log.snapshot()
	if len(calls) != 1 {
		t.Fatalf("the node was asked to withdraw %d job(s) %+v, want exactly the one still outstanding", len(calls), calls)
	}
	if calls[0].jobID != results[1].JobID {
		t.Fatalf("withdrew %q, want the outstanding job %q (never the finished %q)", calls[0].jobID, results[1].JobID, results[0].JobID)
	}
	if calls[0].auth != "Bearer sekrit" {
		t.Fatalf("withdraw carried Authorization %q, want the fleet bearer", calls[0].auth)
	}
	if !strings.Contains(results[1].Result.Reason, "asked to withdraw") {
		t.Fatalf("reason = %q, want it to say the node was asked to withdraw the job", results[1].Result.Reason)
	}
}

// TestRunWithDeadlineWithdrawNeverChangesTheResult: what the node answers is
// irrelevant to what the call publishes. A node that has not shipped the route
// (404 / 405), one that refuses (409: the job already started) and one that errors
// (500) all leave the same result — the call-deadline defer with the job named.
func TestRunWithDeadlineWithdrawNeverChangesTheResult(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			_, inner := remoteRunningForeverServer(t)
			url, log := withWithdrawRoute(t, inner, status)

			results, sum, _ := runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
				[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)

			if len(log.snapshot()) != 1 {
				t.Fatalf("withdraw requests = %d, want the one best-effort ask", len(log.snapshot()))
			}
			r := results[0]
			if sum != (Summary{Deferred: 1}) || r.Err != "" || !r.Result.Deferred || r.Result.DeferClass != core.DeferClassBudget ||
				!strings.HasPrefix(r.Result.Reason, deadlinePrefix+"1 unfinished") || !r.orphanable {
				t.Fatalf("summary %+v result %+v orphanable %v, want the ordinary call-deadline defer whatever the node said", sum, r, r.orphanable)
			}
		})
	}
}

// TestRunWithDeadlineWithdrawsNothingWhenNothingIsOutstanding: a call whose jobs
// all finish before the deadline sends no withdraw at all.
func TestRunWithDeadlineWithdrawsNothingWhenNothingIsOutstanding(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
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
// unwind allowance. The withdraw's own timeout is shorter than that allowance, so
// the goroutine finishes inside it and the published result stays the TRUTHFUL cut
// (the node and the job), not an abandoned "did not stop" that names neither.
func TestRunWithDeadlineWithdrawNeverHoldsTheCallPastTheUnwind(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
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

// TestTheCutSaysWhatTheNodeAnsweredToTheWithdraw: the withdraw is a request, and its answer
// used to reach only a log line while the published reason said "the node was asked" whatever
// happened — a node with no route, one that refused a started job and one that dropped the
// connection all read alike, and a ghost job that survived the request left no trace on the
// row. The reason now carries the answer in words, on the result and in the corpus row.
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
		{"a job the node took back", http.StatusOK, `{"state":"withdrawn","withdrawn":true}`, "the node confirmed it took the job back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			widenUnwind(t, 2*time.Second)
			compressPolls(t, 5*time.Millisecond, time.Second)
			_, inner := remoteRunningForeverServer(t)
			url, _ := withWithdrawAnswer(t, inner, tc.status, tc.body)
			cfg := testCfg(t)
			results, sum, _ := runWithin(t, 4*time.Second, cfg, neverLocal(t),
				[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)
			r := results[0].Result
			if sum != (Summary{Deferred: 1}) || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") || !strings.Contains(r.Reason, "asked to withdraw") {
				t.Fatalf("summary %+v reason %q, want the ordinary call-deadline defer that says the node was asked", sum, r.Reason)
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
	_, inner := remoteRunningForeverServer(t)
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
	if r := results[0].Result.Reason; !strings.Contains(r, "no answer") || strings.Contains(r, "did not stop") {
		t.Fatalf("reason = %q, want it to say the node did not answer (and still be the truthful cut, not an abandoned subtask)", r)
	}
}

// TestTheWithdrawSendsNoBearerWithoutAToken: a delegator with no fleet_auth_token sends no
// Authorization header at all — never an empty "Bearer " a node would read as a bad credential.
func TestTheWithdrawSendsNoBearerWithoutAToken(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
	url, log := withWithdrawRoute(t, inner, http.StatusOK)
	runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{url}, deadlineIn(300*time.Millisecond), nil)
	calls := log.snapshot()
	if len(calls) != 1 || calls[0].auth != "" {
		t.Fatalf("withdraw requests %+v, want exactly one with no Authorization header", calls)
	}
}
