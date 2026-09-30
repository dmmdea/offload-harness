package delegate

// route=queue at the call deadline (ADR 0065, register C-67). The queue lane polls its
// jobs one after another, so a job that a claimant finished while the delegator was
// waiting on an earlier, slower one was never asked about: once the deadline cancelled the
// first poll every later job returned "canceled:" without a request, and the deadline
// published it as "still queued on the holder" — a fact the code never observed — and
// threw the finished answer away. This is the failure PR-6 exists to end (a finished
// result lost), on the one route the call deadline reached last.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
)

// claimAll claims every queued job (as a node's claim loop would) and returns them.
func claimAll(t *testing.T, q *fleetqueue.Queue, want int) []*fleetqueue.Job {
	t.Helper()
	var out []*fleetqueue.Job
	for len(out) < want {
		job, ok, err := q.Claim("claimant", []string{string(core.TaskAgentRun)})
		if err != nil {
			t.Errorf("claim: %v", err)
			return out
		}
		if !ok {
			return out
		}
		out = append(out, job)
	}
	return out
}

func goalOf(t *testing.T, job *fleetqueue.Job) string {
	t.Helper()
	var c core.AgentContract
	if err := json.Unmarshal(job.Payload, &c); err != nil {
		t.Errorf("payload: %v", err)
	}
	return c.Goal
}

// TestQueueRouteReturnsAJobTheHolderFinishedBehindASlowerOne: two jobs; a claimant takes
// both, holds the first for ever and finishes the second. The deadline cancels the poll of
// the first; the second must still come back as the answer the holder has for it, and the
// first must say what the holder says about IT (claimed and running), not that it is
// "still queued". The count is the truly unfinished one (1), not the number cut off from
// being asked (2).
func TestQueueRouteReturnsAJobTheHolderFinishedBehindASlowerOne(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	holder := queueHolder(t)
	cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
	go func() {
		time.Sleep(50 * time.Millisecond)
		for _, job := range claimAll(t, holder.q, 2) {
			if strings.Contains(goalOf(t, job), "two") {
				w, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "claimant", Seat: "seat-x", Output: "job two is done", StopReason: "done"})
				if err := holder.q.Ack(job.ID, "claimant", w, ""); err != nil {
					t.Errorf("ack: %v", err)
				}
			}
		}
	}()

	results, sum, _ := runWithin(t, 4*time.Second, cfg, nil,
		[]core.AgentContract{queueContract("which shipment one?"), queueContract("which shipment two?")}, "queue", nil, deadlineIn(1200*time.Millisecond), nil)

	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary = %+v, want the finished job returned and only the held one deferred", sum)
	}
	done, held := results[1], results[0]
	if done.Err != "" || done.Result.Deferred || done.Result.Output != "job two is done" || done.Node != "claimant" {
		t.Fatalf("the job the holder finished = %+v, want its answer returned", done)
	}
	r := held.Result
	if !r.Deferred || r.DeferClass != core.DeferClassBudget || !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") {
		t.Fatalf("the held job = %+v, want the call-deadline defer counting the one truly unfinished job", r)
	}
	if !strings.Contains(r.Reason, "claimed by a node") || strings.Contains(r.Reason, "still queued") || !strings.Contains(r.Reason, held.JobID) {
		t.Fatalf("reason = %q, want what the holder said (claimed and running) and the job named", r.Reason)
	}
}

// TestQueueRouteSaysItSawTheJobStillQueued: nobody claims either job, so the holder's own
// answer IS "queued": that is what the reason may say, and both count.
func TestQueueRouteSaysItSawTheJobStillQueued(t *testing.T) {
	widenUnwind(t, 2*time.Second)
	holder := queueHolder(t)
	cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, nil,
		[]core.AgentContract{queueContract("which shipment one?"), queueContract("which shipment two?")}, "queue", nil, deadlineIn(300*time.Millisecond), nil)
	if sum != (Summary{Deferred: 2}) {
		t.Fatalf("summary = %+v, want both deferred", sum)
	}
	for i, pr := range results {
		if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"2 unfinished") || !strings.Contains(pr.Result.Reason, "still queued on the holder") {
			t.Fatalf("result %d reason %q, want the count (2) and the holder's own word (queued)", i, pr.Result.Reason)
		}
	}
}

// frontedHolder puts a handler of the test's own in front of a real holder: routes the
// test names stall until the request's context ends, the rest reach the holder.
func frontedHolder(t *testing.T, stallSubmit, stallGet, getIs404 bool) string {
	t.Helper()
	holder := queueHolder(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stall := func() {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}
		switch {
		case r.Method == http.MethodPost && stallSubmit:
			stall()
		case r.Method == http.MethodGet && getIs404:
			http.Error(w, `{"error":"no such job"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && stallGet:
			stall()
		default:
			proxyTo(w, r, holder.url)
		}
	}))
	t.Cleanup(front.Close)
	return front.URL
}

// proxyTo forwards one request to the holder at base.
func proxyTo(w http.ResponseWriter, r *http.Request, base string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// TestQueueRouteWhoseHolderStopsAnsweringSaysItDidNotSee: the holder stops answering job
// lookups. The final look is bounded (it must not put the call past its deadline by more than
// the unwind allowance), and when it gets no answer the reason says the state could not be
// read — it does not assert "still queued" for a job it could not ask about.
func TestQueueRouteWhoseHolderStopsAnsweringSaysItDidNotSee(t *testing.T) {
	url := frontedHolder(t, false, true, false)
	cfg := config.Config{FleetQueueHolder: url, StateDir: t.TempDir()}
	results, sum, elapsed := runWithin(t, 6*time.Second, cfg, nil,
		[]core.AgentContract{queueContract("which shipment one?"), queueContract("which shipment two?")}, "queue", nil, deadlineIn(300*time.Millisecond), nil)
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s: a holder that stopped answering held the call past its deadline", elapsed)
	}
	if sum != (Summary{Deferred: 2}) {
		t.Fatalf("summary = %+v, want both deferred", sum)
	}
	for i, pr := range results {
		if !strings.Contains(pr.Result.Reason, "could not be looked up on the holder") || strings.Contains(pr.Result.Reason, "still queued") || !strings.Contains(pr.Result.Reason, pr.JobID) {
			t.Fatalf("result %d reason %q, want it to say the holder was not asked successfully and name the job", i, pr.Result.Reason)
		}
	}
}

// TestQueueRouteCutSubmitIsNotSubmittedWhenTheHolderHasNoSuchJob: the deadline lands while
// jobs are still being submitted (the holder never answers a submit). The holder has no such
// job, so the results say they had not been submitted — unplaced, and named as such.
func TestQueueRouteCutSubmitIsNotSubmittedWhenTheHolderHasNoSuchJob(t *testing.T) {
	url := frontedHolder(t, true, false, true)
	cfg := config.Config{FleetQueueHolder: url, StateDir: t.TempDir()}
	results, sum, _ := runWithin(t, 6*time.Second, cfg, nil,
		[]core.AgentContract{queueContract("which shipment one?"), queueContract("which shipment two?")}, "queue", nil, deadlineIn(300*time.Millisecond), nil)
	if sum != (Summary{Deferred: 2}) {
		t.Fatalf("summary = %+v, want both deferred (a deadline is never a failure)", sum)
	}
	for i, pr := range results {
		if !pr.Unplaced || !strings.Contains(pr.Result.Reason, "had not been submitted to the holder") || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"2 unfinished") {
			t.Fatalf("result %d = unplaced %v reason %q, want the not-submitted call-deadline defer counting both", i, pr.Unplaced, pr.Result.Reason)
		}
	}
}

// widgetContract is a queue-lane contract whose answer must contain "widget".
func widgetContract(goal string) core.AgentContract {
	c := queueContract(goal)
	c.Acceptance = []string{"contains:widget"}
	return c
}

// TestQueueRouteEvaluatesAcceptanceOnAFinishedJob: a job the holder finished is checked
// against its contract's acceptance like any answer — on the ordinary poll AND on the
// deadline's last look, which reads the holder through the same reader. A finished answer
// that misses its check is failed verification, never a silent success.
func TestQueueRouteEvaluatesAcceptanceOnAFinishedJob(t *testing.T) {
	finish := func(q *fleetqueue.Queue, job *fleetqueue.Job, output string) {
		w, _ := json.Marshal(core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "claimant", Seat: "seat-x", Output: output, StopReason: "done"})
		if err := q.Ack(job.ID, "claimant", w, ""); err != nil {
			t.Errorf("ack: %v", err)
		}
	}
	t.Run("on the ordinary poll", func(t *testing.T) {
		holder := queueHolder(t)
		cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
		go func() {
			time.Sleep(50 * time.Millisecond)
			for _, job := range claimAll(t, holder.q, 1) {
				finish(holder.q, job, "no such token here")
			}
		}()
		_, sum, _ := runWithin(t, 10*time.Second, cfg, nil, []core.AgentContract{widgetContract("which shipment?")}, "queue", nil, deadlineIn(time.Hour), nil)
		if sum.FailedVerification != 1 || sum.Succeeded != 0 {
			t.Fatalf("summary = %+v, want the finished answer failed verification", sum)
		}
	})
	t.Run("on the deadline's last look", func(t *testing.T) {
		widenUnwind(t, 2*time.Second)
		holder := queueHolder(t)
		cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
		go func() {
			time.Sleep(50 * time.Millisecond)
			for _, job := range claimAll(t, holder.q, 2) {
				if strings.Contains(goalOf(t, job), "two") {
					finish(holder.q, job, "no such token here")
				}
			}
		}()
		results, sum, _ := runWithin(t, 4*time.Second, cfg, nil,
			[]core.AgentContract{widgetContract("which shipment one?"), widgetContract("which shipment two?")}, "queue", nil, deadlineIn(1200*time.Millisecond), nil)
		if sum != (Summary{Deferred: 1, FailedVerification: 1}) {
			t.Fatalf("summary = %+v, want the held job deferred and the finished one failed verification", sum)
		}
		if len(results[1].AcceptanceFailures) == 0 {
			t.Fatalf("the finished job carries no acceptance failure: %+v", results[1])
		}
	})
}
