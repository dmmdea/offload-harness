package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The built-in floor: secret-material writes are denied even in the most
// permissive posture — the whole point of a rule floor is that no flag
// combination can reopen it.
func TestSecretFloorDeniesEvenInOpenWritePosture(t *testing.T) {
	p := NewPolicy(false, nil).WithWritePosture(true, true)
	for _, path := range []string{".env", ".env.local", "server.pem", "sub/dir/deploy.key", "id_rsa", "conf/id_ed25519.pub"} {
		d, reason := p.Decide(Action{Kind: ActWrite, Path: path, Exists: true})
		if d != Deny {
			t.Errorf("write %s = %s (%s), want deny", path, d, reason)
		}
		if !strings.Contains(reason, "[critical]") {
			t.Errorf("write %s reason %q must carry the severity tag", path, reason)
		}
		if d, _ := p.Decide(Action{Kind: ActDelete, Path: path, Exists: true}); d != Deny {
			t.Errorf("delete %s = %s, want deny", path, d)
		}
	}
}

// A loaded rule can veto what posture would have allowed (tighten), and paths
// that match no rule keep the posture behaviour byte-for-byte.
func TestLoadedRuleTightensButAbsenceChangesNothing(t *testing.T) {
	p := NewPolicy(false, nil).WithWritePosture(true, false)
	if _, err := p.WithRules([]Rule{{Kind: ActWrite, Glob: "config.json", Decision: Ask, Severity: SevHigh, Reason: "operator config"}}); err != nil {
		t.Fatal(err)
	}
	if d, reason := p.Decide(Action{Kind: ActWrite, Path: "config.json", Exists: true}); d != Ask {
		t.Errorf("rule must override posture allow: got %s (%s)", d, reason)
	}
	if d, _ := p.Decide(Action{Kind: ActWrite, Path: "main.go", Exists: true}); d != Allow {
		t.Errorf("unmatched path must keep posture allow, got %s", d)
	}
}

// Rules are tighten-only: a rule that tries to Allow is rejected at
// validation, as are shell rules (nothing structural to match — the cage owns
// shell) and unknown severities/kinds/bad globs.
func TestRuleValidationRejectsLoosenersAndShell(t *testing.T) {
	cases := []Rule{
		{Kind: ActWrite, Glob: "*.txt", Decision: Allow, Severity: SevLow, Reason: "nope"},
		{Kind: ActShell, Glob: "rm *", Decision: Deny, Severity: SevCritical, Reason: "nope"},
		{Kind: ActionKind("teleport"), Glob: "*", Decision: Deny, Severity: SevLow, Reason: "nope"},
		{Kind: ActWrite, Glob: "[", Decision: Deny, Severity: SevLow, Reason: "bad glob"},
		{Kind: ActWrite, Glob: "*.txt", Decision: Deny, Severity: Severity("apocalyptic"), Reason: "bad sev"},
	}
	for _, r := range cases {
		if _, err := NewPolicy(false, nil).WithRules([]Rule{r}); err == nil {
			t.Errorf("rule %+v must be rejected", r)
		}
	}
}

// A rule cannot resurrect what the unconditional built-ins deny: .git wins
// even if a rule table says only ask.
func TestRulesCannotOverrideGitDeny(t *testing.T) {
	p := NewPolicy(false, nil)
	if _, err := p.WithRules([]Rule{{Kind: ActWrite, Glob: "*", Decision: Ask, Severity: SevLow, Reason: "everything asks"}}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Decide(Action{Kind: ActWrite, Path: ".git/config"}); d != Deny {
		t.Errorf(".git deny must fire before any rule, got %s", d)
	}
}

// Slashless globs match basenames (extension rules cover nested files); globs
// with slashes match the full relative path only.
func TestRuleGlobBasenameSemantics(t *testing.T) {
	r := Rule{Kind: ActWrite, Glob: "*.pem", Decision: Deny, Severity: SevHigh, Reason: "x"}
	if !r.matches(Action{Kind: ActWrite, Path: "deep/nested/cert.pem"}) {
		t.Error("slashless glob must match the basename of a nested path")
	}
	full := Rule{Kind: ActWrite, Glob: "secrets/*.json", Decision: Deny, Severity: SevHigh, Reason: "x"}
	if !full.matches(Action{Kind: ActWrite, Path: "secrets/api.json"}) {
		t.Error("path glob must match its own level")
	}
	if full.matches(Action{Kind: ActWrite, Path: "other/api.json"}) {
		t.Error("path glob must not match outside its directory")
	}
}

// Fetch rules match hosts, giving a tighten-only per-host veto on top of the
// allowlist (e.g. deny a subdomain the allowlist's wildcard would admit).
func TestFetchRuleVetoesAllowlistedHost(t *testing.T) {
	allow, err := NewAllowlist([]string{"*.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPolicyWithEgress(false, nil, allow)
	if _, err := p.WithRules([]Rule{{Kind: ActFetch, Glob: "internal.example.com", Decision: Deny, Severity: SevHigh, Reason: "internal host"}}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Decide(Action{Kind: ActFetch, Path: "internal.example.com"}); d != Deny {
		t.Error("fetch rule must veto an allowlisted host")
	}
	if d, _ := p.Decide(Action{Kind: ActFetch, Path: "www.example.com"}); d != Allow {
		t.Error("unmatched allowlisted host must stay allowed")
	}
}

// LoadRules round-trips the versioned table and fails closed on an invalid or
// missing file — a table the operator believes is active must never half-load.
func TestLoadRulesFailsClosed(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "rules.json")
	os.WriteFile(good, []byte(`[{"kind":"write","glob":"*.bak","decision":"deny","severity":"medium","reason":"backups are owned by the operator"}]`), 0o644)
	rs, err := LoadRules(good)
	if err != nil || len(rs) != 1 {
		t.Fatalf("good table: %v %d", err, len(rs))
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`[{"kind":"shell","glob":"rm *","decision":"deny","severity":"high","reason":"x"}]`), 0o644)
	if _, err := LoadRules(bad); err == nil {
		t.Error("shell rule in a loaded table must fail the whole load")
	}
	if _, err := LoadRules(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing table must be an error, not an empty table")
	}
}

// Review CRITICAL-1: the floor must hold under Windows-resolution aliases —
// case variants and trailing dot/space land on the same file, so they must
// land on the same rule.
func TestFloorHoldsUnderWindowsAliases(t *testing.T) {
	p := NewPolicy(false, nil).WithWritePosture(true, true)
	for _, path := range []string{".ENV", "SERVER.PEM", "sub/DEPLOY.KEY", "ID_RSA", "server.pem.", "server.pem ", ".ENV.local", ".envrc"} {
		if d, reason := p.Decide(Action{Kind: ActWrite, Path: path, Exists: true}); d != Deny {
			t.Errorf("write %q = %s (%s), want deny — Windows alias bypass", path, d, reason)
		}
	}
}

// Review CRITICAL-2: fetch rules are case-insensitive like the allowlist, so a
// redirect Location header cannot dodge an operator's host veto by casing.
func TestFetchRuleCaseInsensitive(t *testing.T) {
	allow, _ := NewAllowlist([]string{"*.example.com"})
	p := NewPolicyWithEgress(false, nil, allow)
	if _, err := p.WithRules([]Rule{{Kind: ActFetch, Glob: "internal.example.com", Decision: Deny, Severity: SevHigh, Reason: "internal"}}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"INTERNAL.example.com", "Internal.Example.Com"} {
		if d, _ := p.Decide(Action{Kind: ActFetch, Path: host}); d != Deny {
			t.Errorf("fetch %q must hit the case-insensitive veto", host)
		}
	}
}

// Review IMPORTANT-3: '**' loads must fail loudly — path.Match does not
// recurse, and a rule that silently covers one level is worse than an error.
func TestDoubleStarGlobRejected(t *testing.T) {
	if _, err := NewPolicy(false, nil).WithRules([]Rule{{Kind: ActWrite, Glob: "secrets/**", Decision: Deny, Severity: SevHigh, Reason: "x"}}); err == nil {
		t.Fatal("'**' glob must be rejected at validation")
	}
}

// A rule escalates classify's Ask to Deny (the floor hard-denies even where
// posture-less classify would merely ask) but never softens a Deny.
func TestRuleEscalatesAskNeverSoftensDeny(t *testing.T) {
	p := NewPolicy(false, nil) // no posture: overwrite would classify as Ask
	if d, _ := p.Decide(Action{Kind: ActWrite, Path: ".env", Exists: true}); d != Deny {
		t.Error("floor must escalate Ask→Deny for secret material")
	}
	// an Ask-rule must NOT soften the .git deny (already covered) nor shell-off deny
	if d, _ := p.Decide(Action{Kind: ActShell, Path: "echo hi"}); d != Deny {
		t.Error("shell-off deny must be untouched by the rule layer")
	}
}

// Review IMPORTANT-4: a rule hit reaches the audit trail as structured fields.
func TestAuditRecordsFiredRuleStructured(t *testing.T) {
	dir := t.TempDir()
	audit := NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	p := NewPolicy(false, audit).WithWritePosture(true, true)
	if d, _ := p.Decide(Action{Kind: ActWrite, Path: ".env", Exists: true}); d != Deny {
		t.Fatal("expected floor deny")
	}
	b, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	if !strings.Contains(line, `"severity":"critical"`) || !strings.Contains(line, `"rule":".env*"`) {
		t.Fatalf("audit entry must carry structured severity+rule, got %s", line)
	}
}
