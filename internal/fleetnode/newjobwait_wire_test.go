// An EXTERNAL test package for the reason healthwire_compat_test.go gives: it runs the delegator's REAL
// health decoder (delegate.FetchNodeView) against this node's REAL health handler, so the proof that
// new_job_wait_sec reaches a delegator is the shipped reader reading the shipped writer, never a copy of
// either.
package fleetnode_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// TestNewJobWaitReachesTheDelegatorOneSlotBeyondTheEstimate: one worker, one running agent job and two
// queued behind it, after a finished job gave the node a 0.4 s wall sample. The deepest queued job waits
// excess 2 x 0.4 s; the job the delegator is about to send waits 3 x 0.4 s. Both arrive, decoded, one
// slot (0.4 s) apart.
func TestNewJobWaitReachesTheDelegatorOneSlotBeyondTheEstimate(t *testing.T) {
	cfg := config.Config{ImageGenScript: "C:/x/comfy-generate.mjs", FleetMaxConcurrentJobs: 1, FleetMaxQueueDepth: -1}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "wire-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	spec := fleetnode.AcceptSpec{Agent: true}
	finished := make(chan struct{})
	if !jobs.Admit("wall-sample", spec, func(context.Context) (json.RawMessage, error) {
		time.Sleep(400 * time.Millisecond)
		close(finished)
		return json.RawMessage(`{}`), nil
	}) {
		t.Fatal("the wall-sample job was refused")
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the wall-sample job never finished")
	}
	time.Sleep(60 * time.Millisecond) // the store stamps finishedAt as the closure returns

	release := make(chan struct{})
	defer close(release)
	block := func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{}`), nil
	}
	for _, id := range []string{"b1", "b2", "b3"} {
		if !jobs.Admit(id, spec, block) {
			t.Fatalf("Admit(%s) was refused", id)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if q, r := jobs.CountsCapped(); q == 2 && r == 1 {
			break
		}
		if time.Now().After(deadline) {
			q, r := jobs.CountsCapped()
			t.Fatalf("the node never settled at 2 queued / 1 running (got %d/%d)", q, r)
		}
		time.Sleep(2 * time.Millisecond)
	}

	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
	}
	if view.NewJobWaitSec == nil || view.QueueWaitEstimateSec == nil {
		t.Fatalf("decoded new_job_wait_sec=%v queue_wait_estimate_sec=%v, want both published", view.NewJobWaitSec, view.QueueWaitEstimateSec)
	}
	deepest, forNew := *view.QueueWaitEstimateSec, *view.NewJobWaitSec
	if deepest < 0.7 || deepest > 1.0 {
		t.Fatalf("queue_wait_estimate_sec = %v, want about 0.8 (excess 2 x a 0.4 s wall / 1 worker)", deepest)
	}
	if math.Abs(forNew-1.5*deepest) > 0.02 {
		t.Fatalf("new_job_wait_sec = %v against queue_wait_estimate_sec = %v, want 3/2 of it: one slot (a whole wall) deeper, the arrival queues behind everything admitted", forNew, deepest)
	}
	// The number is the arithmetic the delegator derives from the same counters when a node publishes nothing.
	derived := float64(view.JobsRunning+view.JobsQueued-view.MaxConcurrentJobs+1) * view.RecentAgentWallSec / float64(view.MaxConcurrentJobs)
	if view.RecentAgentWallSec > 0 && math.Abs(derived-forNew) > 0.02 {
		t.Fatalf("published %v but the delegator would derive %v from the same counters: one number must price a new job", forNew, derived)
	}
}

// TestAFreeWorkersZeroReachesTheDelegatorAsAKnownZero: the same node with every worker free. Its wall sample
// exists, so the node can say a new job waits 0 s, and the delegator decodes that as a PRESENT zero, not as
// no claim - the difference between "start now" and a fall back to counters that include uncapped renders.
func TestAFreeWorkersZeroReachesTheDelegatorAsAKnownZero(t *testing.T) {
	cfg := config.Config{ImageGenScript: "C:/x/comfy-generate.mjs", FleetMaxConcurrentJobs: 2, FleetMaxQueueDepth: -1}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID:  "wire-node",
		Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true
		},
		Cfg: cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
	}
	if view.NewJobWaitSec != nil {
		t.Fatalf("a node with no wall sample published new_job_wait_sec = %v, want no claim", *view.NewJobWaitSec)
	}

	spec := fleetnode.AcceptSpec{Agent: true}
	finished := make(chan struct{})
	if !jobs.Admit("wall-sample", spec, func(context.Context) (json.RawMessage, error) {
		time.Sleep(50 * time.Millisecond)
		close(finished)
		return json.RawMessage(`{}`), nil
	}) {
		t.Fatal("the wall-sample job was refused")
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the wall-sample job never finished")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		view, err = delegate.FetchNodeView(context.Background(), ts.URL, "")
		if err != nil {
			t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
		}
		if view.NewJobWaitSec != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node never published new_job_wait_sec although an agent job finished: a free worker with a wall sample is a claim, and its absence sends the delegator to counters that include uncapped jobs")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if *view.NewJobWaitSec != 0 {
		t.Fatalf("decoded new_job_wait_sec = %v, want 0: both workers are free", *view.NewJobWaitSec)
	}
}
