package delegate

// A capacity wait may not hold a run slot to the call's horizon while subtasks of the call have not
// started (ADR 0073, amended 2026-10-10).
//
// A call runs runConcurrency (4) subtasks at a time. The first version of the wait ran every subtask to the
// call's deadline less the reserve, so four subtasks waiting on a full fleet held all four slots for the
// rest of the call: the others did not start until the waiters ended, and then started with less than the
// reserve left, where a placed job can only be cut. Two rules close it, and these tests pin both:
// a wait that holds a slot somebody is waiting for holds it for the TTL and then gives it up and keeps
// waiting (the first version of this rule ended the subtask at the TTL, which refused work a node freeing a
// minute later would have taken: register C16), and a subtask is not started inside the reserve.

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
)

// The clocks of the two tests that pin a wait holding a run slot: the four-slot call below and the six-slot call of ADR 0076
// (deal_width_test.go). Both run heldSlotWait on these numbers, so they cannot drift apart.
const (
	// heldTTL is the built-in wait compressed: what a wait that holds a slot somebody is waiting for is held to.
	heldTTL = 600 * time.Millisecond
	// heldWindow is the call's whole time. The last subtask begins one TTL in and waits for the horizon, so it has about
	// 2.4 s to outwait the TTL, and a runner that spent a second of the call setting it up still leaves a window to judge.
	heldWindow = 3500 * time.Millisecond
	// heldReserve is what a wait the call bounds leaves unspent. The call's deadline can cut the last subtask only when the
	// runner stalls for about this long across the horizon, so a cut is a stall of half a second or the defect.
	heldReserve = 500 * time.Millisecond
	// heldPoll is the wait's tick: a handful of re-asks, so what is credited is the wait and not the runner's speed (the
	// reason it is 250 ms in TestAWaitRunsToTheCallDeadlineNotTheConfiguredTTL).
	heldPoll = 250 * time.Millisecond
)

// quietGap is a stretch in which the test process's own heartbeat did not run.
type quietGap struct{ from, to time.Time }

// watchQuiet beats every 5 ms and records each stretch of at least atLeast between two beats: the runner going quiet (the
// process starved of CPU, the machine paused), read off a clock the code under test does not touch. It is the one evidence
// of a stall in the last subtask's final sleep that the call's own events cannot give, because there the defect (a wait
// that ignores its horizon) and the stall end the same way, cut by the call's deadline. The returned function stops it and
// returns the stretches.
func watchQuiet(t *testing.T, atLeast time.Duration) func() []quietGap {
	t.Helper()
	var (
		gaps []quietGap
		once sync.Once
	)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		beat := time.NewTicker(5 * time.Millisecond)
		defer beat.Stop()
		last := time.Now()
		for {
			select {
			case <-quit:
				return
			case <-beat.C:
				now := time.Now()
				if now.Sub(last) >= atLeast {
					gaps = append(gaps, quietGap{last, now})
				}
				last = now
			}
		}
	}()
	stop := func() []quietGap {
		once.Do(func() { close(quit); <-done })
		return gaps
	}
	t.Cleanup(func() { stop() })
	return stop
}

// heldSlotWait runs the contracts as one call against one node that never has room, with one run slot fewer than there are
// contracts (tune sets how many slots the node publishes), and pins ADR 0073 decision 9 as amended on 2026-10-10 (register C16):
// the subtasks that hold a run slot while another has not started give it up at the TTL instead of ending there, so the last
// one begins while the node is still full, and every subtask waits for the call's horizon and ends bound by the call. Before the
// amendment the holders ended at the TTL as capacity defers while the call had most of its time left.
//
// What each assertion proves, and what it does not read:
//
//   - The reasons are rendered from the wait's own decision, so they read no clock. Every subtask's is bounded by the call and
//     says the TTL did not bound it; none names the TTL or says that subtasks "had not started" (the words of a holder cut at its
//     TTL, which would come back if the wait stopped extending itself).
//   - Every subtask that began outlived the TTL by half again (the call's own events: its start and its end), which is what
//     shows a holder was not cut at the TTL. A starved runner can only lengthen what these measure.
//   - The last subtask began while the node was still full, within a TTL and the reserve of the call's start. That is what shows
//     the holders gave their slots up at the TTL: a wait that kept its slot to the horizon starts the last subtask only when a
//     slot frees, and in this call none does.
//   - The credit is not asserted as a floor. Every re-ask of the node is an attempt, charged to the budget and left out of the
//     credit, and a starved runner's re-asks can take all of a wait, so a floor reads the runner's speed. What holds on any
//     runner is that the credit is no longer than the clock.
//
// A skip is allowed only on evidence the runner stalled: the call's own deadline cut came late, or the runner's heartbeat went
// quiet across the horizon, where a subtask that ignores its horizon and a runner stalled across it end the same way (cut by the
// call's deadline). A subtask the deadline cut with no such evidence is the defect and fails.
func heldSlotWait(t *testing.T, contracts []core.AgentContract, tune func(*fakeNode)) {
	t.Helper()
	slots := len(contracts) - 1
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, heldPoll, 0)
	withCallReserve(t, heldReserve)
	withBuiltInWait(t, heldTTL)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, tune)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	// The call's own events say when each subtask began and ended: the production code's clock, not the test's.
	var mu sync.Mutex
	began, ended := map[int]time.Time{}, map[int]time.Time{}
	opts := deadlineIn(heldWindow)
	opts.OnProgress = func(ev ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Kind == "started" {
			began[ev.Index] = time.Now()
		} else {
			ended[ev.Index] = time.Now()
		}
	}
	opened, horizon := opts.Deadline.Add(-heldWindow), opts.Deadline.Add(-heldReserve)
	stopQuiet := watchQuiet(t, heldReserve/2)

	// The limit is a hang guard, not a speed bound: a call that ends at its deadline in 3.5 s took 10 to 20 s to return in
	// three of forty runs at forty busy loops on one P, and that is a frozen runner and not a call that never returns.
	results, sum, elapsed := runWithin(t, 30*time.Second, cfg, neverLocal(t), contracts, "remote", []string{url}, opts, nil)

	quiet := stopQuiet()
	mu.Lock()
	defer mu.Unlock()
	if sum.Deferred != len(contracts) {
		t.Fatalf("summary %+v, want every subtask deferred for capacity: the node never had room", sum)
	}
	at := func(ts time.Time) time.Duration { return ts.Sub(opened).Round(time.Millisecond) }

	// stalled says why the runner stalled, or "" when nothing shows it.
	stalled := func() string {
		var why []string
		if elapsed > heldWindow+heldReserve/2 {
			why = append(why, fmt.Sprintf("the call took %s against a deadline of %s: its own cut came late", elapsed.Round(time.Millisecond), heldWindow))
		}
		for _, g := range quiet {
			if g.to.After(horizon) && g.from.Before(opts.Deadline) {
				why = append(why, fmt.Sprintf("the runner was quiet for %s across the horizon", g.to.Sub(g.from).Round(time.Millisecond)))
			}
		}
		return strings.Join(why, "; ")
	}

	for i, pr := range results {
		r := pr.Result.Reason
		if strings.Contains(r, callDeadlinePrefix) {
			// The call's deadline cut this wait, which a runner stalled across the horizon does to any of them.
			if why := stalled(); why != "" {
				t.Skip(why + ": nothing to prove about a wait the deadline cut")
			}
			t.Errorf("result %d reason = %q, want a wait that ran to the call's horizon: the call's deadline cut it and nothing shows the runner stalled", i, r)
			continue
		}
		for _, want := range []string{"before the call's deadline", "bounded by the call's deadline, not by agent_placement_wait_sec"} {
			if !strings.Contains(r, want) {
				t.Errorf("result %d reason = %q, want it to contain %q: a wait that held a run slot runs on to the call's horizon", i, r, want)
			}
		}
		for _, bad := range []string{"subtask(s) of the call had not started", "no node had room within " + heldTTL.String()} {
			if strings.Contains(r, bad) {
				t.Errorf("result %d reason = %q names %q: that wait ended at its TTL, which is the defect", i, r, bad)
			}
		}
		b, ok := began[i]
		if !ok {
			t.Errorf("result %d never began: it was never given a run slot, though the holders gave theirs up at their %s TTL", i, heldTTL)
			continue
		}
		life := ended[i].Sub(b)
		if want := heldTTL * 3 / 2; life < want {
			t.Errorf("result %d lived %s, want at least %s: it should outwait the %s TTL it used to be cut at", i, life.Round(time.Millisecond), want, heldTTL)
		}
		if got := pr.CapacityWaitSec; got > life.Seconds() {
			t.Errorf("result %d was credited %.2f s but lived %s: a wait is credited no more than the clock says", i, got, life.Round(time.Millisecond))
		}
	}
	lastBegan := "never"
	if b, ok := began[slots]; ok {
		lastBegan = at(b).String()
		if late := b.Sub(opened); late > heldTTL+2*heldReserve {
			t.Errorf("the last subtask began %s into the call, want it started by the slots the first ones gave up at their %s TTL (by about %s): it waited for a slot that never freed", at(b), heldTTL, (heldTTL + 2*heldReserve).Round(time.Millisecond))
		}
	}
	t.Logf("call %s of a %s window (horizon %s); %d subtask(s), the last began %s", elapsed.Round(time.Millisecond), heldWindow, at(horizon), len(results), lastBegan)
}

// TestAWaitHoldingARunSlotGivesItUpAtItsTTLAndKeepsWaiting: five subtasks, four run slots, one node that never has room. The
// first four hold the four slots for the TTL (the built-in wait compressed) and then give them up, so the fifth begins; none
// ends at the TTL, and all five wait for the call's horizon and end as the capacity defer that says the call bounded them.
// heldSlotWait says what is asserted and why none of it reads the runner's speed.
func TestAWaitHoldingARunSlotGivesItUpAtItsTTLAndKeepsWaiting(t *testing.T) {
	contracts := make([]core.AgentContract, runConcurrency+1)
	for i := range contracts {
		contracts[i] = remoteContract()
	}
	heldSlotWait(t, contracts, nil)
}

// TestASubtaskIsNotStartedInsideTheReserve: five subtasks on a local seat that takes 1 s each, a 2 s call
// and a 1.2 s reserve. The first four start at once (2 s left); the fifth gets a slot with about 1 s left,
// no more than a placed job needs, and is not started: a capacity defer that says so, with the seat asked
// four times.
func TestASubtaskIsNotStartedInsideTheReserve(t *testing.T) {
	withCallReserve(t, 1200*time.Millisecond)
	cfg := testCfg(t)
	var started atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		started.Add(1)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
		return localOK(), nil
	}
	contracts := make([]core.AgentContract, runConcurrency+1)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "answer the question"}
	}
	results, sum, _ := runWithin(t, 10*time.Second, cfg, local, contracts, "local", nil, deadlineIn(2*time.Second), nil)

	if got := started.Load(); got != int64(runConcurrency) {
		t.Fatalf("the seat was started %d times, want %d: the fifth subtask got its slot inside the reserve and must not begin", got, runConcurrency)
	}
	last := results[runConcurrency]
	if sum.Deferred != 1 || sum.Succeeded != runConcurrency {
		t.Fatalf("summary %+v, want the four started subtasks to succeed and the fifth to defer", sum)
	}
	if !last.Result.Deferred || last.Result.DeferClass != core.DeferClassCapacity || !last.Unplaced || last.deadlineCut {
		t.Fatalf("last result %+v (cut %v), want an unplaced capacity defer: the deadline had not passed, so it is not the call-deadline cut", last.Result, last.deadlineCut)
	}
	for _, want := range []string{"not started: the call's deadline left no room", "no more than the 1s a placed job needs to run in"} {
		if !strings.Contains(last.PlacementReason, want) || !strings.Contains(last.Result.Reason, want) {
			t.Errorf("placement reason %q / result reason %q, want both to contain %q", last.PlacementReason, last.Result.Reason, want)
		}
	}
	if strings.Contains(last.Result.Reason, callDeadlinePrefix) {
		t.Errorf("reason = %q opens with the call-deadline prefix, a grep key for \"the deadline passed\", which it had not", last.Result.Reason)
	}
}

// TestALaterChunkIsNotStartedInsideTheReserve: the same rule across the chunks of a batched call (a list
// longer than one batch of 16, ADR 0076). The first chunk runs in two waves of 2 s and uses the call's
// time, and the subtasks of its third wave are not started; the second chunk finds about 2 s left of a
// 3 s reserve and starts nothing, so its subtasks end at once as the same defer instead of beginning in
// the last seconds. The scale is wide on purpose: the second wave must start with more than the reserve
// left (it does, with a second to spare) and the call must not reach its deadline first (it has two), so
// a loaded runner has to stall for a full second to move either.
func TestALaterChunkIsNotStartedInsideTheReserve(t *testing.T) {
	withCallReserve(t, 3*time.Second)
	cfg := testCfg(t)
	var started atomic.Int64
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		started.Add(1)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		return localOK(), nil
	}
	contracts := make([]core.AgentContract, MaxBatchSubtasks+2)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "answer the question"}
	}
	results, sum, err := RunBatched(t.Context(), cfg, local, contracts, "local", nil, deadlineIn(6*time.Second))
	if err != nil {
		t.Fatalf("RunBatched: %v", err)
	}
	// Two waves of runConcurrency (4) started: route=local has no deal to size the call from.
	if got := started.Load(); got != int64(2*runConcurrency) {
		t.Fatalf("the seat was started %d times, want %d (two waves of the first chunk): the third wave, and the second chunk, began inside the reserve", got, 2*runConcurrency)
	}
	if sum.Batches != 2 || sum.Deferred != len(contracts)-2*runConcurrency {
		t.Fatalf("summary %+v, want two chunks and the %d subtasks past the second wave deferred", sum, len(contracts)-2*runConcurrency)
	}
	for _, pr := range results[2*runConcurrency:] {
		if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity || !strings.Contains(pr.Result.Reason, "the call's deadline left no room") {
			t.Errorf("later result %+v, want the no-room capacity defer", pr.Result)
		}
	}
}

// TestTheReserveIsTakenOnlyOutOfACallLongerThanIt: a call of 10 s under a 15 s reserve has nothing to take
// the reserve out of, so it keeps starting its subtasks (the deadline cuts them, as before ADR 0073); a
// call that had more than the reserve and has used it up starts nothing; a call with no deadline is never
// refused.
func TestTheReserveIsTakenOnlyOutOfACallLongerThanIt(t *testing.T) {
	withCallReserve(t, 15*time.Second)
	short := newCallDeadline(context.Background(), &RunOptions{Deadline: time.Now().Add(10 * time.Second)}, 1)
	if left, no := short.noRoom(); no {
		t.Errorf("a 10 s call under a 15 s reserve reports no room (left %s): it would never start anything", left)
	}
	long := newCallDeadline(context.Background(), &RunOptions{Deadline: time.Now().Add(1500 * time.Second)}, 1)
	if left, no := long.noRoom(); no {
		t.Errorf("a fresh 1,500 s call reports no room (left %s)", left)
	}
	spent := &callDeadline{at: time.Now().Add(10 * time.Second), span: 1500 * time.Second}
	if left, no := spent.noRoom(); !no || left > 10*time.Second {
		t.Errorf("a 1,500 s call with 10 s left reports no room = %v (left %s), want true", no, left)
	}
	var none *callDeadline
	if _, no := none.noRoom(); no {
		t.Error("a call with no deadline reports no room")
	}
}
