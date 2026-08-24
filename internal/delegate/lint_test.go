package delegate

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func lintContract(goal string, docText string, acceptance ...string) []string {
	c := core.AgentContract{Goal: goal, Acceptance: acceptance}
	if docText != "" {
		c.Context = []core.ContextDoc{{Name: "d.md", Text: docText}}
	}
	return LintAcceptance(c)
}

func wantWarn(t *testing.T, warns []string, marker string) {
	t.Helper()
	for _, w := range warns {
		if strings.Contains(w, marker) {
			return
		}
	}
	t.Errorf("no warning containing %q in %v", marker, warns)
}

func wantNoWarn(t *testing.T, warns []string, marker string) {
	t.Helper()
	for _, w := range warns {
		if strings.Contains(w, marker) {
			t.Errorf("unexpected %q warning: %v", marker, warns)
		}
	}
}

func TestLintCleanContractIsSilent(t *testing.T) {
	// Grounded (412 is in the docs), not parrot-passable (412 not in the goal).
	warns := lintContract("how many pallets does Rotterdam hold?", "Rotterdam holds 412 pallets", "contains:412", "nonempty:count")
	if len(warns) != 0 {
		t.Fatalf("clean contract warned: %v", warns)
	}
}

func TestLintParrotPassable(t *testing.T) {
	// The one measured shape: the check's needle appears in the goal itself.
	warns := lintContract("does the report mention Z-Image and 0.75.0?", "Z-Image shipped as 0.75.0", "contains:0.75.0", "regex:(?i)z-image")
	wantWarn(t, warns, "PARROT-PASSABLE")
	wantNoWarn(t, warns, "UNGROUNDED")
}

func TestLintConjunctionSuppressesParrotWarn(t *testing.T) {
	// One echoable check beside a grounded non-echoable one: the conjunction
	// still discriminates, so no parrot warning.
	warns := lintContract("does the report mention 0.75.0?", "Z-Image shipped as 0.75.0 on blackwell-8", "contains:0.75.0", "contains:blackwell-8")
	if len(warns) != 0 {
		t.Fatalf("discriminating conjunction warned: %v", warns)
	}
}

func TestLintUngrounded(t *testing.T) {
	warns := lintContract("list the open decisions", "1. review queue 2. mirror config", "min_items:decisions:2", "contains:OptiPlex")
	wantWarn(t, warns, "UNGROUNDED")
	warns = lintContract("list the open decisions", "1. review queue 2. mirror config", "regex:(?i)optiplex")
	wantWarn(t, warns, "UNGROUNDED")
}

func TestLintShapeOnly(t *testing.T) {
	warns := lintContract("summarize the docs", "a turbine was replaced", "nonempty:summary", "min_items:points:2")
	wantWarn(t, warns, "SHAPE-ONLY")
	// Shape-only subsumes parrot-passable: exactly one aggregate warning.
	if len(warns) != 1 {
		t.Fatalf("shape-only contract should warn exactly once, got %v", warns)
	}
}

func TestLintNotContainsParrotEdge(t *testing.T) {
	// A parrot's output IS the goal. not_contains:<s> with s ABSENT from the
	// goal passes a parrot -> warn when it is the only content check.
	warns := lintContract("summarize a.md and b.md", "turbine and lemonade notes", "not_contains:does not exist", "nonempty:summary")
	wantWarn(t, warns, "PARROT-PASSABLE")
	// With s PRESENT in the goal, a parrot FAILS the check -> no warning.
	warns = lintContract("if a doc does not exist, say so", "turbine notes", "not_contains:does not exist", "nonempty:summary")
	wantNoWarn(t, warns, "PARROT-PASSABLE")
	// And not_contains is never judged for grounding (its reference is the
	// OUTPUT's absence, not the docs).
	wantNoWarn(t, warns, "UNGROUNDED")
}

// TestLintAggregateHeadlinesCoOccurringWarnings: when a per-check UNGROUNDED
// warning co-fires with the PARROT-PASSABLE aggregate (a check present in the
// goal but absent from the docs triggers both), the AGGREGATE must be
// warns[0] — the caller reads the first line as the verdict and the per-check
// lines as its detail. Pinned because the first version appended the
// aggregate last, contradicting its own doc comment (review finding).
func TestLintAggregateHeadlinesCoOccurringWarnings(t *testing.T) {
	warns := lintContract("does the doc mention XYZ?", "docs about something else", "contains:XYZ")
	if len(warns) != 2 {
		t.Fatalf("want UNGROUNDED + PARROT-PASSABLE, got %v", warns)
	}
	if !strings.Contains(warns[0], "PARROT-PASSABLE") {
		t.Errorf("warns[0] = %q, want the aggregate verdict first", warns[0])
	}
	if !strings.Contains(warns[1], "UNGROUNDED") {
		t.Errorf("warns[1] = %q, want the per-check detail second", warns[1])
	}
}

func TestLintSkipsUnparseableChecks(t *testing.T) {
	// Validate rejects these upstream with a hard error; the lint must not
	// double-report or panic when handed one anyway.
	warns := lintContract("goal", "docs", "bogus-verb", "contains:docs")
	wantNoWarn(t, warns, "bogus-verb")
}
