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
// shipped example table is held to). The per-rule assertions run against the
// RAW embedded JSON, not UnattendedRules() — that function already validates,
// so checking its output would restate its own guarantees and could never fail
// (review finding 2026-08-14).
func TestUnattendedRulesTableLoads(t *testing.T) {
	if _, err := UnattendedRules(); err != nil {
		t.Fatalf("embedded default table failed to load: %v", err)
	}
	var rules []Rule
	if err := json.Unmarshal(unattendedRulesJSON, &rules); err != nil {
		t.Fatalf("raw embedded JSON unparsable: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("embedded default table is empty — it would provide no gate at all")
	}
	var catchAllIdx = -1
	for i, r := range rules {
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
			catchAllIdx = i
		}
	}
	if catchAllIdx < 0 {
		t.Fatal("no `delete *` catch-all — the measured unparked destructive class would sail through")
	}
	// First match wins: every deny in the table must sit ABOVE the ask
	// catch-all or it is dead code (the shipped example table had exactly this
	// bug — its catch-all sat first and shadowed three critical denies).
	for i, r := range rules {
		if r.Decision == Deny && r.Kind == ActDelete && i > catchAllIdx {
			t.Errorf("delete deny %q sits BELOW the `delete *` catch-all (index %d > %d) — it can never fire", r.Glob, i, catchAllIdx)
		}
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
	// Effective decision is Deny for BOTH a deny rule and a queued ask on an
	// unattended run, so pinning `Deny` alone cannot tell them apart — 16 of
	// 20 rules were mutation-unpinned that way (review finding 2026-08-14).
	// The reason string separates them: a hard deny carries the severity
	// marker and NO "denied;"/"denied &" prefix; a queued ask carries both.
	cases := []struct {
		name   string
		a      Action
		want   Decision
		reason string // required substring of the reason ("" = no constraint)
		queued bool   // true: must be an unattended-queued ask, not a hard deny
	}{
		{"workflow write hard-denies", Action{Kind: ActWrite, Path: ".github/workflows/ci.yml", Exists: false}, Deny, "[critical]", false},
		{"workflow delete hard-denies", Action{Kind: ActDelete, Path: ".github/workflows/ci.yml", Exists: true}, Deny, "[critical]", false},
		{"weights write hard-denies", Action{Kind: ActWrite, Path: "models/m.gguf", Exists: true}, Deny, "[critical]", false},
		{"weights delete hard-denies", Action{Kind: ActDelete, Path: "models/m.gguf", Exists: true}, Deny, "[critical]", false},
		{"safetensors write hard-denies", Action{Kind: ActWrite, Path: "models/m.safetensors", Exists: true}, Deny, "[critical]", false},
		{"evidence write hard-denies", Action{Kind: ActWrite, Path: "runs/ledger.jsonl", Exists: true}, Deny, "[critical]", false},
		{"go.sum write hard-denies", Action{Kind: ActWrite, Path: "go.sum", Exists: true}, Deny, "[high]", false},
		{"lockfile write hard-denies", Action{Kind: ActWrite, Path: "package-lock.json", Exists: true}, Deny, "[high]", false},
		{"pnpm lock write hard-denies", Action{Kind: ActWrite, Path: "pnpm-lock.yaml", Exists: true}, Deny, "[high]", false},
		{"safetensors delete hard-denies", Action{Kind: ActDelete, Path: "models/m.safetensors", Exists: true}, Deny, "[critical]", false},
		{"generic lockfile write hard-denies", Action{Kind: ActWrite, Path: "yarn.lock", Exists: true}, Deny, "[high]", false},
		{"go.mod write queues", Action{Kind: ActWrite, Path: "go.mod", Exists: true}, Deny, "[high]", true},
		{"package.json write queues", Action{Kind: ActWrite, Path: "package.json", Exists: true}, Deny, "[high]", true},
		{"requirements.txt write queues", Action{Kind: ActWrite, Path: "requirements.txt", Exists: true}, Deny, "[high]", true},
		{"Gemfile write queues (case-folded)", Action{Kind: ActWrite, Path: "Gemfile", Exists: true}, Deny, "[high]", true},
		{"pyproject.toml write queues", Action{Kind: ActWrite, Path: "pyproject.toml", Exists: true}, Deny, "[high]", true},
		{"Cargo.toml write queues (case-folded)", Action{Kind: ActWrite, Path: "Cargo.toml", Exists: true}, Deny, "[high]", true},
		{"config overwrite queues", Action{Kind: ActWrite, Path: "config.json", Exists: true}, Deny, "[high]", true},
		{"settings.json write queues", Action{Kind: ActWrite, Path: ".vscode/settings.json", Exists: true}, Deny, "[high]", true},
		{"yaml write queues", Action{Kind: ActWrite, Path: "compose.yaml", Exists: false}, Deny, "[medium]", true},
		{"yml write queues", Action{Kind: ActWrite, Path: "ci.yml", Exists: false}, Deny, "[medium]", true},
		{"toml write queues", Action{Kind: ActWrite, Path: "app.toml", Exists: false}, Deny, "[medium]", true},
		{"ini write queues", Action{Kind: ActWrite, Path: "setup.ini", Exists: false}, Deny, "[medium]", true},
		{"new source file allowed", Action{Kind: ActWrite, Path: "src/fresh.py", Exists: false}, Allow, "", false},
		{"source overwrite allowed by posture", Action{Kind: ActWrite, Path: "src/app.py", Exists: true}, Allow, "", false},
		{"makefile allowed", Action{Kind: ActWrite, Path: "Makefile", Exists: false}, Allow, "", false},
	}
	for _, tc := range cases {
		d, reason := res.Policy.Decide(tc.a)
		if d != tc.want {
			t.Errorf("%s: got %q (%s), want %q", tc.name, d, reason, tc.want)
			continue
		}
		if tc.reason != "" && !strings.Contains(reason, tc.reason) {
			t.Errorf("%s: reason %q missing severity marker %q", tc.name, reason, tc.reason)
		}
		if tc.want == Deny {
			// Anchor on the unattended-ask WRAPPER, not on a word a rule's own
			// prose could also contain (review round 2, 2026-08-14: a deny rule
			// whose reason said "denied outright" would have misclassified as a
			// queued ask under a bare substring check).
			if isQueuedAsk := strings.HasPrefix(reason, "requires approval; unattended run →"); isQueuedAsk != tc.queued {
				t.Errorf("%s: queued-ask=%v (reason %q), want queued-ask=%v", tc.name, isQueuedAsk, reason, tc.queued)
			}
		}
	}
}

// The fired rule's severity/glob must reach the ask queue even at the DEFAULT
// posture (no --allow-delete), where classify already answers Ask and an Ask
// rule is not strictly stricter — the composition previously dropped the rule
// there, losing exactly the fields the morning review sorts by (review finding
// 2026-08-14).
func TestAskRuleSeverityReachesQueueAtDefaultPosture(t *testing.T) {
	askQ := filepath.Join(t.TempDir(), "asks.jsonl")
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite:   true, // no AllowDelete: classify's own Ask path
		AskQueuePath: askQ,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if d, reason := res.Policy.Decide(Action{Kind: ActDelete, Path: "src/notify.py", Exists: true}); d != Deny || !strings.Contains(reason, "denied & queued") {
		t.Fatalf("default-posture delete = %q (%s), want deny-and-queue", d, reason)
	}
	b, rerr := os.ReadFile(askQ)
	if rerr != nil {
		t.Fatalf("ask queue not written: %v", rerr)
	}
	var entry struct {
		Severity string `json:"severity"`
		Rule     string `json:"rule"`
		Reason   string `json:"reason"`
	}
	if jerr := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]), &entry); jerr != nil {
		t.Fatalf("ask queue line unparsable: %v (%q)", jerr, string(b))
	}
	if entry.Rule != "*" || entry.Severity != string(SevHigh) {
		t.Fatalf("queued entry lost the fired rule at default posture: %+v", entry)
	}
}

// With no ask queue attached the deny must stand AND the reason must say the
// truth — "NOT queued" — instead of pointing the morning review at a queue
// that does not exist (review finding 2026-08-14).
func TestNoQueueReasonTellsTheTruth(t *testing.T) {
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true, AllowDelete: true, // no AskQueuePath
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	d, reason := res.Policy.Decide(Action{Kind: ActDelete, Path: "src/x.py", Exists: true})
	if d != Deny {
		t.Fatalf("delete without a queue = %q, want deny", d)
	}
	if !strings.Contains(reason, "NOT queued") || strings.Contains(reason, "denied & queued") {
		t.Fatalf("reason must state nothing was queued; got %q", reason)
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

	// Write-only is NOT benign under --rules off: the default table would have
	// hard-denied new CI-workflow files and queued config writes, so opting
	// out with just --allow-write is a real downgrade and must be announced
	// too (review finding 2026-08-14 — the note previously keyed only on
	// delete/overwrite/shell/github).
	res, err = Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:1", Model: "m", MaxSteps: 1,
		ReadRoot: t.TempDir(), Unattended: true,
		AllowWrite: true,
		RulesPath:  RulesOff,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	found = false
	for _, n := range res.Notes {
		if strings.Contains(n, "UNGATED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no UNGATED note on a write-only --rules off run; notes=%v", res.Notes)
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
