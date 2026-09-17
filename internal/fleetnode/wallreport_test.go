package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The wall on the poll payload (register D-116). A contract the caller left
// unsized is stamped timeout_auto and THIS node sizes the wall from its own
// seat's measured rate. Until now that number reached the delegator only on the
// final result — exactly the message a node that dies mid-run never sends — so
// the delegator held its poll clock at the wire cap for every such contract.
// The node now publishes the wall it is RUNNING under, on every poll of a
// running job.

// TestRunningJobPublishesTheWallItIsRunningUnder: the executing lane reports
// its wall (core.ReportWall) and a poll of the still-running job carries it as
// `wall_sec`.
func TestRunningJobPublishesTheWallItIsRunningUnder(t *testing.T) {
	reported := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		core.ReportWall(ctx, 742)
		close(reported)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"schema_version":1,"output":"ok","structured":{"answer":"ok"}}`)}
	}}
	s, _ := loopbackAgentServer(t, agentQueueCfg(t, 8, 1), runner)
	defer close(release)

	if rec := dispatchAgent(t, s, "wall-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	<-reported

	rec := do(t, s, http.MethodGet, "/fleet/jobs/wall-1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["state"] != string(JobRunning) {
		t.Fatalf("state = %v, want running (body %s)", body["state"], rec.Body.String())
	}
	got, ok := body["wall_sec"].(float64)
	if !ok || int(got) != 742 {
		t.Fatalf("wall_sec = %v, want 742 — a delegator polling a timeout_auto contract has nothing else to bound by (body %s)", body["wall_sec"], rec.Body.String())
	}
}

// TestJobWithoutAReportedWallPublishesNoWallField: a lane that reports no wall
// (every non-agent task, and an agent run on a node too old to report one)
// must publish a payload byte-identical to the pre-D-116 shape — `wall_sec` is
// omitempty and must actually be omitted.
func TestJobWithoutAReportedWallPublishesNoWallField(t *testing.T) {
	release := make(chan struct{})
	s, _ := loopbackAgentServer(t, agentQueueCfg(t, 8, 1), &fakeRunner{fn: blockingAgentRun(release)})
	defer close(release)

	if rec := dispatchAgent(t, s, "wall-2"); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d, want 202", rec.Code)
	}
	rec := do(t, s, http.MethodGet, "/fleet/jobs/wall-2", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d, want 200", rec.Code)
	}
	if _, present := decodeMap(t, rec)["wall_sec"]; present {
		t.Fatalf("a run that reported no wall must publish no wall_sec: %s", rec.Body.String())
	}
}

// TestSetWallNeverRewritesATerminalJob: the wall is a fact about a RUNNING
// job. Once the job is terminal its result carries its own wall, and a late
// report must not edit a finished record.
func TestSetWallNeverRewritesATerminalJob(t *testing.T) {
	jobs := NewJobs(0, 1)
	t.Cleanup(func() { jobs.DrainAndStop(0) })
	done := make(chan struct{})
	if !jobs.AcceptAgent("term-1", func(context.Context) (json.RawMessage, error) {
		close(done)
		return json.RawMessage(`{"ok":true}`), nil
	}) {
		t.Fatal("Admit refused a fresh id")
	}
	<-done
	// Drain to a terminal state, then try to stamp a wall onto it.
	jobs.DrainAndStop(0)
	jobs.SetWall("term-1", 900)
	v, ok := jobs.Get("term-1")
	if !ok {
		t.Fatal("the job vanished")
	}
	if v.State != JobDone && v.State != JobError {
		t.Fatalf("state = %s, want a terminal state", v.State)
	}
	if v.WallSec != 0 {
		t.Fatalf("wall_sec = %d on a terminal job, want it left alone", v.WallSec)
	}
	jobs.SetWall("no-such-job", 900) // an unknown id is a no-op, never a panic
}
