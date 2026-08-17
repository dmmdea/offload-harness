package main

// CLI-surface coverage for the `delegate` verb (multi-node delegation,
// Task 6), in the refiner_cli_test.go pattern: unit-test the verb's parsing
// helpers directly — a full-process smoke needs a live planner seat and lives
// in the Task-8 e2e instead. parseContractFile must accept BOTH file shapes
// (one subtask object, or an array) and carry context_paths through, since
// the file is the CLI's whole intake surface.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

func writeContract(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "contract.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseContractFileSingleObject(t *testing.T) {
	p := writeContract(t, `{
		"goal": "digest the docs",
		"context": [{"name": "a.md", "text": "alpha"}],
		"context_paths": ["notes/b.md"],
		"output_schema": {"properties": {"answer": {"type": "string"}}},
		"acceptance": ["nonempty:answer"],
		"max_steps": 6
	}`)
	specs, err := parseContractFile(p)
	if err != nil {
		t.Fatalf("parseContractFile: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(specs))
	}
	s := specs[0]
	if s.Goal != "digest the docs" || s.MaxSteps != 6 {
		t.Errorf("spec = %+v", s.AgentContract)
	}
	if len(s.Context) != 1 || s.Context[0].Name != "a.md" {
		t.Errorf("context = %+v", s.Context)
	}
	if len(s.ContextPaths) != 1 || s.ContextPaths[0] != "notes/b.md" {
		t.Errorf("context_paths = %v (the delegator-side extension must ride the file format)", s.ContextPaths)
	}
	if len(s.Acceptance) != 1 || len(s.OutputSchema) == 0 {
		t.Errorf("acceptance/schema = %v/%s", s.Acceptance, s.OutputSchema)
	}
}

func TestParseContractFileArray(t *testing.T) {
	p := writeContract(t, `[{"goal": "one"}, {"goal": "two", "timeout_sec": 30}]`)
	specs, err := parseContractFile(p)
	if err != nil {
		t.Fatalf("parseContractFile: %v", err)
	}
	if len(specs) != 2 || specs[0].Goal != "one" || specs[1].TimeoutSec != 30 {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestParseContractFileErrors(t *testing.T) {
	if _, err := parseContractFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file must error, not return zero subtasks")
	}
	p := writeContract(t, `{"goal": unquoted}`)
	if _, err := parseContractFile(p); err == nil {
		t.Error("malformed JSON must error")
	}
}

// TestDelegateExitContract pins the verb's exit code against the run summary
// (H-1). Defers and failed-verification are RESULT shapes — the JSON reports
// them and the exit stays 0 — but a BROKEN node is not a result: an
// infrastructure/config-class defer means an operator has to act, and a
// scripted caller that only checks the exit code must not read it as success.
func TestDelegateExitContract(t *testing.T) {
	cases := []struct {
		name    string
		sum     delegate.Summary
		wantErr string // "" = exit 0
	}{
		{"all green", delegate.Summary{Succeeded: 3}, ""},
		{"an honest abstention stays zero", delegate.Summary{Succeeded: 1, Deferred: 1}, ""},
		{"failed verification stays zero", delegate.Summary{FailedVerification: 2}, ""},
		{"transport failure exits non-zero", delegate.Summary{Failed: 1}, "failed (transport/config)"},
		{"a broken node exits non-zero", delegate.Summary{Deferred: 2, Infrastructure: 2}, "infrastructure/config"},
		{"failures win the message", delegate.Summary{Failed: 1, Deferred: 1, Infrastructure: 1}, "failed (transport/config)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := delegateExitErr(tc.sum)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want exit 0", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("summary %+v must exit non-zero", tc.sum)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestRepeatedFlagCollectsEveryValue(t *testing.T) {
	var r repeatedFlag
	for _, v := range []string{"http://a:18811", "http://b:18811"} {
		if err := r.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(r) != 2 || r[0] != "http://a:18811" || r[1] != "http://b:18811" {
		t.Fatalf("repeatedFlag = %v", r)
	}
}

// TestRunDelegateRefusesWhenDelegationDisabled invokes runDelegate itself —
// the verb had NO test that ever called it, while its MCP twin's registration
// gate is byte-pinned (TestAgentDelegateToolGated). The two surfaces share one
// switch (roast delta 13: a box is a DELEGATOR only by explicit opt-in), so the
// CLI half needs the same pin or the flag could be honored on one surface and
// ignored on the other with a fully green suite.
//
// The refusal must also come BEFORE any work: no contract read, no pipeline
// opened, no network. Asserted by passing a --contract path that does not
// exist — if the gate ever moved below the file read, the error would name the
// missing file instead of the disabled role.
func TestRunDelegateRefusesWhenDelegationDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"agent_delegation_enabled": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runDelegate([]string{"--config", cfgPath, "--contract", filepath.Join(dir, "no-such-contract.json")})
	if err == nil {
		t.Fatal("runDelegate succeeded with agent_delegation_enabled false — the CLI must refuse exactly as the MCP tool refuses to register")
	}
	if !strings.Contains(err.Error(), "agent_delegation_enabled") {
		t.Fatalf("err = %q, want the disabled-role refusal naming agent_delegation_enabled", err)
	}
	if strings.Contains(err.Error(), "no-such-contract") {
		t.Fatalf("err = %q — the gate must precede the contract read, so a disabled box never touches the caller's files", err)
	}
}

// TestRunDelegateEnabledStillValidatesItsInputs: with the role ON the gate is
// out of the way and the verb's own argument contract takes over — a missing
// --contract is the verb's error, not a silent no-op run.
func TestRunDelegateEnabledStillValidatesItsInputs(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"agent_delegation_enabled": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runDelegate([]string{"--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "--contract required") {
		t.Fatalf("err = %v, want the missing---contract refusal", err)
	}
}
