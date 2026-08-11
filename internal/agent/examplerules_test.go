package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// The shipped example table must actually LOAD. LoadRules fails closed on a bad
// or missing file, so a table with a typo'd kind/decision is not a soft warning
// — it takes the whole agent run down. Shipping one untested would hand the
// operator a rule file that bricks their first unattended run.
//
// Context (measured 2026-08-11): with no table loaded, the ONLY per-call risk
// gate above the capability flags is the model's own security_risk annotation,
// which was a literal constant "low" across 36/36 structurally destructive
// calls — 0% park-gate recall. This table is the structural replacement.
func TestExampleAgentRulesTableLoads(t *testing.T) {
	rules, err := LoadRules(filepath.Join("..", "..", "examples", "agent-rules.json"))
	if err != nil {
		t.Fatalf("shipped example table failed to load: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("example table is empty — it would provide no gate at all")
	}
	for _, r := range rules {
		if err := r.validate(); err != nil {
			t.Errorf("rule %q invalid: %v", r.Glob, err)
		}
		if r.Decision == Allow {
			t.Errorf("rule %q decides allow — the table is tighten-only", r.Glob)
		}
		if r.Reason == "" {
			t.Errorf("rule %q has no reason; the audit trail would be unreadable", r.Glob)
		}
	}
}

// The gap the table exists to close: a delete must not sail through on the
// model's say-so. Applied to a broker, the table has to actually stop one.
func TestExampleRulesStopAnUnannotatedDelete(t *testing.T) {
	rules, err := LoadRules(filepath.Join("..", "..", "examples", "agent-rules.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Unattended + the most permissive write posture, so the ONLY thing that
	// can stop a delete is the rule table under test.
	pol, err := NewPolicy(true, nil).WithWritePosture(true, true).WithRules(rules)
	if err != nil {
		t.Fatalf("WithRules: %v", err)
	}
	// Exactly the call shape the probe measured: a source file deleted on an
	// unattended run, self-declared "low" by the model.
	if d, _ := pol.Decide(Action{Kind: ActDelete, Path: "src/notify.py", Exists: true}); d == Allow {
		t.Fatal("a delete still resolves to Allow with the table loaded — " +
			"the structural gate is not doing its job")
	}
	// And a lockfile write is denied outright, not merely queued.
	if d, _ := pol.Decide(Action{Kind: ActWrite, Path: "package-lock.json", Exists: true}); d != Deny {
		t.Fatalf("package-lock.json write = %q, want deny", d)
	}
}

// An unattended run granted destructive capability with no rules table must SAY
// so. Silence there is what produced a 0%-recall gate nobody knew was inert.
func TestUngatedUnattendedRunIsAnnounced(t *testing.T) {
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowOverwrite: true, AllowDelete: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var found bool
	for _, n := range res.Notes {
		if strings.Contains(n, "UNGATED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no UNGATED note for an unattended destructive run; notes=%v", res.Notes)
	}
}

// ...and must NOT cry wolf on a read-only run, or the warning gets tuned out.
func TestReadOnlyUnattendedRunIsNotAnnounced(t *testing.T) {
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, n := range res.Notes {
		if strings.Contains(n, "UNGATED") {
			t.Fatalf("UNGATED note on a read-only run: %q", n)
		}
	}
}
