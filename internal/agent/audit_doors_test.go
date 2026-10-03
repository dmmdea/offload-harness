package agent

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for one test and returns what was written.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// TestAdvisoryAuditKeepsAnAllowWhenTheWriteFails (register SF-02): in warn mode an
// audit write failure is reported, once, and never turns an allow into a deny; the
// enforcing trail (today's behaviour) still downgrades it.
func TestAdvisoryAuditKeepsAnAllowWhenTheWriteFails(t *testing.T) {
	dir := t.TempDir() // the audit "file" is a DIRECTORY, so every write fails
	act := Action{Kind: ActWrite, Path: "notes/out.txt"}

	if d, reason := NewPolicy(true, NewAuditLog(dir)).Decide(act); d != Deny || !strings.Contains(reason, "audit write failed") {
		t.Fatalf("enforcing trail: got %q (%s), want deny with the audit reason", d, reason)
	}

	logged := captureLog(t)
	p := NewPolicy(true, NewAuditLog(dir)).WithAuditAdvisory(true)
	for i := 0; i < 3; i++ {
		d, reason := p.Decide(act)
		if d != Allow {
			t.Fatalf("advisory trail, decision %d: got %q (%s), want allow", i, d, reason)
		}
		if strings.Contains(reason, "audit write failed") {
			t.Errorf("advisory trail must not rewrite the decision's reason: %q", reason)
		}
	}
	if n := strings.Count(logged.String(), "audit write failed"); n != 1 {
		t.Errorf("advisory trail warned %d times, want exactly once: %q", n, logged.String())
	}
}

// TestAdvisoryAuditStillWritesEveryDecision: warn mode changes only what a FAILED
// write does; a working trail records every decision, allow and deny alike.
func TestAdvisoryAuditStillWritesEveryDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	p := NewPolicy(true, NewAuditLog(path)).WithAuditAdvisory(true)
	p.Decide(Action{Kind: ActWrite, Path: "a.txt"})
	p.Decide(Action{Kind: ActDelete, Path: "a.txt", Exists: true})
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("no audit file: %v", err)
	}
	defer f.Close()
	rows := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		rows++
	}
	if rows != 2 {
		t.Errorf("audit rows = %d, want 2 (one per decision)", rows)
	}
}

// TestDoorAuditPlan pins what each agent door hands Build for each audit_all_doors
// mode: off is today's behaviour (no trail unless browse asks for one), warn adds an
// advisory trail, enforce an enforcing one; a browse run always keeps the enforcing
// trail its grant requires; an unknown mode and an unresolvable path under enforce
// refuse the run instead of running it without the record the operator asked for.
func TestDoorAuditPlan(t *testing.T) {
	const home = "/h/.local-offload/agent-audit.jsonl"
	prev := doorAuditPath
	t.Cleanup(func() { doorAuditPath = prev })
	doorAuditPath = func(string) string { return home }

	for _, c := range []struct {
		mode     string
		browse   bool
		path     string
		advisory bool
		refuse   bool
	}{
		{"", false, "", false, false},
		{"off", false, "", false, false},
		{" OFF ", false, "", false, false},
		{"warn", false, home, true, false},
		{"enforce", false, home, false, false},
		{"off", true, home, false, false},
		{"warn", true, home, false, false},
		{"enforce", true, home, false, false},
		{"on", false, "", false, true},
	} {
		got := DoorAudit(c.mode, c.browse, "")
		if got.Path != c.path || got.Advisory != c.advisory || (got.Refuse != "") != c.refuse {
			t.Errorf("DoorAudit(%q, browse=%v) = %+v, want path %q advisory %v refuse %v", c.mode, c.browse, got, c.path, c.advisory, c.refuse)
		}
	}

	doorAuditPath = func(string) string { return "" }
	if got := DoorAudit("enforce", false, ""); got.Refuse == "" || got.Path != "" {
		t.Errorf("enforce with no resolvable path must refuse, got %+v", got)
	}
	if got := DoorAudit("warn", false, ""); got.Refuse != "" || got.Path != "" || got.Note == "" {
		t.Errorf("warn with no resolvable path must run without a trail and say so, got %+v", got)
	}
}

// TestBuildNeverMakesABrowseTrailAdvisory: the browse grant requires an enforcing
// trail, so a build that asks for advisory audit on a browse run keeps enforcing.
func TestBuildNeverMakesABrowseTrailAdvisory(t *testing.T) {
	dir := t.TempDir()
	res, err := Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir,
		Unattended: true, AllowWrite: true, Worktree: dir,
		AuditPath:     t.TempDir(), // a directory: every audit write fails
		AuditAdvisory: true, AllowBrowse: true, BrowseHosts: []string{"example.com"},
		Browse: func(context.Context, BrowseInput) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := res.Policy.Decide(Action{Kind: ActWrite, Path: "a.txt"}); d != Deny {
		t.Errorf("browse run with a failing trail = %q, want deny (the trail stays enforcing)", d)
	}
	res, err = Build(BuildConfig{
		PlannerBase: "http://127.0.0.1:11436", Model: "m", ReadRoot: dir,
		Unattended: true, AllowWrite: true, Worktree: dir,
		AuditPath: t.TempDir(), AuditAdvisory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := res.Policy.Decide(Action{Kind: ActWrite, Path: "a.txt"}); d != Allow {
		t.Errorf("non-browse advisory run with a failing trail = %q, want allow", d)
	}
}
