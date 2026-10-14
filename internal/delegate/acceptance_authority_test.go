package delegate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The delegator is the only place acceptance is evaluated, and the only place
// the contract's context documents are at hand, so the grounded level is closed
// here: EvalAcceptance hands the contract's documents to the check.

func authorityContract(acceptance ...string) core.AgentContract {
	return core.AgentContract{
		Goal:       "what does the spec say about the cache?",
		Context:    []core.ContextDoc{{Name: "spec.md", Text: "The retry limit is 7 and the cache is named pelican."}},
		Acceptance: acceptance,
	}
}

func TestEvalAcceptanceGradesGroundedAgainstTheContractDocuments(t *testing.T) {
	c := authorityContract("contains:@grounded:pelican")
	if f := EvalAcceptance(c, core.AgentWireResult{Output: "the cache is named pelican"}); len(f) != 0 {
		t.Fatalf("a claim the documents carry must pass, got %v", f)
	}
	f := EvalAcceptance(authorityContract("contains:@grounded:walrus"), core.AgentWireResult{Output: "the cache is named walrus"})
	if len(f) != 1 || !strings.Contains(f[0], "none of the 1 context documents") {
		t.Fatalf("a claim no document carries must fail and say why, got %v", f)
	}
}

func TestEvalAcceptanceReportsUnknownAsAFailureNeverAPass(t *testing.T) {
	// No context documents: grounded authority is unreachable, so the check is
	// unknown, and an unknown is a failure to the merge gate.
	c := authorityContract("contains:@grounded:pelican")
	c.Context = nil
	f := EvalAcceptance(c, core.AgentWireResult{Output: "the cache is named pelican"})
	if len(f) != 1 || !strings.Contains(f[0], "unknown") || !strings.HasPrefix(f[0], "contains:@grounded:pelican: ") {
		t.Fatalf("a grounded check with no documents must be reported unknown, got %v", f)
	}

	// An empty answer: a qualified not_contains no longer passes vacuously...
	q := authorityContract("not_contains:@answer:PLACEHOLDER")
	if f := EvalAcceptance(q, core.AgentWireResult{}); len(f) != 1 || !strings.Contains(f[0], "unknown") {
		t.Fatalf("a qualified not_contains over an empty answer must be unknown, got %v", f)
	}
	// ...while the unqualified one does, exactly as before.
	u := authorityContract("not_contains:PLACEHOLDER")
	if f := EvalAcceptance(u, core.AgentWireResult{}); len(f) != 0 {
		t.Fatalf("the unqualified not_contains over an empty answer is unchanged (passes), got %v", f)
	}
}

func TestEvalAcceptanceKeepsUnqualifiedContractsExactlyAsTheyWere(t *testing.T) {
	c := authorityContract("contains:pelican", "regex:retry limit is [0-9]+", "not_contains:walrus", "nonempty:summary")
	structured := json.RawMessage(`{"summary":"the cache is named pelican"}`)
	ok := core.AgentWireResult{Output: "the retry limit is 7 and the cache is named pelican", Structured: structured}
	if f := EvalAcceptance(c, ok); len(f) != 0 {
		t.Fatalf("a passing legacy result must pass, got %v", f)
	}
	bad := core.AgentWireResult{Output: "walrus", Structured: structured}
	f := EvalAcceptance(c, bad)
	want := []string{
		`contains:pelican: "pelican" not found in output`,
		"regex:retry limit is [0-9]+: pattern did not match output",
		`not_contains:walrus: forbidden "walrus" found in output`,
	}
	if strings.Join(f, "\n") != strings.Join(want, "\n") {
		t.Fatalf("failure text changed:\n got %q\nwant %q", f, want)
	}
}

func TestEvalAcceptanceAMixedContractClosesOnlyAtItsWeakestCheck(t *testing.T) {
	c := authorityContract("contains:@grounded:pelican", "contains:@answer:cache")
	wire := core.AgentWireResult{Output: "the cache is named pelican"}
	outs := GradeAcceptance(c, wire)
	if got, ok := core.ClosedAt(outs); !ok || got != core.AuthAnswer {
		t.Fatalf("ClosedAt = (%v, %v), want (answer, true)", got, ok)
	}
}

func TestLintFlagsAnAuthorityTheContractCannotReach(t *testing.T) {
	// grounded with no documents
	c := core.AgentContract{Goal: "g", Acceptance: []string{"contains:@grounded:pelican"}}
	wantWarn(t, LintAcceptance(c), "AUTHORITY-UNREACHABLE")

	// field authority with no output_schema
	c = authorityContract("contains:@field=summary:pelican")
	wantWarn(t, LintAcceptance(c), "AUTHORITY-UNREACHABLE")

	// field authority on a field the schema does not declare
	c = authorityContract("contains:@field=summary:pelican")
	c.OutputSchema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"}}}`)
	wantWarn(t, LintAcceptance(c), "AUTHORITY-UNREACHABLE")

	// reachable: silent about authority
	c = authorityContract("contains:@field=summary:pelican", "contains:@grounded:pelican")
	c.OutputSchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	wantNoWarn(t, LintAcceptance(c), "AUTHORITY-UNREACHABLE")
}
