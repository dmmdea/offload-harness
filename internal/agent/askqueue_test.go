package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolicyAskQueueParksDeferredAsks proves the P5b supervision surface: on an
// UNATTENDED run, an action that needs approval (ask) is denied-and-queued AND
// parked in the reviewable ask-queue, while an allowed action is not parked.
func TestPolicyAskQueueParksDeferredAsks(t *testing.T) {
	dir := t.TempDir()
	pol := NewPolicyWithEgress(true /*unattended*/, NewAuditLog(filepath.Join(dir, "audit.jsonl")), Allowlist{}).
		WithAskQueue(NewAuditLog(filepath.Join(dir, "asks.jsonl")))
	askPath := filepath.Join(dir, "asks.jsonl")

	// A delete classifies as Ask → unattended → deny-and-queue → parked.
	if d, _ := pol.Decide(Action{Kind: ActDelete, Path: "foo.txt"}); d != Deny {
		t.Fatalf("an unattended delete should deny-and-queue; got %q", d)
	}
	b, err := os.ReadFile(askPath)
	if err != nil {
		t.Fatalf("the ask-queue file should exist after a deferred ask: %v", err)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Errorf("ask-queue should have exactly 1 parked ask; got %d lines: %s", n, b)
	}
	if !strings.Contains(string(b), `"decision":"ask"`) || !strings.Contains(string(b), `"kind":"delete"`) {
		t.Errorf("a parked ask should record kind=delete decision=ask; got %s", b)
	}

	// An ALLOW (creating a new file) must NOT be parked.
	if d, _ := pol.Decide(Action{Kind: ActWrite, Path: "new.txt", Exists: false}); d != Allow {
		t.Fatalf("creating a new file should be allowed; got %q", d)
	}
	b2, _ := os.ReadFile(askPath)
	if n := strings.Count(string(b2), "\n"); n != 1 {
		t.Errorf("an allowed action must not be parked in the ask-queue; got %d lines: %s", n, b2)
	}
}

// TestPolicyNoAskQueueStillDeniesUnattended confirms the ask-queue is optional:
// without it, an unattended ask still deny-and-queues (no panic, no behavior change).
func TestPolicyNoAskQueueStillDeniesUnattended(t *testing.T) {
	pol := NewPolicy(true, nil) // no audit, no ask-queue
	if d, _ := pol.Decide(Action{Kind: ActDelete, Path: "x"}); d != Deny {
		t.Errorf("unattended delete should still deny without an ask-queue; got %q", d)
	}
}
