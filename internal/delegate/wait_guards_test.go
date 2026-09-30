// wait_guards_test.go: pins for the behaviour of the re-placement filters, the capacity wait and the
// local capacity defer that the first suite left unguarded (each of these tests was written against a
// one-line mutation of the production code that the whole package survived, and each now fails it).

package delegate

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// ---- withRoom: the filters and the busy list ------------------------------------

func TestWithRoomFiltersAndTheBusyList(t *testing.T) {
	mk := func(id string, mut func(*NodeView)) NodeView {
		v := eligibleRemote()
		v.NodeID = id
		v.AgentCtxTokens = 32768
		v.MaxConcurrentJobs, v.JobsRunning, v.QueueDepth, v.MaxQueueDepth = 4, 0, 0, 8
		if mut != nil {
			mut(&v)
		}
		return v
	}
	slowEst := 444.0
	views := []NodeView{
		mk("free", nil),
		mk("cooling", nil),
		mk("gate-full", func(v *NodeView) { v.MaxQueueDepth = 1 }),
		mk("no-idle-slot", func(v *NodeView) { v.MaxConcurrentJobs, v.JobsRunning, v.QueueDepth, v.MaxQueueDepth = 1, 1, 1, 4 }),
		mk("backlogged", func(v *NodeView) { v.QueueWaitEstimateSec = &slowEst }),
		mk("full", func(v *NodeView) { v.QueueDepth, v.MaxQueueDepth = 8, 8 }),
		mk("cannot-run-it", func(v *NodeView) { v.AgentEnabled = false }),
	}
	bases := []string{"http://192.0.2.61:1", "http://192.0.2.62:1", "http://192.0.2.63:1", "http://192.0.2.64:1", "http://192.0.2.65:1", "http://192.0.2.66:1", "http://192.0.2.67:1"}
	r := &runner{cfg: testCfg(t), route: "remote"}
	r.cool.hold(bases[1], time.Now().Add(time.Minute))
	holdGate(t, bases[2], 1, 1, 0)
	c := plainContract()
	c.TimeoutSec = 300
	st := Subtask{Contract: c, EstTokens: 100}

	names := func(vs []NodeView) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = v.NodeID
		}
		return strings.Join(out, ",")
	}
	gotV, gotB, busy := r.withRoom(st, views, bases)
	if got := names(gotV); got != "free,no-idle-slot" {
		t.Fatalf("normal band candidates = %q, want free,no-idle-slot (cooling, gate-full, backlogged and full held out)", got)
	}
	if len(gotB) != len(gotV) || gotB[0] != bases[0] || gotB[1] != bases[3] {
		t.Fatalf("bases = %v, want them index-parallel with the views", gotB)
	}
	joined := strings.Join(busy, " | ")
	for _, want := range []string{"cooling: cooling down after its own refusal", "gate-full: process gate", "backlogged: backlog (", "444 s", "full: no room (queue_depth 8 of max_queue_depth 8)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("busy = %q, want it to say %q", joined, want)
		}
	}
	if strings.Contains(joined, "cannot-run-it") {
		t.Errorf("busy = %q lists a node the contract can never run on: waiting for it is a wait for nothing", joined)
	}
	if len(busy) != 4 {
		t.Errorf("busy = %q, want exactly the four eligible nodes that were held out for being busy", joined)
	}

	// A sheddable run takes only a node with an idle execution slot.
	r.priority = core.BandSheddable
	gotV, _, busy = r.withRoom(st, views, bases)
	if got := names(gotV); got != "free" {
		t.Fatalf("sheddable band candidates = %q, want free (a sheddable run needs an idle slot)", got)
	}
	if !strings.Contains(strings.Join(busy, " | "), "no-idle-slot: no room (no idle execution slot") {
		t.Errorf("busy = %q, want the sheddable rule named for no-idle-slot", strings.Join(busy, " | "))
	}
}

// ---- refusals filed by the chain and by the wait -------------------------------

// TestALaterRefusalInTheChainCoolsItsNodeToo: a refusal met on the SECOND node of a chain
// cools that node exactly as the first one did (a and b each refuse once with a long
// Retry-After; c is full for a moment). Without the cooldown the wait re-asks b.
func TestALaterRefusalInTheChainCoolsItsNodeToo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var cFree atomic.Bool
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = "300" })
	b, bURL := refusingNode(t, "node-b", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = "300" })
	c, cURL := acceptingNode(t, "node-c", "answer from c", func(f *fakeNode) {
		f.maxQueueDepth = 1
		f.queueDepthFn = func() int {
			if cFree.Load() {
				return 0
			}
			return 1
		}
	})
	go func() {
		time.Sleep(600 * time.Millisecond)
		cFree.Store(true)
	}()
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL, cURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-c" {
		t.Fatalf("summary = %+v node = %q, want the subtask on node-c once it frees", sum, results[0].Node)
	}
	if a.dispatches.Load() != 1 || b.dispatches.Load() != 1 || c.dispatches.Load() != 1 {
		t.Fatalf("dispatches a=%d b=%d c=%d, want 1 each: a refusal met later in the chain must cool its node exactly like the first one did", a.dispatches.Load(), b.dispatches.Load(), c.dispatches.Load())
	}
}

// TestARefusalInsideTheWaitCoolsItsNode: a node that refuses inside the wait is not asked
// again every tick (20 ms here): its Retry-After is its cooldown.
func TestARefusalInsideTheWaitCoolsItsNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	compressWallUnit(t, 100*time.Millisecond) // Retry-After: 2 "s" = a 200 ms cooldown
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "2"
		f.dispatchHook = func(int64) int { return http.StatusServiceUnavailable }
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	_, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deferred != 1 {
		t.Fatalf("summary = %+v, want the capacity defer of a node that never had room", sum)
	}
	// 1 s of wait at a 200 ms (jittered 160-240 ms) cooldown: about 5 asks. Without the cooldown the 20 ms tick asks ~50 times.
	if got := a.dispatches.Load(); got > 12 {
		t.Fatalf("the node saw %d dispatches in a 1 s wait at a 200 ms cooldown, want about 5 - the wait is re-asking a node that just refused it every tick", got)
	}
}

// ---- the wait judges its candidates against what is left of the budget --------

// TestWaitJudgesTheBacklogGateAgainstWhatIsLeftOfTheBudget: node A refuses after spending 2.5 s of
// the 30 s wall and stays cooling for the whole wait, so node B is the wait's only candidate. B's 29.5 s
// start fits the contract's whole wall and does not fit the ~27 s that remain: the wait must hold it
// out, exactly as re-placement does, or it hands B a job the delegator abandons while B runs it.
func TestWaitJudgesTheBacklogGateAgainstWhatIsLeftOfTheBudget(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			time.Sleep(2500 * time.Millisecond)
			return http.StatusServiceUnavailable
		}
		f.dispatchRetryAfter = "300"
	})
	eta := 29.5
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) { f.queueWaitEstimate = &eta })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.dispatches.Load(); got != 0 {
		t.Fatalf("node-b (29.5 s to start) was dispatched %d time(s) by the capacity wait with ~27 s of the budget left - the wait judged it against the original 30 s wall", got)
	}
	if sum.Deferred != 1 {
		t.Fatalf("summary = %+v reason = %q, want the capacity defer of a subtask no node could start in time", sum, results[0].Result.Reason)
	}
}

// TestWaitSkipsAGateFullNodeForOneThatHasRoom: other Runs of this process hold node A's whole admission
// ceiling open, so the wait must pass over A - the better-ranked node - for node B, which has room.
func TestWaitSkipsAGateFullNodeForOneThatHasRoom(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	a, aURL := acceptingNode(t, "node-a", "answer from a", func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 4, 4 })
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth, f.jobsRunning, f.queueDepth = 4, 4, 1, 1
	})
	holdGate(t, aURL, 4, 4, 0)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-b" || a.dispatches.Load() != 0 || b.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v node = %q a=%d b=%d reason = %q, want the wait to skip the gate-full node-a and land on node-b, which has room", sum, results[0].Node, a.dispatches.Load(), b.dispatches.Load(), results[0].Result.Reason)
	}
}

// TestWaitDropsAHeldOutNodeThatNoLongerIsHeldOut: node A's backlog clears after two reads, it is then
// asked, refuses for a long Retry-After and cools for the rest of the wait. The defer must not go on
// claiming A was held out by the backlog gate: only the LAST tick's read decides who is held out.
func TestWaitDropsAHeldOutNodeThatNoLongerIsHeldOut(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var reads atomic.Int64
	slow, none := 444.0, 0.0
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "300"
		f.queueWaitEstimateFn = func() *float64 {
			if reads.Add(1) <= 3 {
				return &slow
			}
			return &none
		}
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	c := plainContract()
	c.TimeoutSec = 300
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{c}, "remote", []string{aURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reason := results[0].Result.Reason
	if sum.Deferred != 1 || a.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v dispatches = %d reason = %q, want the node asked once after its backlog cleared (fixture)", sum, a.dispatches.Load(), reason)
	}
	if strings.Contains(reason, "held out by the backlog gate") {
		t.Fatalf("reason = %q still claims a node the last tick did not hold out was held out", reason)
	}
}

func TestHeldOutNotePrintsEachNodesArithmeticOnce(t *testing.T) {
	held := map[string]string{"http://192.0.2.1:1": "node-a: backlog (444 s to start)", "http://192.0.2.2:1": "node-b: backlog (500 s to start)"}
	got := heldOutNote(held, "1 refusal(s): node-a: backlog (444 s to start)")
	if !strings.Contains(got, "node-b: backlog (500 s to start)") || strings.Contains(got, "node-a") {
		t.Fatalf("note = %q, want only node-b: the deal's own verdict line already carries node-a's arithmetic verbatim", got)
	}
	if got := heldOutNote(held, "node-a: backlog (444 s to start); node-b: backlog (500 s to start)"); got != "" {
		t.Fatalf("note = %q, want nothing when every held-out node is already named", got)
	}
	if got := heldOutNote(nil, ""); got != "" {
		t.Fatalf("note = %q for no held-out node", got)
	}
}

// ---- the local seat's capacity defer -------------------------------------------

// TestLocalCapacityDeferThenAFullRemoteWaitsInsteadOfDeferring: the local seat deferred the run as
// capacity and the only remote is full for a moment. That is a busy fleet, not a finished one: the
// subtask waits for the remote (INV-4). Counted, not timed: the remote is full for its first two health
// reads, so a starved CPU cannot free it before the delegator has looked.
func TestLocalCapacityDeferThenAFullRemoteWaitsInsteadOfDeferring(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var reads atomic.Int64
	node, url := acceptingNode(t, "node-busy", "answer from the remote", func(f *fakeNode) {
		f.maxQueueDepth = 1
		f.queueDepthFn = func() int {
			if reads.Add(1) >= 3 {
				return 0
			}
			return 1
		}
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-busy" || node.dispatches.Load() != 1 || localCalls.Load() != 1 {
		t.Fatalf("summary = %+v node = %q reason = %q local = %d remote = %d, want the subtask to wait for the full remote and land there (INV-4)", sum, results[0].Node, results[0].Result.Reason, localCalls.Load(), node.dispatches.Load())
	}
}

// TestLocalCapacityDeferWithNoRemotesIsPublishedAtOnce: with no remote configured there is no other
// node to offer the work to, so the local seat's own defer is the answer - not a wait for a node that
// does not exist.
func TestLocalCapacityDeferWithNoRemotesIsPublishedAtOnce(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 2
	var localCalls atomic.Int64
	start := time.Now()
	results, sum, err := Run(t.Context(), cfg, capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deferred != 1 || !strings.Contains(results[0].Result.Reason, "seat busy") {
		t.Fatalf("summary = %+v result = %+v, want the local seat's own capacity defer", sum, results[0].Result)
	}
	if strings.Contains(results[0].Result.Reason, "capacity wait: no node had room") {
		t.Fatalf("reason = %q: a delegator with no remotes waited for a node that does not exist", results[0].Result.Reason)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s: with no remote to re-place to, the local defer must be published at once", elapsed)
	}
}

// TestLocalCapacityDeferWithOnlyAnIneligibleRemoteIsNotWaitedOn: the remote exists but can never run the
// contract, so a wait for it would be a wait for nothing (the whole placement wait, 2 s here, 120 s in
// production) before publishing the same defer. Only a node that is merely BUSY is worth waiting for.
func TestLocalCapacityDeferWithOnlyAnIneligibleRemoteIsNotWaitedOn(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-off", "answer", func(f *fakeNode) { f.agentEnabled = false })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 2
	var localCalls atomic.Int64
	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s: the subtask waited for a remote that can never run the contract", elapsed)
	}
	if node.dispatches.Load() != 0 || sum.Deferred != 1 || !strings.Contains(results[0].Result.Reason, "seat busy") {
		t.Fatalf("summary = %+v result = %+v, want the local seat's own capacity defer published at once", sum, results[0].Result)
	}
}

// TestLocalDeferThatIsNotACapacityDeferIsNeverReplaced: only a capacity defer with no step run is
// re-placeable - a seat that ran steps has owned the contract (a double-run hazard), and a class
// other than capacity is not about a full line.
func TestLocalDeferThatIsNotACapacityDeferIsNeverReplaced(t *testing.T) {
	cases := map[string]struct {
		steps int
		class string
	}{
		"capacity defer after steps ran": {2, core.DeferClassCapacity},
		"infrastructure defer, no steps": {0, core.DeferClassInfrastructure},
		"budget defer, no steps":         {0, core.DeferClassBudget},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			node, url := acceptingNode(t, "node-room", "answer from the remote", nil)
			local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
				return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
					Deferred: true, DeferClass: tc.class, Steps: tc.steps, Reason: "the local seat's own reason"}, nil
			}
			results, sum, err := Run(t.Context(), testCfg(t), local, []core.AgentContract{plainContract()}, "auto", []string{url})
			if err != nil {
				t.Fatal(err)
			}
			if node.dispatches.Load() != 0 || sum.Deferred != 1 || results[0].Result.DeferClass != tc.class {
				t.Fatalf("remote saw %d dispatches, summary %+v, class %q - the local defer was re-placed (a seat that ran steps, or a non-capacity class, must never move)", node.dispatches.Load(), sum, results[0].Result.DeferClass)
			}
		})
	}
}

// TestExhaustedLocalCapacityDeferShape: the local seat deferred as capacity after a remote's 503 and
// nothing else has room. It stays the defer it is - Unplaced (nothing ran it), never a recovered
// re-placement, the one real refusal counted once, and its own reason plus why no other node took it.
func TestExhaustedLocalCapacityDeferShape(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.GPULockPath = busyLocal(t) // media lease: route=auto asks the remote first
	var localCalls atomic.Int64
	results, sum, err := Run(t.Context(), cfg, capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Deferred != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want the local seat's capacity defer, not a failure", sum)
	}
	if !pr.Unplaced || sum.ReplacementRecovered != 0 {
		t.Fatalf("unplaced=%v recovered=%d: a defer no node ran must never count as a recovered re-placement", pr.Unplaced, sum.ReplacementRecovered)
	}
	if pr.Replacements != 1 || sum.Replaced != 1 {
		t.Fatalf("replacements=%d summary.replaced=%d, want the one real refusal (the 503) counted once", pr.Replacements, sum.Replaced)
	}
	if !strings.Contains(pr.Result.Reason, "no other node could take it") {
		t.Fatalf("reason = %q, want the story of why no other node took it", pr.Result.Reason)
	}
}

// TestWaitTriesTheLocalSeatOnceAndGoesOnWhenItDefers: the lease clears, the wait sends the subtask to
// the local seat, and the seat's line filled in the meantime - it defers as capacity. That is one more
// refusal, the seat is not asked twice, and the wait goes on for the remote.
func TestWaitTriesTheLocalSeatOnceAndGoesOnWhenItDefers(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var remoteOpen atomic.Bool
	node, url := acceptingNode(t, "node-remote", "answer from the remote", func(f *fakeNode) {
		f.dispatchHook = func(int64) int { // full until the test opens it
			if remoteOpen.Load() {
				return 0
			}
			return http.StatusServiceUnavailable
		}
	})
	dir, lease := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 10
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = lease.Release()
		time.Sleep(200 * time.Millisecond)
		remoteOpen.Store(true)
	}()
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, capacityDeferLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-remote" || localCalls.Load() != 1 {
		t.Fatalf("summary = %+v node = %q local asked %d times remote %d, want the seat asked ONCE (it deferred), then the remote once it opened", sum, results[0].Node, localCalls.Load(), node.dispatches.Load())
	}
	if !strings.Contains(results[0].ReplacementNote, "(local seat): deferred (capacity)") {
		t.Fatalf("replacement_note = %q, want the local seat's defer named as a refusal", results[0].ReplacementNote)
	}
}

// TestNoRemotesJoinsTheSeatsLineOnceTheLeaseClears: with no remote to spare the subtask for, the seat's own
// line IS the wait - it joins it as soon as no lease reserves the seat, even though the line is full.
func TestNoRemotesJoinsTheSeatsLineOnceTheLeaseClears(t *testing.T) {
	compressWait(t, 20*time.Millisecond, 0)
	dir, lease := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.FleetMaxConcurrentJobs = 1
	cfg.AgentPlacementWaitSec = 2
	occupant := gpuactivity.Start(cfg.GPULockPath, cfg.StateDir, gpuactivity.Run{Seat: cfg.AgentPlannerModel(""), Kind: "contract", Goal: "occupies the seat", Phase: gpuactivity.PhaseRunning})
	if occupant == nil {
		t.Fatal("fixture: could not register a run on the local seat")
	}
	t.Cleanup(occupant.End)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = lease.Release()
	}()
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), []core.AgentContract{plainContract()}, "auto", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 || localCalls.Load() != 1 {
		t.Fatalf("summary = %+v local = %d, want the subtask to join the seat's own line once the lease cleared", sum, localCalls.Load())
	}
}

// ---- the retry's alternative reads the fleet, on every route -------------------

// TestRetryAlternativeReadsTheFleetAgainOnSpread: a retry runs after a whole attempt, and route=spread's
// run-start snapshot is minutes old by then - so the alternative is read fresh, like every other route's.
func TestRetryAlternativeReadsTheFleetAgainOnSpread(t *testing.T) {
	noProbeMemo(t)
	b, bURL := acceptingNode(t, "node-b", "answer", func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 1, 1 })
	r := &runner{cfg: testCfg(t), route: "spread", remotes: []string{bURL}}
	r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(t.Context())
	before := b.healths.Load()
	st := Subtask{Contract: plainContract(), EstTokens: 100}
	if _, _, found := r.remoteAlternative(t.Context(), st, newPlacements()); !found {
		t.Fatal("fixture: the only remote must be the alternative")
	}
	if b.healths.Load() == before {
		t.Fatal("remoteAlternative reused the run-start snapshot on route=spread instead of reading the fleet again")
	}
}

// ---- both research doors are digest-shaped -------------------------------------

func TestBothResearchDoorsAreDigestShaped(t *testing.T) {
	for _, door := range []string{"offload_research", "cli:research"} {
		c := digestContract(researchGoals[0])
		c.Door = door
		st := Subtask{Contract: c, EstTokens: 100}
		if got := inferKind(st); got != KindMechanical {
			t.Errorf("door %q: inferKind = %v, want mechanical", door, got)
		}
		if _, rule := shapeOf(st); rule != "research-digest" {
			t.Errorf("door %q: shapeOf rule = %q, want research-digest", door, rule)
		}
	}
}

// ---- small arithmetic pins -------------------------------------------------------

// TestStartsWithinPatienceBoundaryAndWorkerFloor: an ETA exactly at the patience fits (a start at the
// deadline is still a start); a node that publishes no worker count is read as one worker in the
// arithmetic it prints.
func TestStartsWithinPatienceBoundaryAndWorkerFloor(t *testing.T) {
	at, past := 300.0, 300.5
	v := NodeView{QueueWaitEstimateSec: &at}
	if ok, why := startsWithinPatience(v, 300*time.Second); !ok {
		t.Fatalf("a 300 s ETA against a 300 s patience was held out (%s), want it to fit", why)
	}
	v.QueueWaitEstimateSec = &past
	if ok, _ := startsWithinPatience(v, 300*time.Second); ok {
		t.Fatal("a 300.5 s ETA fits a 300 s patience")
	}
	derived := NodeView{JobsRunning: 2, RecentAgentWallSec: 100}
	if got := startArithmetic(derived); !strings.Contains(got, "/ 1 worker(s)") {
		t.Fatalf("arithmetic = %q, want a node that publishes no worker count read as one worker", got)
	}
}

// TestPageBackoffUntilNeverReadsZero: the backed-off page says when it opens again, at least a second
// away; and the record map is pruned of stale pages as it grows, so a long-lived server never
// accumulates pages it will not see again.
func TestPageBackoffUntilNeverReadsZero(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	if got := pageBackoffUntil(now.Add(-(pageBackoff - 100*time.Millisecond)), now); got != "in 1s" {
		t.Fatalf("pageBackoffUntil with 100 ms left = %q, want the 1 s floor", got)
	}
	c := &pageRetryCap{pages: map[string]*pageRecord{}, now: func() time.Time { return now }}
	for i := 0; i < 600; i++ {
		c.record("stale-"+string(rune('a'+i%26))+strings.Repeat("x", i/26), true)
	}
	now = now.Add(pageBackoff + time.Minute)
	c.record("fresh", true)
	if len(c.pages) != 1 {
		t.Fatalf("%d page records kept after the backoff passed for all but one, want the stale ones pruned", len(c.pages))
	}
}
