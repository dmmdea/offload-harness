package main

// CLI-surface coverage for the `delegate` verb (multi-node delegation,
// Task 6), in the refiner_cli_test.go pattern: unit-test the verb's parsing
// helpers directly — a full-process smoke needs a live planner seat and lives
// in the Task-8 e2e instead. parseContractFile must accept BOTH file shapes
// (one subtask object, or an array) and carry context_paths through, since
// the file is the CLI's whole intake surface.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
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
		{"a broken node exits non-zero", delegate.Summary{Deferred: 2, Infrastructure: 2, LostToStack: 2}, "infrastructure/config"},
		{"failures win the message", delegate.Summary{Failed: 1, Deferred: 1, Infrastructure: 1, LostToStack: 1}, "failed (transport/config)"},
		// R5-2, the wide-exit-rule row: a subtask eaten by a broken box while a
		// sibling finished exits non-zero HERE. Since C-75 the MCP tool's
		// delegateIsError deliberately does NOT flag this exact summary (its table
		// pins it as false: a partial result is a successful call whose body says
		// what is missing), so the two surfaces diverge on purpose — an exit code
		// sits beside the printed results, an error flag makes the client cut the body.
		{"a subtask lost to a broken box beside a sibling success", delegate.Summary{Succeeded: 1, Deferred: 1, Infrastructure: 1, LostToStack: 1}, "infrastructure/config"},
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

// TestDelegateExitErrReportsEveryLoudCount (C-L): the exit-code mapper
// returned on the FIRST non-zero class, so a run with both failures and
// infrastructure defers named only the failures — and an operator fixing the
// transport error would not know a node was also broken until the next run.
func TestDelegateExitErrReportsEveryLoudCount(t *testing.T) {
	if err := delegateExitErr(delegate.Summary{Succeeded: 3, Deferred: 1}); err != nil {
		t.Fatalf("err = %v, want nil — abstentions and budget defers are RESULT shapes, exit 0", err)
	}
	both := delegateExitErr(delegate.Summary{Failed: 2, Deferred: 3, Infrastructure: 3})
	if both == nil {
		t.Fatal("a run with failures AND infrastructure defers must exit non-zero")
	}
	msg := both.Error()
	if !strings.Contains(msg, "2 subtask") || !strings.Contains(msg, "3 subtask") {
		t.Errorf("err = %q, want BOTH counts named", msg)
	}
	if only := delegateExitErr(delegate.Summary{Failed: 2}); only == nil || !strings.Contains(only.Error(), "2 subtask") {
		t.Errorf("failed-only err = %v, want the failure count named", only)
	}
	if only := delegateExitErr(delegate.Summary{Deferred: 1, Infrastructure: 1}); only == nil ||
		!strings.Contains(only.Error(), "1 subtask") {
		t.Errorf("infrastructure-only err = %v, want the infrastructure count named", only)
	}
}

// TestTheVisionVerbsSayTheirAutoRouteAlsoReadsTheVisionSeat is the CLI help's half of the MCP route text: the flag shared by
// vqa, ocr and assess-image names both triggers of the auto route, the lease and the busy vision seat.
func TestTheVisionVerbsSayTheirAutoRouteAlsoReadsTheVisionSeat(t *testing.T) {
	for _, want := range []string{"local GPU lease is held", "the local vision seat is busy"} {
		if !strings.Contains(visionRouteHelp, want) {
			t.Errorf("visionRouteHelp = %q, want it to say %q", visionRouteHelp, want)
		}
	}
}

// TestDelegateAndResearchVerbsRefuseABadPinReasonBeforeLoadingAnything (ADR 0078): --pin-reason is checked against
// --route before the config is read or a contract or a page is touched, and the refusal lists the valid set. The
// config path does not exist: had the check come after the config load, the error would be about the file.
func TestDelegateAndResearchVerbsRefuseABadPinReasonBeforeLoadingAnything(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-config.json")
	for _, tc := range []struct {
		name string
		run  func([]string) error
		args []string
	}{
		{"delegate: a value outside the set", runDelegate, []string{"--route", "local", "--pin-reason", "because"}},
		{"delegate: privacy with remote", runDelegate, []string{"--route", "remote", "--pin-reason", "privacy"}},
		{"delegate: a reason with the default route (auto)", runDelegate, []string{"--pin-reason", "operator"}},
		{"delegate: a reason with spread", runDelegate, []string{"--route", "spread", "--pin-reason", "operator"}},
		{"delegate: a reason with queue", runDelegate, []string{"--route", "queue", "--pin-reason", "measurement"}},
		{"research: a value outside the set", runResearch, []string{"--route", "local", "--pin-reason", "because"}},
		{"research: locality with remote", runResearch, []string{"--route", "remote", "--pin-reason", "locality"}},
		{"research: a reason with the default route (spread)", runResearch, []string{"--pin-reason", "operator"}},
		{"research: a reason with auto", runResearch, []string{"--route", "auto", "--pin-reason", "operator"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(append([]string{"--config", missing}, tc.args...))
			if err == nil {
				t.Fatal("a bad pin_reason was accepted")
			}
			for _, want := range []string{"pin_reason", "privacy (route local)", "operator (route local or remote)"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "no-such-config") {
				t.Errorf("err = %q is about the config file: the pin_reason must be refused before anything is loaded", err)
			}
		})
	}
}

// TestTheCLIVerbsOfferPinReasonToTheEngine: --pin-reason is carried into the engine's options together with the
// door's half of the contract, PinNeedsReason, without which a bare --route local would stay a hard pin on the verbs while
// the MCP tools treat it as a hint.
func TestTheCLIVerbsOfferPinReasonToTheEngine(t *testing.T) {
	rescue := delegate.RescueFunc(func(context.Context, core.AgentContract, string, time.Duration) (delegate.Rescued, error) {
		return delegate.Rescued{}, nil
	})
	opts := cliPinOptions(1, "t", rescue, "measurement")
	if opts.PinReason != "measurement" || !opts.PinNeedsReason || opts.Priority != 1 || opts.Tenant != "t" || opts.Rescue == nil {
		t.Fatalf("options = %+v, want the reason, the door's opt-in and the caller's own fields", opts)
	}
	if bare := cliPinOptions(0, "", rescue, ""); bare.PinReason != "" || !bare.PinNeedsReason {
		t.Fatalf("options = %+v, want no reason but still the door's opt-in: a bare route is a hint here too", bare)
	}
}
