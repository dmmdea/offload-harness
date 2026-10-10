package delegate

// A capacity wait may not hold a run slot to the call's horizon while subtasks of the call have not
// started (ADR 0073).
//
// A call runs runConcurrency (4) subtasks at a time. The first version of the wait ran every subtask to the
// call's deadline less the reserve, so four subtasks waiting on a full fleet held all four slots for the
// rest of the call: the others did not start until the waiters ended, and then started with less than the
// reserve left, where a placed job can only be cut. Two rules close it, and these tests pin both:
// a wait that holds a slot somebody is waiting for keeps the bound it had before the deadline existed
// (only the last subtask to start waits for the horizon), and a subtask is not started inside the reserve.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
// contracts (tune sets how many slots the node publishes), and pins ADR 0073 decision 9: the subtasks that hold a slot while
// another has not started wait only the TTL and say why; the last, started when the first slot freed, has nothing behind it
// and waits for the call's horizon.
//
// What each assertion proves, and what it does not read:
//
//   - The reasons are rendered from the wait's own decision, so they read no clock and run before any skip. A holder's names
//     the TTL, the zero agent_placement_wait_sec and the subtasks that had not started; the last subtask's is bounded by the
//     call and does not say "had not started" (a last subtask held to the TTL does, as it would if the call still counted a
//     begun subtask as unstarted). A holder whose reason says the call bounded its wait fails, unless its wait can have begun
//     late enough to have nothing behind it (after the last subtask did, or with under a TTL left to the horizon): a wait
//     credited c that ended at e began no later than e-c, which is what a slow first dispatch leaves, with its start event
//     long before. A stall elsewhere in the call excuses none of that, so holders run to the horizon fail on a loaded runner
//     too. A holder is given no credit ceiling: the credit counts a late wake as idle time, so a ceiling would read the
//     runner's speed, and the reason already says which bound the wait had.
//   - A holder lived at least its TTL, and the last subtask at least one and a half. Both read the call's own events, and a
//     starved runner can only lengthen what they measure. This is what shows the last subtask outwaiting the TTL.
//   - The credit is not asserted as a floor. Every re-ask of the node is an attempt, charged to the budget and left out of the
//     credit, and a starved runner's re-asks can take all of a wait (0.00 s credited over a 2.4 s wait, at forty busy loops on
//     one P), so a floor reads the runner's speed. What holds on any runner is that the credit is no longer than the clock.
//
// A skip is allowed only on evidence the runner stalled that does not come from the last subtask's bound: the call had little
// left when it began, its own deadline cut came late, the first slot to free was held long past its TTL or was taken late,
// the last subtask's re-asks of the node took a quarter of a second or more, or the runner's heartbeat went quiet across the
// horizon. A window under two TTLs is skipped as well: it cannot tell a wait held to the TTL from one run to the horizon. A
// last subtask the call's deadline cut, or that never began, with none of that, is the defect (a wait that ignores its bound,
// or holders that ignore their TTL) and fails. The thresholds on lateness (all but the setup one) are a quarter of a second,
// half the reserve: the deadline cuts the last subtask only after a stall of the whole reserve, so a stall that did it leaves
// evidence of that size. The price is that a quarter-second stall in a call that also ignores its horizon is excused as well.
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
	// three of twenty runs at forty busy loops on one P, and that is a frozen runner and not a call that never returns.
	results, sum, elapsed := runWithin(t, 30*time.Second, cfg, neverLocal(t), contracts, "remote", []string{url}, opts, nil)

	quiet := stopQuiet()
	mu.Lock()
	defer mu.Unlock()
	if sum.Deferred != len(contracts) {
		t.Fatalf("summary %+v, want every subtask deferred for capacity: the node never had room", sum)
	}
	b7, began7 := began[slots]
	at := func(ts time.Time) time.Duration { return ts.Sub(opened).Round(time.Millisecond) }
	lastBegan := "never"
	if began7 {
		lastBegan = at(b7).String()
	}
	// outside is how much of a subtask's life, from its start event to its end event, was not credited as idle wait: its
	// dispatches before the wait and its re-asks of the node during it (the credit leaves both out). Milliseconds on a runner
	// that keeps up, and 0 for a subtask that never began.
	outside := func(i int) time.Duration {
		b, ok := began[i]
		if !ok {
			return 0
		}
		return ended[i].Sub(b) - time.Duration(results[i].CapacityWaitSec*float64(time.Second))
	}

	// stalled says why the runner stalled, from what is not the last subtask's own wait, or "" when nothing shows it.
	stalled := func() string {
		var why []string
		if b0, ok := began[0]; !ok || opts.Deadline.Sub(b0) < 3*heldTTL+heldReserve {
			why = append(why, "the call had under three TTLs and the reserve left when its first subtask began: the runner spent the call setting it up")
		}
		if elapsed > heldWindow+heldReserve/2 {
			why = append(why, fmt.Sprintf("the call took %s against a deadline of %s: its own cut came late", elapsed.Round(time.Millisecond), heldWindow))
		}
		// The first slot to free, when it freed the way the TTL frees it: a holder run to the horizon is the defect and
		// outlives its TTL by design, so it is no evidence of a stall.
		first := -1
		for i := range slots {
			if e, ok := ended[i]; ok && strings.Contains(results[i].Result.Reason, "subtask(s) of the call had not started") && (first < 0 || e.Before(ended[first])) {
				first = i
			}
		}
		if b, ok := began[first]; ok && first >= 0 {
			if life := ended[first].Sub(b); life > heldTTL+heldReserve/2 {
				why = append(why, fmt.Sprintf("the first slot to free was held %s against a TTL of %s", life.Round(time.Millisecond), heldTTL))
			}
			if began7 && b7.Sub(ended[first]) > heldReserve/2 {
				why = append(why, fmt.Sprintf("the last subtask began %s after the first slot freed", b7.Sub(ended[first]).Round(time.Millisecond)))
			}
		}
		if spent := outside(slots); spent >= heldReserve/2 {
			why = append(why, fmt.Sprintf("the last subtask's re-asks of the node took %s of the %s it lived", spent.Round(time.Millisecond), ended[slots].Sub(b7).Round(time.Millisecond)))
		}
		for _, g := range quiet {
			if g.to.After(horizon) && g.from.Before(opts.Deadline) {
				why = append(why, fmt.Sprintf("the runner was quiet for %s across the horizon", g.to.Sub(g.from).Round(time.Millisecond)))
			}
		}
		return strings.Join(why, "; ")
	}

	for i, pr := range results[:slots] {
		r := pr.Result.Reason
		switch {
		case strings.Contains(r, "subtask(s) of the call had not started"):
			for _, want := range []string{"no node had room within " + heldTTL.String(), "agent_placement_wait_sec=0"} {
				if !strings.Contains(r, want) {
					t.Errorf("result %d reason = %q, want it to contain %q: a wait holding a run slot keeps its TTL", i, r, want)
				}
			}
			if strings.Contains(r, "before the call's deadline") {
				t.Errorf("result %d reason = %q says the call bounded a wait that held a slot the last subtask needed", i, r)
			}
			if b, ok := began[i]; ok {
				if life := ended[i].Sub(b); life < heldTTL {
					t.Errorf("result %d lived %s, want at least its %s TTL", i, life.Round(time.Millisecond), heldTTL)
				}
			}
		case strings.Contains(r, "before the call's deadline"):
			// The call bounded this wait, which is what a wait with nothing behind it does: one that began after the last
			// subtask did, or with under a TTL left to the horizon. A holder whose first dispatch was slow gets there with its
			// start event long before, but a wait credited c that ended at e began no later than e-c, so a wait that cannot
			// have begun that late is the defect. This is a decision string, so a stall elsewhere in the call excuses nothing
			// here: holders run to the horizon fail on a loaded runner too.
			from := horizon.Add(-heldTTL)
			if began7 && b7.Before(from) {
				from = b7
			}
			if latest := ended[i].Add(-time.Duration(pr.CapacityWaitSec * float64(time.Second))); latest.Before(from) {
				t.Errorf("result %d reason = %q, want a wait that held its run slot only for the %s TTL: it began no later than %s into the call, before the last subtask did (%s) and with room before the horizon (%s)", i, r, heldTTL, at(latest), lastBegan, at(horizon.Add(-heldTTL)))
			}
		case strings.Contains(r, callDeadlinePrefix):
			// The call's deadline cut this wait, which a runner stalled across the horizon does to any of them.
			if stalled() == "" {
				t.Errorf("result %d reason = %q, want a wait that ended at its %s TTL: the call's deadline cut it and nothing shows the runner stalled", i, r, heldTTL)
			}
		default:
			t.Errorf("result %d reason = %q, want it to name the %s TTL and the subtasks that had not started", i, r, heldTTL)
		}
	}

	last := results[slots]
	reason := last.Result.Reason
	var avail, life time.Duration
	thin := ""
	if began7 {
		avail, life = horizon.Sub(b7), ended[slots].Sub(b7)
		if avail < 2*heldTTL {
			thin = fmt.Sprintf("the last subtask began only %s before the horizon, under two TTLs of %s", avail.Round(time.Millisecond), heldTTL)
		}
	}
	lastRan := "never began"
	if began7 {
		lastRan = fmt.Sprintf("began %s and ended %s", at(b7), at(ended[slots]))
	}
	t.Logf("call %s of a %s window (horizon %s); last subtask %s, credited %.2f s; %d quiet stretch(es)",
		elapsed.Round(time.Millisecond), heldWindow, at(horizon), lastRan, last.CapacityWaitSec, len(quiet))
	switch {
	case !began7 || strings.Contains(reason, callDeadlinePrefix):
		// The call's deadline cut it, or it never got a slot. A wait that ignores its horizon and a runner stalled across it
		// end the same way, so only evidence from elsewhere in the call may excuse it.
		if why := strings.Join(slices.DeleteFunc([]string{stalled(), thin}, func(s string) bool { return s == "" }), "; "); why != "" {
			t.Skip(why + ": nothing to prove about the last subtask's wait")
		}
		what := "was cut by the call's deadline"
		if !began7 {
			what = "never began"
		}
		t.Fatalf("the last subtask %s (its reason %q) and nothing shows the runner stalled: a holder's wait did not end at its TTL to free a run slot, or the last subtask's wait ran past the call's horizon", what, reason)
	case strings.Contains(reason, "had not started"):
		t.Errorf("the last subtask's reason = %q, want it bounded by the call: nothing was behind it, so it was not held to the TTL", reason)
	case !strings.Contains(reason, "before the call's deadline"):
		t.Errorf("the last subtask's reason = %q, want it bounded by the call: nothing was behind it, so it may wait to the horizon", reason)
	}
	if thin != "" {
		t.Skip(thin + ": nothing to prove about the last subtask's wait")
	}

	if want := heldTTL * 3 / 2; life < want {
		t.Errorf("the last subtask lived %s, want at least %s: it should outwait the %s TTL the earlier ones were held to and run on to the call's horizon", life.Round(time.Millisecond), want, heldTTL)
	}
	if got := last.CapacityWaitSec; got > life.Seconds() {
		t.Errorf("the last subtask was credited %.2f s but lived %s: a wait is credited no more than the clock says", got, life.Round(time.Millisecond))
	}
}

// TestAWaitHoldingARunSlotKeepsItsTTLWhileSubtasksAreUnstarted: five subtasks, four run slots, one node
// that never has room. The first four wait for the TTL (the built-in wait compressed) and end as the
// capacity defer they always ended as, saying why the call's own time did not bound them; the fifth
// started when the first slot freed, has nothing behind it, and waits for the horizon. heldSlotWait
// says what is asserted and why none of it reads the runner's speed.
func TestAWaitHoldingARunSlotKeepsItsTTLWhileSubtasksAreUnstarted(t *testing.T) {
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
