package delegate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A document fingerprint the contract itself can never close is a defect of the contract, not a fact about the node that
// answered. The intake lint already says so (AUTHORITY-UNREACHABLE: the check "can only ever grade unknown"), and the
// failure text carries the fingerprint tag for an unknown just as it does for a fail, so the tag alone cannot tell the two
// apart. The quarantine and the cross-seat retry read the grading instead: a contract-caused unknown is still a reported
// failure, but it strikes no node and is not retried elsewhere (every node would grade it unknown).

// fingerprintNoteOutcome feeds noteFingerprint n results and reports what the quarantine holds for the node afterwards.
func fingerprintNoteOutcome(t *testing.T, c core.AgentContract, wire core.AgentWireResult, n int) (blocked bool, strikes, passes int) {
	t.Helper()
	q := NewQuarantine(10 * time.Minute)
	r := &runner{quarantine: q}
	base := "http://node-a:18811"
	for i := 0; i < n; i++ {
		r.noteFingerprint(base, c, wire, EvalAcceptance(c, wire))
	}
	blocked = q.Blocked(base) // takes q.mu itself, so read it before holding the lock
	q.mu.Lock()
	defer q.mu.Unlock()
	return blocked, len(q.strikes[base]), len(q.passes[base])
}

func TestAContractCausedUnknownFingerprintNeverQuarantinesTheNode(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	tests := []struct {
		name     string
		contract core.AgentContract
		wire     core.AgentWireResult
	}{
		{
			name:     "grounded check on a contract with no context documents",
			contract: qualifiedFingerprintContract(),
			wire:     absentDigestWithAnswer("pelican"),
		},
		{
			name:     "grounded check on a contract whose only document is blank",
			contract: qualifiedFingerprintContract(core.ContextDoc{Name: "page.md", Text: " \n\t"}),
			wire:     absentDigestWithAnswer("pelican"),
		},
		{
			// the contract has no documents AND the answer is empty: no answer could have closed it, so it is still the contract's
			name:     "grounded check, no documents, empty answer",
			contract: qualifiedFingerprintContract(),
			wire:     core.AgentWireResult{},
		},
		{
			name: "field check on a contract with no output_schema",
			contract: core.AgentContract{Goal: "g", Acceptance: []string{"regex:@field=summary:(?P<docanchor>pelican)"},
				Context: []core.ContextDoc{{Name: "page.md", Text: "pelican"}}},
			wire: core.AgentWireResult{Structured: json.RawMessage(`{"other":"x"}`)},
		},
		{
			name: "field check on a field the output_schema does not declare",
			contract: core.AgentContract{Goal: "g", Acceptance: []string{"regex:@field=verdict:(?P<docanchor>pelican)"},
				OutputSchema: schema, Context: []core.ContextDoc{{Name: "page.md", Text: "pelican"}}},
			wire: core.AgentWireResult{Structured: json.RawMessage(`{"summary":"pelican"}`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// still reported as a failure that says unknown (it is not dropped, not excused)
			f := EvalAcceptance(tt.contract, tt.wire)
			if len(f) != 1 || !strings.Contains(f[0], "unknown at") || !failsDocumentFingerprint(f) {
				t.Fatalf("EvalAcceptance = %v, want the one unknown fingerprint reported as a failure", f)
			}
			if fingerprintFailedByNode(tt.contract, tt.wire, f) {
				t.Errorf("a contract-caused unknown is read as a node failure: %v", f)
			}
			// and it is neither a strike nor a pass, however often it repeats
			blocked, strikes, passes := fingerprintNoteOutcome(t, tt.contract, tt.wire, 4)
			if blocked || strikes != 0 || passes != 0 {
				t.Fatalf("after 4 results: blocked=%v strikes=%d passes=%d, want an untouched node", blocked, strikes, passes)
			}
		})
	}
}

// The control: an unknown the contract COULD have closed is the result's. The documents are there and the answer is empty,
// or the schema asks for the field and the result lacks it, so a node that answers this way is the one at fault, as an
// unqualified fingerprint over an empty answer has always been (a fail, and a strike).
func TestAResultCausedUnknownFingerprintStillStrikes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}}}`)
	tests := []struct {
		name     string
		contract core.AgentContract
		wire     core.AgentWireResult
		verdict  core.Verdict
	}{
		{
			name:     "grounded check, documents present, empty answer",
			contract: qualifiedFingerprintContract(core.ContextDoc{Name: "page.md", Text: "the pelican lives here"}),
			wire:     core.AgentWireResult{},
			verdict:  core.VerdictUnknown,
		},
		{
			name: "field check, field declared, absent from the result",
			contract: core.AgentContract{Goal: "g", Acceptance: []string{"regex:@field=summary:(?P<docanchor>pelican)"},
				OutputSchema: schema, Context: []core.ContextDoc{{Name: "page.md", Text: "pelican"}}},
			wire:    core.AgentWireResult{Structured: json.RawMessage(`{"other":"x"}`)},
			verdict: core.VerdictUnknown,
		},
		{
			name:     "grounded check, documents present, off-document answer",
			contract: qualifiedFingerprintContract(core.ContextDoc{Name: "page.md", Text: "the pelican lives here"}),
			wire:     core.AgentWireResult{Output: "the latest release is newer"},
			verdict:  core.VerdictFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if o := GradeAcceptance(tt.contract, tt.wire); len(o) != 1 || o[0].Verdict != tt.verdict {
				t.Fatalf("GradeAcceptance = %+v, want one %s", o, tt.verdict)
			}
			f := EvalAcceptance(tt.contract, tt.wire)
			if !fingerprintFailedByNode(tt.contract, tt.wire, f) {
				t.Fatalf("a result-caused fingerprint failure is not read as the node's: %v", f)
			}
			if blocked, _, _ := fingerprintNoteOutcome(t, tt.contract, tt.wire, 2); !blocked {
				t.Fatal("two such results did not quarantine the node")
			}
		})
	}
}

// Beside a contract-caused unknown, a failure that IS the node's still strikes: the exclusion is for the one check, not
// for the result.
func TestAContractCausedUnknownDoesNotShieldAnotherFingerprintFailure(t *testing.T) {
	c := core.AgentContract{
		Goal:    "g",
		Context: []core.ContextDoc{{Name: "page.md", Text: "the pelican lives here"}},
		Acceptance: []string{
			"regex:@field=summary:(?P<docanchor>pelican)", // no output_schema: unreachable, always unknown
			"regex:(?P<docanchor>walrus)",                 // a plain fingerprint the answer misses
		},
	}
	wire := core.AgentWireResult{Output: "something else entirely"}
	f := EvalAcceptance(c, wire)
	if len(f) != 2 {
		t.Fatalf("failures = %v, want the unknown and the miss", f)
	}
	if !fingerprintFailedByNode(c, wire, f) {
		t.Fatal("the plain fingerprint miss was hidden by the contract-caused unknown")
	}
}

// The cross-seat retry reads the same classification: a second node given a contract that cannot be closed grades the
// same unknown, so a contract-caused unknown keeps no retry, while a fingerprint the node missed keeps its one.
func TestAContractCausedUnknownFingerprintKeepsNoCrossSeatRetry(t *testing.T) {
	grade := func(c core.AgentContract, wire core.AgentWireResult) PlacedResult {
		pr := PlacedResult{Result: wire}
		pr.gradeAcceptance(c, wire)
		return pr
	}
	defective := qualifiedFingerprintContract()
	defective.Door = "offload_research"
	pr := grade(defective, absentDigestWithAnswer("pelican"))
	if len(pr.AcceptanceFailures) != 1 {
		t.Fatalf("failures = %v, want the unknown reported", pr.AcceptanceFailures)
	}
	if !skipsRetryAsResearchAcceptanceOnly(defective, pr) {
		t.Fatal("a contract-caused unknown fingerprint keeps its cross-seat retry: another node would grade the same unknown")
	}

	missed := qualifiedFingerprintContract(core.ContextDoc{Name: "page.md", Text: "the pelican lives here"})
	missed.Door = "offload_research"
	pr = grade(missed, core.AgentWireResult{Output: "the latest release is newer"})
	if len(pr.AcceptanceFailures) != 1 || skipsRetryAsResearchAcceptanceOnly(missed, pr) {
		t.Fatalf("a missed fingerprint lost its cross-seat retry (failures=%v)", pr.AcceptanceFailures)
	}
}

// An honest absence is neither a pass nor a strike (Quarantine). A qualified field fingerprint whose field is empty grades
// unknown, and the absence sentence sits in ANOTHER field, so the digest is an honest absence: the unknown is no fact about
// the node, and excusesFingerprint (a fail only) does not drop it. Two such results must not block the node. The control
// beside it: the same unknown over a digest that makes a claim is still the node's, and still strikes.
func TestAnUnknownFingerprintOverAnHonestAbsenceStrikesNoNode(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"note":{"type":"string"}}}`)
	contract := core.AgentContract{Goal: "g", Acceptance: []string{"regex:@field=summary:(?P<docanchor>pelican)"},
		OutputSchema: schema, Context: []core.ContextDoc{{Name: "page.md", Text: "the pelican lives here"}}}
	tests := []struct {
		name       string
		structured string
		absent     bool
	}{
		{"empty field, absence sentence in another field", `{"summary":"","note":"the page does not mention it"}`, true},
		{"empty field, a claim in another field", `{"summary":"","note":"the heron lives here"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire := core.AgentWireResult{Structured: json.RawMessage(tt.structured)}
			if HonestAbsence(wire.Structured) != tt.absent {
				t.Fatalf("HonestAbsence(%s) = %v, want %v", tt.structured, !tt.absent, tt.absent)
			}
			f := EvalAcceptance(contract, wire)
			if len(f) != 1 || !strings.Contains(f[0], "unknown at") || !failsDocumentFingerprint(f) {
				t.Fatalf("EvalAcceptance = %v, want the one unknown fingerprint reported", f)
			}
			if got := fingerprintFailedByNode(contract, wire, f); got == tt.absent {
				t.Errorf("fingerprintFailedByNode = %v, want %v", got, !tt.absent)
			}
			blocked, strikes, passes := fingerprintNoteOutcome(t, contract, wire, 2)
			if tt.absent && (blocked || strikes != 0 || passes != 0) {
				t.Fatalf("honest absence: blocked=%v strikes=%d passes=%d, want an untouched node", blocked, strikes, passes)
			}
			if !tt.absent && !blocked {
				t.Fatalf("a non-absent unknown must still strike: blocked=%v strikes=%d", blocked, strikes)
			}
		})
	}
}

// A node that PASSED the real fingerprint earned a pass even when a sibling qualified check is a contract-caused unknown
// (that check is reported as a failure, which used to end noteFingerprint at its first case without recording the pass).
func TestAPassedFingerprintBesideAContractCausedUnknownIsStillAPass(t *testing.T) {
	c := core.AgentContract{
		Goal:    "g",
		Context: []core.ContextDoc{{Name: "page.md", Text: "the pelican lives here"}},
		Acceptance: []string{
			"regex:(?P<docanchor>pelican)",                // the real fingerprint: the answer carries it
			"regex:@field=summary:(?P<docanchor>pelican)", // no output_schema: unreachable, always unknown
		},
	}
	wire := core.AgentWireResult{Output: "the pelican"}
	f := EvalAcceptance(c, wire)
	if len(f) != 1 || fingerprintFailedByNode(c, wire, f) {
		t.Fatalf("setup: failures=%v, want only the contract-caused unknown", f)
	}
	blocked, strikes, passes := fingerprintNoteOutcome(t, c, wire, 1)
	if blocked || strikes != 0 || passes != 1 {
		t.Fatalf("blocked=%v strikes=%d passes=%d, want the one pass recorded", blocked, strikes, passes)
	}

	// and the unknown alone, with nothing passed, is still neither
	only := core.AgentContract{Goal: "g", Context: c.Context, Acceptance: c.Acceptance[1:]}
	if blocked, strikes, passes := fingerprintNoteOutcome(t, only, wire, 1); blocked || strikes != 0 || passes != 0 {
		t.Fatalf("unknown only: blocked=%v strikes=%d passes=%d, want an untouched node", blocked, strikes, passes)
	}
}
