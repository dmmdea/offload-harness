package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded default table must load and stay tighten-only — it takes every
// unattended Build down with it if it cannot (fail closed, same contract the
// shipped example table is held to).
func TestUnattendedRulesTableLoads(t *testing.T) {
	rules, err := UnattendedRules()
	if err != nil {
		t.Fatalf("embedded default table failed to load: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("embedded default table is empty — it would provide no gate at all")
	}
	var hasDeleteCatchAll bool
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
		if r.Kind == ActDelete && r.Glob == "*" {
			hasDeleteCatchAll = true
		}
	}
	if !hasDeleteCatchAll {
		t.Error("no `delete *` catch-all — the measured unparked destructive class would sail through")
	}
}

// The gap the default closes (TO-1, measured 2026-08-11): a structurally
// destructive call must park on an unattended run WITHOUT any reliance on the
// model's self-declared security_risk — that annotation was a constant "low"
// on 81/81 destructive calls, and it never enters Policy.Decide at all. This
// drives the REAL broker built by Build with the most permissive write posture,
// so the only thing that can stop the delete is the default table under test.
func TestDefaultRulesParkDeleteWithoutModelDeclaredRisk(t *testing.T) {
	askQ := filepath.Join(t.TempDir(), "asks.jsonl")
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowOverwrite: true, AllowDelete: true,
		AskQueuePath: askQ,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Exactly the call shape the probe measured: a source file deleted on an
	// unattended run. Posture ALLOWS it; the default table must veto to ask,
	// which unattended resolves to deny-and-queue.
	d, reason := res.Policy.Decide(Action{Kind: ActDelete, Path: "src/notify.py", Exists: true})
	if d != Deny {
		t.Fatalf("unattended delete = %q (%s), want deny-and-queue via the default table", d, reason)
	}
	if !strings.Contains(reason, "denied & queued") {
		t.Fatalf("unattended delete denied but not queued: %q", reason)
	}
	b, rerr := os.ReadFile(askQ)
	if rerr != nil {
		t.Fatalf("ask queue not written: %v", rerr)
	}
	var entry struct {
		Kind     string `json:"kind"`
		Path     string `json:"path"`
		Decision string `json:"decision"`
		Severity string `json:"severity"`
		Rule     string `json:"rule"`
	}
	if jerr := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]), &entry); jerr != nil {
		t.Fatalf("ask queue line unparsable: %v (%q)", jerr, string(b))
	}
	if entry.Kind != "delete" || entry.Rule != "*" || entry.Severity != string(SevHigh) {
		t.Fatalf("queued entry does not carry the fired rule as structured fields: %+v", entry)
	}

	// Evidence files are hard-denied, not queued — the deny rules sit ABOVE the
	// delete catch-all, so order in the table is load-bearing; this pins it.
	d, reason = res.Policy.Decide(Action{Kind: ActDelete, Path: "runs/ledger.jsonl", Exists: true})
	if d != Deny || !strings.Contains(reason, "[critical]") {
		t.Fatalf("evidence delete = %q (%s), want a [critical] hard deny from *.jsonl", d, reason)
	}
}

// The default table gates the high-blast-radius surfaces without strangling
// ordinary work: config/manifest writes queue, generated files deny, and plain
// source creation/overwrite stays governed by the posture flags the operator
// set. An unattended agent whose every source edit queues cannot do the job it
// was granted — that failure mode would push operators straight to --rules off.
func TestDefaultRulesGateConfigNotOrdinarySource(t *testing.T) {
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowOverwrite: true, AllowDelete: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cases := []struct {
		name string
		a    Action
		want Decision
	}{
		{"config overwrite queues", Action{Kind: ActWrite, Path: "config.json", Exists: true}, Deny},
		{"lockfile write denies", Action{Kind: ActWrite, Path: "package-lock.json", Exists: true}, Deny},
		{"workflow write denies", Action{Kind: ActWrite, Path: ".github/workflows/ci.yml", Exists: false}, Deny},
		{"new source file allowed", Action{Kind: ActWrite, Path: "src/fresh.py", Exists: false}, Allow},
		{"source overwrite allowed by posture", Action{Kind: ActWrite, Path: "src/app.py", Exists: true}, Allow},
	}
	for _, tc := range cases {
		if d, reason := res.Policy.Decide(tc.a); d != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, d, reason, tc.want)
		}
	}
}

// `--rules off` is the explicit escape hatch: no default table, posture rules
// alone, and the UNGATED note so the opt-out is visible in the run's notes.
func TestRulesOffEscapeHatchIsUngatedAndAnnounced(t *testing.T) {
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowOverwrite: true, AllowDelete: true,
		RulesPath: RulesOff,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if d, reason := res.Policy.Decide(Action{Kind: ActDelete, Path: "src/notify.py", Exists: true}); d != Allow {
		t.Fatalf("delete with --rules off = %q (%s), want posture-governed allow", d, reason)
	}
	var found bool
	for _, n := range res.Notes {
		if strings.Contains(n, "UNGATED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no UNGATED note on an explicit --rules off destructive run; notes=%v", res.Notes)
	}
}

// An operator table REPLACES the default rather than stacking on top of it —
// replacement is the only way an operator can loosen the delete catch-all,
// since rules themselves are tighten-only.
func TestOperatorTableReplacesDefault(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(table, []byte(`[{"kind":"write","glob":"*.secret","decision":"deny","severity":"critical","reason":"operator rule"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowOverwrite: true, AllowDelete: true,
		RulesPath: table,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The default delete catch-all must NOT be in force…
	if d, reason := res.Policy.Decide(Action{Kind: ActDelete, Path: "src/x.py", Exists: true}); d != Allow {
		t.Fatalf("delete under an operator table with no delete rule = %q (%s), want allow", d, reason)
	}
	// …while the operator's own rule is.
	if d, _ := res.Policy.Decide(Action{Kind: ActWrite, Path: "creds.secret", Exists: false}); d != Deny {
		t.Fatalf("operator rule did not fire, got %q", d)
	}
}
