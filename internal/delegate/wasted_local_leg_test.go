package delegate

// The wasted local leg (plan P7, register C-86). A lease this process does not hold that
// fences every local seat a contract could run on turns the local run away at the seat's own
// pre-check (agenttask.go, S-26), after the delegator has spent an attempt, a ledger row and an
// intent record dialling it. The delegator reads the same verdict first (ForeignFence over the
// contract's chain of seats) and, when there is another node to wait for, waits in line
// instead of dialling a seat that cannot take the work. These tests run the real lease write
// path in a scratch lease directory over the flagship fixture; only the card table is
// synthetic, and the local runner is a counting seam.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// noDecision is the local decider of a box whose placement table is not under test: the zero
// Decision runs the planner seat, and keeps the run off nvidia-smi.
func noDecision(context.Context, core.AgentContract, Subtask) placetable.Decision {
	return placetable.Decision{}
}

func fencedRunOptions() *RunOptions { return &RunOptions{LocalDecider: noDecision} }

// ineligibleNode is a remote that answers health and cannot take the contract.
func ineligibleNode(t *testing.T) (*fakeNode, string) {
	t.Helper()
	off := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	return off, off.server().URL
}

// The headline: every local seat of the chain sits on a card a media render holds, no remote
// is eligible, and the contract waits in line instead of being dialled onto the render.
func TestNoWastedLocalLegWhenLocalSeatFenced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0) // card 0 holds the flagship AND the single layer's seat
	off, url := ineligibleNode(t)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1

	var localCalls atomic.Int64
	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("the local runner was called %d times: a seat a media render fences cannot take the run, so it must not be dialled", localCalls.Load())
	}
	if off.dispatches.Load() != 0 {
		t.Fatalf("the ineligible remote was dispatched to %d times", off.dispatches.Load())
	}
	if time.Since(start) < time.Second {
		t.Fatalf("returned after %s: the subtask must wait its place in line for the lease to clear, not give up at once", time.Since(start))
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity || sum.Deferred != 1 {
		t.Fatalf("result = %+v summary = %+v, want a capacity defer after the wait", pr.Result, sum)
	}
	for _, want := range []string{"fenced", "media"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to name the fence (%q missing)", pr.Result.Reason, want)
		}
	}
}

// The control arms: the same box with the lease on a card the contract does not need, and
// with the lease gone, runs locally at once.
func TestFencedLocalSeatControlArmsStillRunLocal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2) // the single layer's card-0 seat is free
	_, url := ineligibleNode(t)
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 {
		t.Fatalf("local=%d summary=%+v: a lease on card 2 leaves the card-0 seat free, so the run goes local", localCalls.Load(), sum)
	}
}

// A lease that clears while the subtask waits lets it run on the seat: the place in line was
// kept, not lost.
func TestFencedLocalSeatTakesTheWorkWhenTheLeaseClears(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	l, err := b.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{leaseCard0}})
	if err != nil {
		t.Fatal(err)
	}
	_, url := ineligibleNode(t)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = l.Release()
	}()
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 || sum.Waited != 1 {
		t.Fatalf("local=%d summary=%+v, want the contract to run locally once the render released the card", localCalls.Load(), sum)
	}
	if !strings.Contains(results[0].PlacementReason, "fence cleared") {
		t.Errorf("placement reason = %q, want the cleared fence recorded", results[0].PlacementReason)
	}
}

// With no other node to wait for there is no leg to save: the local run gives the seat's own
// fast, honest answer, as it always did.
func TestFencedLocalSeatWithNoRemoteIsStillDialled(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	var localCalls atomic.Int64
	_, _, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", nil, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local=%d: with no remote configured the seat's own pre-check is the answer, and it is dialled", localCalls.Load())
	}
}

// route=local is the caller's explicit choice and is never gated by the delegator.
func TestFencedLocalSeatIsStillDialledOnRouteLocal(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	_, url := ineligibleNode(t)
	var localCalls atomic.Int64
	_, _, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "local", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local=%d: route=local runs where the caller said", localCalls.Load())
	}
}

// The re-placement's last resort is the local seat only when it can take the work: a remote
// refused the first dispatch, the local seat is fenced, so the subtask waits for the remote to
// take it again (a place in line), and the local runner is never called.
func TestFencedLocalSeatIsNotTheLastResortOfAReplacement(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	node, url := acceptingNode(t, "node-busy", "zorblax after the wait", func(f *fakeNode) {
		f.dispatchHook = freesAfter(1, http.StatusServiceUnavailable)
	})
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5

	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("the local runner was called %d times: the fenced seat is not a place to re-place onto", localCalls.Load())
	}
	if sum.Succeeded != 1 || results[0].Node != "node-busy" || node.dispatches.Load() != 2 {
		t.Fatalf("summary=%+v node=%q dispatches=%d, want the remote to take it on its second ask", sum, results[0].Node, node.dispatches.Load())
	}
}

// route=spread deals the local slot to a seat the render fences: with the mirror, the fenced
// seat is out of the rotation (like a text reservation) and the remotes take every subtask.
func TestSpreadDealsNothingToAFencedLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	nodeA, urlA := eligibleNode(t, "node-a", "zorblax from A")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls), contracts(4), "spread", []string{urlA, urlB}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || localCalls.Load() != 0 || nodeA.dispatches.Load() != 2 || nodeB.dispatches.Load() != 2 {
		t.Fatalf("summary=%+v local=%d A=%d B=%d, want 4 succeeded dealt 0/2/2: the fenced seat is out of the deal", sum, localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// And the control: the render on a card the contracts do not need leaves the local slot in
// the deal, exactly as with no lease at all.
func TestSpreadKeepsALocalSeatTheLeaseDoesNotFence(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard2)
	nodeA, urlA := eligibleNode(t, "node-a", "zorblax from A")
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls), contracts(4), "spread", []string{urlA, urlB}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || localCalls.Load() != 2 || nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("summary=%+v local=%d A=%d B=%d, want 4 succeeded dealt 2/1/1: the card-0 seat is free", sum, localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// A holder's own child is exempt from the lease it runs under, so it is never kept off the
// seat by it (ForeignFence, not Fenced).
func TestFencedLocalSeatIsNotFencedAgainstTheHoldersOwnChild(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	l, err := b.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{leaseCard0}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(l.Epoch(), 10))
	r := b.runner()
	r.remotes = []string{"http://node-x:1"}
	if _, _, fenced := r.fencedLocal(agentContract("")); fenced {
		t.Fatal("the process runs under the lease: its fence is not a fence against it")
	}
}

// The same verdict when the placement is derived per subtask rather than read off a deal (a
// white-box run that reaches attempt() directly): no eligible remote, a fenced seat, no dial.
func TestNoWastedLocalLegOnTheDefaultPlacementPath(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	_, url := ineligibleNode(t)
	var localCalls atomic.Int64
	r := &runner{
		cfg: b.cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{url},
		intent:         openIntentLedger(b.cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{} },
		decider:        noDecision,
	}
	pr := r.runOne(t.Context(), 0, remoteContract())
	if localCalls.Load() != 0 {
		t.Fatalf("the local runner was called %d times on the default placement path", localCalls.Load())
	}
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity || !strings.Contains(pr.Result.Reason, "fenced") {
		t.Fatalf("result = %+v, want the capacity defer naming the fence", pr.Result)
	}
}

// route=spread with no eligible remote deals the slot local so it has a view, then flags it: a
// fenced seat is a place in line for the subtask, never a dial.
func TestSpreadWithNoEligibleRemoteAndAFencedSeatWaitsInLine(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	off, url := ineligibleNode(t)
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), b.cfg, passingLocal(&localCalls), contracts(2), "spread", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || off.dispatches.Load() != 0 || sum.Succeeded != 0 || sum.Deferred != 2 {
		t.Fatalf("local=%d dispatches=%d summary=%+v, want nothing dialled and both subtasks deferred as capacity", localCalls.Load(), off.dispatches.Load(), sum)
	}
	for _, pr := range results {
		if pr.Result.DeferClass != core.DeferClassCapacity || !strings.Contains(pr.Result.Reason, "fenced") {
			t.Errorf("result = %+v, want a capacity defer naming the fence", pr.Result)
		}
	}
}

// A contract naming a layer this box does not declare can never run on the local seat, so a
// lease that fences the seat is no place it stands in: the wait names only the node that declares
// the layer.
func TestAFencedWaitForALayerTheBoxDoesNotDeclareNamesNoLocalPlace(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 50*time.Millisecond, 100*time.Millisecond)
	node, url := refusingNode(t, "fast-node", http.StatusServiceUnavailable, func(f *fakeNode) { f.layers = oneCardRows(t) })
	dir, _ := holdLease(t, gpulease.ClassMedia, "render")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 1
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), layerContracts(1, "fast"), "auto", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if !pr.Result.Deferred || strings.Contains(pr.Result.Reason, "fenced") || strings.Contains(pr.Result.Reason, "render") {
		t.Fatalf("result = %+v, want a capacity defer that does not blame a lease the seat could never have used", pr.Result)
	}
	for _, p := range pr.PlaceKeeping {
		if p.Node != "fast-node" {
			t.Fatalf("place_keeping = %+v, want only the node that declares the layer", pr.PlaceKeeping)
		}
	}
	if node.dispatches.Load() < 2 {
		t.Fatalf("the declaring node was asked %d time(s), want it re-asked during the wait", node.dispatches.Load())
	}
}

// fencedLocal answers per route: auto reads the fence, route=local never does (the caller chose
// the seat), and a delegator with no remotes has nowhere else to wait.
func TestFencedLocalIsAskedPerRouteAndRoster(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	c := agentContract("")
	r := b.runner()
	r.remotes = []string{"http://node-x:1"}
	r.route = "auto"
	if _, why, fenced := r.fencedLocal(c); !fenced || !strings.Contains(why, "media") {
		t.Fatalf("auto with a remote: fenced=%v why=%q, want the render's fence", fenced, why)
	}
	r.route = "local"
	if _, _, fenced := r.fencedLocal(c); fenced {
		t.Fatal("route=local is never gated")
	}
	r.route, r.remotes = "auto", nil
	if _, _, fenced := r.fencedLocal(c); fenced {
		t.Fatal("with no remote there is no other node to wait for")
	}
}

// A remote that refuses with a non-capacity answer (a 500) is excluded from the rest of the wait.
// With the local seat fenced the subtask still stands in line - it ends as the capacity defer the
// wait files, never as a "placement refused" failure naming a refusal that is not the subtask's.
func TestFencedLocalSeatKeepsASubtaskInLineAfterANonCapacityRefusal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	node, url := acceptingNode(t, "node-broken", "never answered", func(f *fakeNode) {
		f.dispatchHook = freesAfter(100, http.StatusInternalServerError)
	})
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if localCalls.Load() != 0 || node.dispatches.Load() != 1 {
		t.Fatalf("local=%d dispatches=%d, want the broken node asked once and the fenced seat never dialled", localCalls.Load(), node.dispatches.Load())
	}
	if sum.Failed != 0 || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary=%+v result=%+v, want a capacity defer: the fenced seat is a place in line", sum, pr.Result)
	}
}

// The local lease's place reports the soonest end among the leases that hold the seat.
func TestLocalPlaceTakesTheSoonestEndAmongTheLeases(t *testing.T) {
	now := time.Now()
	hour := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 1, ExpiresAt: now.Add(time.Hour)}
	two := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 2, ExpiresAt: now.Add(2 * time.Hour)}
	past := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 3, ExpiresAt: now.Add(-time.Hour)}
	open := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 4}
	all := hour
	all.Leases = []gpulease.Info{two, past, open, hour}
	p := localPlace(NodeView{NodeID: "box"}, "fenced", all)
	if p.Node != "box" || p.On != "lease" || p.EtaSec < 3500 || p.EtaSec > 3600 {
		t.Fatalf("place = %+v, want the hour-long lease's end, ignoring the two-hour one, the lapsed one and the open-ended one", p)
	}
	if p := localPlace(NodeView{NodeID: "box"}, "fenced", past); p.EtaSec != 0 {
		t.Fatalf("a lapsed window says nothing about when the seat frees: %+v", p)
	}
}
