// run_accept.go - Delegator-side acceptance grading and the parse-fingerprint strike memory.

package delegate

import (
	"log"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// EvalAcceptance runs every contract acceptance check against the result —
// DELEGATOR-side, before merge (roast delta 3). An unparseable check fails
// closed with the parse error as the failure: Validate should have caught it,
// and an unmet precondition is a failed check, never a skipped one.
//
// Exported (package-private until askjob) so the single-seat offload_ask lane,
// which runs its contract through Pipeline.RunAgentContract instead of
// delegate.Run, evaluates acceptance by the SAME rules rather than a second
// copy of them. An acceptance check nothing evaluates is decoration.
func EvalAcceptance(contract core.AgentContract, wire core.AgentWireResult) []string {
	failures, _ := EvalAcceptanceGraded(contract, wire)
	return failures
}

// EvalAcceptanceGraded is EvalAcceptance plus the authority the contract closed at, from the same grading (so the two
// cannot disagree about what passed). closedAt is core.ClosedAt over the graded checks: the weakest authority among them,
// and only when every check passed. It is stated only for a result that was accepted whole: any failure (an unknown, a
// list-fidelity failure included), a check that was excused instead of passed, and a single unqualified check all leave it
// core.AuthNone, which says nothing. A weaker authority never closes a stronger claim, so a contract mixing a grounded
// check with an answer check closes at answer, and one that also carries a plain check closes at none.
func EvalAcceptanceGraded(contract core.AgentContract, wire core.AgentWireResult) (failures []string, closedAt core.Authority) {
	outcomes := make([]core.AcceptanceOutcome, 0, len(contract.Acceptance))
	for _, a := range contract.Acceptance {
		o, parsed := gradeCheck(a, contract, wire)
		outcomes = append(outcomes, o)
		if o.Verdict == core.VerdictPass {
			continue
		}
		// A digest that claims nothing and says the page does not carry what was asked restates none of the page's
		// words, and is not about another document either (HonestAbsence reads the structure, not a phrase). Only a
		// FAILED fingerprint is excused (excusesFingerprint): an unknown stays a reported failure.
		if excusesFingerprint(a, o, parsed, wire) {
			continue
		}
		// An unknown (empty or absent evidence at a qualified check's level) is a failed check here, never a pass;
		// its reason says "unknown" so it is told apart from a wrong answer.
		failures = append(failures, o.Reason)
	}
	// Beside the contract's own checks, the object is read against its answer (fidelity.go):
	// no check names the lists a node's re-pack may have emptied. A no-op for a contract with no
	// schema or no list in it, which is every contract of the single-seat ask lane.
	if why := listFidelityFailure(contract, wire.Structured, wire.Output); why != "" {
		failures = append(failures, why)
	}
	if len(failures) == 0 {
		// An excused check is a fail outcome, so ClosedAt leaves such a contract open.
		if at, ok := core.ClosedAt(outcomes); ok {
			closedAt = at
		}
	}
	return failures, closedAt
}

// gradeAcceptance records on the placed result what the delegator found when it graded the contract against wire: the
// failures and the authority the contract closed at. It is the one call every site that grades a result makes, so the two
// fields are always set together and a result that was not graded carries neither.
func (pr *PlacedResult) gradeAcceptance(contract core.AgentContract, wire core.AgentWireResult) {
	pr.AcceptanceFailures, pr.AcceptanceAuthority = EvalAcceptanceGraded(contract, wire)
}

// publishedAuthority is the wire spelling of the authority a result closed at: the level's name, and "" for none, so the
// key is omitted and a result no qualified contract produced publishes byte-identically to before.
func publishedAuthority(a core.Authority) string {
	if a == core.AuthNone {
		return ""
	}
	return a.String()
}

// gradeCheck grades one acceptance string against the result and the contract's context documents. parsed is false
// when the string does not parse: that is a failed check carrying the parse error, as it always was.
func gradeCheck(a string, contract core.AgentContract, wire core.AgentWireResult) (o core.AcceptanceOutcome, parsed bool) {
	chk, err := core.ParseAcceptanceCheck(a)
	if err != nil {
		return core.AcceptanceOutcome{Check: a, Verdict: core.VerdictFail, Reason: err.Error()}, false
	}
	return chk.Grade(wire, contract.Context), true
}

// GradeAcceptance is EvalAcceptance's per-check reading: one outcome per acceptance string, in order, each pass, fail
// or unknown, with the authority the check demanded. It is the same grading EvalAcceptance runs (gradeCheck), without
// the document-fingerprint excuse and the list-fidelity read that EvalAcceptance adds, so it is the raw verdicts. The
// authority a contract closed at, as the delegator publishes it, is EvalAcceptanceGraded's: it applies the excuse and the
// fidelity read before it states one, which core.ClosedAt over these raw outcomes does not.
func GradeAcceptance(contract core.AgentContract, wire core.AgentWireResult) []core.AcceptanceOutcome {
	out := make([]core.AcceptanceOutcome, 0, len(contract.Acceptance))
	for _, a := range contract.Acceptance {
		o, _ := gradeCheck(a, contract, wire)
		out = append(out, o)
	}
	return out
}

// fetchViews probes every configured remote's /fleet/health, returning the
// views, a PARALLEL base-URL slice (NodeView carries no base — placement is
// pure; the pairing is the runner's business), and the probe FAILURES. A node
// that cannot answer is not a candidate — the gate's fail-toward-local posture
// — but the reason it could not answer is the operator's whole diagnosis: a
// wrong token (401), a node that is down (dial refused), and a stale VRAM
// snapshot (503) all end as "not a candidate", and dropping the error made
// them indistinguishable from a node that merely failed the gate.
// strikeOnFingerprint strikes base in the caller's Quarantine when any failed
// acceptance is a DOCUMENT FINGERPRINT check (research.DocFingerprint tags its
// regex with the named group `docanchor`). Contract-caused failures — a
// user's over-strict contains:, a thin page's min_items, a fingerprint the contract
// can never close (noteFingerprint filters it, fingerprintFailedByNode) — never strike.
func (r *runner) strikeOnFingerprint(base string, failures []string) {
	if r.quarantine == nil || !failsDocumentFingerprint(failures) {
		return
	}
	if r.quarantine.Strike(base) {
		r.quarantined.Add(1)
		log.Printf("delegate: node %s quarantined for %s after off-document answers (document fingerprint failed at least twice, and more often than it passed)", base, DefaultQuarantineTTL)
	}
}

// noteFingerprint is the runner's reading of one result against the document fingerprint: a contract that carries the
// check and failed it is a strike against the node, one that carries it and PASSED is a pass (Quarantine.Pass), and one that
// carries none says nothing about the node. A result the check missed that is an honest absence (excusedAbsence) is neither:
// it did not fail the contract, but it restated nothing of the page either, so it is no evidence that the node reads the
// right document and must not stand among its passes. Nor is a fingerprint the contract can never close (an unknown the
// intake lint warns about, as a grounded check on a contract with no context documents): it is a defect of the contract,
// still reported as a failure, and no node could have passed it, so it neither strikes nor passes (fingerprintFailedByNode).
// An unknown over an honest absence is the same: neither. A fingerprint the node PASSED beside such a failure is still a pass.
// The local seat has no call here, as for strikeOnFingerprint.
func (r *runner) noteFingerprint(base string, contract core.AgentContract, wire core.AgentWireResult, failures []string) {
	if r.quarantine == nil {
		return
	}
	switch {
	case failsDocumentFingerprint(failures):
		if fingerprintFailedByNode(contract, wire, failures) {
			r.strikeOnFingerprint(base, failures)
		} else if passedFingerprint(contract, wire) && !excusedAbsence(contract, wire) {
			// every reported fingerprint failure is the contract's or an honest absence (neither strikes), but a sibling
			// fingerprint the node PASSED is still the pass it earned
			r.quarantine.Pass(base)
		}
	case failsDocumentFingerprint(contract.Acceptance):
		// the check is on the contract (failsDocumentFingerprint reads the tag in either form) and did not fail
		if !excusedAbsence(contract, wire) {
			r.quarantine.Pass(base)
		}
	}
}

// passedFingerprint reports whether the contract carries a document-fingerprint check that the result graded PASS.
func passedFingerprint(contract core.AgentContract, wire core.AgentWireResult) bool {
	for _, a := range contract.Acceptance {
		if !strings.Contains(a, docAnchorTag) {
			continue
		}
		if o, parsed := gradeCheck(a, contract, wire); parsed && o.Verdict == core.VerdictPass {
			return true
		}
	}
	return false
}
