package delegate

// The document fingerprint and an honest absence (absence.go). The fingerprint is a regex of the page's own words; a digest
// that says "the page does not carry what was asked" restates none of them and used to fail it (and strike the node, and
// send the contract to a second seat). It is excused from the STRUCTURE of the digest, not from a phrase in a regex: the
// regex stays free of every sentence the goal contains, so an echoed goal and a stock phrase beside a claim still fail.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestHonestAbsenceReadsTheStructureOfTheDigest(t *testing.T) {
	for name, tc := range map[string]struct {
		structured string
		want       bool
	}{
		"every list empty and an absence verdict":    {`{"key_facts":[],"numbers":[],"quotes":[],"verdict":"The page does not address it."}`, true},
		"entries that are each an absence":           {`{"key_facts":["not on page","not on page"],"verdict":"Not on the page."}`, true},
		"a field of a custom schema":                 {`{"answer":"The document does not mention any revisions."}`, true},
		"an absence nested in an object":             {`{"found":{"detail":"not documented in this text"}}`, true},
		"a contraction":                              {`{"answer":"The page doesn't address the pin voltage."}`, true},
		"two sentences that each say it":             {`{"answer":"The page does not mention the date. It is not documented in this text either."}`, true},
		"empty strings and nulls beside one absence": {`{"a":"","b":null,"c":[],"d":"not on page"}`, true},
		"error codes the page does not carry":        {`{"answer":"The page does not mention any error codes."}`, true},

		"punctuation only":                   {`{"answer":"..."}`, false},
		"nothing said at all":                {`{"key_facts":[],"numbers":[],"quotes":[],"verdict":""}`, false},
		"no structured result":               {``, false},
		"not an object":                      {`["not on page"]`, false},
		"not JSON":                           {`not on page`, false},
		"a claim in a list":                  {`{"key_facts":["The release ships a new scheduler."],"verdict":"The page does not address it."}`, false},
		"a number is a claim":                {`{"numbers":[1.24],"verdict":"The page does not address it."}`, false},
		"a boolean is a claim":               {`{"found":false,"verdict":"The page does not address it."}`, false},
		"a stock phrase beside a claim":      {`{"key_facts":["The release ships a new scheduler.","The date is not specified."],"verdict":"Notes on a release."}`, false},
		"a claim after the absence":          {`{"answer":"The page does not mention the date. It describes a new scheduler."}`, false},
		"a claim brought in with a contrast": {`{"answer":"The page does not mention the date, but describes a new scheduler."}`, false},
		"could not read the file":            {`{"answer":"I could not read the file, so the page does not address it."}`, false},
		"the file was not found":             {`{"answer":"The file was not found; the page does not address it."}`, false},
		"the file is missing":                {`{"answer":"The file is missing, so the page does not mention it."}`, false},
		"an error while opening":             {`{"answer":"An error occurred while opening the file; the page does not mention it."}`, false},
		"the goal's length of text":          {`{"answer":"` + strings.Repeat("the page does not address it ", 20) + `"}`, false},
		"a sentence that says nothing of it": {`{"answer":"A guide to flash chips."}`, false},
	} {
		if got := HonestAbsence(json.RawMessage(tc.structured)); got != tc.want {
			t.Errorf("%s: HonestAbsence(%s) = %v, want %v", name, clipReason(tc.structured), got, tc.want)
		}
	}
}

func structuredWire(structured string) core.AgentWireResult {
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Structured: json.RawMessage(structured), StopReason: "done"}
}

const absenceDigest = `{"answer":"The page does not address it."}`

// EvalAcceptance drops the fingerprint failure of an honest absence and no other failure, and keeps the fingerprint failure
// of every digest that is not one.
func TestEvalAcceptanceExcusesOnlyTheFingerprintOfAnHonestAbsence(t *testing.T) {
	c := fingerprintContract()
	if got := EvalAcceptance(c, structuredWire(absenceDigest)); len(got) != 0 {
		t.Fatalf("an honest absence failed: %v", got)
	}
	if got := EvalAcceptance(c, structuredWire(`{"answer":"The latest stable release ships a new scheduler."}`)); len(got) != 1 || !failsDocumentFingerprint(got) {
		t.Fatalf("a digest about another document: failures %v, want the fingerprint", got)
	}
	// another failed check stays a failure beside the excused fingerprint
	strict := c
	strict.Acceptance = append(append([]string{}, c.Acceptance...), "contains:zorblax")
	got := EvalAcceptance(strict, structuredWire(absenceDigest))
	if len(got) != 1 || failsDocumentFingerprint(got) || !strings.Contains(got[0], "zorblax") {
		t.Fatalf("failures %v, want only the contains check: the excuse is for the fingerprint alone", got)
	}
	// a contract that carries no fingerprint gets no excuse for any other check
	plain := remoteContract()
	plain.Acceptance = []string{"regex:(?i)setpts|atempo", "nonempty:answer"}
	if got := EvalAcceptance(plain, structuredWire(absenceDigest)); len(got) != 1 || !strings.Contains(got[0], "setpts") {
		t.Fatalf("failures %v, want the plain regex check to fail: only the document fingerprint is excused", got)
	}
}

// An honest absence is no evidence about the node: not a strike, not a pass. A pass is a result that restated the page.
func TestAnExcusedAbsenceIsNeitherAStrikeNorAPass(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	r := &runner{quarantine: q}
	base := "http://node-a:18811"
	c := fingerprintContract()
	absent := structuredWire(absenceDigest)
	restated := structuredWire(`{"answer":"the clip shows setpts in its filter graph"}`)
	off := structuredWire(`{"answer":"the latest stable release is newer than that"}`)
	note := func(w core.AgentWireResult) { r.noteFingerprint(base, c, w, EvalAcceptance(c, w)) }

	for i := 0; i < 5; i++ {
		note(absent)
	}
	q.mu.Lock()
	passes, strikes := len(q.passes[base]), len(q.strikes[base])
	q.mu.Unlock()
	if passes != 0 || strikes != 0 {
		t.Fatalf("five honest absences left %d pass(es) and %d strike(s), want neither", passes, strikes)
	}
	// so they do not shield a node that then answers about another document twice
	note(off)
	note(off)
	if !q.Blocked(base) {
		t.Fatal("two off-document answers after five honest absences did not quarantine the node: the absences were counted as passes")
	}

	q2 := NewQuarantine(10 * time.Minute)
	r2 := &runner{quarantine: q2}
	r2.noteFingerprint(base, c, restated, EvalAcceptance(c, restated))
	q2.mu.Lock()
	defer q2.mu.Unlock()
	if len(q2.passes[base]) != 1 {
		t.Fatalf("a result that restated the page is %d pass(es), want 1", len(q2.passes[base]))
	}
}
