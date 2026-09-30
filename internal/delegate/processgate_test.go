// processgate_test.go: the contract of the process-wide gate and the per-page
// retry cap (ADR 0063, decisions 7 and 8), pinned with the literals the ADR
// states rather than with the production constants - a constant compared with
// itself passes whatever its value.

package delegate

import (
	"testing"
	"time"
)

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
