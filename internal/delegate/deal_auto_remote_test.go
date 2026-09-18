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
	"strings"
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

// TestDealAutoRemotePreMintsAndReusesTheJobID (review round 1, MEDIUM item
// 3): the job id used as W-11's P2C draw seed is minted ONCE at deal time
// (dealAutoRemote), not a throwaway random string unrelated to what the run
// eventually dispatches — the published result's JobID, and the id the
// remote actually received on the wire, must be the SAME value the deal
// pre-minted.
func TestDealAutoRemotePreMintsAndReusesTheJobID(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-a", "qube answered", nil)

	contract := remoteContract()
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the subtask to have succeeded", sum)
	}
	dispatchedID, _ := node.lastJobID.Load().(string)
	if dispatchedID == "" {
		t.Fatal("the node never recorded a dispatched job id")
	}
	if results[0].JobID != dispatchedID {
		t.Fatalf("published JobID = %q, want it to equal the id actually dispatched to the node (%q) — the deal's pre-minted id must be the one reused, not a fresh one minted later in attempt()", results[0].JobID, dispatchedID)
	}
}

// TestDealAutoRemoteSlotCarriesAPreMintedJobID is the unit-level twin: a
// direct dealAutoRemote call must hand every slot a non-empty, well-formed
// job id, computed BEFORE placeAutoRemote's own ranking runs (so it is
// available as that ranking's P2C seed).
func TestDealAutoRemoteSlotCarriesAPreMintedJobID(t *testing.T) {
	r := &runner{route: "remote"}
	a := eligibleRemote()
	a.NodeID = "node-a"
	contract := remoteContract()
	slots := r.dealAutoRemote([]core.AgentContract{contract}, localNode(), []NodeView{a}, []string{"http://node-a"}, true, nil)
	if len(slots) != 1 {
		t.Fatalf("slots = %v, want 1", slots)
	}
	if !strings.HasPrefix(slots[0].jobID, "agd-") {
		t.Fatalf("slot.jobID = %q, want a well-formed pre-minted id (agd- prefix)", slots[0].jobID)
	}
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
	// Review round 1, BLOCKER item 2: the capacityWait exit must carry the
	// per-node verdict line, not just the old aggregate "every eligible
	// remote is at headroom" sentence with no node names attached. This
	// fixture is saturated on BOTH ceilings (so awaitCapacity's own
	// hasRoom-based tick loop, which knows nothing about W-06 headroom,
	// also refuses it and the wait genuinely times out) — the ranking-word
	// it earns is therefore "queue" (checked before "cap" in gate order),
	// which TestPlaceAutoRemoteCapacityWaitNamesEachNodesHeadroom below
	// exercises directly against a headroom-only-full fixture.
	for _, want := range []string{"node-a: queue (1/1 queue_depth)", "node-b: queue (1/1 queue_depth)"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("capacity defer reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
}

// TestPlaceAutoRemoteCapacityWaitNamesEachNodesHeadroom (review round 1,
// BLOCKER item 2): a headroom-only-full node (not admission-saturated, so
// the distinguishing case from the "queue" ranking word above) must be
// narrated with its own running/headroom numbers on the capacityWait exit,
// not just the old aggregate sentence.
func TestPlaceAutoRemoteCapacityWaitNamesEachNodesHeadroom(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	a := eligibleRemote()
	a.NodeID, a.MaxConcurrentJobs, a.JobsRunning = "node-a", 4, 4
	b := eligibleRemote()
	b.NodeID, b.MaxConcurrentJobs, b.JobsRunning = "node-b", 4, 4
	r := &runner{route: "remote"}
	slot := r.placeAutoRemote("seed", st, localNode(), []NodeView{a, b}, []string{"http://node-a", "http://node-b"}, true, map[string]int{}, nil)
	if !slot.capacityWait {
		t.Fatalf("slot = %+v, want capacityWait", slot)
	}
	for _, want := range []string{"node-a: cap (4/4 running, 0 headroom)", "node-b: cap (4/4 running, 0 headroom)"} {
		if !strings.Contains(slot.reason, want) {
			t.Errorf("capacityWait reason = %q, want it to contain %q", slot.reason, want)
		}
	}
}

// TestRunAutoJointDealNamesADeadRemoteAsProbe (review round 1, BLOCKER item
// 2): a remote that never answers its health probe is still CONFIGURED and
// must still be named in placement_reason — the documented `probe` verdict
// was dead code before this, because placementVerdictLine only ever walked
// the successfully-probed views.
func TestRunAutoJointDealNamesADeadRemoteAsProbe(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	noProbeMemo(t)
	_, chosenURL := acceptingNode(t, "node-chosen", "qube from chosen", nil)
	// deadListener (probe_fanout_test.go): accepts the TCP connection and
	// hangs up with no HTTP response at all — probeUnreachable's shape, so
	// FetchNodeView returns a transport error and the base lands in `failed`.
	deadURL, _ := deadListener(t)

	contract := remoteContract()
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{chosenURL, deadURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the reachable node to have run it", sum)
	}
	pr := results[0]
	if pr.Node != "node-chosen" {
		t.Fatalf("Node = %q, want node-chosen", pr.Node)
	}
	if !strings.Contains(pr.PlacementReason, deadURL+": probe (") {
		t.Fatalf("PlacementReason = %q, want the dead remote named with a probe(...) verdict", pr.PlacementReason)
	}
}
