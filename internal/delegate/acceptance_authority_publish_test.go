package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The authority a contract closed at (ENH-13b). core.ClosedAt says it for a set of graded checks; the delegator states it on
// what it publishes: EvalAcceptanceGraded returns it beside the failures, PlacedResult carries it, and the result the caller
// reads (acceptance_authority) and the corpus row repeat it. It is the weakest authority among the checks, and only when the
// whole contract was accepted: a failure, an unknown, an excused fingerprint, a list-fidelity failure or a single
// unqualified check leaves it unstated, so a result publishes a claim only when every check backed it.

func TestEvalAcceptanceGradedStatesTheAuthorityTheContractClosedAt(t *testing.T) {
	structured := json.RawMessage(`{"summary":"the cache is named pelican"}`)
	grounded := core.AgentWireResult{Output: "the cache is named pelican", Structured: structured}
	tests := []struct {
		name      string
		contract  core.AgentContract
		wire      core.AgentWireResult
		wantFails int
		want      core.Authority
	}{
		{"every check grounded", authorityContract("contains:@grounded:pelican", "regex:@grounded:pel[a-z]+"), grounded, 0, core.AuthGrounded},
		{"a grounded and an answer check close at answer", authorityContract("contains:@grounded:pelican", "contains:@answer:cache"), grounded, 0, core.AuthAnswer},
		{"a grounded and a field check close at field", authorityContract("contains:@grounded:pelican", "contains:@field=summary:pelican"), grounded, 0, core.AuthField},
		{"one unqualified check leaves the claim unstated", authorityContract("contains:@grounded:pelican", "contains:cache"), grounded, 0, core.AuthNone},
		{"a contract with no acceptance states nothing", authorityContract(), grounded, 0, core.AuthNone},
		{"a failed check states nothing", authorityContract("contains:@grounded:pelican", "contains:@answer:walrus"), grounded, 1, core.AuthNone},
		{"an unknown states nothing", authorityContract("contains:@field=ghost:x", "contains:@answer:cache"), grounded, 1, core.AuthNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fails, got := EvalAcceptanceGraded(tt.contract, tt.wire)
			if len(fails) != tt.wantFails || got != tt.want {
				t.Fatalf("EvalAcceptanceGraded = (%v, %v), want %d failure(s) and %v", fails, got, tt.wantFails, tt.want)
			}
			// EvalAcceptance is the same grading: one source of the failures.
			if plain := EvalAcceptance(tt.contract, tt.wire); strings.Join(plain, "\n") != strings.Join(fails, "\n") {
				t.Fatalf("EvalAcceptance = %v, EvalAcceptanceGraded failures = %v", plain, fails)
			}
		})
	}
}

// An accepted contract states no authority when something beside its own checks failed it, or when a check was excused
// rather than passed.
func TestEvalAcceptanceGradedStatesNothingWhenTheResultWasNotFullyAccepted(t *testing.T) {
	// the list-fidelity read fails a hollow object even though both qualified checks pass
	hollow := fidelityContract()
	hollow.Acceptance = []string{"contains:@answer:Intake guide", "contains:@field=doc_title:Intake guide"}
	wire := core.AgentWireResult{Output: invertedAnswer(11), Structured: json.RawMessage(hollowStructured)}
	fails, got := EvalAcceptanceGraded(hollow, wire)
	if len(fails) == 0 || got != core.AuthNone {
		t.Fatalf("a hollow object: (%v, %v), want a fidelity failure and no authority", fails, got)
	}

	// an honest absence excuses a failed fingerprint: no failure is reported, but that check did not PASS, so the contract
	// did not close at the authority of the checks beside it
	c := qualifiedFingerprintContract(core.ContextDoc{Name: "page.md", Text: "the pelican lives here"})
	c.Acceptance = append(c.Acceptance, "contains:@answer:nothing")
	fails, got = EvalAcceptanceGraded(c, absentDigestWithAnswer("nothing relevant"))
	if len(fails) != 0 {
		t.Fatalf("the excused fingerprint was reported: %v", fails)
	}
	if got != core.AuthNone {
		t.Fatalf("a contract with an excused check closed at %v, want none", got)
	}
}

// End to end: the local run path grades through EvalAcceptanceGraded, the result carries the authority, and what is
// published says it, the corpus row included. Anything not fully accepted, and every contract with an unqualified check,
// publishes byte-identically to before (no key).
func TestTheClosedAtAuthorityIsPublishedOnTheResult(t *testing.T) {
	answer := func(output string) LocalRunner {
		return func(_ context.Context, _ core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
			return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "local-seat", Output: output, StopReason: "done"}, nil
		}
	}
	run := func(t *testing.T, contract core.AgentContract, output string) ([]PlacedResult, Summary, string) {
		t.Helper()
		cfg := testCfg(t)
		results, sum, err := Run(t.Context(), cfg, answer(output), []core.AgentContract{contract}, "local", nil)
		if err != nil || len(results) != 1 {
			t.Fatalf("Run: %v (%d results)", err, len(results))
		}
		return results, sum, cfg.BaseDir()
	}
	corpusRow := func(t *testing.T, base string) map[string]any {
		t.Helper()
		dir := filepath.Join(base, "delegation-log")
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("delegation-log: %v (%d entries)", err, len(entries))
		}
		raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &row); err != nil {
			t.Fatalf("corpus row: %v (%s)", err, raw)
		}
		return row
	}

	t.Run("a contract closed at answer publishes answer", func(t *testing.T) {
		results, sum, base := run(t, authorityContract("contains:@grounded:pelican", "contains:@answer:cache"), "the cache is named pelican")
		if sum.Succeeded != 1 || len(results[0].AcceptanceFailures) != 0 {
			t.Fatalf("summary %+v failures %v, want one accepted result", sum, results[0].AcceptanceFailures)
		}
		if results[0].AcceptanceAuthority != core.AuthAnswer {
			t.Fatalf("PlacedResult.AcceptanceAuthority = %v, want answer", results[0].AcceptanceAuthority)
		}
		if got := wireRowOf(t, results, sum)["acceptance_authority"]; got != "answer" {
			t.Fatalf("published acceptance_authority = %v, want \"answer\"", got)
		}
		if got := corpusRow(t, base)["acceptance_authority"]; got != "answer" {
			t.Fatalf("corpus acceptance_authority = %v, want \"answer\"", got)
		}
	})

	t.Run("a contract with an unqualified check publishes no key", func(t *testing.T) {
		results, sum, base := run(t, authorityContract("contains:@grounded:pelican", "contains:cache"), "the cache is named pelican")
		if sum.Succeeded != 1 {
			t.Fatalf("summary %+v, want one accepted result", sum)
		}
		if _, ok := wireRowOf(t, results, sum)["acceptance_authority"]; ok {
			t.Fatal("a contract closed at none published acceptance_authority")
		}
		if _, ok := corpusRow(t, base)["acceptance_authority"]; ok {
			t.Fatal("the corpus row of a contract closed at none carries acceptance_authority")
		}
	})

	t.Run("a failed verification publishes no key", func(t *testing.T) {
		results, sum, base := run(t, authorityContract("contains:@grounded:pelican", "contains:@answer:walrus"), "the cache is named pelican")
		if sum.FailedVerification != 1 || results[0].AcceptanceAuthority != core.AuthNone {
			t.Fatalf("summary %+v authority %v, want one failed verification and no authority", sum, results[0].AcceptanceAuthority)
		}
		if _, ok := wireRowOf(t, results, sum)["acceptance_authority"]; ok {
			t.Fatal("a failed contract published acceptance_authority")
		}
		if _, ok := corpusRow(t, base)["acceptance_authority"]; ok {
			t.Fatal("the corpus row of a failed contract carries acceptance_authority")
		}
	})
}

// A result the call deadline rewrote has no answer to have been accepted: the authority goes with the failures.
func TestACutResultDropsTheAuthorityItCarried(t *testing.T) {
	over := &runner{cfg: testCfg(t), call: pastDeadline(3)}
	got := over.cutByDeadline(PlacedResult{ranLocal: true, Err: "local run: x", AcceptanceAuthority: core.AuthGrounded})
	if got.AcceptanceAuthority != core.AuthNone {
		t.Fatalf("a cut outcome kept acceptance authority %v", got.AcceptanceAuthority)
	}
}
