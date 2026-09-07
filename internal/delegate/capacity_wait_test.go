package delegate

// The capacity wait and the scheduling bands (0.113.18, fleet-flow chapter L5
// + L6, delegator side). Every test here turns the wait ON explicitly —
// testCfg switches it off for the established suite — and compresses the poll
// interval and the refusal cooldown so a wait is milliseconds, not minutes.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// compressWait shortens the wait loop's clocks for one test.
func compressWait(t *testing.T, poll, cooldown time.Duration) {
	t.Helper()
	oldPoll, oldCool := placementPollInterval, refusalCooldown
	placementPollInterval, refusalCooldown = poll, cooldown
	t.Cleanup(func() { placementPollInterval, refusalCooldown = oldPoll, oldCool })
}

// freesAfter returns a dispatch hook that refuses with `status` the first n
// dispatches and acks the rest — a node that is full now and frees later.
func freesAfter(n int64, status int) func(int64) int {
	return func(k int64) int {
		if k <= n {
			return status
		}
		return 0
	}
}

// TestRunWaitsForCapacityAndLandsWhenTheNodeFrees is the defect the wait
// exists for: the only node is full at dispatch (503), so before 0.113.18 the
// subtask ended as "placement refused" and its work was never done. Now the
// delegator waits, re-reads health, and lands the subtask on the node once it
// takes work again — and the wait is NOT charged to the contract's budget.
func TestRunWaitsForCapacityAndLandsWhenTheNodeFrees(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var seenBudget atomic.Int64
	node, url := acceptingNode(t, "node-busy", "qube after the wait", func(f *fakeNode) {
		f.dispatchHook = freesAfter(2, http.StatusServiceUnavailable)
		f.onDispatch = func(_ string, c core.AgentContract) { seenBudget.Store(int64(c.TimeoutSec)) }
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := Summary{Succeeded: 1, Waited: 1, Replaced: 1, ReplacementRecovered: 1}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v (one success after a capacity wait)", sum, want)
	}
	pr := results[0]
	if pr.Node != "node-busy" || pr.Err != "" || pr.Result.Deferred {
		t.Fatalf("result = %+v, want the work landed on node-busy", pr)
	}
	if got := node.dispatches.Load(); got != 3 {
		t.Fatalf("node saw %d dispatches, want 3 (two refusals inside the wait, then the ack)", got)
	}
	if !strings.Contains(pr.PlacementReason, "capacity wait") || pr.Replacements != 2 {
		t.Fatalf("placement = %q replacements = %d, want the wait named and both refusals counted", pr.PlacementReason, pr.Replacements)
	}
	// Budget: the contract asked for 30 s; the seat that finally took it must
	// be handed (almost) all of it — the wait is credited, only the attempts
	// are charged, and those took milliseconds here.
	if b := seenBudget.Load(); b < 28 {
		t.Fatalf("budget handed to the node after the wait = %d s, want ~30 (the wait must not be charged)", b)
	}
}

// TestRunCapacityWaitTimesOutAsACapacityDefer: a node that never frees. The
// wait is bounded by agent_placement_wait_sec, the outcome is a DEFER of class
// capacity (not a broken stack, not a budget the seat ran out of), and the
// node was re-asked during the wait rather than written off after one 503.
func TestRunCapacityWaitTimesOutAsACapacityDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("Run returned after %s — it did not wait the configured second", waited)
	}
	want := Summary{Deferred: 1, Waited: 1, Replaced: 1}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v (a deferred subtask that waited; NOT replacement_recovered — nothing ran it)", sum, want)
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v, want deferred with defer_class capacity", pr.Result)
	}
	if BrokenStackDefer(pr.Result.DeferClass) {
		t.Fatal("a capacity defer must not read as a broken stack")
	}
	for _, s := range []string{"no node had room", "node-full", "agent_placement_wait_sec=1"} {
		if !strings.Contains(pr.Result.Reason, s) {
			t.Errorf("reason = %q, want it to contain %q", pr.Result.Reason, s)
		}
	}
	if pr.CapacityWaitSec < 0.9 {
		t.Errorf("capacity_wait_sec = %.2f, want ~1", pr.CapacityWaitSec)
	}
	if got := node.dispatches.Load(); got < 3 {
		t.Fatalf("node saw %d dispatches, want it re-asked during the wait (>= 3)", got)
	}
}

// TestRunSheddableIsShedNotWaited: priority -1 never waits. With no node
// holding an idle slot the subtask is shed at once (class capacity, summary
// shed), the node is asked exactly once, and no wall clock is spent.
func TestRunSheddableIsShedNotWaited(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-busy", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5 // would wait 5 s at band 0

	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url},
		&RunOptions{Priority: core.BandSheddable})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("Run took %s — a sheddable subtask must not wait", waited)
	}
	want := Summary{Deferred: 1, Shed: 1, Replaced: 1}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v", sum, want)
	}
	pr := results[0]
	if pr.Result.DeferClass != core.DeferClassCapacity || !strings.Contains(pr.Result.Reason, "shed (priority -1)") {
		t.Fatalf("result = %+v, want a capacity-class shed naming priority -1", pr.Result)
	}
	if got := node.dispatches.Load(); got != 1 {
		t.Fatalf("node saw %d dispatches, want exactly 1", got)
	}
	if p, _ := node.lastPriority.Load().(*int); p == nil || *p != core.BandSheddable {
		t.Fatalf("dispatch carried priority %v, want -1 in the envelope", p)
	}
}

// TestRunReservedLocalLandsOnARemoteThatFrees completes L3 ("re-route, not
// defer"): the local seat is under a text lease, the one remote is full at
// first — before 0.113.18 this waited on the LOCAL lease alone and deferred.
// Now the wait watches the remote too, and the subtask lands there the moment
// it takes work, while the lease is still held.
func TestRunReservedLocalLandsOnARemoteThatFrees(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-remote", "qube from the remote", func(f *fakeNode) {
		f.dispatchHook = freesAfter(1, http.StatusServiceUnavailable)
	})
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 5

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := Summary{Succeeded: 1, Waited: 1, Replaced: 1, ReplacementRecovered: 1}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v", sum, want)
	}
	if results[0].Node != "node-remote" {
		t.Fatalf("node = %q, want the remote that freed", results[0].Node)
	}
	if got := node.dispatches.Load(); got != 2 {
		t.Fatalf("remote saw %d dispatches, want 2 (refused, then took it)", got)
	}
	if !Reserved(LocalLease(dir, "")) {
		t.Fatal("control: the lease must still be held when the remote takes the work")
	}
}

// TestRunReservedLocalStillDefersNamingTheHolderWhenNothingFrees pins the
// established deferral: lease held, no remote at all, wait exhausted — the
// holder is named and the class is infrastructure, exactly as 0.113.14 did.
func TestRunReservedLocalStillDefersNamingTheHolderWhenNothingFrees(t *testing.T) {
	compressWait(t, 20*time.Millisecond, 0)
	dir, _ := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "auto", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 || sum.Infrastructure != 1 || sum.Waited != 0 {
		t.Fatalf("summary = %+v, want one infrastructure deferral (waited is not set on the holder-naming deferral)", sum)
	}
	if r := results[0].Result.Reason; !strings.Contains(r, `reason="soak"`) || !strings.Contains(r, "reserved") {
		t.Fatalf("reason = %q, want the holder named", r)
	}
}

// TestRunWaitDisabledKeepsThePreWaitOutcome is the control arm: with
// agent_placement_wait_sec negative the refused chain ends exactly as before —
// a "placement refused" FAILURE, nothing waited, nothing deferred.
func TestRunWaitDisabledKeepsThePreWaitOutcome(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t) // AgentPlacementWaitSec: -1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Failed: 1}) {
		t.Fatalf("summary = %+v, want the pre-0.113.18 failure", sum)
	}
	if !strings.HasPrefix(results[0].Err, replacementExhaustedPrefix) {
		t.Fatalf("err = %q, want %q", results[0].Err, replacementExhaustedPrefix)
	}
	if got := node.dispatches.Load(); got != 1 {
		t.Fatalf("node saw %d dispatches, want 1 (no wait, no re-ask)", got)
	}
}

// TestDispatchCarriesBandAndTenant: the envelope carries `priority` only when
// non-zero (a band-0 run's bytes are unchanged) and the tenant rides the
// header, never a new envelope field (older nodes reject unknown fields).
func TestDispatchCarriesBandAndTenant(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-a", "qube", nil)
	cfg := testCfg(t)

	_, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url},
		&RunOptions{Priority: 7, Tenant: "session-A"})
	if err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: err=%v summary=%+v", err, sum)
	}
	if p, _ := node.lastPriority.Load().(*int); p == nil || *p != core.BandUrgent {
		t.Fatalf("priority on the wire = %v, want 7 clamped to %d", p, core.BandUrgent)
	}
	if tn, _ := node.lastTenant.Load().(string); tn != "session-A" {
		t.Fatalf("tenant header = %q, want session-A", tn)
	}

	// Band 0, no tenant: neither key is on the wire.
	_, sum, err = RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil || sum.Succeeded != 1 {
		t.Fatalf("Run: err=%v summary=%+v", err, sum)
	}
	if p, _ := node.lastPriority.Load().(*int); p != nil {
		t.Fatalf("priority on the wire = %d for a band-0 run, want the field absent", *p)
	}
	if tn, _ := node.lastTenant.Load().(string); tn != "" {
		t.Fatalf("tenant header = %q for an anonymous run, want absent", tn)
	}
}

// TestHasRoomReadsSaturationBeforeArithmetic: a node that publishes
// saturation is judged by its own verdict; one that does not is judged by the
// counters it publishes; a sheddable contract needs the idle slot.
func TestHasRoomReadsSaturationBeforeArithmetic(t *testing.T) {
	high := NodeView{SaturationKnown: true, SaturationHigh: true, IdleSlot: false}
	if hasRoom(high, false) || saturated(high) != true {
		t.Fatal("saturation.high must read as saturated / no room")
	}
	busy := NodeView{SaturationKnown: true, SaturationScore: 0.5, IdleSlot: false}
	if !hasRoom(busy, false) {
		t.Fatal("a busy-but-not-high node has room for band 0")
	}
	if hasRoom(busy, true) {
		t.Fatal("a node without an idle slot has no room for a sheddable contract")
	}
	old := NodeView{QueueDepth: 3, MaxQueueDepth: 8, JobsRunning: 4, MaxConcurrentJobs: 4}
	if !hasRoom(old, false) || hasRoom(old, true) {
		t.Fatal("an older node is judged on its counters: room for band 0, no idle slot for sheddable")
	}
	full := NodeView{QueueDepth: 8, MaxQueueDepth: 8}
	if hasRoom(full, false) {
		t.Fatal("queue at its ceiling has no room, saturation block or not")
	}
	if !hasRoom(NodeView{}, true) {
		t.Fatal("an idle node that publishes nothing: QueueDepth 0 proves the slot")
	}
}

// TestFetchNodeViewDecodesSaturation pins the wire decode: present → known
// with the three fields; absent → unknown (never blamed).
func TestFetchNodeViewDecodesSaturation(t *testing.T) {
	srv := healthServer(t, `{"node_id":"n","saturation":{"score":0.75,"high":true,"idle_slot":false}}`, nil)
	v, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !v.SaturationKnown || v.SaturationScore != 0.75 || !v.SaturationHigh || v.IdleSlot {
		t.Fatalf("decoded %+v, want known/0.75/high/no idle slot", v)
	}
	srv2 := healthServer(t, `{"node_id":"n"}`, nil)
	v2, err := FetchNodeView(context.Background(), srv2.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.SaturationKnown || saturated(v2) {
		t.Fatalf("an older node decoded %+v, want saturation unknown and not saturated", v2)
	}
}
