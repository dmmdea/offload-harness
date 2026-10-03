package delegate

// A fallback must not hide a failure (register C-D: "a fleet that has been down for a week must
// not read green"). Before the wasted-local-leg change, route=auto with every remote failing its
// health probe and the local seat busy under somebody else's lease fell through to the local
// placement, which flags the fleet as unreachable, and the run counted as Infrastructure. The
// fenced sentinel waits in line instead of dialling the fenced seat, and returned from the
// placement without the class the "no eligible remote" verdict carries, so the same dead fleet
// ended as a plain capacity defer with Infrastructure 0. The class rides the sentinel now and is
// stamped on whatever ends the wait, unless a remote answered during it.

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// deadRemote is a remote that refuses at connect: every health probe fails.
func deadRemote(t *testing.T) string {
	t.Helper()
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "dead-node",
		pollState: func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	srv := node.server()
	base := srv.URL
	srv.Close()
	return base
}

// The finding's fixture: a dead remote, a foreign media lease on the card the seat needs, the
// wait on. The wait ends as the capacity defer it always ended as, and the dead fleet is counted.
func TestDeadFleetWithAFencedLocalSeatIsStillInfrastructure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 {
		t.Fatalf("the fenced local seat was dialled %d times", localCalls.Load())
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v, want the capacity defer the wait files", pr.Result)
	}
	if sum.Infrastructure != 1 || sum.Deferred != 1 {
		t.Fatalf("summary = %+v, want the deferred subtask counted as Infrastructure: a fleet that never answers must not read green", sum)
	}
}

// With the wait switched off the sentinel ends at once as the fenced defer: still counted.
func TestDeadFleetWithAFencedLocalSeatAndNoWaitIsStillInfrastructure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec, cfg.AgentLeaseWaitSec = -1, 0
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || sum.Infrastructure != 1 || sum.Deferred != 1 {
		t.Fatalf("local=%d summary=%+v, want one deferred subtask counted as Infrastructure", localCalls.Load(), sum)
	}
}

// route=spread deals the slot local under the fence and flags it; the dead fleet rides along.
func TestDeadFleetWithAFencedLocalSeatOnASpreadIsStillInfrastructure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		contracts(2), "spread", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || sum.Deferred != 2 || sum.Infrastructure != 2 {
		t.Fatalf("local=%d summary=%+v, want both deferred subtasks counted as Infrastructure", localCalls.Load(), sum)
	}
}

// The same on the default placement path (a white-box run that reaches attempt() directly).
func TestDeadFleetWithAFencedLocalSeatOnTheDefaultPathIsStillInfrastructure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = -1
	cfg.AgentLeaseWaitSec = 0
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{deadRemote(t)},
		intent:         openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{} },
		decider:        noDecision,
	}
	pr := r.runOne(t.Context(), 0, remoteContract())
	if localCalls.Load() != 0 || !pr.Result.Deferred {
		t.Fatalf("local=%d result=%+v, want the fenced defer", localCalls.Load(), pr.Result)
	}
	if !pr.remotesUnreachable {
		t.Fatal("the dead fleet was not recorded on the result: the default path drops the class the fallback used to carry")
	}
}

// When the lease clears and the local seat takes the work, the dead fleet is still reported on
// the success: the placement the fall-through used to make carried it.
func TestDeadFleetIsStillReportedWhenTheFenceClearsAndTheSeatRuns(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	l, err := b.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{leaseCard0}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = l.Release()
	}()
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 || sum.Infrastructure != 1 {
		t.Fatalf("local=%d summary=%+v, want the local success PLUS the dead fleet counted", localCalls.Load(), sum)
	}
}

// Control: a remote that ANSWERS health and cannot take the contract is no dead fleet. The
// subtask waits in line behind the fence and ends as a capacity defer with nothing infrastructure.
func TestAnAnsweringButIneligibleRemoteIsNotADeadFleetUnderAFence(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1
	_, url := ineligibleNode(t)
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deferred != 1 || sum.Infrastructure != 0 {
		t.Fatalf("summary = %+v, want a capacity defer and nothing infrastructure: the remote answered", sum)
	}
}

// The same control with the wait switched off: no tick runs, so nothing but the class the
// placement carried can say whether the fleet was dead. A remote that answered the placement's
// probe and could not take the contract is not a dead fleet, and the fenced defer is not counted.
func TestAnAnsweringButIneligibleRemoteIsNotADeadFleetUnderAFenceWithNoWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec, cfg.AgentLeaseWaitSec = -1, 0
	_, url := ineligibleNode(t)
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || sum.Deferred != 1 || sum.Infrastructure != 0 {
		t.Fatalf("local=%d summary=%+v, want the fenced defer and nothing infrastructure: the remote answered", localCalls.Load(), sum)
	}
}

// Control: a remote that fails health at first and comes back during the wait took the work, so
// the fleet was not dead: the run reports no infrastructure failure.
func TestARemoteThatComesBackDuringTheWaitIsNotADeadFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	b.hold(gpulease.ClassMedia, leaseCard0)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	// The placement's own probes fail; the wait's later ticks find it healthy.
	node, url := acceptingNode(t, "node-back", "zorblax from the node that came back", func(f *fakeNode) {
		f.healthFailFn = func(n int64) bool { return n <= 3 }
	})
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if node.dispatches.Load() != 1 || sum.Succeeded != 1 || results[0].Node != "node-back" {
		t.Fatalf("dispatches=%d summary=%+v node=%q, want the work to land on the node that came back", node.dispatches.Load(), sum, results[0].Node)
	}
	if sum.Infrastructure != 0 {
		t.Fatalf("summary = %+v: the fleet answered and took the work, it was not dead", sum)
	}
}

// holdTextThenRelease takes a TEXT reservation on card 0 (it reserves the seat) and releases it
// after a short while, so a subtask waiting behind it gets the seat.
func holdTextThenRelease(t *testing.T, b *flagshipLeaseBox) {
	t.Helper()
	l, err := b.m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "weights A/B", Devices: []string{leaseCard0}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = l.Release()
	}()
	t.Cleanup(func() { _ = l.Release() })
}

// The text-reservation sentinel carries the dead fleet too: the lease clears, the seat runs the
// work, and the run still reports the fleet it declined to wait for.
func TestDeadFleetIsStillReportedWhenATextReservationClearsAndTheSeatRuns(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	holdTextThenRelease(t, b)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 1 || sum.Succeeded != 1 || sum.Infrastructure != 1 {
		t.Fatalf("local=%d summary=%+v, want the local success PLUS the dead fleet counted", localCalls.Load(), sum)
	}
}

func TestDeadFleetIsStillReportedWhenATextReservationClearsOnASpread(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	holdTextThenRelease(t, b)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	var localCalls atomic.Int64
	_, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		contracts(2), "spread", []string{deadRemote(t)}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 2 || sum.Succeeded != 2 || sum.Infrastructure != 2 {
		t.Fatalf("local=%d summary=%+v, want both local successes PLUS the dead fleet counted", localCalls.Load(), sum)
	}
}

func TestDeadFleetIsStillReportedWhenATextReservationClearsOnTheDefaultPath(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	holdTextThenRelease(t, b)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 5
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{deadRemote(t)},
		intent:         openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{} },
		decider:        noDecision,
	}
	pr := r.runOne(t.Context(), 0, remoteContract())
	if localCalls.Load() != 1 || pr.Result.Deferred || !pr.remotesUnreachable {
		t.Fatalf("local=%d deferred=%v unreachable=%v, want the local success carrying the dead fleet", localCalls.Load(), pr.Result.Deferred, pr.remotesUnreachable)
	}
}
