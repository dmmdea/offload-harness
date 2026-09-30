package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// This file pins DELETE /fleet/jobs/{id} (ADR 0064): the delegator's way to take
// back a job the node has not started, behind the same bearer gate as the poll.

const withdrawToken = "s3cret"

var withdrawAuth = map[string]string{"Authorization": "Bearer " + withdrawToken}

// withdrawFixture is an agent node with ONE execution slot and a runner that
// blocks: "running-1" takes the slot, "queued-1" waits behind it, both
// dispatched over HTTP like a real delegator would.
func withdrawFixture(t *testing.T) (s *Server, jobs *Jobs, release func()) {
	t.Helper()
	cfg := agentLaneCfg(t, withdrawToken)
	cfg.FleetMaxConcurrentJobs = 1
	rel := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(rel) }) }
	s, jobs = newTestServer(t, cfg, &fakeRunner{fn: blockingAgentRun(rel)}, authOpts(true))
	t.Cleanup(release) // registered after newTestServer, so it runs BEFORE the drain
	for _, id := range []string{"running-1", "queued-1"} {
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody(id), withdrawAuth); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d, want 202 (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	waitJobState(t, jobs, "running-1", JobRunning)
	if v, _ := jobs.Get("queued-1"); v.State != JobAccepted {
		t.Fatalf("queued-1 state = %v, want accepted behind the held slot", v.State)
	}
	return s, jobs, release
}

// TestWithdrawRequiresTheAgentBearer: an agent job's withdraw sits behind the
// same bearer gate as its poll. A caller without the token learns nothing about
// the job — not that it exists, not its state — and cannot take it back.
func TestWithdrawRequiresTheAgentBearer(t *testing.T) {
	s, jobs, _ := withdrawFixture(t)

	for _, tc := range []struct {
		name   string
		id     string
		header map[string]string
	}{
		{"queued job, no token", "queued-1", nil},
		{"queued job, wrong token", "queued-1", map[string]string{"Authorization": "Bearer wrong"}},
		{"queued job, wrong scheme", "queued-1", map[string]string{"Authorization": "Basic " + withdrawToken}},
		// The verdict for a RUNNING job would be 409 with its state: an
		// unauthorized caller must get the auth answer instead, never the state.
		{"running job, no token", "running-1", nil},
	} {
		rec := do(t, s, http.MethodDelete, "/fleet/jobs/"+tc.id, "", tc.header)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
		if msg, _ := decodeMap(t, rec)["error"].(string); msg != "unauthorized" {
			t.Fatalf("%s: error = %q, want exactly %q", tc.name, msg, "unauthorized")
		}
	}
	if v, _ := jobs.Get("queued-1"); v.State != JobAccepted {
		t.Fatalf("an unauthorized withdraw took effect: queued-1 is %v", v.State)
	}

	// An id the store does not hold is a plain 404 with or without the token, as it
	// is for the poll: job ids are caller-generated, so a 404 discloses nothing.
	if rec := do(t, s, http.MethodDelete, "/fleet/jobs/never-seen", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id without a token = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	// With the token the same request is honoured.
	if rec := do(t, s, http.MethodDelete, "/fleet/jobs/queued-1", "", withdrawAuth); rec.Code != http.StatusOK {
		t.Fatalf("authorized withdraw = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestWithdrawAnswers walks the route's whole answer table: withdrawn, refused
// because the job started, idempotent repeat, unknown, and the follow-on facts
// a poller and a duplicate dispatch then meet.
func TestWithdrawAnswers(t *testing.T) {
	s, jobs, release := withdrawFixture(t)

	// A never-started job: 200, the wire says so in both the state and a flag.
	rec := do(t, s, http.MethodDelete, "/fleet/jobs/queued-1", "", withdrawAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("withdraw queued = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["job_id"] != "queued-1" || m["state"] != WithdrawnState || m["withdrawn"] != true {
		t.Fatalf("withdraw body = %v, want job_id queued-1, state %q, withdrawn true", m, WithdrawnState)
	}

	// The node says what became of it: a poll reaches a terminal state with the
	// stable withdrawn text, and the job never runs.
	pm := decodeMap(t, do(t, s, http.MethodGet, "/fleet/jobs/queued-1", "", withdrawAuth))
	if pm["state"] != string(JobError) || pm["error"] != ErrWithdrawn {
		t.Fatalf("poll after withdraw = %v, want state error with %q", pm, ErrWithdrawn)
	}

	// Idempotent: a repeat (the answer was lost) reads the same verdict.
	if rec = do(t, s, http.MethodDelete, "/fleet/jobs/queued-1", "", withdrawAuth); rec.Code != http.StatusOK {
		t.Fatalf("repeat withdraw = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// A duplicate dispatch of the id meets the known-job path: 409, never a second run.
	if rec = do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody("queued-1"), withdrawAuth); rec.Code != http.StatusConflict {
		t.Fatalf("re-dispatch of a withdrawn id = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// A running job is refused with its state, and is left running.
	rec = do(t, s, http.MethodDelete, "/fleet/jobs/running-1", "", withdrawAuth)
	if rec.Code != http.StatusConflict {
		t.Fatalf("withdraw running = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	m = decodeMap(t, rec)
	if m["state"] != string(JobRunning) || m["withdrawn"] != false {
		t.Fatalf("running-job body = %v, want state running, withdrawn false", m)
	}
	if v, _ := jobs.Get("running-1"); v.State != JobRunning {
		t.Fatalf("a refused withdraw disturbed the running job: %v", v.State)
	}

	// Unknown id: 404.
	if rec = do(t, s, http.MethodDelete, "/fleet/jobs/never-seen", "", withdrawAuth); rec.Code != http.StatusNotFound {
		t.Fatalf("withdraw unknown = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	// A finished job is refused too, and keeps its result for the poll that follows.
	release()
	waitJobState(t, jobs, "running-1", JobDone)
	rec = do(t, s, http.MethodDelete, "/fleet/jobs/running-1", "", withdrawAuth)
	if rec.Code != http.StatusConflict || decodeMap(t, rec)["state"] != string(JobDone) {
		t.Fatalf("withdraw done = %d %s, want 409 with state done", rec.Code, rec.Body.String())
	}
}

// TestWithdrawIsForAgentJobsOnly: a media job belongs to a dispatcher this node
// does not control, and its route is not behind the agent bearer — so the node
// never lets a withdraw reach it, token or no token.
func TestWithdrawIsForAgentJobsOnly(t *testing.T) {
	cfg := imageCfg()
	cfg.FleetMaxConcurrentJobs = 1
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	release := holdSlot(t, jobs, "holder")
	defer release()
	if !jobs.Accept("media-queued", func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }) {
		t.Fatal("media job not admitted")
	}
	rec := do(t, s, http.MethodDelete, "/fleet/jobs/media-queued", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("withdraw of a media job = %d, want 405 (body %s)", rec.Code, rec.Body.String())
	}
	if v, _ := jobs.Get("media-queued"); v.State != JobAccepted {
		t.Fatalf("a refused withdraw changed the media job: %v", v.State)
	}
}

// leaseServer builds a Server over a store driven by a hand-held clock, so the
// poll lease can be aged without sleeping through it.
func leaseServer(t *testing.T, cfg config.Config, run Runner) (*Server, *Jobs, *leaseClock) {
	t.Helper()
	clk := newLeaseClock()
	jobs := newJobs(time.Hour, clk.Now, time.Hour, cfg.FleetConcurrencyLimit())
	jobs.SetPollLease(time.Minute)
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	o := *authOpts(true)
	o.Cfg = cfg
	return New(run, jobs, o), jobs, clk
}

// TestPollRefreshesTheLeaseButNothingElseDoes: an authorized poll is a poll; a
// 401'd one, and the unauthenticated jobs feed the fleet overview reads, are not
// — an observer must not be able to keep a ghost alive.
func TestPollRefreshesTheLeaseButNothingElseDoes(t *testing.T) {
	cfg := agentLaneCfg(t, withdrawToken)
	cfg.FleetMaxConcurrentJobs = 1
	cfg.FleetMaxQueueDepth = 16 // room for the four dispatches below
	rel := make(chan struct{})
	var once sync.Once
	s, jobs, clk := leaseServer(t, cfg, &fakeRunner{fn: blockingAgentRun(rel)})
	t.Cleanup(func() { once.Do(func() { close(rel) }) }) // after leaseServer: runs before its drain

	// The dispatch path admits agent jobs PollLeased; a dispatch through the
	// door under test is what the reaper must be able to see.
	for _, id := range []string{"holder", "polled", "unauthorized", "listed"} {
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody(id), withdrawAuth); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	waitJobState(t, jobs, "holder", JobRunning)

	clk.Advance(50 * time.Second)
	if rec := do(t, s, http.MethodGet, "/fleet/jobs/polled", "", withdrawAuth); rec.Code != http.StatusOK {
		t.Fatalf("authorized poll = %d", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/jobs/unauthorized", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated poll = %d, want 401", rec.Code)
	}
	if rec := do(t, s, http.MethodGet, "/fleet/jobs", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("jobs feed = %d", rec.Code)
	}

	clk.Advance(50 * time.Second) // 100 s since admission, 50 s since the one real poll
	if n := jobs.reap(); n != 2 {
		t.Fatalf("reap took %d job(s), want exactly the two nobody really polled", n)
	}
	for id, want := range map[string]JobState{"polled": JobAccepted, "unauthorized": JobError, "listed": JobError} {
		if v, _ := jobs.Get(id); v.State != want {
			t.Errorf("%s state = %v, want %v", id, v.State, want)
		}
	}
	// The reaped job's poller, back after the gap, reads why: a terminal state,
	// not a mystery.
	pm := decodeMap(t, do(t, s, http.MethodGet, "/fleet/jobs/listed", "", withdrawAuth))
	if pm["state"] != string(JobError) || pm["error"] != ErrReaped {
		t.Fatalf("poll of a reaped job = %v, want state error with %q", pm, ErrReaped)
	}
}

// TestPulledJobsAreNotPollLeased: the pull queue's claim loop admits agent jobs
// nobody polls this node for (the result travels by ack), so it must not stamp
// the lease on them — or the reaper would kill every queued pull after a minute.
func TestPulledJobsAreNotPollLeased(t *testing.T) {
	s, _ := newTestServer(t, agentLaneCfg(t, withdrawToken), &fakeRunner{}, authOpts(true))
	if spec := s.claimSpec("agent", nil); spec.PollLeased {
		t.Fatal("claimSpec marked a pulled agent job PollLeased: the reaper would take it while it waits for a slot")
	}
}
