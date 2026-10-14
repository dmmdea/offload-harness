package delegate

// An honest absence and the document fingerprint.
//
// The research lane's acceptance carries one wrong-document tripwire, the `docanchor` regex: an alternation of the page's
// own prose words. A digest passes it by restating any one of them. A digest that says "the page does not carry what was
// asked" restates none (the goal that tells the seat to say so is the one place those words live, and the anchors are
// chosen off the goal's vocabulary), so the tripwire failed it, struck the node that wrote it and sent the contract to a
// second seat: 9 of 254 research answers in two days, every one faithful or honestly absent and none off-document.
//
// The first cure put the absence phrases into the regex as a last alternative. That made the goal itself pass the check
// (the goal says "the page does not address it"; the lint rightly called every research contract parrot-passable) and made
// a stock phrase ("not specified") anywhere in a digest about the wrong document a pass. A phrase in a regex cannot be told
// from the same phrase in the goal or in a sentence that claims something, so the cure is structural and lives here:
// the regex holds the page's words and nothing else, and a result that misses it is excused only when its structured digest
// makes no claim at all, which is what HonestAbsence reads.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// docAnchorTag opens the named group research.AnchorCheck tags its regex with. It is the one spelling the delegator keys
// the fingerprint on (failsDocumentFingerprint, the quarantine, the retry rule and this file).
const docAnchorTag = "(?P<docanchor>"

// maxAbsenceWords bounds one statement of absence. The research goal asks every string field to stay under 40 words, so a
// longer sentence is an essay or the goal itself echoed back, never "nothing here".
const maxAbsenceWords = 60

// absencePhrase is what a statement of absence says: the page (or the document, or the text) does not carry something.
// "not found" is deliberately not here, it is an error (readFailure).
var absencePhrase = regexp.MustCompile(`(?i)not on (?:the |this )?page|(?:does not|doesn['’]t) (?:contain|address|document|specify|describe|mention|state|cover)|no mention|not (?:documented|stated|specified|mentioned|covered|present)`)

// readFailure is what a seat that could not read its file writes. Such a sentence can carry an absence phrase too ("I could
// not read the file, so the page does not address it"), and it is an error, not a finding about any page. It names the
// FAILURE TO READ and not the bare words: "the page does not mention any error codes" is an honest absence.
var readFailure = regexp.MustCompile(`(?i)\b(?:` +
	`(?:could not|couldn['’]t|cannot|can['’]t|unable to|not able to|failed to|fail(?:s|ed) to) (?:read|open|access|load|find|locate|see|retrieve)` +
	`|error[^.;]{0,30}\b(?:read|reading|open|opening|load|loading|access|accessing)` +
	`|(?:read|open|load|access)(?:ing)? error` +
	`|no such file|not found|does not exist|doesn['’]t exist|unreadable|not readable|permission denied|timed out` +
	`|(?:file|document|context|page) (?:is |was )?(?:missing|empty|absent)|missing (?:file|document|context)` +
	`)\b`)

// HonestAbsence reports whether a structured digest makes no claim about any document: it is an object, and every string in
// it, at any depth (a list entry as much as a field), is empty or is a short statement that the page does not carry what was
// asked ("not on page", "the page does not address it", "not documented in this text"), with at least one such statement. A
// number or a boolean is a claim, so is any other sentence, and so is a statement of failure to read the file. An absence says
// nothing about any document, so it cannot be about the wrong one; a digest that says anything else is judged as it always was.
//
// It reads STRUCTURE (the decoded fields), never the loop's prose and never a phrase somewhere in a text: one stock phrase
// beside a claim does not make the claim an absence, and the goal echoed back as a field is far over maxAbsenceWords.
func HonestAbsence(structured json.RawMessage) bool {
	if len(bytes.TrimSpace(structured)) == 0 {
		return false
	}
	var root any
	if err := json.Unmarshal(structured, &root); err != nil {
		return false
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return false
	}
	statements := 0
	return absenceOnly(obj, &statements) && statements > 0
}

// contrast is a word that brings a claim in beside an absence ("does not mention the date, but describes the scheduler").
var contrast = regexp.MustCompile(`(?i)\b(?:but|however|although|though|whereas|except|instead|rather|only|yet)\b`)

// sentenceEnd splits a string into its sentences.
var sentenceEnd = regexp.MustCompile(`[.!?;]+(?:\s+|$)`)

// absenceStatement: the string is nothing but statements of absence. It is short (maxAbsenceWords), EVERY sentence of it
// carries an absence phrase (a second sentence that says what the page does hold is a claim), none of it brings a claim in
// with a contrast word, and none of it reports a failure to read.
func absenceStatement(s string) bool {
	if len(strings.Fields(s)) > maxAbsenceWords || readFailure.MatchString(s) || contrast.MatchString(s) {
		return false
	}
	sentences := 0
	for _, part := range sentenceEnd.Split(s, -1) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		sentences++
		if !absencePhrase.MatchString(part) {
			return false
		}
	}
	return sentences > 0
}

// absenceOnly walks the decoded digest: true while everything in it is empty or a statement of absence (counted).
func absenceOnly(v any, statements *int) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return true
		}
		*statements++
		return absenceStatement(s)
	case []any:
		for _, e := range t {
			if !absenceOnly(e, statements) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range t {
			if !absenceOnly(e, statements) {
				return false
			}
		}
		return true
	default:
		// a number or a boolean asserts a value
		return false
	}
}

// excusesFingerprint is the one reading of the honest-absence excuse, shared by the merge gate (EvalAcceptance) and the
// quarantine (excusedAbsence) so the two cannot disagree: a check is excused only when it is a document fingerprint that
// PARSED and was graded a FAIL, and the digest is an honest absence. An unknown is not excused (an unknown is never a pass,
// and dropping it would read it as one), and neither is a parse error. o and parsed come from gradeCheck, the same grading
// that decides the verdict.
func excusesFingerprint(a string, o core.AcceptanceOutcome, parsed bool, wire core.AgentWireResult) bool {
	return parsed && o.Verdict == core.VerdictFail && strings.Contains(a, docAnchorTag) && HonestAbsence(wire.Structured)
}

// excusedAbsence reports whether the result misses the contract's document fingerprint and is excused because its digest is
// an honest absence (HonestAbsence). EvalAcceptance drops that failure; the quarantine reads this to count the result as
// neither a strike nor a pass, since an absence says nothing about whether the node reads the right document. The check is
// graded the way EvalAcceptance grades it (gradeCheck, with the contract's context documents), never through Eval, which has
// none: a grounded fingerprint the documents carry PASSES, and a passing result is a quarantine pass, not an excused absence.
func excusedAbsence(contract core.AgentContract, wire core.AgentWireResult) bool {
	if !HonestAbsence(wire.Structured) {
		return false
	}
	for _, a := range contract.Acceptance {
		if !strings.Contains(a, docAnchorTag) {
			continue
		}
		o, parsed := gradeCheck(a, contract, wire)
		if excusesFingerprint(a, o, parsed, wire) {
			return true
		}
	}
	return false
}

// fingerprintFailedByNode: placeholder, the old reading (the tag alone).
func fingerprintFailedByNode(contract core.AgentContract, wire core.AgentWireResult, failures []string) bool {
	return failsDocumentFingerprint(failures)
}
