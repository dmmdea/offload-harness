package delegate

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// holdLease takes a lease of the given class in a fresh dir and returns the
// dir plus the release. The dir doubles as cfg.GPULockPath so the runner reads
// exactly the lease the test holds — the same resolver, never a second one.
func holdLease(t *testing.T, class gpulease.Class, reason string) (string, *gpulease.Lease) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lease")
	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	lease, err := m.TryAcquire(class, gpulease.Options{Reason: reason, Origin: "lease-gate-test", TTL: time.Minute})
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return dir, lease
}

// runUnderLease marks this test process as running UNDER lease - GPU_LEASE_EPOCH, the way a
// delegation launched by `gpu reserve -- ...` runs. The lease is still held, so placement reads
// the local GPU as busy and consults the fleet; but ForeignFence exempts the holder's own child,
// so the delegator does not treat the local seat as fenced against the run. This is how a test
// with a fake local runner makes "the GPU is busy, then fall back local" true: a lease held by
// somebody ELSE fences the seat, a real local run would be turned away at once at the seat's own
// pre-check (S-26), and the delegator no longer dials it (GPU routing P7).
func runUnderLease(t *testing.T, lease *gpulease.Lease) {
	t.Helper()
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(lease.Epoch(), 10))
}

// TestReserved pins the class rule: only a HELD TEXT lease reserves the seat.
// Media steers placement (LocalBusy) and is arbitrated at the affinity gate;
// an idle or unresolvable lease reads as not reserved.
func TestReserved(t *testing.T) {
	if Reserved(gpulease.Info{}) {
		t.Fatal("zero Info must not read as reserved")
	}
	if Reserved(gpulease.Info{Held: true, Class: gpulease.ClassMedia}) {
		t.Fatal("a media lease must not reserve the seat for placement (ADR 0026 arbitrates it)")
	}
	if !Reserved(gpulease.Info{Held: true, Class: gpulease.ClassText}) {
		t.Fatal("a held text lease must reserve the seat")
	}
	if Reserved(gpulease.Info{Held: false, Class: gpulease.ClassText}) {
		t.Fatal("a released text lease must not reserve the seat")
	}
	dir, lease := holdLease(t, gpulease.ClassText, "weights A/B")
	if info := LocalLease(dir, ""); !Reserved(info) || info.Reason != "weights A/B" {
		t.Fatalf("LocalLease on a held text lease = %+v, want Reserved with the holder's reason", info)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if Reserved(LocalLease(dir, "")) {
		t.Fatal("reserved after release")
	}
}

// TestReservedInheritedLeaseIsExempt mirrors the affinity gate's rule: a
// process running UNDER the holder (GPU_LEASE_EPOCH == the held epoch) is not
// reserved out of its own seat; a stale or wrong epoch exempts nothing.
func TestReservedInheritedLeaseIsExempt(t *testing.T) {
	dir, _ := holdLease(t, gpulease.ClassText, "own bench")
	info := LocalLease(dir, "")
	if !Reserved(info) {
		t.Fatal("held text lease must reserve for a stranger")
	}
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch, 10))
	if Reserved(info) {
		t.Fatal("the holder's own child (inherited epoch) must not be reserved out")
	}
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(info.Epoch+1, 10))
	if !Reserved(info) {
		t.Fatal("a stale epoch must not exempt")
	}
	t.Setenv("GPU_LEASE_EPOCH", "garbage")
	if !Reserved(info) {
		t.Fatal("an unparsable epoch must not exempt")
	}
}

// TestHolderLine: the line a deferred caller reads must carry what decides
// their next move — class, the holder's reason/origin, and the expiry.
func TestHolderLine(t *testing.T) {
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	line := HolderLine(LocalLease(dir, ""))
	for _, want := range []string{"class=text", `reason="weights A/B"`, `origin="lease-gate-test"`, "expires="} {
		if !strings.Contains(line, want) {
			t.Errorf("HolderLine = %q, want it to contain %q", line, want)
		}
	}
}

// TestRunAutoReservedLocalDefersNamingTheHolder is the 2026-09-05 08:04
// incident as a test: a TEXT lease reserves the seat, the only remote fails
// the gate, and route=auto used to run the contract locally anyway
// ("queued-local beats ineligible-remote"). Now: no local run, a deferred
// result, class infrastructure, the holder named.
func TestRunAutoReservedLocalDefersNamingTheHolder(t *testing.T) {
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := off.server().URL
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if localCalls.Load() != 0 || off.dispatches.Load() != 0 {
		t.Fatalf("local=%d dispatches=%d, want 0/0 — the reserved seat must not run the contract", localCalls.Load(), off.dispatches.Load())
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("result = %+v, want a deferred infrastructure-class result", pr.Result)
	}
	for _, want := range []string{"reserved", `reason="weights A/B"`, "agent_lease_wait_sec=0", "no eligible remote"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
	if sum.Infrastructure != 1 || sum.Succeeded != 0 {
		t.Fatalf("summary = %+v, want the defer counted as infrastructure", sum)
	}
}

// TestRunAutoReservedLocalWaitsForRelease: with agent_lease_wait_sec set, the
// placement waits for the holder; once the lease is released the contract runs
// locally and the reason records the wait. The knob is what turns a benchmark's
// reservation from a refusal into a queue.
func TestRunAutoReservedLocalWaitsForRelease(t *testing.T) {
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := off.server().URL
	dir, lease := holdLease(t, gpulease.ClassText, "short soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentLeaseWaitSec = 10
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_ = lease.Release()
	}()
	var localCalls atomic.Int64
	start := time.Now()
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 {
		t.Fatalf("local=%d summary=%+v, want the contract to run locally once the lease cleared", localCalls.Load(), sum)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("Run returned after %s — it did not wait for the release", waited)
	}
	if !strings.Contains(results[0].PlacementReason, "lease cleared") {
		t.Errorf("placement reason = %q, want the cleared lease recorded", results[0].PlacementReason)
	}
}

// TestRunAutoMediaLeaseThatFencesTheSeatIsAPlaceInLineNotADial: a MEDIA lease with no eligible
// remote used to run locally, "arbitrated at the affinity gate" (the reservation rule was
// text-only). Since the seat's own fence pre-check (S-26) that local run is turned away at once,
// so the dial only wasted an attempt, a ledger row and an intent record. The delegator now reads
// the same verdict first (GPU routing P7): with another node to wait for, the subtask stands in
// line and ends as the capacity defer the pre-check would have filed, naming the fence. With the
// wait off (this suite's default) that is immediate.
func TestRunAutoMediaLeaseThatFencesTheSeatIsAPlaceInLineNotADial(t *testing.T) {
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := off.server().URL
	dir, _ := holdLease(t, gpulease.ClassMedia, "render")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if localCalls.Load() != 0 || sum.Succeeded != 0 || sum.Deferred != 1 {
		t.Fatalf("local=%d summary=%+v, want nothing dialled onto the fenced seat and one capacity defer", localCalls.Load(), sum)
	}
	res := results[0].Result
	if res.DeferClass != core.DeferClassCapacity || !strings.Contains(res.Reason, "fenced") || !strings.Contains(res.Reason, "media render holds the cards") {
		t.Errorf("result = %+v, want a capacity defer naming the fence", res)
	}
}

// TestRunAutoMediaLeaseWithNoRemoteStillRunsLocal: with no remote configured there is no other
// node to wait for, so nothing is saved by not dialling: the local run's own pre-check is the
// fast, honest answer, and the delegator leaves it to it.
func TestRunAutoMediaLeaseWithNoRemoteStillRunsLocal(t *testing.T) {
	dir, _ := holdLease(t, gpulease.ClassMedia, "render")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "auto", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 {
		t.Fatalf("local=%d summary=%+v, want the local run when there is no node to wait for", localCalls.Load(), sum)
	}
	if !strings.Contains(results[0].PlacementReason, "queued-local beats ineligible-remote") && !strings.Contains(results[0].PlacementReason, "no remote") {
		t.Errorf("placement reason = %q", results[0].PlacementReason)
	}
}

// TestRunSpreadReservedLocalDealsRemotesOnly: route=spread ignored the lease
// entirely — the exact route the 08:04 contracts used. With a text lease held
// and two eligible remotes, four subtasks must land 0 local / 2 A / 2 B.
func TestRunSpreadReservedLocalDealsRemotesOnly(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "zorblax from A")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(4), "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 {
		t.Fatalf("summary = %+v, want 4 succeeded", sum)
	}
	if localCalls.Load() != 0 || nodeA.dispatches.Load() != 2 || nodeB.dispatches.Load() != 2 {
		t.Fatalf("distribution local=%d A=%d B=%d, want 0/2/2 — the reserved seat is out of the deal", localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
	for i, pr := range results {
		if pr.ranLocal {
			t.Errorf("subtask %d ran local on a reserved seat (reason %q)", i, pr.PlacementReason)
		}
	}
}

// TestRunSpreadReservedLocalWithNoRemoteDefers: a spread whose only remote
// fails the gate used to run every subtask local; on a reserved seat every
// subtask defers instead, naming the holder — and none runs.
func TestRunSpreadReservedLocalWithNoRemoteDefers(t *testing.T) {
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := off.server().URL
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(2), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || off.dispatches.Load() != 0 || sum.Succeeded != 0 || sum.Infrastructure != 2 {
		t.Fatalf("local=%d dispatches=%d summary=%+v, want nothing run and both deferred as infrastructure", localCalls.Load(), off.dispatches.Load(), sum)
	}
	for _, pr := range results {
		if !pr.Result.Deferred || !strings.Contains(pr.Result.Reason, "reserved") || !strings.Contains(pr.Result.Reason, `reason="weights A/B"`) {
			t.Errorf("result = %+v, want a defer naming the reservation and its holder", pr.Result)
		}
		if !strings.HasPrefix(pr.PlacementReason, "route=spread: local seat reserved (") {
			t.Errorf("placement reason = %q", pr.PlacementReason)
		}
	}
}

// TestRunLocalRouteIsNotGated: route=local is the caller's explicit choice —
// a reserved seat still runs it (the caller may BE the holder's own session).
func TestRunLocalRouteIsNotGated(t *testing.T) {
	dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	_, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 {
		t.Fatalf("local=%d summary=%+v, want route=local to run regardless of the lease", localCalls.Load(), sum)
	}
}

var _ = json.RawMessage(nil) // keep the import set identical to the sibling test files

// TestRunSpreadMediaLeaseThatFencesTheSeatTakesItOutOfTheDeal (review 2026-09-06, amended by
// GPU routing P7): a MEDIA lease never entered the spread deal before 0.113.14, because its
// render was "arbitrated at the affinity gate" - the gate waited for the render and admitted the
// load. Since the seat's fence pre-check (S-26) that gate turns a run away at once, so a slot
// dealt onto a fenced seat was a leg spent on nothing. A lease that fences every local seat of
// a contract's chain now removes the local slot exactly as a text reservation does; with two
// eligible remotes four subtasks deal A, B, A, B. (A lease on a card the contracts do not need
// leaves the slot in: TestSpreadKeepsALocalSeatTheLeaseDoesNotFence.)
func TestRunSpreadMediaLeaseThatFencesTheSeatTakesItOutOfTheDeal(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "zorblax from A")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	dir, _ := holdLease(t, gpulease.ClassMedia, "render")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	var localCalls atomic.Int64
	_, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(4), "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || localCalls.Load() != 0 || nodeA.dispatches.Load() != 2 || nodeB.dispatches.Load() != 2 {
		t.Fatalf("summary=%+v local=%d A=%d B=%d, want 4 succeeded dealt 0/2/2 — the fenced seat is out of the deal", sum, localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}
