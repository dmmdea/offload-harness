// placement_hold_test.go: ADR 0063 — placement HOLDS, it never sleeps on a node
// that just refused and never refuses work a node could take later.
//
// The pins here: re-placement re-reads the fleet (a snapshot taken before the
// refusal is never trusted) and asks only nodes that pass the room, backlog and
// cooldown checks; a local-leg capacity defer is re-placeable while a remote's
// post-ack outcome never is; the local seat is a re-placement candidate only
// when the seat-cap FIFO has a free slot; the queue budget is derived from the
// node's own ETA; and a node whose backlog outlasts the caller's patience is
// held out (a placement FEASIBILITY refusal that prints its arithmetic, the
// same class as feasibleFinal — never a pass rule, never a speed preference)
// and re-read every tick, never refused for good.

package delegate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
)

// slowNodeShaped is the reference slow node of the 2026-09-29 diagnosis: one
// worker, busy, a 443.6 s median wall, admission ceiling 2. A job sent there
// cannot START inside 300 s, yet its queue depth (1) sits under the limit (2).
func slowNodeShaped() NodeView {
	v := eligibleRemote()
	v.NodeID = "slow-node"
	v.AgentCtxTokens = 32768
	v.MaxConcurrentJobs, v.JobsRunning, v.JobsQueued = 1, 1, 0
	v.QueueDepth, v.MaxQueueDepth = 1, 2
	v.RecentAgentWallSec = 443.6
	return v
}

// slowNodeTune makes a fake node advertise slowNodeShaped's numbers on health.
func slowNodeTune(f *fakeNode) {
	f.maxConcurrentJobs, f.jobsRunning, f.queueDepth, f.maxQueueDepth = 1, 1, 1, 2
	f.recentAgentWallSec = 443.6
}

// capacityDeferLocal is a local seat whose run cap held the contract in line
// for its wall and then refused: the shape runAgentTask's seat-cap defer has
// (deferred, class capacity, zero steps, the wait on the wire).
func capacityDeferLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Deferred: true, DeferClass: core.DeferClassCapacity, AdmissionWaitSec: 0.05,
			Reason: "seat busy: seat local-seat is at its local run cap (4 runs ahead of this one, cap 4) after waiting 5m0s"}, nil
	}
}

// ---- re-placement ---------------------------------------------------------

// TestRunSpreadReplacementReprobesTheFleet: route=spread deals from ONE
// snapshot taken at the start of the run, and re-placement used to reuse it —
// so a node that the run's own siblings had filled since was still "the node
// with room". It reads the fleet again, and a memoised snapshot that predates
// the refusal is not a re-read.
func TestRunSpreadReplacementReprobesTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var slots atomic.Int64 // node-b's occupied slots, as its live health reports them
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth = 1, 1
		f.jobsRunningFn = func() int { return int(slots.Load()) }
		f.queueDepthFn = func() int { return int(slots.Load()) }
	})
	r := &runner{cfg: testCfg(t), route: "spread", remotes: []string{bURL}}
	// The run-start snapshot: node-b has room. It is also the run's probe memo,
	// well inside its 2 s life when the refusal comes.
	r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(t.Context())
	if len(r.spreadViews) != 1 || saturated(r.spreadViews[0]) {
		t.Fatalf("fixture: the run-start snapshot must show room on node-b, got %+v", r.spreadViews)
	}
	slots.Store(1) // a sibling took node-b's last slot after that probe
	pl := newPlacements()
	pl.tried["http://192.0.2.10:18811"] = true // the node that just refused (never dialled)
	pl.capacityRefusal = true

	probesBefore := b.healths.Load()
	chosen, why, ok := r.replacementNode(t.Context(), plainContract(), pl, 1)
	if ok && chosen.base == bURL {
		t.Fatalf("re-placed on node-b (%s) although its live health says it is full — the run-start snapshot was reused", chosen.reason)
	}
	if b.healths.Load() == probesBefore {
		t.Fatalf("replacementNode never re-read node-b's health (why = %q)", why)
	}
}

// TestRunReplacementSkipsANodeThatJustRefusedForCapacity: node A refuses with a
// Retry-After and node B is full right now. The wait must not ask A again while
// its cooldown runs (its health still advertises the room it just denied), and
// must place the subtask on B the moment B frees.
func TestRunReplacementSkipsANodeThatJustRefusedForCapacity(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	compressWallUnit(t, 100*time.Millisecond) // A's Retry-After of 60 "s" is a 6 s cooldown
	var bFree atomic.Bool
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = "60" })
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
		f.maxQueueDepth = 1
		f.queueDepthFn = func() int {
			if bFree.Load() {
				return 0
			}
			return 1 // at its admission ceiling: no room
		}
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 20
	go func() {
		time.Sleep(300 * time.Millisecond)
		bFree.Store(true)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	results, sum, err := RunWith(ctx, cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-b" {
		t.Fatalf("summary = %+v node = %q, want the subtask placed on node-b once it freed", sum, results[0].Node)
	}
	if got := a.dispatches.Load(); got != 1 {
		t.Fatalf("node-a saw %d dispatches, want exactly 1 — it was asked again inside its own Retry-After cooldown", got)
	}
	if got := b.dispatches.Load(); got != 1 {
		t.Fatalf("node-b saw %d dispatches, want 1", got)
	}
}

// TestRunPlacedAfterA202IsNeverReplaced is the guard the new re-placement paths
// must not weaken: once a node ACKED (202) the job, nothing it later says moves
// the contract to another node — a queue deadline leaves it to the node, and a
// capacity defer the node itself reports is an observed terminal, not a refusal
// (only the LOCAL leg's capacity defer, which no node ever owned, is re-placed).
func TestRunPlacedAfterA202IsNeverReplaced(t *testing.T) {
	t.Run("queue deadline", func(t *testing.T) {
		compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
		a := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "node-a",
			pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "accepted"}, http.StatusOK },
		}
		aURL := a.server().URL
		b, bURL := acceptingNode(t, "node-b", "answer from b", nil)
		contract := plainContract()
		contract.TimeoutSec = 1
		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{aURL, bURL})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.HasPrefix(results[0].Err, "queue deadline") || sum.Failed != 1 {
			t.Fatalf("err = %q summary = %+v, want the queue-deadline failure of the node that owns the job", results[0].Err, sum)
		}
		if b.dispatches.Load() != 0 {
			t.Fatalf("node-b saw %d dispatches — a job node-a acked was re-placed", b.dispatches.Load())
		}
	})
	t.Run("capacity defer reported by the node", func(t *testing.T) {
		compressPolls(t, 10*time.Millisecond, time.Second)
		a := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "node-a",
			pollState: func(int64) (map[string]any, int) {
				w := remoteWire("", "")
				w.NodeID, w.Steps, w.StopReason = "node-a", 0, ""
				w.Deferred, w.DeferClass, w.Reason = true, core.DeferClassCapacity, "seat busy: node-a's seat is at its run cap"
				return doneWire(t, w), http.StatusOK
			},
		}
		aURL := a.server().URL
		b, bURL := acceptingNode(t, "node-b", "answer from b", nil)
		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum.Deferred != 1 || results[0].Result.DeferClass != core.DeferClassCapacity {
			t.Fatalf("summary = %+v result = %+v, want node-a's own capacity defer published as it came", sum, results[0].Result)
		}
		if b.dispatches.Load() != 0 {
			t.Fatalf("node-b saw %d dispatches — a defer node-a filed after its ack was re-placed", b.dispatches.Load())
		}
	})
}

// ---- local-leg capacity defers -------------------------------------------

// TestRunLocalSeatBusyDeferIsReplacedOnARemoteWithRoom: the local seat held the
// contract at its run cap and deferred it (capacity, zero steps). Four comments
// called that re-placeable and nothing consumed it: 26 of 52 jobs re-placed onto
// the local seat died there. A node with room takes it now.
func TestRunLocalSeatBusyDeferIsReplacedOnARemoteWithRoom(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-room", "answer from the remote", nil)
	var localCalls atomic.Int64
	results, sum, err := Run(t.Context(), testCfg(t), capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-room" {
		t.Fatalf("summary = %+v node = %q, want the contract re-placed and done on node-room", sum, results[0].Node)
	}
	if localCalls.Load() != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("local ran %d times and the node saw %d dispatches, want 1 and 1", localCalls.Load(), node.dispatches.Load())
	}
	if results[0].Replacements != 1 || !strings.Contains(results[0].ReplacementNote, "seat busy") {
		t.Fatalf("replacements = %d note = %q, want the local seat's defer named as the refusal", results[0].Replacements, results[0].ReplacementNote)
	}
}

// TestLocalCapacityDeferWaitIsCreditedNotCharged: the local run spent 2.5 s in the
// seat's own line before it deferred. That is queueing, so the re-placement is
// handed the whole budget the contract asked for, not the budget minus the line.
func TestLocalCapacityDeferWaitIsCreditedNotCharged(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var seen atomic.Int64
	_, url := acceptingNode(t, "node-room", "answer from the remote", func(f *fakeNode) {
		f.onDispatch = func(_ string, c core.AgentContract) { seen.Store(int64(c.TimeoutSec)) }
	})
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		time.Sleep(2500 * time.Millisecond) // waited in the seat's own line
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Deferred: true, DeferClass: core.DeferClassCapacity, AdmissionWaitSec: 2.5,
			Reason: "seat busy: seat local-seat is at its local run cap after waiting 2.5s"}, nil
	}
	contract := plainContract() // TimeoutSec: 30
	_, sum, err := Run(t.Context(), testCfg(t), local, []core.AgentContract{contract}, "auto", []string{url})
	if err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: summary %+v err %v, want the contract re-placed and done", sum, err)
	}
	if got := seen.Load(); got < int64(contract.TimeoutSec-2) {
		t.Fatalf("the re-placed contract carried timeout_sec=%d after a 2.5 s wait in the seat's line, want >= %d — the wait was charged", got, contract.TimeoutSec-2)
	}
}

// TestRunLocalCapacityDeferOnRouteLocalWaitsInPlace: route=local has nowhere to
// re-place to — the seat's own line (bounded by the run's wall) is the wait —
// so its capacity defer is the answer, and no remote is asked.
func TestRunLocalCapacityDeferOnRouteLocalWaitsInPlace(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-room", "answer from the remote", nil)
	var localCalls atomic.Int64
	results, sum, err := Run(t.Context(), testCfg(t), capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "local", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 || results[0].Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary = %+v result = %+v, want the local capacity defer as it came", sum, results[0].Result)
	}
	if node.dispatches.Load() != 0 || localCalls.Load() != 1 {
		t.Fatalf("node saw %d dispatches and local ran %d times, want 0 and 1 — route=local never leaves the box", node.dispatches.Load(), localCalls.Load())
	}
}

// TestRunLocalCapacityDeferStaysADeferWhenNothingElseHasRoom: with every remote
// full and the wait switched off, the local seat's capacity defer is published
// as the defer it is — never converted into a "placement refused" FAILURE, which
// would tell the caller no node was willing when the fleet was merely busy.
func TestRunLocalCapacityDeferStaysADeferWhenNothingElseHasRoom(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-full", "answer", func(f *fakeNode) {
		f.maxQueueDepth, f.queueDepth = 1, 1 // saturated: no room
	})
	var localCalls atomic.Int64
	results, sum, err := Run(t.Context(), testCfg(t), capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if sum.Deferred != 1 || sum.Failed != 0 || pr.Err != "" {
		t.Fatalf("summary = %+v err = %q, want a DEFER (capacity), not a failure", sum, pr.Err)
	}
	if pr.Result.DeferClass != core.DeferClassCapacity || !strings.Contains(pr.Result.Reason, "seat busy") {
		t.Fatalf("result = %+v, want the local seat's own capacity defer", pr.Result)
	}
	if node.dispatches.Load() != 0 {
		t.Fatalf("the full node saw %d dispatches, want 0", node.dispatches.Load())
	}
}

// TestReplacementDoesNotLandOnAFullLocalSeat: the local seat is a re-placement
// candidate only when the run-cap FIFO has a free slot ahead of a newcomer. A
// full seat used to be the reserved last resort: the subtask joined its line at
// once and died there (26 of the day's 52 local re-placements). Now the capacity
// wait holds it until a slot frees — and then runs it.
func TestReplacementDoesNotLandOnAFullLocalSeat(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.GPULockPath = busyLocal(t) // a media lease: the local seat is "busy", so route=auto asks the remote first
	cfg.FleetMaxConcurrentJobs = 1
	cfg.AgentPlacementWaitSec = 10

	seatRun := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: cfg.AgentPlannerModel(""), Kind: "contract", Goal: "occupies the seat", Phase: gpuactivity.PhaseRunning})
	if seatRun == nil {
		t.Fatal("fixture: could not register a run on the local seat")
	}
	var freedAt, ranAt atomic.Int64
	go func() {
		time.Sleep(300 * time.Millisecond)
		freedAt.Store(time.Now().UnixNano())
		seatRun.End()
	}()
	local := func(ctx context.Context, c core.AgentContract, o LocalOptions) (core.AgentWireResult, error) {
		ranAt.Store(time.Now().UnixNano())
		return passingLocal(new(atomic.Int64))(ctx, c, o)
	}

	results, sum, err := RunWith(t.Context(), cfg, local, []core.AgentContract{plainContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the subtask to run locally once the seat had a slot", sum)
	}
	if ranAt.Load() == 0 || freedAt.Load() == 0 || ranAt.Load() < freedAt.Load() {
		t.Fatalf("the local seat ran the subtask before its run-cap line had a free slot (ran %d, freed %d) — a full seat was a candidate", ranAt.Load(), freedAt.Load())
	}
	if !strings.Contains(results[0].PlacementReason, "capacity wait") {
		t.Fatalf("placement = %q, want the wait named", results[0].PlacementReason)
	}
}

// ---- the queue budget and the backlog gate --------------------------------

// TestQueueBudgetIsDerivedFromTheNodesETA: a node that says a new job waits 480 s
// is given 1.5 x 480 + 30 s = 750 s to start it — not the fixed five minutes
// that abandoned jobs a node was about to run (58 queue deadlines on 09-29, the
// node running every one of them anyway). Clamped to [60 s, the caller's patience].
func TestQueueBudgetIsDerivedFromTheNodesETA(t *testing.T) {
	est := func(sec float64) NodeView { v := slowNodeShaped(); v.QueueWaitEstimateSec = &sec; return v }
	if got := queueBudgetFor(est(480), 20*time.Minute); got < 750*time.Second || got != 750*time.Second {
		t.Fatalf("budget for a node with a 480 s ETA = %s, want 12m30s (1.5 x 480 + 30 s, past the old 5 m ceiling)", got)
	}
	if got := queueBudgetFor(est(480), 600*time.Second); got != 600*time.Second {
		t.Fatalf("budget = %s, want the caller's 600 s patience to cap it", got)
	}
	if got := queueBudgetFor(est(0), 20*time.Minute); got != 60*time.Second {
		t.Fatalf("budget for a node saying no wait = %s, want the 60 s floor", got)
	}
	if got := queueBudgetFor(est(0), 45*time.Second); got != 45*time.Second {
		t.Fatalf("budget = %s, want a patience under the floor to win — a wait never outlasts what the caller gave it", got)
	}
	if got := queueBudgetFor(est(100), 20*time.Minute); got != 180*time.Second {
		t.Fatalf("budget for a 100 s ETA = %s, want 3m0s", got)
	}
}

// TestQueueBudgetKeepsTheOldCeilingWhenTheNodePublishesNoETA: no estimate and no
// recent wall is no opinion — never a shorter wait than the one that always
// applied (min(patience, 5 minutes)).
func TestQueueBudgetKeepsTheOldCeilingWhenTheNodePublishesNoETA(t *testing.T) {
	if got := queueBudgetFor(NodeView{}, 20*time.Minute); got != maxQueuedWait {
		t.Fatalf("budget for a node with no ETA = %s, want the old %s ceiling", got, maxQueuedWait)
	}
	if got := queueBudgetFor(NodeView{}, 100*time.Second); got != 100*time.Second {
		t.Fatalf("budget = %s, want the 100 s patience", got)
	}
}

// TestQueueDeadlineIsNoLongerFixedAtFiveMinutes: through the real poll loop. A
// node that publishes "no wait" and then never starts the job is given up on at
// the 60 s floor (compressed here), not at the contract's whole poll budget.
func TestQueueDeadlineIsNoLongerFixedAtFiveMinutes(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // 60 "s" = 300 ms; the 600 s contract's poll budget = 3 s
	zero := 0.0
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		queueWaitEstimate: &zero,
		pollState:         func(int64) (map[string]any, int) { return map[string]any{"state": "accepted"}, http.StatusOK },
	}
	srv := node.server()
	contract := plainContract()
	contract.TimeoutSec = 600

	began := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(began)
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue-deadline failure", sum, results[0].Err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("gave up after %s — the queue budget was the whole poll budget, not derived from the node's 'no wait' ETA (floor 300 ms here)", elapsed)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("gave up after only %s — under the 60 s floor", elapsed)
	}
}

// TestQueueBudgetIsRefreshedWhenTheJobSitsQueued: the budget is derived from the
// ETA of the snapshot the job was placed from, and a spread run's snapshot can be
// minutes old. The moment a job is seen sitting in the node's backlog the delegator
// reads the node's ETA AGAIN and, when it is longer, extends the wait to match -
// otherwise a node that filled up after the snapshot would have its job abandoned at
// the 60 s floor while it was about to start it (and it would run anyway).
func TestQueueBudgetIsRefreshedWhenTheJobSitsQueued(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // the 60 s floor is 300 ms; a 200 s ETA earns 330 s = 1.65 s
	var healths atomic.Int64
	zero, busy := 0.0, 200.0
	begin := time.Now()
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		// The placement snapshot says "no wait"; every later read says 200 s.
		queueWaitEstimateFn: func() *float64 {
			if healths.Add(1) == 1 {
				return &zero
			}
			return &busy
		},
		pollState: func(int64) (map[string]any, int) {
			if time.Since(begin) < 900*time.Millisecond { // queued behind other tenants' jobs
				return map[string]any{"state": "accepted"}, http.StatusOK
			}
			return doneWire(t, remoteWire("the answer", `{"answer":"the answer"}`)), http.StatusOK
		},
	}
	srv := node.server()
	contract := plainContract()
	contract.TimeoutSec = 600

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q, want the job to have been waited for: its node said 200 s when read again", sum, results[0].Err)
	}
}

// TestReplacementPatienceIsWhatIsLeftOfTheBudget: the backlog gate holds a
// re-placement to the wall it will actually be dispatched with - what the first
// attempt left - not the contract's original one. The second node needs 29.5 s to
// start a job: inside the whole 30 s wall (and its grace), outside the ~27 s that
// remain after the first node spent 2.5 s before refusing. Sent there, the job would
// be given up on before it started, and the node would run it anyway.
func TestReplacementPatienceIsWhatIsLeftOfTheBudget(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			time.Sleep(2500 * time.Millisecond)
			return http.StatusServiceUnavailable
		}
	})
	eta := 29.5
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) { f.queueWaitEstimate = &eta })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.dispatches.Load(); got != 0 {
		t.Fatalf("node-b (29.5 s to start) was dispatched %d time(s) with ~27 s of the budget left — the gate used the contract's original wall", got)
	}
	if sum.Deferred != 1 || results[0].Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary = %+v result = %+v, want the capacity defer of a subtask no node could start in time", sum, results[0].Result)
	}
}

// TestCapacityWaitSurvivesAGateTakenBetweenTheReadAndTheDispatch: another Run of
// the process can take the chosen node's last gate slot in the window between the
// wait reading the fleet and dispatching. That dispatch is turned away without
// reaching the node: it is neither a refusal nor a placement, the wait goes on, and
// the subtask lands when the slot frees.
func TestCapacityWaitSurvivesAGateTakenBetweenTheReadAndTheDispatch(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-a", "answer from a", func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 1, 1 })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	var once sync.Once
	r.beforeForced = func(base string) {
		once.Do(func() {
			rel, ok := processGate.tryAcquire(base, 1) // a sibling Run takes the last slot
			if !ok {
				t.Error("fixture: the node's gate was not free")
				return
			}
			go func() {
				time.Sleep(150 * time.Millisecond)
				rel()
			}()
		})
	}
	pl := newPlacements()
	seed := PlacedResult{waitCapacity: true, pendingReason: "test: nothing had room"}
	pr := r.awaitCapacity(t.Context(), 0, plainContract(), time.Now(), 30, pl, seed, nil, "")
	if pr.Err != "" || pr.Result.Deferred || pr.Result.Output == "" {
		t.Fatalf("result = err %q deferred %v output %q — the wait published a turned-away dispatch as the subtask's outcome", pr.Err, pr.Result.Deferred, pr.Result.Output)
	}
	if node.dispatches.Load() != 1 || pl.attempts != 1 {
		t.Fatalf("node saw %d dispatches and the subtask counts %d placements, want exactly 1 each (a turned-away dispatch is no placement)", node.dispatches.Load(), pl.attempts)
	}
	if pr.Replacements != 0 {
		t.Fatalf("replacements = %d, want 0: the gate turned the dispatch away, no node refused it", pr.Replacements)
	}
}

// TestETAArithmeticSurvivesAbsurdEstimates: a node publishes whatever it publishes.
// An estimate too large for a Duration must read as "far too long" (held out, and a
// queue budget capped at the caller's patience), never overflow into a small or
// negative wait; a negative estimate is a node bug and reads as "no wait".
func TestETAArithmeticSurvivesAbsurdEstimates(t *testing.T) {
	huge, negative := 1e15, -50.0
	v := slowNodeShaped()
	v.QueueWaitEstimateSec = &huge
	if ok, _ := startsWithinPatience(v, 20*time.Minute); ok {
		t.Fatal("a 1e15 s ETA fits a 20 minute patience — the arithmetic overflowed")
	}
	if got := queueBudgetFor(v, 20*time.Minute); got != 20*time.Minute {
		t.Fatalf("queue budget for a 1e15 s ETA = %s, want it capped at the 20 minute patience", got)
	}
	v.QueueWaitEstimateSec = &negative
	if sec, known := etaStartFor(v); !known || sec != 0 {
		t.Fatalf("etaStartFor(-50) = %v/%v, want a known zero", sec, known)
	}
	if ok, _ := startsWithinPatience(v, time.Second); !ok {
		t.Fatal("a negative estimate must read as no wait")
	}
	if got := queueBudgetFor(v, 20*time.Minute); got != 60*time.Second {
		t.Fatalf("queue budget for a negative estimate = %s, want the 60 s floor", got)
	}
}

// TestHasRoomRefusesANodeWhoseBacklogExceedsThePatience: the slow node's queue
// depth (1) is under its admission limit (2), so hasRoom said yes — and a job
// sent there waited 444 s, was abandoned at the 300 s deadline, and ran anyway.
func TestHasRoomRefusesANodeWhoseBacklogExceedsThePatience(t *testing.T) {
	v := slowNodeShaped()
	if !hasRoom(v, false) {
		t.Fatal("fixture: the node's queue depth is under its limit, so its raw capacity check passes")
	}
	if hasRoomWithin(v, false, 300*time.Second) {
		t.Fatal("hasRoomWithin(444 s to start, 300 s patience) = true, want the node held out")
	}
	if !hasRoomWithin(v, false, 600*time.Second) {
		t.Fatal("a 600 s patience covers a 444 s start; the node must be a candidate")
	}
	if !hasRoomWithin(v, false, 0) {
		t.Fatal("no patience bound (0) must leave the capacity check unchanged")
	}
	if !hasRoomWithin(NodeView{}, false, 1*time.Second) {
		t.Fatal("a node publishing no ETA is no opinion: it must stay a candidate")
	}
}

// TestStartsWithinPatiencePrintsItsArithmetic: the gate is a feasibility refusal
// and says how it got there — the node's numbers, the patience, nothing about
// which seat is faster.
func TestStartsWithinPatiencePrintsItsArithmetic(t *testing.T) {
	v := slowNodeShaped()
	ok, why := startsWithinPatience(v, 300*time.Second)
	if ok {
		t.Fatal("a 444 s start must not fit a 300 s patience")
	}
	for _, want := range []string{"444 s", "300 s", "1 running", "0 queued", "1 worker", "443.6"} {
		if !strings.Contains(why, want) {
			t.Errorf("reason = %q, want it to contain %q", why, want)
		}
	}
	est := 480.0
	v.QueueWaitEstimateSec = &est
	if ok, why := startsWithinPatience(v, 300*time.Second); ok || !strings.Contains(why, "queue_wait_estimate_sec") || !strings.Contains(why, "480 s") {
		t.Fatalf("ok = %v reason = %q, want the node's own estimate named", ok, why)
	}
	if ok, why := startsWithinPatience(v, 0); !ok || why != "" {
		t.Fatalf("patience 0 = no bound; got ok=%v %q", ok, why)
	}
}

// TestCapacityWaitDoesNotPlaceOnANodeThatCannotStartInTime: the only node is
// slow-node-shaped. The capacity wait re-reads it every tick and never dispatches
// to it (a held-out node is re-read, not refused for good); the outcome is a
// capacity defer that names the arithmetic. Before, hasRoom said yes at depth
// 1 < limit 2 and the wait dispatched the job to a node that could not start it.
func TestCapacityWaitDoesNotPlaceOnANodeThatCannotStartInTime(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "slow-node", "answer", slowNodeTune)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	contract := plainContract()
	contract.TimeoutSec = 300

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := node.dispatches.Load(); got != 0 {
		t.Fatalf("the node that cannot start a job inside its patience was dispatched %d time(s)", got)
	}
	pr := results[0]
	if sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary = %+v result = %+v, want a capacity defer", sum, pr.Result)
	}
	// The caller's patience is the contract's poll budget: its 300 s wall plus the
	// (compressed) grace.
	patience := fmt.Sprintf("past the %.0f s", (300*time.Second + pollGrace).Seconds())
	for _, want := range []string{"slow-node", "444 s", patience, "backlog"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("defer reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
	if healths := node.healths.Load(); healths < 3 {
		t.Fatalf("the held-out node's health was read %d time(s) — it must be re-read every tick", healths)
	}
}

// TestDealHoldsBackANodeThatCannotStartInTime: the joint deal (route=auto/remote)
// gives the slow node nothing while another node can start the job in time, and
// the placement reason says why in a word and a number.
func TestDealHoldsBackANodeThatCannotStartInTime(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	slow := slowNodeShaped()
	fast := eligibleRemote()
	fast.NodeID = "fast-node"
	r := &runner{route: "remote"}
	dealt := map[string]int{}
	slot := r.placeAutoRemote("seed", st, localNode(), []NodeView{slow, fast}, []string{"http://slow", "http://fast"}, true, dealt, nil)
	if slot.capacityWait || slot.base != "http://fast" {
		t.Fatalf("slot = %+v, want the deal on fast-node", slot.placement)
	}
	if dealt["http://slow"] != 0 {
		t.Fatalf("the slow node was dealt %d subtask(s)", dealt["http://slow"])
	}
	if want := "slow-node: backlog ("; !strings.Contains(slot.reason, want) || !strings.Contains(slot.reason, "444 s") {
		t.Fatalf("reason = %q, want the held-out node narrated as %q with its arithmetic", slot.reason, want)
	}
	// With nowhere else, the subtask waits for capacity instead of being dealt.
	only := r.placeAutoRemote("seed", st, localNode(), []NodeView{slow}, []string{"http://slow"}, true, map[string]int{}, nil)
	if !only.capacityWait {
		t.Fatalf("slot = %+v, want the capacity wait — the only node cannot start the job in time", only.placement)
	}
}

// TestSpreadDealHoldsBackANodeThatCannotStartInTime is the same rule for
// route=spread's rotation.
func TestSpreadDealHoldsBackANodeThatCannotStartInTime(t *testing.T) {
	slow := slowNodeShaped()
	slow.AgentCtxTokens = 131072
	fast := fitMidRemote
	fast.NodeID = "fast-node"
	r := fitRunner(slow, fast)
	r.spreadLease.Held = true // a text lease takes the local seat out of the rotation
	r.spreadLease.Class = "text"
	contracts := make([]core.AgentContract, 4)
	for i := range contracts {
		contracts[i] = fitSubtask(fitMechGoal, 100).Contract
		contracts[i].TimeoutSec = 300
	}
	for i, sl := range r.dealSpread(contracts, fitLocal()) {
		if sl.view.NodeID == "slow-node" {
			t.Fatalf("subtask %d was dealt to the node that cannot start it inside the caller's patience (%s)", i, sl.reason)
		}
	}
}

// ---- helpers --------------------------------------------------------------

var _ = fmt.Sprintf

// plainContract is remoteContract with an acceptance any non-empty answer passes:
// the tests that are not about verification use it, so their nodes can answer in
// plain words.
func plainContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{"nonempty:answer"}
	return c
}

// verifiedContract is remoteContract with an acceptance only an answer containing
// "verified" passes: the tests that need a first attempt to FAIL verification (and a
// retry or a page issue to succeed) use it.
func verifiedContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{"contains:verified", "nonempty:answer"}
	return c
}
