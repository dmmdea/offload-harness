package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"
)

// The reaper is the one unattended state change in ADR 0064: it ends a job with no
// delegator left to say so, and the withdraw logs nothing. Before these counters the
// only record of either was one stderr line per reaper pass on a process that may be
// detached, and the jobs feed keeps a terminal record for an hour and hides an agent
// row's text from a caller without the bearer. A default-on, time-based reaper is
// exactly the thing whose false positives an operator has to be able to COUNT.

// TestJobsCountWhatTheyTookBack: a store counts the jobs it took out of the backlog,
// by route. A repeat withdraw is the same fact (not a second withdrawal), a running
// job is refused and not counted, and a job no route touched is not counted.
func TestJobsCountWhatTheyTookBack(t *testing.T) {
	clk := newLeaseClock()
	j := newJobs(time.Hour, clk.Now, time.Hour, 1)
	j.SetPollLease(time.Minute)
	defer j.DrainAndStop(time.Second)
	release := holdSlot(t, j, "holder")
	defer release()

	run := func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	for _, id := range []string{"w1", "w2", "g1", "g2", "g3"} {
		if !j.Admit(id, AcceptSpec{Agent: true, PollLeased: true}, run) {
			t.Fatalf("%s was not admitted", id)
		}
	}
	if w, r := j.TakenBack(); w != 0 || r != 0 {
		t.Fatalf("a store that has taken nothing back reports %d withdrawn, %d reaped", w, r)
	}

	j.Withdraw("w1")
	j.Withdraw("w2")
	j.Withdraw("w1")     // a repeat is the same fact, not a second withdrawal
	j.Withdraw("holder") // running: refused, not counted
	j.Withdraw("nobody") // unknown: not counted
	if w, r := j.TakenBack(); w != 2 || r != 0 {
		t.Fatalf("after 2 withdrawals (plus a repeat, a running job and an unknown id) TakenBack = %d withdrawn, %d reaped, want 2 and 0", w, r)
	}

	clk.Advance(2 * time.Minute)
	if n := j.reap(); n != 3 {
		t.Fatalf("reap took %d job(s), want the three ghosts (the withdrawn are terminal, the holder runs)", n)
	}
	if w, r := j.TakenBack(); w != 2 || r != 3 {
		t.Fatalf("TakenBack = %d withdrawn, %d reaped, want 2 and 3", w, r)
	}
	if n := j.reap(); n != 0 {
		t.Fatalf("a second pass reaped %d job(s), want none left", n)
	}
	if w, r := j.TakenBack(); w != 2 || r != 3 {
		t.Fatalf("an empty pass moved the counters: %d withdrawn, %d reaped", w, r)
	}
}

// TestJobsDoNotCountADrainAsWhatTheyTookBack: a shutdown marks its queued jobs
// never-started, which is a third route to a job that never ran but not one a
// delegator or the reaper took: it does not move either counter.
func TestJobsDoNotCountADrainAsWhatTheyTookBack(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	release := holdSlot(t, j, "holder")
	run := func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	if !j.Admit("queued", AcceptSpec{Agent: true, PollLeased: true}, run) {
		t.Fatal("queued was not admitted")
	}
	release()
	j.DrainAndStop(time.Second)
	if w, r := j.TakenBack(); w != 0 || r != 0 {
		t.Fatalf("a drain moved the counters: %d withdrawn, %d reaped", w, r)
	}
}

// TestHealthPublishesWhatTheNodeTookBack: the counters ride /fleet/health as two
// additive omitempty fields, so a node that has taken nothing back publishes a
// byte-identical payload, and a fleet view polling health can chart the reaper.
func TestHealthPublishesWhatTheNodeTookBack(t *testing.T) {
	cfg := agentLaneCfg(t, withdrawToken)
	cfg.FleetMaxConcurrentJobs = 1
	cfg.FleetMaxQueueDepth = 16
	rel := make(chan struct{})
	var once sync.Once
	s, jobs, clk := leaseServer(t, cfg, &fakeRunner{fn: blockingAgentRun(rel)})
	t.Cleanup(func() { once.Do(func() { close(rel) }) }) // after leaseServer: runs before its drain

	health := func() map[string]any {
		t.Helper()
		rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("health = %d (body %s)", rec.Code, rec.Body.String())
		}
		return decodeMap(t, rec)
	}
	h := health()
	for _, key := range []string{"jobs_withdrawn", "jobs_reaped"} {
		if _, present := h[key]; present {
			t.Fatalf("a node that took nothing back publishes %q: the payload is no longer byte-identical for it", key)
		}
	}

	for _, id := range []string{"holder", "w1", "g1", "g2"} {
		if rec := do(t, s, http.MethodPost, "/fleet/dispatch", agentLaneBody(id), withdrawAuth); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	waitJobState(t, jobs, "holder", JobRunning)
	if rec := do(t, s, http.MethodDelete, "/fleet/jobs/w1", "", withdrawAuth); rec.Code != http.StatusOK {
		t.Fatalf("withdraw = %d (body %s)", rec.Code, rec.Body.String())
	}
	clk.Advance(2 * time.Minute)
	if n := jobs.reap(); n != 2 {
		t.Fatalf("reap took %d job(s), want the two ghosts", n)
	}

	h = health()
	if h["jobs_withdrawn"] != float64(1) || h["jobs_reaped"] != float64(2) {
		t.Fatalf("health jobs_withdrawn = %v, jobs_reaped = %v, want 1 and 2", h["jobs_withdrawn"], h["jobs_reaped"])
	}
}
