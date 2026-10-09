package delegate

// The capacity wait runs to the call's deadline (ADR 0073).
//
// Until ADR 0073 the wait ended at agent_placement_wait_sec (120 s by default) whatever the call
// had left, and a call has 1,500 s: 85 of the 281 deferred delegate rows of one day were
// "capacity wait: no node had room within 2m0s" for subtasks that a node freeing a minute later
// would have taken (the 2026-10-07 diagnosis, M1). ADRs 0063 and 0065 both named the fix as the
// next step and kept it back until the call had a deadline of its own, because an open-ended wait
// must never outlive the client's abort.
//
// A call that has a deadline waits until it, less a reserve a placed job needs to run in. A call
// that has none keeps the configured TTL. Every clock here is compressed to milliseconds; the
// reserve and the built-in TTL are vars for that, as the poll clocks are.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// withCallReserve sets the part of a call a capacity wait leaves unspent for one test.
func withCallReserve(t *testing.T, d time.Duration) {
	t.Helper()
	old := callWaitReserve
	callWaitReserve = d
	t.Cleanup(func() { callWaitReserve = old })
}

// withBuiltInWait compresses the 120 s a call with no deadline waits when agent_placement_wait_sec
// is unset.
func withBuiltInWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := placementWaitDefault
	placementWaitDefault = d
	t.Cleanup(func() { placementWaitDefault = old })
}

// fullUntil is a dispatch hook for a node that is full for d after its FIRST dispatch, and takes work
// after that. It is decided by the clock, not by a count, so how often the wait ticks cannot move the
// instant the node frees; and the clock starts at the first dispatch, not when the hook is made, so
// the test's own setup (the fixture, the health read, the deal) never eats into the window on a loaded
// runner (CI, 2026-10-08: 0.44 s of a 1.5 s window, and capacity_wait_sec read 1.06 under its 1.2 floor).
func fullUntil(d time.Duration) func(int64) int {
	var (
		mu      sync.Mutex
		freesAt time.Time
	)
	return func(int64) int {
		mu.Lock()
		defer mu.Unlock()
		if freesAt.IsZero() {
			freesAt = time.Now().Add(d)
		}
		if time.Now().Before(freesAt) {
			return http.StatusServiceUnavailable
		}
		return 0
	}
}

// TestAWaitRunsToTheCallDeadlineNotTheConfiguredTTL is the defect: the one node is full for 1.5 s
// from its first dispatch and then frees; the TTL is 1 s (the configured setting, standing in for the 120 s that ended the
// diagnosed waits) and the call has 4 s. Before ADR 0073 the wait ended at 1 s as a capacity defer
// and the node took nothing; now the subtask waits for the node and lands on it, and the call is
// over before its deadline.
//
// The credited capacity_wait_sec is the wait's idle time: every re-ask of the node is an attempt,
// charged to the budget and left out of the credit (awaitCapacity's spanStart). At a 20 ms poll the
// wait re-asked the node some 75 times inside the 1.5 s, and on a loaded CI runner those re-asks
// took 0.44 s of it (2026-10-08, credited 1.06 under the 1.2 floor). The poll here is 250 ms: a
// handful of re-asks, so what is credited is the wait and not the runner's speed.
func TestAWaitRunsToTheCallDeadlineNotTheConfiguredTTL(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 250*time.Millisecond, 0)
	withCallReserve(t, 300*time.Millisecond)
	node, url := acceptingNode(t, "node-late", "answer after a long wait", func(f *fakeNode) {
		f.dispatchHook = fullUntil(1500 * time.Millisecond)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1 // on HEAD the wait ends here, 0.5 s before the node frees

	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "remote", []string{url}, deadlineIn(4*time.Second), nil)

	pr := results[0]
	if sum.Succeeded != 1 || sum.Waited != 1 || pr.Node != "node-late" || pr.Result.Deferred {
		t.Fatalf("summary %+v result node %q deferred %v (%s), want the subtask placed on the node once it freed", sum, pr.Node, pr.Result.Deferred, pr.Result.Reason)
	}
	if elapsed < 1400*time.Millisecond || elapsed > 3700*time.Millisecond {
		t.Fatalf("the call took %s, want it to wait out the 1.5 s the node was full and finish before the 4 s deadline", elapsed)
	}
	t.Logf("elapsed %s, capacity_wait_sec %.2f, dispatches %d", elapsed, pr.CapacityWaitSec, node.dispatches.Load())
	if pr.CapacityWaitSec < 1.2 {
		t.Fatalf("capacity_wait_sec = %.2f, want the ~1.5 s the wait lasted credited", pr.CapacityWaitSec)
	}
	if node.dispatches.Load() < 3 {
		t.Fatalf("the node saw %d dispatches, want it re-asked during the wait", node.dispatches.Load())
	}
}

// TestAnUnsetWaitFollowsTheCallDeadlineToo: the same with agent_placement_wait_sec unset, the
// operator's default. The built-in 120 s (compressed here to 0.4 s) is the wait of a call with no
// deadline; this call has one.
func TestAnUnsetWaitFollowsTheCallDeadlineToo(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 300*time.Millisecond)
	withBuiltInWait(t, 400*time.Millisecond)
	_, url := acceptingNode(t, "node-late", "answer after a long wait", func(f *fakeNode) {
		f.dispatchHook = fullUntil(1200 * time.Millisecond)
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	results, sum, _ := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "remote", []string{url}, deadlineIn(3*time.Second), nil)
	if sum.Succeeded != 1 || results[0].Node != "node-late" {
		t.Fatalf("summary %+v result %+v, want the subtask placed once the node freed at 1.2 s, past the built-in TTL", sum, results[0].Result)
	}
}

// TestACallWithNoDeadlineKeepsTheBuiltInWait is the control arm: no deadline, no setting, the
// built-in wait (compressed to 0.6 s) is the bound, and the defer says so in the words it always
// had, naming the setting that bounded it.
func TestACallWithNoDeadlineKeepsTheBuiltInWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	withBuiltInWait(t, 600*time.Millisecond)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, nil, nil)

	r := results[0].Result
	if sum.Deferred != 1 || r.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary %+v result %+v, want the wait's own capacity defer", sum, r)
	}
	if elapsed < 550*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("the wait took %s, want the built-in wait (0.6 s here)", elapsed)
	}
	for _, want := range []string{"no node had room within 600ms", "agent_placement_wait_sec=0", "raise agent_placement_wait_sec"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", r.Reason, want)
		}
	}
}

// TestAWaitThatNeverFreesEndsBeforeTheCallDeadlineAsACapacityDefer: the node never frees. The wait
// ends at the deadline less the reserve (2 s of a 3 s call), as a capacity defer that says the
// call bounded it, and nothing is dispatched inside the reserve: a job placed there could only be
// cut, and would run on in its node with nobody waiting for it.
//
// The clock assertions are one-sided on purpose: the wait lasted at least most of the way to the
// horizon (a lower bound with slack), and the last dispatch left well before the deadline (the
// reserve is 1 s and the allowance 400 ms, so a starved runner has to stall for 600 ms to fail it).
// That the call ended before its deadline is not timed at all: a call that missed it would be the
// call-deadline cut, which the class and the cut flag below already refuse.
func TestAWaitThatNeverFreesEndsBeforeTheCallDeadlineAsACapacityDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, time.Second)
	var lastDispatch atomic.Int64
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchHook = func(int64) int { lastDispatch.Store(time.Now().UnixNano()); return 0 } // 0 falls through to the 503
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	opts := deadlineIn(3 * time.Second)
	results, sum, elapsed := runWithin(t, 10*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, opts, nil)

	pr := results[0]
	if sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity || pr.deadlineCut {
		t.Fatalf("summary %+v result %+v cut %v, want a capacity defer, not a call-deadline cut", sum, pr.Result, pr.deadlineCut)
	}
	if elapsed < 1700*time.Millisecond {
		t.Fatalf("the call took %s, want it to wait to the horizon (2 s here), not give up early", elapsed)
	}
	horizon := opts.Deadline.Add(-time.Second)
	if late := time.Duration(lastDispatch.Load() - horizon.UnixNano()); late > 400*time.Millisecond {
		t.Fatalf("a dispatch went out %s after the horizon: the reserve was spent placing work", late)
	}
	for _, want := range []string{"before the call's deadline", "bounded by the call's deadline, not by agent_placement_wait_sec", "re-run later or add a node"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
	if strings.Contains(pr.Result.Reason, "raise agent_placement_wait_sec") {
		t.Errorf("reason = %q tells the caller to raise a setting that never bounded this wait", pr.Result.Reason)
	}
}

// TestASheddableSubtaskIsShedAtOnceWhateverTheCallHasLeft: measurement traffic never waits, with
// or without a deadline. It is shed on the first refusal, as it always was.
func TestASheddableSubtaskIsShedAtOnceWhateverTheCallHasLeft(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 300*time.Millisecond)
	node, url := refusingNode(t, "node-busy", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	opts := deadlineIn(5 * time.Second)
	opts.Priority = core.BandSheddable
	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, opts, nil)

	// A wait would have run to the horizon of the 5 s call, so 3 s separates "shed at once" from "waited"
	// with room for a loaded runner.
	if elapsed > 3*time.Second {
		t.Fatalf("the sheddable subtask took %s: it waited for capacity", elapsed)
	}
	if sum.Shed != 1 || !results[0].shed || results[0].Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("summary %+v result %+v, want it shed (capacity class)", sum, results[0].Result)
	}
	if node.dispatches.Load() != 1 {
		t.Fatalf("the node saw %d dispatches, want exactly the one refusal", node.dispatches.Load())
	}
}

// TestAnOffSwitchIsNotTurnedOnByTheCallDeadline: a negative agent_placement_wait_sec switches the
// wait off for every call. A deadline does not switch it back on.
func TestAnOffSwitchIsNotTurnedOnByTheCallDeadline(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 300*time.Millisecond)
	node, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t) // AgentPlacementWaitSec: -1

	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(5*time.Second), nil)

	if elapsed > 3*time.Second || sum != (Summary{Failed: 1}) || !strings.HasPrefix(results[0].Err, replacementExhaustedPrefix) {
		t.Fatalf("took %s, summary %+v, err %q: want the pre-0.113.18 failure at once", elapsed, sum, results[0].Err)
	}
	if node.dispatches.Load() != 1 {
		t.Fatalf("the node saw %d dispatches, want 1 (no wait, no re-ask)", node.dispatches.Load())
	}
}

// TestACallWithLessLeftThanTheReserveDoesNotWait: the call has less left than a placed job needs.
// There is nothing to wait for, and this is neither the operator's off switch ("placement
// refused", wait disabled) nor a wait that ran: it is a capacity defer that says why it did not
// wait.
func TestACallWithLessLeftThanTheReserveDoesNotWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 5*time.Second)
	node, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(2*time.Second), nil)

	pr := results[0]
	// No stopwatch bound: with a reserve longer than the 2 s call there is no wait to measure, and a runner
	// stalled past the deadline would make this the call-deadline cut, which the class below refuses.
	if sum.Deferred != 1 || sum.Failed != 0 || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("took %s, summary %+v, result %+v: want an immediate capacity defer, not a failure", elapsed, sum, pr.Result)
	}
	for _, want := range []string{"the call had only", "so it did not wait"} {
		if !strings.Contains(pr.Result.Reason, want) {
			t.Errorf("reason = %q, want it to contain %q", pr.Result.Reason, want)
		}
	}
	if strings.Contains(pr.Result.Reason, "wait disabled") || strings.Contains(pr.Err, replacementExhaustedPrefix) {
		t.Errorf("result %+v: a call with no time left was reported as the operator's off switch", pr)
	}
	if node.dispatches.Load() != 1 {
		t.Fatalf("the node saw %d dispatches, want the one refusal that started it", node.dispatches.Load())
	}
}

// TestALeaseWaitIsBoundedByTheCallToo: the local seat is reserved by a text lease and there is
// nowhere else. The wait runs to the horizon, and the holder-naming deferral that ends it does not
// tell the caller to set agent_lease_wait_sec longer: the wait already ran as long as the call
// could spare. (The clock bound is one-sided, as in the test above: the call's end before its
// deadline is refused by the capacity class and the holder-naming words, not by a stopwatch.)
func TestALeaseWaitIsBoundedByTheCallToo(t *testing.T) {
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, time.Second)
	dir, _ := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 0

	results, sum, elapsed := runWithin(t, 8*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "auto", nil, deadlineIn(3*time.Second), nil)

	if sum.Deferred != 1 || sum.Infrastructure != 1 {
		t.Fatalf("summary %+v, want the holder-naming infrastructure deferral", sum)
	}
	if elapsed < 1700*time.Millisecond {
		t.Fatalf("the call took %s, want it to wait out the lease to the horizon (2 s here)", elapsed)
	}
	r := results[0].Result.Reason
	for _, want := range []string{`reason="soak"`, "bounded by the call's deadline", "release the lease"} {
		if !strings.Contains(r, want) {
			t.Errorf("reason = %q, want it to contain %q", r, want)
		}
	}
	if strings.Contains(r, "set agent_lease_wait_sec to wait longer") {
		t.Errorf("reason = %q tells the caller to lengthen a wait that already ran as long as the call could spare", r)
	}
}

// TestCapacityWaitForChoosesTheBound pins the rule itself, one row per way the bound can come out
// (ADR 0073): the call's deadline less the reserve for a call that has one and nothing left to start,
// whatever the TTL or a lease wait says; the TTL (or the lease wait, when longer) while subtasks of the
// call have not started, unless the call ends sooner; the TTL for a call that has no deadline; the off
// switch for every call; and the TTL for a decided seed, whose expiry runs the contract.
func TestCapacityWaitForChoosesTheBound(t *testing.T) {
	withBuiltInWait(t, 120*time.Second)
	withCallReserve(t, 15*time.Second)
	start := time.Now()
	at := start.Add(1500 * time.Second)
	// call is a call whose subtasks have all started: the one waiting is the last, nothing is behind it.
	call := newCallDeadline(context.Background(), &RunOptions{Deadline: at}, 1)
	call.begin()
	// crowded is a call with a subtask still to start (waiting for a run slot, or in a later chunk).
	crowded := newCallDeadline(context.Background(), &RunOptions{Deadline: at}, 3)
	crowded.begin()
	crowded.begin()
	// closing is a crowded call with 100 ms of wait left before the horizon.
	closing := newCallDeadline(context.Background(), &RunOptions{Deadline: start.Add(15*time.Second + 100*time.Millisecond)}, 3)
	closing.begin()

	cases := []struct {
		name     string
		cfg      config.Config
		call     *callDeadline
		decided  bool
		wantWait time.Duration
		wantEnd  time.Time
		byCall   bool
	}{
		{"unset, call deadline: to the deadline less the reserve", config.Config{}, call, false, 1485 * time.Second, at.Add(-15 * time.Second), true},
		{"explicit TTL, call deadline: the TTL does not bound it", config.Config{AgentPlacementWaitSec: 30}, call, false, 1485 * time.Second, at.Add(-15 * time.Second), true},
		{"unset, no deadline: the built-in wait", config.Config{}, nil, false, 120 * time.Second, start.Add(120 * time.Second), false},
		{"explicit TTL, no deadline: the TTL", config.Config{AgentPlacementWaitSec: 30}, nil, false, 30 * time.Second, start.Add(30 * time.Second), false},
		{"off, call deadline: off", config.Config{AgentPlacementWaitSec: -1}, call, false, 0, start, false},
		{"decided seed, call deadline: the TTL, so its expiry can run the seat", config.Config{}, call, true, 120 * time.Second, start.Add(120 * time.Second), false},
		{"lease wait longer than the call: the call, a wait past its deadline helps no one", config.Config{AgentLeaseWaitSec: 3600}, call, false, 1485 * time.Second, at.Add(-15 * time.Second), true},
		{"lease wait shorter than the call: the call", config.Config{AgentLeaseWaitSec: 60}, call, false, 1485 * time.Second, at.Add(-15 * time.Second), true},
		{"a lease wait set and little time left: the little time, not the lease wait", config.Config{AgentLeaseWaitSec: 10}, closing, false, 100 * time.Millisecond, start.Add(100 * time.Millisecond), true},
		{"subtasks still to start, unset: the built-in wait, not the call", config.Config{}, crowded, false, 120 * time.Second, start.Add(120 * time.Second), false},
		{"subtasks still to start, explicit TTL: the TTL", config.Config{AgentPlacementWaitSec: 30}, crowded, false, 30 * time.Second, start.Add(30 * time.Second), false},
		{"subtasks still to start, lease wait longer than the TTL: the lease wait, as before the deadline existed", config.Config{AgentLeaseWaitSec: 300}, crowded, false, 300 * time.Second, start.Add(300 * time.Second), false},
		{"subtasks still to start, lease wait longer than the call: the call ends it", config.Config{AgentLeaseWaitSec: 3600, AgentPlacementWaitSec: 30}, closing, false, 100 * time.Millisecond, start.Add(100 * time.Millisecond), true},
		{"subtasks still to start but the TTL outlasts the call: the call", config.Config{AgentPlacementWaitSec: 3000}, crowded, false, 1485 * time.Second, at.Add(-15 * time.Second), true},
		{"off but a lease wait set: the lease wait, as before", config.Config{AgentPlacementWaitSec: -1, AgentLeaseWaitSec: 60}, call, false, 60 * time.Second, start.Add(60 * time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{cfg: tc.cfg, call: tc.call}
			got := r.capacityWaitFor(start, tc.decided)
			if got.wait != tc.wantWait || (tc.wantWait > 0 && !got.deadline.Equal(tc.wantEnd)) || (got.call != nil) != tc.byCall {
				t.Fatalf("wait %s ending %s by-call %v, want %s ending %s by-call %v", got.wait, got.deadline.Sub(start), got.call != nil, tc.wantWait, tc.wantEnd.Sub(start), tc.byCall)
			}
		})
	}

	// A call with no more left than the reserve has no wait to run, and says so.
	short := newCallDeadline(context.Background(), &RunOptions{Deadline: start.Add(10 * time.Second)}, 1)
	got := (&runner{cfg: config.Config{}, call: short}).capacityWaitFor(start, false)
	if got.wait != 0 || got.call == nil || !got.call.noTime {
		t.Fatalf("a 10 s call under a 15 s reserve = %+v, want no wait, bounded by the call, flagged noTime", got)
	}
}

// TestNothingIsPlacedInTheReserveAfterTheLastSleep: a TTL wait looks once more after its last
// sleep, at its own end; a wait bounded by the call must not, because what it would place there
// is placed in the reserve it exists to keep. The poll interval is long enough that the wait
// looks twice only (now, and at the horizon, where the sleep is clamped to), and the lease clears
// between the two: the second look would find the seat free and run the subtask on it.
func TestNothingIsPlacedInTheReserveAfterTheLastSleep(t *testing.T) {
	compressWait(t, 10*time.Second, 0)
	withCallReserve(t, 300*time.Millisecond)
	dir, lease := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 0
	var localCalls atomic.Int64
	time.AfterFunc(500*time.Millisecond, func() { _ = lease.Release() })

	results, sum, _ := runWithin(t, 8*time.Second, cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", nil, deadlineIn(1500*time.Millisecond), nil)

	if localCalls.Load() != 0 {
		t.Fatalf("the local seat ran the subtask %d time(s) at the end of the wait, inside the reserve", localCalls.Load())
	}
	if sum.Deferred != 1 || !strings.Contains(results[0].Result.Reason, "bounded by the call's deadline") {
		t.Fatalf("summary %+v result %+v, want the wait's own deferral, bounded by the call", sum, results[0].Result)
	}
}
