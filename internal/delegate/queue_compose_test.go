// queue_compose_test.go: the queue deadline is decided by two changes that meet in
// runRemote's poll loop - its INSTANT comes from the node's own ETA (ADR 0063: the
// budget is derived, and read again when the job sits queued) and what is DONE there is
// asking the node to take the job back (ADR 0064: only a confirmation re-places it).
// Each has its own tests; these pin the seams between them, so a later edit to one
// cannot silently drop the other.

package delegate

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestQueueDeadlineNamesTheUnreadBudgetBeforeTheWithdrawClause: a queue deadline can
// carry two notes, one from each change - the budget the job was abandoned at was never
// re-checked (its node's health could not be read again once the job sat queued), and the
// delegator asked the node to take the job back and it did not. The first says how the wait
// was sized and the second what was done at its end, so they read in that order, and the
// row still ENDS with the withdraw clause the operator guide names.
func TestQueueDeadlineNamesTheUnreadBudgetBeforeTheWithdrawClause(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // the 60 "s" floor is 300 ms
	f := queuedNode(t, func(f *fakeNode) { f.healthFailFn = func(n int64) bool { return n >= 2 } })
	probe := &withdrawProbe{} // no answer scripted: an older node, which answers 405
	url := probe.front(t, f.server()).URL
	contract := plainContract()
	contract.TimeoutSec = 600

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if sum.Failed != 1 || !strings.HasPrefix(r.Err, "queue deadline") {
		t.Fatalf("summary = %+v err = %q, want the queue deadline: nothing extended the wait and the node did not take the job back", sum, r.Err)
	}
	const refresh = "; the queue budget was derived from the placement snapshot"
	const withdraw = "; withdraw not confirmed: HTTP 405: the node has no withdraw route (an older node)"
	at, wt := strings.Index(r.Err, refresh), strings.Index(r.Err, withdraw)
	if at < 0 || wt < 0 || at > wt {
		t.Fatalf("err = %q, want the budget note (at %d) before the withdraw clause (at %d)", r.Err, at, wt)
	}
	if !strings.HasSuffix(r.Err, withdraw) {
		t.Fatalf("err = %q, want the row to END with the withdraw clause", r.Err)
	}
	if got := probe.deletes.Load(); got != 1 {
		t.Fatalf("withdraw attempts = %d, want exactly 1", got)
	}
	if r.Replacements != 0 {
		t.Fatalf("replacements = %d: a withdraw the node did not confirm must not re-place the job (it may still start)", r.Replacements)
	}
}

// TestEtaDerivedQueueDeadlineIsWhereTheWithdrawFires: a node that says a new job starts now
// earns the 60 s floor (ADR 0063) - not the contract's whole poll budget - and the delegator
// asks the node to take the job back THERE (ADR 0064). The node confirms, so the subtask is
// re-placed on the other node and the intent is closed as withdrawn.
func TestEtaDerivedQueueDeadlineIsWhereTheWithdrawFires(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond) // the floor is 300 ms; the 600 "s" contract's poll budget is over 3 s
	zero := 0.0
	stuck := stuckNode(t, "node-stuck")
	stuck.queueWaitEstimate = &zero
	probe := &withdrawProbe{answer: confirmsWithdrawal}
	stuckURL := probe.front(t, stuck.server()).URL
	idle, idleURL := acceptingNode(t, "node-idle", "answer from the idle node", nil)
	cfg := testCfg(t)
	contract := withdrawContract()
	contract.TimeoutSec = 600

	began := time.Now()
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{stuckURL, idleURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(began)
	r := results[0]
	if r.Err != "" || r.Node != "node-idle" || r.Replacements != 1 {
		t.Fatalf("err = %q node = %q replacements = %d, want the job re-placed on node-idle after node-stuck took it back", r.Err, r.Node, r.Replacements)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("finished after %s: the withdraw waited for the contract's whole poll budget, not the deadline the node's 'no wait' ETA earned (300 ms here)", elapsed)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("finished after only %s: the job was taken back before the 60 s floor", elapsed)
	}
	stuckJob, _ := stuck.lastJobID.Load().(string)
	if ids, _ := probe.sent(); len(ids) != 1 || ids[0] != stuckJob {
		t.Fatalf("withdraws sent = %v, want exactly one, for the job dispatched to node-stuck (%q)", ids, stuckJob)
	}
	if idle.dispatches.Load() != 1 {
		t.Fatalf("node-idle saw %d dispatches, want 1", idle.dispatches.Load())
	}
	closed, open := intentNotes(t, cfg.StateDir)
	if closed[stuckJob] != intentNoteWithdrawn || len(open) != 0 {
		t.Fatalf("intent for the withdrawn job closed as %q (open=%v), want %q and nothing left for recovery", closed[stuckJob], open, intentNoteWithdrawn)
	}
}

// TestConfirmedWithdrawCoolsTheNodeBeforeItIsAskedAgain: a confirmed withdrawal is filed as
// a capacity refusal (ADR 0064), so it is one to the placement machinery (ADR 0063): the
// node goes on the run's cooldown - a refusalCooldown, the withdrawal carries no
// Retry-After - and the capacity wait, the only thing that asks a node the subtask has
// already tried, leaves it alone until that ends. The roster is the one node, so the
// subtask waits for it and lands on it, and no sooner than its cooldown after the
// withdrawal.
func TestConfirmedWithdrawCoolsTheNodeBeforeItIsAskedAgain(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 5*time.Millisecond)
	const cooldown = 800 * time.Millisecond
	compressWait(t, 20*time.Millisecond, cooldown)
	begin := time.Now()
	var withdrawnAt, redispatchedAt atomic.Int64
	zero := 0.0
	var node *fakeNode
	node = &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-stuck",
		queueWaitEstimate: &zero,
		dispatchHook: func(n int64) int {
			if n == 2 {
				redispatchedAt.Store(int64(time.Since(begin)))
			}
			return 0
		},
		// The first job sits `accepted` until it is taken back; the second is the one the
		// node finishes.
		pollByJob: func(string, int64) (map[string]any, int) {
			if node.dispatches.Load() >= 2 {
				w := remoteWire("answer after the withdrawal", `{"answer":"answer after the withdrawal"}`)
				w.NodeID = "node-stuck"
				return doneWire(t, w), http.StatusOK
			}
			return map[string]any{"state": "accepted"}, http.StatusOK
		},
	}
	probe := &withdrawProbe{answer: func(n int64, id string) (int, map[string]any) {
		withdrawnAt.Store(int64(time.Since(begin)))
		return confirmsWithdrawal(n, id)
	}}
	url := probe.front(t, node.server()).URL
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	contract := withdrawContract()
	contract.TimeoutSec = 600

	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r := results[0]; r.Err != "" || r.Node != "node-stuck" {
		t.Fatalf("err = %q node = %q, want the subtask to wait for the node it was taken back from and land on it", r.Err, r.Node)
	}
	if withdrawnAt.Load() == 0 || redispatchedAt.Load() == 0 {
		t.Fatalf("fixture: withdrawn at %d, re-dispatched at %d - the node was not asked twice", withdrawnAt.Load(), redispatchedAt.Load())
	}
	gap := time.Duration(redispatchedAt.Load() - withdrawnAt.Load())
	// The cooldown is jittered once, by up to 20 % either way (jitterFrac).
	if floor := time.Duration(float64(cooldown) * (1 - jitterFrac)); gap < floor {
		t.Fatalf("the node was asked again %s after it took the job back, want at least %s: a confirmed withdrawal did not cool it like a capacity refusal", gap, floor)
	}
}
