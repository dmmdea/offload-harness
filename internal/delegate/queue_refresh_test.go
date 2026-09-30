// queue_refresh_test.go: the queue budget of a job the node holds `accepted` is derived
// from the node's ETA, and re-derived from a fresh read of the node when the job is first
// seen queued (ADR 0063, decision 4) - because the placement snapshot can be minutes old.

package delegate

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// queuedNode is a node whose first health read (the placement snapshot) says a new job
// starts now, and that holds every job `accepted` for 900 ms before answering. Later
// health reads say the queue is 200 s deep - so the queue budget must be extended by the
// refresh, or the job is abandoned at the 60 s floor (300 ms at the compressed clock)
// while the node runs it anyway.
func queuedNode(t *testing.T, tune func(*fakeNode)) *fakeNode {
	t.Helper()
	var reads atomic.Int64
	busy, zero := 200.0, 0.0
	begin := time.Now()
	f := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		queueWaitEstimateFn: func() *float64 {
			if reads.Add(1) == 1 {
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
	if tune != nil {
		tune(f)
	}
	return f
}

func runQueuedJob(t *testing.T, f *fakeNode) ([]PlacedResult, Summary) {
	t.Helper()
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // 60 "s" floor = 300 ms; a 200 s ETA earns 330 s = 1.65 s
	srv := f.server()
	contract := plainContract()
	contract.TimeoutSec = 600
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return results, sum
}

// TestQueueRefreshExtendsTheWaitFromAFreshRead is the guard for the refresh itself.
func TestQueueRefreshExtendsTheWaitFromAFreshRead(t *testing.T) {
	results, sum := runQueuedJob(t, queuedNode(t, nil))
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q, want the job waited for: the fresh read says 200 s", sum, results[0].Err)
	}
}

// TestQueueRefreshReadsOnceAndNeverShrinksTheWait: the placement snapshot said 200 s and
// every later read says the queue is empty. A shorter later reading changes nothing (the
// job will simply start sooner), and one successful read is the only read: not one per
// queued poll against a node that is already overloaded.
func TestQueueRefreshReadsOnceAndNeverShrinksTheWait(t *testing.T) {
	var reads atomic.Int64
	busy, zero := 200.0, 0.0
	f := queuedNode(t, func(f *fakeNode) {
		f.queueWaitEstimateFn = func() *float64 {
			if reads.Add(1) == 1 {
				return &busy
			}
			return &zero
		}
	})
	results, sum := runQueuedJob(t, f)
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q, want the job waited for: the placement snapshot said 200 s and a shorter later reading must not shrink the wait", sum, results[0].Err)
	}
	if got := f.healths.Load(); got > 4 {
		t.Fatalf("the node's health was read %d times for one queued job, want at most 4 (placement + one refresh)", got)
	}
}

// TestQueueRefreshFailureIsNamedOnTheDeadline: every read after the placement fails. The
// job is abandoned at the budget the stale snapshot gave it, and the deadline message says
// that budget was never re-checked and why, instead of leaving no trace that the safeguard
// did not run.
func TestQueueRefreshFailureIsNamedOnTheDeadline(t *testing.T) {
	logs := captureLog(t)
	f := queuedNode(t, func(f *fakeNode) { f.healthFailFn = func(n int64) bool { return n >= 2 } })
	results, sum := runQueuedJob(t, f)
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue deadline (fixture: nothing extends the wait)", sum, results[0].Err)
	}
	for _, want := range []string{"placement snapshot", "could not be read again", "3 attempt(s)"} {
		if !strings.Contains(results[0].Err, want) {
			t.Errorf("err = %q, want it to contain %q", results[0].Err, want)
		}
	}
	if got := f.healths.Load(); got != 4 {
		t.Fatalf("the node's health was read %d times, want the placement read plus 3 bounded refresh attempts", got)
	}
	if !strings.Contains(logs.String(), "queue-budget refresh") {
		t.Fatalf("the failed refresh was not logged:\n%s", logs.String())
	}
}

// TestQueueRefreshIsTriedAgainAfterAFailure: one failed read is no reason to give up on the
// safeguard. The first refresh fails, the second succeeds and extends the wait, and the job
// - which the node starts at 900 ms - is waited for.
func TestQueueRefreshIsTriedAgainAfterAFailure(t *testing.T) {
	f := queuedNode(t, func(f *fakeNode) { f.healthFailFn = func(n int64) bool { return n == 2 } })
	results, sum := runQueuedJob(t, f)
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q, want the second refresh to extend the wait so the job is waited for", sum, results[0].Err)
	}
}

// TestQueueBudgetRisesWhenTheNodePublishesALargerWall: the queue budget is capped by the caller's
// patience (the poll budget), and an auto contract's poll budget is re-based on the wall the node
// publishes - lowered when the wall is first seen, RAISED when a later one is larger. A raised
// patience must lift the queue budget with it, or a job the node's own ETA says needs longer is
// abandoned at the old cap. Compressed clock (5 ms per "second"): the node advertises a fast seat,
// so the first patience is (300 + 300 admission) "s" = 3.02 s and the ETA-derived budget (915 "s")
// is capped there; the node then publishes a 300 "s" wall (patience 1.52 s), then a 900 "s" one
// (patience 4.52 s, budget 4.52 s), and starts the job at 3.7 s - past the first cap, inside the raised one.
func TestQueueBudgetRisesWhenTheNodePublishesALargerWall(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond)
	est := 590.0
	begin := time.Now()
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		seatRate:          map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget:        map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		queueWaitEstimate: &est,
		pollState: func(n int64) (map[string]any, int) {
			switch {
			case n == 1:
				return map[string]any{"state": "accepted", "wall_sec": 300}, http.StatusOK
			case time.Since(begin) < 3700*time.Millisecond:
				return map[string]any{"state": "accepted", "wall_sec": 900}, http.StatusOK
			}
			return doneWire(t, remoteWire("the answer", `{"answer":"the answer"}`)), http.StatusOK
		},
	}
	srv := node.server()
	c := autoContract()
	c.Acceptance = []string{"nonempty:answer"} // any answer passes: this test is about how long the job is waited for
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{c}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v err = %q, want the job waited for: the node published a 900 s wall, which raised the patience and the queue budget with it", sum, results[0].Err)
	}
}

// TestQueueBudgetStaysDerivedFromTheETAWhenTheWallRises: the other half. A node that says a job starts
// now earns the 60 "s" floor (300 ms here), and a wall it later publishes raises the caller's patience -
// not the budget: the ETA still says the job should have started, and a job still queued a second later
// is abandoned at the floor, not held until the raised patience.
func TestQueueBudgetStaysDerivedFromTheETAWhenTheWallRises(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond)
	est := 0.0
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		seatRate:          map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget:        map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		queueWaitEstimate: &est,
		pollState: func(n int64) (map[string]any, int) {
			wall := 300
			if n > 1 {
				wall = 900
			}
			return map[string]any{"state": "accepted", "wall_sec": wall}, http.StatusOK
		},
	}
	srv := node.server()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || !strings.HasPrefix(results[0].Err, "queue deadline after 300ms") {
		t.Fatalf("summary = %+v err = %q, want the queue deadline at the 300 ms floor the node's own ETA earned, whatever wall it published", sum, results[0].Err)
	}
}
