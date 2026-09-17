// deal_auto_remote_test.go: W-06 (PR-5 item 6, register S-11/S-13) — route=
// auto and route=remote compute ONE joint deal over ONE fleet snapshot,
// respecting per-node headroom (max_concurrent_jobs − jobs_running − what
// this deal has already committed): a node at 0 headroom gets NOTHING (no
// floor), the next candidate is tried, and a fleet fully exhausted routes
// through the existing capacity wait rather than dispatching to an
// already-full node.

package delegate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// dealNode is an accepting remote with a fixed one-slot headroom, used to
// force the joint deal to spread across several nodes rather than piling
// every subtask onto whichever one wins roster-order ties.
func dealNode(t *testing.T, id string, maxConcurrent int, jobsRunning int) (*fakeNode, string) {
	t.Helper()
	f, url := acceptingNode(t, id, "qube from "+id, func(f *fakeNode) {
		f.maxConcurrentJobs = maxConcurrent
		f.jobsRunning = jobsRunning
		// queue_depth = jobs_running + jobs_queued by construction (nodeview.go);
		// an inconsistent fixture (depth 0 with jobs_running > 0) would read as
		// provablyStartsNow's FIRST, cheapest proof ("QueueDepth == 0 — the node
		// holds no job at all") and mask the headroom key entirely.
		f.queueDepth = jobsRunning
	})
	return f, url
}

// TestRunAutoJointDealSpreadsAcrossThreeDistinctNodes: three eligible remotes
// (one slot of headroom each), the local seat held by a lease, three
// route=auto subtasks in one Run — each must land on a DIFFERENT node, not
// all three piling onto whichever one wins the roster-order tie.
func TestRunAutoJointDealSpreadsAcrossThreeDistinctNodes(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	a, urlA := dealNode(t, "node-a", 1, 0)
	b, urlB := dealNode(t, "node-b", 1, 0)
	c, urlC := dealNode(t, "node-c", 1, 0)

	dir, _ := holdLease(t, gpulease.ClassText, "held for the deal test")
	cfg := testCfg(t)
	cfg.GPULockPath = dir

	var localCalls atomic.Int64
	subtasks := []core.AgentContract{remoteContract(), remoteContract(), remoteContract()}
	subtasks[0].Goal, subtasks[1].Goal, subtasks[2].Goal = "qube task one", "qube task two", "qube task three"
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), subtasks, "auto", []string{urlA, urlB, urlC})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 3 {
		t.Fatalf("summary = %+v, want all three subtasks to succeed", sum)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("local runner called %d times, want 0 — the seat is reserved", localCalls.Load())
	}
	seen := map[string]bool{}
	for _, pr := range results {
		seen[pr.Node] = true
	}
	if len(seen) != 3 {
		t.Fatalf("subtasks landed on %d distinct nodes (%v), want 3", len(seen), seen)
	}
	for name, n := range map[string]*fakeNode{"node-a": a, "node-b": b, "node-c": c} {
		if n.dispatches.Load() != 1 {
			t.Errorf("%s dispatches = %d, want exactly 1", name, n.dispatches.Load())
		}
	}
}

// TestRunAutoJointDealGivesAFullNodeZeroSubtasks: 8 contracts, one remote
// already at its own concurrency ceiling (4/4 running) and another idle — the
// full node must receive NONE of the 8, not even one "to be fair": headroom
// has no floor.
func TestRunAutoJointDealGivesAFullNodeZeroSubtasks(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	full, fullURL := dealNode(t, "node-full", 4, 4) // headroom 0 from the start
	idle, idleURL := dealNode(t, "node-idle", 0, 0) // unpublished ceiling = unlimited

	subtasks := make([]core.AgentContract, 8)
	for i := range subtasks {
		c := remoteContract()
		c.Goal = "qube extraction task"
		subtasks[i] = c
	}
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), subtasks, "remote", []string{fullURL, idleURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 8 {
		t.Fatalf("summary = %+v, want all 8 to succeed on the idle node", sum)
	}
	if full.dispatches.Load() != 0 {
		t.Fatalf("the full node (4/4 running) received %d dispatch(es), want 0 — headroom has no floor", full.dispatches.Load())
	}
	if idle.dispatches.Load() != 8 {
		t.Fatalf("the idle node received %d dispatch(es), want all 8", idle.dispatches.Load())
	}
	for _, pr := range results {
		if pr.Node != "node-idle" {
			t.Errorf("subtask landed on %q, want node-idle", pr.Node)
		}
	}
}

// TestRunAutoJointDealBothFullGoesToCapacityWait: every eligible remote is at
// its own headroom ceiling — the subtask must go through the EXISTING
// capacity wait (agent_placement_wait_sec), not dispatch to an over-full node
// and not defer some other way.
func TestRunAutoJointDealBothFullGoesToCapacityWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	// Full on BOTH ceilings — max_concurrent_jobs (headroom 0, the W-06 key
	// under test) AND max_queue_depth (saturated(), so the EXISTING capacity
	// wait's own room check, hasRoom/awaitCapacity, does not admit it into a
	// backlog either). A node merely at its concurrency ceiling with queue
	// room left is a legitimate "queue it there" target for the wait, not
	// this test's scenario.
	full := func(f *fakeNode) { f.maxConcurrentJobs, f.jobsRunning, f.queueDepth, f.maxQueueDepth = 1, 1, 1, 1 }
	a, urlA := acceptingNode(t, "node-a", "qube from a", full)
	b, urlB := acceptingNode(t, "node-b", "qube from b", full)

	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1 // enable the wait, short so the test stays fast

	contract := remoteContract()
	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{urlA, urlB})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.dispatches.Load() != 0 || b.dispatches.Load() != 0 {
		t.Fatalf("a full node must never be dispatched to: a=%d b=%d", a.dispatches.Load(), b.dispatches.Load())
	}
	pr := results[0]
	if !pr.waited {
		t.Fatalf("result = %+v, want the subtask to have gone through the capacity wait", pr)
	}
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result.Result = %+v, want a deferred capacity-class outcome", pr.Result)
	}
	if sum.Deferred != 1 {
		t.Fatalf("summary = %+v, want exactly one capacity defer", sum)
	}
}
