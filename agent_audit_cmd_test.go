package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// TestAgentAuditVerifyNamesTheBrokenRunAndSeq (register SF-08): the verify verb passes a
// clean chained trail and fails a tampered one, naming the run and the seq.
func TestAgentAuditVerifyNamesTheBrokenRunAndSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	l := agent.NewAuditLog(path).WithChain("run-cli")
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := l.Record(agent.Action{Kind: agent.ActWrite, Path: p}, agent.Allow, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.EndRun(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := agentAuditVerify(path, false, &out); err != nil {
		t.Fatalf("a clean trail failed verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 chained run") {
		t.Errorf("summary does not count the run: %s", out.String())
	}
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"path":"b.txt"`), []byte(`"path":"x.txt"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := agentAuditVerify(path, false, &out)
	if err == nil {
		t.Fatalf("a tampered trail passed verify:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "run run-cli seq 3") {
		t.Errorf("verify does not name the run and seq: %s", out.String())
	}
}

// TestAgentAuditVerifyStrictFailsAnOpenRun (SF-08 review 1): a cut tail is an open
// run; plain verify reports it, --strict fails it.
func TestAgentAuditVerifyStrictFailsAnOpenRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-audit.jsonl")
	l := agent.NewAuditLog(path).WithChain("run-open")
	if err := l.Record(agent.Action{Kind: agent.ActWrite, Path: "a.txt"}, agent.Allow, "ok"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := agentAuditVerifyMode(path, false, false, &out); err != nil {
		t.Errorf("plain verify failed an open run: %v", err)
	}
	if !strings.Contains(out.String(), "OPEN   run run-open") {
		t.Errorf("plain verify did not report the open run: %s", out.String())
	}
	out.Reset()
	if err := agentAuditVerifyMode(path, false, true, &out); err == nil {
		t.Error("--strict passed an open run")
	}
}
