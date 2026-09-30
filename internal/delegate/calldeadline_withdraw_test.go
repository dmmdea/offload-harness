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
	log := &withdrawLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		log.mu.Lock()
		log.calls = append(log.calls, withdrawCall{jobID: r.PathValue("id"), auth: r.Header.Get("Authorization")})
		log.mu.Unlock()
		w.WriteHeader(status)
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
