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
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestAWaitHoldingARunSlotKeepsItsTTLWhileSubtasksAreUnstarted: five subtasks, four run slots, one node
// that never has room. The first four wait for the TTL (0.3 s, the built-in wait compressed) and end as
// the capacity defer they always ended as, saying why the call's own time did not bound them; the fifth
// started when the first slot freed, has nothing behind it, and waits for the horizon.
func TestAWaitHoldingARunSlotKeepsItsTTLWhileSubtasksAreUnstarted(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	withCallReserve(t, 200*time.Millisecond)
	withBuiltInWait(t, 300*time.Millisecond)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 0

	contracts := make([]core.AgentContract, runConcurrency+1)
	for i := range contracts {
		contracts[i] = remoteContract()
	}
	results, sum, _ := runWithin(t, 10*time.Second, cfg, neverLocal(t), contracts, "remote", []string{url}, deadlineIn(2500*time.Millisecond), nil)

	if sum.Deferred != len(contracts) {
		t.Fatalf("summary %+v, want every subtask deferred for capacity: the node never had room", sum)
	}
	for i, pr := range results[:runConcurrency] {
		r := pr.Result.Reason
		for _, want := range []string{"no node had room within 300ms", "agent_placement_wait_sec=0", "subtask(s) of the call had not started"} {
			if !strings.Contains(r, want) {
				t.Errorf("result %d reason = %q, want it to contain %q: a wait holding a run slot keeps its TTL", i, r, want)
			}
		}
		if strings.Contains(r, "before the call's deadline") {
			t.Errorf("result %d reason = %q says the call bounded a wait that held a slot the fifth subtask needed", i, r)
		}
		if pr.CapacityWaitSec > 1.2 {
			t.Errorf("result %d waited %.2f s, want about the 0.3 s TTL: it held its slot for the call's horizon instead", i, pr.CapacityWaitSec)
		}
	}
	last := results[runConcurrency]
	if !strings.Contains(last.Result.Reason, "before the call's deadline") || strings.Contains(last.Result.Reason, "had not started") {
		t.Errorf("the last subtask's reason = %q, want it bounded by the call: nothing was behind it, so it may wait to the horizon", last.Result.Reason)
	}
	if last.CapacityWaitSec < 1.0 {
		t.Errorf("the last subtask waited %.2f s, want it to outwait the 0.3 s TTL the earlier ones were held to", last.CapacityWaitSec)
	}
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
