// processgate_test.go: the contract of the process-wide gate and the per-page
// retry cap (ADR 0063, decisions 7 and 8), pinned with the literals the ADR
// states rather than with the production constants - a constant compared with
// itself passes whatever its value.

package delegate

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// holdGate takes n of base's process-gate slots as if other Runs of this process held
// dispatches open there, and gives them back after d (and, whatever happens, when the
// test ends: the gate is a process global). d <= 0 holds them until the test ends.
func holdGate(t *testing.T, base string, ceiling, n int, d time.Duration) {
	t.Helper()
	var releases []func()
	for i := 0; i < n; i++ {
		rel, ok := processGate.tryAcquire(base, ceiling)
		if !ok {
			t.Fatal("fixture: the gate was not free")
		}
		releases = append(releases, rel)
		t.Cleanup(rel)
	}
	if d > 0 {
		go func() {
			time.Sleep(d)
			for _, rel := range releases {
				rel()
			}
		}()
	}
}

// TestPageCapIsThreeIssuesAndFifteenMinutes: "three failed issues" and "15
// minutes" are the ADR's words. The shipped tests read pageMaxIssues and
// pageBackoff themselves, so changing either constant left them green.
func TestPageCapIsThreeIssuesAndFifteenMinutes(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := &pageRetryCap{pages: map[string]*pageRecord{}, now: func() time.Time { return now }}
	for i := 1; i <= 2; i++ {
		c.record("k", true)
		if ok, _ := c.admit("k"); !ok {
			t.Fatalf("backed off after %d failed issue(s); the ADR lets the original and two re-issues run", i)
		}
	}
	c.record("k", true)
	if ok, _ := c.admit("k"); ok {
		t.Fatal("not backed off after three failed issues")
	}
	now = now.Add(14 * time.Minute)
	if ok, _ := c.admit("k"); ok {
		t.Fatal("released after 14 minutes; the ADR says 15")
	}
	now = now.Add(2 * time.Minute)
	if ok, _ := c.admit("k"); !ok {
		t.Fatal("still backed off after 16 minutes; the ADR says 15")
	}
}

// TestPageKeyNeedsAContextDocument: a research contract that carries no context
// document has no page to key. Keyed anyway, every such contract would share one
// key and back the others off.
func TestPageKeyNeedsAContextDocument(t *testing.T) {
	c := pageContract("x")
	c.Context = nil
	if k, ok := pageKeyFor(c); ok {
		t.Fatalf("a research contract with no context document was keyed (%q): every such contract would share one key and back the others off", k)
	}
}

// TestAdmissionCeilingIsTheQueueDepth: the process gate holds a node to its
// max_queue_depth, which counts running AND queued jobs - the number a `503 queue
// full` is decided on - not to its worker count. Every gate fixture publishes equal
// values for the two, which is why this needs a node that publishes different ones.
func TestAdmissionCeilingIsTheQueueDepth(t *testing.T) {
	if got := admissionCeiling(NodeView{MaxConcurrentJobs: 1, MaxQueueDepth: 3}); got != 3 {
		t.Fatalf("admission ceiling = %d, want max_queue_depth 3 (it counts running AND queued)", got)
	}
	if got := admissionCeiling(NodeView{MaxConcurrentJobs: 2}); got != 0 {
		t.Fatalf("admission ceiling with no published max_queue_depth = %d, want 0 (unknown is never a limit)", got)
	}
}

// TestGateTurnAwayIsNoRefusalAndKeepsItsStory: the process gate turned the dispatch
// away, so the node was never asked - the doc comments say "never a refusal", and
// Summary.Replaced counts subtasks re-placed after a node refused them. The hold is
// no replacement, but the reason the subtask waited stays on the result.
func TestGateTurnAwayIsNoRefusalAndKeepsItsStory(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-gated", "answer", func(f *fakeNode) { f.maxQueueDepth = 2 })
	holdGate(t, url, 2, 2, 150*time.Millisecond) // other Runs hold the node's whole admission ceiling
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Succeeded != 1 || sum.Waited != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v dispatches = %d, want one success after one wait and exactly one dispatch", sum, node.dispatches.Load())
	}
	if sum.Replaced != 0 || sum.ReplacementRecovered != 0 || pr.Replacements != 0 || pr.ReplacementNote != "" {
		t.Fatalf("summary = %+v replacements = %d note = %q, want no replacement: no node refused anything", sum, pr.Replacements, pr.ReplacementNote)
	}
	if !strings.Contains(pr.PlacementReason, "capacity wait") || !strings.Contains(pr.PlacementReason, "process gate") {
		t.Fatalf("placement reason = %q, want the wait and the gate that caused it", pr.PlacementReason)
	}
}

// TestGateTurnAwayWithTheWaitOffIsACapacityDefer: with the wait off, a merely full
// node is a capacity defer that names the gate - never an infrastructure-class defer
// naming a lease nobody holds (the branch that made it so was untested), and the
// node is never asked.
func TestGateTurnAwayWithTheWaitOffIsACapacityDefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "node-gated", "answer", func(f *fakeNode) { f.maxQueueDepth = 4 })
	holdGate(t, url, 4, 4, 0)
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 {
		t.Fatalf("the node saw %d dispatches although this process already holds its whole admission ceiling open", node.dispatches.Load())
	}
	if sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity || sum.Infrastructure != 0 {
		t.Fatalf("summary = %+v class = %q reason = %q, want a CAPACITY defer (a busy node), not an infrastructure one naming a lease nobody holds", sum, pr.Result.DeferClass, pr.Result.Reason)
	}
	if !strings.Contains(pr.Result.Reason, "process gate") || pr.Replacements != 0 || sum.Replaced != 0 {
		t.Fatalf("reason = %q replacements = %d replaced = %d, want the gate named and no refusal counted", pr.Result.Reason, pr.Replacements, sum.Replaced)
	}
}

// TestGateHoldWithNoBudgetLeftIsABudgetDeferNotAPlacementRefusal: a hold that no node
// ever refused, ended by a contract whose timeout_sec is already under the retry
// floor, is a budget defer - the way a decided seat's wait files it - not `placement
// refused: 0 node(s) refused this subtask` with a replacement count of -1.
func TestGateHoldWithNoBudgetLeftIsABudgetDeferNotAPlacementRefusal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-gated", "answer", func(f *fakeNode) { f.maxQueueDepth = 1 })
	holdGate(t, url, 1, 1, 100*time.Millisecond)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	contract := plainContract()
	contract.TimeoutSec = 5 // under the 10 s floor another attempt needs
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Failed != 0 || sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassBudget {
		t.Fatalf("summary = %+v class = %q err = %q dispatches = %d, want a budget defer and no dispatch", sum, pr.Result.DeferClass, pr.Err, node.dispatches.Load())
	}
	if pr.Replacements < 0 || sum.Replaced != 0 || !strings.Contains(pr.Result.Reason, "process gate") {
		t.Fatalf("replacements = %d replaced = %d reason = %q, want no refusal counted and the gate named", pr.Replacements, sum.Replaced, pr.Result.Reason)
	}
}
