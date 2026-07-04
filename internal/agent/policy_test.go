package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyNewWriteAllowed(t *testing.T) {
	for _, unattended := range []bool{false, true} {
		p := NewPolicy(unattended, nil)
		d, _ := p.Decide(Action{Kind: ActWrite, Path: "notes/out.txt", Exists: false})
		if d != Allow {
			t.Errorf("new write (unattended=%v) = %q, want allow", unattended, d)
		}
	}
}

func TestPolicyOverwriteAsksThenDeniesUnattended(t *testing.T) {
	// attended: overwriting an existing file requires approval (ask)
	if d, _ := NewPolicy(false, nil).Decide(Action{Kind: ActWrite, Path: "a.txt", Exists: true}); d != Ask {
		t.Errorf("attended overwrite = %q, want ask", d)
	}
	// unattended: ask resolves to deny-and-queue
	if d, _ := NewPolicy(true, nil).Decide(Action{Kind: ActWrite, Path: "a.txt", Exists: true}); d != Deny {
		t.Errorf("unattended overwrite = %q, want deny", d)
	}
}

func TestPolicyDeleteAsksThenDeniesUnattended(t *testing.T) {
	if d, _ := NewPolicy(false, nil).Decide(Action{Kind: ActDelete, Path: "a.txt", Exists: true}); d != Ask {
		t.Errorf("attended delete = %q, want ask", d)
	}
	if d, _ := NewPolicy(true, nil).Decide(Action{Kind: ActDelete, Path: "a.txt", Exists: true}); d != Deny {
		t.Errorf("unattended delete = %q, want deny", d)
	}
}

func TestPolicyGitPathDeniedUnconditionally(t *testing.T) {
	for _, unattended := range []bool{false, true} {
		p := NewPolicy(unattended, nil)
		for _, path := range []string{".git/config", ".git", "sub/.git/hooks/pre-commit"} {
			if d, _ := p.Decide(Action{Kind: ActWrite, Path: path, Exists: false}); d != Deny {
				t.Errorf("write to %q (unattended=%v) = %q, want deny", path, unattended, d)
			}
		}
	}
}

// REGRESSION (fresh-context review wrote into .git via these): the .git deny must
// survive Windows case-insensitivity + trailing-dot/space + ".." normalization.
func TestPolicyGitDenyRobust(t *testing.T) {
	bypasses := []string{
		".GIT/hooks/post-checkout", ".git./hooks/pre-push", "sub/../.GIT/hooks/x",
		".Git/config", "dir/.GIT./f", ".git ", "a/.GIT", ".git",
	}
	for _, unattended := range []bool{false, true} {
		p := NewPolicy(unattended, nil)
		for _, path := range bypasses {
			if d, _ := p.Decide(Action{Kind: ActWrite, Path: path, Exists: false}); d != Deny {
				t.Errorf(".git bypass %q (unattended=%v) = %q, want deny", path, unattended, d)
			}
		}
	}
	// a legitimately-named file must NOT be falsely denied
	if d, _ := NewPolicy(true, nil).Decide(Action{Kind: ActWrite, Path: ".gitignore", Exists: false}); d != Allow {
		t.Errorf(".gitignore should be allowed (not a .git component), got %q", d)
	}
}

// S4: an ALLOW that cannot be audited must be downgraded to DENY.
func TestPolicyAuditFailureDowngradesAllow(t *testing.T) {
	dir := t.TempDir() // point the audit "file" at a DIRECTORY → OpenFile fails
	p := NewPolicy(true, NewAuditLog(dir))
	d, reason := p.Decide(Action{Kind: ActWrite, Path: "new.txt", Exists: false})
	if d != Deny {
		t.Errorf("unauditable allow = %q, want deny (downgraded)", d)
	}
	if !strings.Contains(reason, "audit") {
		t.Errorf("downgrade reason should mention audit failure: %q", reason)
	}
}

func TestPolicyAuditRecordsDecisions(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	au := NewAuditLog(auditPath)
	p := NewPolicy(true, au)
	p.Decide(Action{Kind: ActWrite, Path: "new.txt", Exists: false}) // allow
	p.Decide(Action{Kind: ActDelete, Path: "old.txt", Exists: true}) // ask→deny (unattended)

	f, err := os.Open(auditPath)
	if err != nil {
		t.Fatalf("audit not written: %v", err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2", len(lines))
	}
	if lines[0]["decision"] != "allow" || lines[0]["path"] != "new.txt" {
		t.Errorf("audit[0] = %v", lines[0])
	}
	if lines[1]["decision"] != "deny" || lines[1]["kind"] != "delete" {
		t.Errorf("audit[1] = %v", lines[1])
	}
	if !strings.Contains(lines[1]["reason"].(string), "approval") {
		t.Errorf("deny reason should mention approval: %v", lines[1]["reason"])
	}
}
