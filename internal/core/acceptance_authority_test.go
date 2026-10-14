package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// The authority qualifier. A qualified check names the evidence it
// reads and the authority that evidence carries; an unqualified check is
// byte-for-byte what it was. These tests pin both halves: the qualified form
// (unknown on empty evidence, a weaker authority never closing a stronger
// claim) and the unqualified form (every case a legacy contract can reach).

// authDocs is one context document the grounded level verifies against.
var authDocs = []ContextDoc{{Name: "spec.md", Text: "The retry limit is 7 and the cache is named pelican."}}

func mustParse(t *testing.T, s string) AcceptanceCheck {
	t.Helper()
	c, err := ParseAcceptanceCheck(s)
	if err != nil {
		t.Fatalf("ParseAcceptanceCheck(%q): %v", s, err)
	}
	return c
}

// TestAuthorityQualifierParses: the qualifier sits in the ARGUMENT tail so the
// verb spelling is unchanged (the closed verb set and a node one release behind
// both depend on that), it is stripped out of Arg so the lint reads the real
// substring, and a name that merely starts with "@" stays a literal.
func TestAuthorityQualifierParses(t *testing.T) {
	tests := []struct {
		in        string
		kind      AcceptanceKind
		auth      Authority
		arg       string
		fields    []string
		wantError string // substring of the parse error; "" = must parse
	}{
		{in: "contains:pelican", kind: AccContains, auth: AuthNone, arg: "pelican"},
		{in: "contains:@answer:pelican", kind: AccContains, auth: AuthAnswer, arg: "pelican"},
		{in: "contains:@grounded:pelican", kind: AccContains, auth: AuthGrounded, arg: "pelican"},
		{in: "contains:@field=summary:pelican", kind: AccContains, auth: AuthField, arg: "pelican", fields: []string{"summary"}},
		{in: "regex:@answer:pel[a-z]+", kind: AccRegex, auth: AuthAnswer, arg: "pel[a-z]+"},
		{in: "regex:@grounded:pel[a-z]+", kind: AccRegex, auth: AuthGrounded, arg: "pel[a-z]+"},
		{in: "regex:@field=summary:pel[a-z]+", kind: AccRegex, auth: AuthField, arg: "pel[a-z]+", fields: []string{"summary"}},
		{in: "not_contains:@answer:PLACEHOLDER", kind: AccNotContains, auth: AuthAnswer, arg: "PLACEHOLDER"},
		{in: "not_contains:@field=summary:PLACEHOLDER", kind: AccNotContains, auth: AuthField, arg: "PLACEHOLDER", fields: []string{"summary"}},
		// the rest of the argument is untouched, colons included
		{in: "contains:@answer:a:b", kind: AccContains, auth: AuthAnswer, arg: "a:b"},
		// a name that merely starts with @ is a literal substring, exactly as before
		{in: "contains:@mention", kind: AccContains, auth: AuthNone, arg: "@mention"},
		{in: "contains:@mention:x", kind: AccContains, auth: AuthNone, arg: "@mention:x"},
		{in: "contains:@Answer:x", kind: AccContains, auth: AuthNone, arg: "@Answer:x"},
		{in: "regex:@\\w+", kind: AccRegex, auth: AuthNone, arg: "@\\w+"},
		// text that only begins with the letters @field is not a reserved prefix (the reserved ones are "@field=" and
		// "@field:"): a literal on every verb that takes a qualifier
		{in: "contains:@field", kind: AccContains, auth: AuthNone, arg: "@field"},
		{in: "contains:@fieldname:x", kind: AccContains, auth: AuthNone, arg: "@fieldname:x"},
		{in: "contains:@fieldx", kind: AccContains, auth: AuthNone, arg: "@fieldx"},
		{in: "regex:@field", kind: AccRegex, auth: AuthNone, arg: "@field"},
		{in: "regex:@fieldname:x", kind: AccRegex, auth: AuthNone, arg: "@fieldname:x"},
		{in: "not_contains:@field", kind: AccNotContains, auth: AuthNone, arg: "@field"},
		{in: "not_contains:@fieldname:x", kind: AccNotContains, auth: AuthNone, arg: "@fieldname:x"},
		// verbs that take no qualifier read their argument literally, as before
		{in: "nonempty:@answer:x", kind: AccNonempty, auth: AuthNone, arg: "@answer:x", fields: []string{"@answer:x"}},
		{in: "diff_touches:@answer:x", kind: AccDiffTouches, auth: AuthNone, arg: "@answer:x"},

		// malformed or empty after the qualifier
		{in: "contains:@answer:", wantError: "empty substring"},
		{in: "contains:@grounded:", wantError: "empty substring"},
		{in: "regex:@answer:", wantError: "empty pattern"},
		{in: "not_contains:@answer:", wantError: "empty substring"},
		{in: "contains:@field=summary:", wantError: "empty substring"},
		{in: "contains:@field:x", wantError: "@field=<name>"},
		{in: "contains:@field=:x", wantError: "@field=<name>"},
		{in: "contains:@field=summary", wantError: "@field=<name>"},
		{in: "contains:@field=", wantError: "@field=<name>"},
		{in: "regex:@field:x", wantError: "@field=<name>"},
		{in: "not_contains:@field:x", wantError: "@field=<name>"},
		{in: "regex:@answer:(", wantError: "error parsing regexp"},
		// absence cannot be corroborated by sources
		{in: "not_contains:@grounded:PLACEHOLDER", wantError: "grounded"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c, err := ParseAcceptanceCheck(tt.in)
			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("parsed, want an error containing %q", tt.wantError)
				}
				if !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error %q does not contain %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			if c.Kind != tt.kind || c.Authority != tt.auth || c.Arg != tt.arg {
				t.Fatalf("parsed to kind %q authority %v arg %q, want kind %q authority %v arg %q",
					c.Kind, c.Authority, c.Arg, tt.kind, tt.auth, tt.arg)
			}
			if got, want := strings.Join(c.Fields(), ","), strings.Join(tt.fields, ","); got != want {
				t.Fatalf("Fields() = %q, want %q", got, want)
			}
		})
	}
}

// TestAuthorityOrdering: the three levels are ordered weakest to strongest and
// an unqualified check claims none of them.
func TestAuthorityOrdering(t *testing.T) {
	if !(AuthNone < AuthAnswer && AuthAnswer < AuthField && AuthField < AuthGrounded) {
		t.Fatalf("want none < answer < field < grounded, got %d %d %d %d", AuthNone, AuthAnswer, AuthField, AuthGrounded)
	}
	for _, a := range []Authority{AuthNone, AuthAnswer, AuthField, AuthGrounded} {
		if !a.AtLeast(a) {
			t.Errorf("%v is not at least itself", a)
		}
	}
	if AuthAnswer.AtLeast(AuthGrounded) || AuthField.AtLeast(AuthGrounded) || AuthAnswer.AtLeast(AuthField) {
		t.Error("a weaker authority must not be at least a stronger one")
	}
	if !AuthGrounded.AtLeast(AuthAnswer) || !AuthGrounded.AtLeast(AuthField) || !AuthField.AtLeast(AuthAnswer) {
		t.Error("a stronger authority is at least a weaker one")
	}
	names := map[Authority]string{AuthNone: "none", AuthAnswer: "answer", AuthField: "field", AuthGrounded: "grounded"}
	for a, want := range names {
		if got := a.String(); got != want {
			t.Errorf("Authority(%d).String() = %q, want %q", a, got, want)
		}
	}
}

// TestQualifiedCheckGradesPassFailUnknown: the verdict table. "unknown" is the
// answer to an empty or absent evidence set and is never a pass.
func TestQualifiedCheckGradesPassFailUnknown(t *testing.T) {
	structured := json.RawMessage(`{"summary":"the cache is named pelican","count":7,"empty":"","none":[],"tags":["alpha","beta"],"nested":{"k":"deep value"}}`)
	prose := "I read the spec: the retry limit is 7."
	tests := []struct {
		name  string
		check string
		res   AgentWireResult
		docs  []ContextDoc
		want  Verdict
		why   string // substring of the reason; "" = none required
	}{
		// ---- answer authority reads Output only
		{"answer pass", "contains:@answer:retry limit", AgentWireResult{Output: prose}, nil, VerdictPass, ""},
		{"answer fail", "contains:@answer:pelican", AgentWireResult{Output: prose}, nil, VerdictFail, "not found in the answer text"},
		{"answer unknown on empty output", "contains:@answer:pelican", AgentWireResult{Output: ""}, nil, VerdictUnknown, "the answer text is empty"},
		{"answer unknown on blank output", "contains:@answer:pelican", AgentWireResult{Output: " \n\t "}, nil, VerdictUnknown, "the answer text is empty"},
		{"answer does not fall back to structured bytes", "contains:@answer:pelican", AgentWireResult{Structured: structured}, nil, VerdictUnknown, "the answer text is empty"},
		{"answer regex pass", "regex:@answer:limit is [0-9]+", AgentWireResult{Output: prose}, nil, VerdictPass, ""},
		{"answer regex fail", "regex:@answer:limit is [a-z]+", AgentWireResult{Output: prose}, nil, VerdictFail, "did not match"},
		{"answer regex unknown on empty", "regex:@answer:.*", AgentWireResult{}, nil, VerdictUnknown, "the answer text is empty"},

		// ---- field authority reads one named structured field
		{"field pass on a string", "contains:@field=summary:pelican", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"field pass on a number", "contains:@field=count:7", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"field pass inside an array", "contains:@field=tags:beta", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"field pass inside an object", "contains:@field=nested:deep", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"field reads values, not keys", "contains:@field=nested:k", AgentWireResult{Structured: structured}, nil, VerdictFail, "not found"},
		{"field fail", "contains:@field=summary:walrus", AgentWireResult{Structured: structured}, nil, VerdictFail, `not found in structured field "summary"`},
		{"field unknown without a structured result", "contains:@field=summary:pelican", AgentWireResult{Output: "pelican"}, nil, VerdictUnknown, "no structured output"},
		{"field unknown when not an object", "contains:@field=summary:x", AgentWireResult{Structured: json.RawMessage(`["x"]`)}, nil, VerdictUnknown, "not a JSON object"},
		{"field unknown when absent", "contains:@field=ghost:x", AgentWireResult{Structured: structured}, nil, VerdictUnknown, `field "ghost" is absent`},
		{"field unknown when an empty string", "contains:@field=empty:x", AgentWireResult{Structured: structured}, nil, VerdictUnknown, `field "empty" is an empty string`},
		{"field unknown when an empty array", "contains:@field=none:x", AgentWireResult{Structured: structured}, nil, VerdictUnknown, `field "none" is an empty array`},
		{"field regex pass", "regex:@field=summary:pel[a-z]+", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"field regex fail", "regex:@field=summary:^walrus", AgentWireResult{Structured: structured}, nil, VerdictFail, "did not match"},

		// ---- grounded authority: in the answer AND verbatim in a context document
		{"grounded pass", "contains:@grounded:pelican", AgentWireResult{Output: "the cache is named pelican"}, authDocs, VerdictPass, ""},
		{"grounded pass through a structured field", "contains:@grounded:pelican", AgentWireResult{Structured: structured}, authDocs, VerdictPass, ""},
		{"grounded fail when the answer says it and no document does", "contains:@grounded:walrus", AgentWireResult{Output: "the cache is named walrus"}, authDocs, VerdictFail, "none of the 1 context documents"},
		{"grounded fail when the document has it and the answer does not", "contains:@grounded:pelican", AgentWireResult{Output: "no idea"}, authDocs, VerdictFail, "not found in the answer"},
		{"grounded unknown with no context documents", "contains:@grounded:pelican", AgentWireResult{Output: "the cache is named pelican"}, nil, VerdictUnknown, "no context documents"},
		{"grounded unknown with only blank documents", "contains:@grounded:pelican", AgentWireResult{Output: "pelican"}, []ContextDoc{{Name: "a", Text: " \n"}}, VerdictUnknown, "no context documents"},
		{"grounded unknown on an empty answer", "contains:@grounded:pelican", AgentWireResult{}, authDocs, VerdictUnknown, "the answer is empty"},
		{"grounded regex pass", "regex:@grounded:pel[a-z]+", AgentWireResult{Output: "pelican"}, authDocs, VerdictPass, ""},
		{"grounded regex fail when the match is not in the documents", "regex:@grounded:wal[a-z]+", AgentWireResult{Output: "walrus"}, authDocs, VerdictFail, "none of the 1 context documents"},
		{"grounded regex unknown with no documents", "regex:@grounded:pel[a-z]+", AgentWireResult{Output: "pelican"}, nil, VerdictUnknown, "no context documents"},
		{"grounded regex ignores an empty match", "regex:@grounded:z*", AgentWireResult{Output: "pelican"}, authDocs, VerdictFail, ""},

		// ---- not_contains: an empty evidence set is unknown, not a pass
		{"not_contains answer pass", "not_contains:@answer:PLACEHOLDER", AgentWireResult{Output: prose}, nil, VerdictPass, ""},
		{"not_contains answer fail", "not_contains:@answer:retry", AgentWireResult{Output: prose}, nil, VerdictFail, "forbidden"},
		{"not_contains answer unknown on empty", "not_contains:@answer:PLACEHOLDER", AgentWireResult{}, nil, VerdictUnknown, "the answer text is empty"},
		{"not_contains field pass", "not_contains:@field=summary:PLACEHOLDER", AgentWireResult{Structured: structured}, nil, VerdictPass, ""},
		{"not_contains field fail", "not_contains:@field=summary:pelican", AgentWireResult{Structured: structured}, nil, VerdictFail, "forbidden"},
		{"not_contains field unknown when absent", "not_contains:@field=ghost:PLACEHOLDER", AgentWireResult{Structured: structured}, nil, VerdictUnknown, `field "ghost" is absent`},
		{"not_contains field unknown when empty", "not_contains:@field=empty:PLACEHOLDER", AgentWireResult{Structured: structured}, nil, VerdictUnknown, `field "empty" is an empty string`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustParse(t, tt.check)
			o := c.Grade(tt.res, tt.docs)
			if o.Verdict != tt.want {
				t.Fatalf("verdict = %q (%s), want %q", o.Verdict, o.Reason, tt.want)
			}
			if (o.Verdict == VerdictPass) != (o.Reason == "") {
				t.Fatalf("reason must be empty exactly on a pass: verdict %q reason %q", o.Verdict, o.Reason)
			}
			if o.Reason != "" && !strings.HasPrefix(o.Reason, tt.check+": ") {
				t.Fatalf("reason %q must open with the check, because failures are classified by that prefix", o.Reason)
			}
			if tt.why != "" && !strings.Contains(o.Reason, tt.why) {
				t.Fatalf("reason %q does not contain %q", o.Reason, tt.why)
			}
			if o.Verdict == VerdictUnknown && !strings.Contains(o.Reason, "unknown") {
				t.Fatalf("an unknown verdict must say so in its reason: %q", o.Reason)
			}
			if o.Authority != c.Authority || o.Check != tt.check {
				t.Fatalf("outcome carries authority %v check %q, want %v %q", o.Authority, o.Check, c.Authority, tt.check)
			}
			// Eval has no context documents; it must agree with Grade(res, nil) and never pass an unknown.
			pass, reason := c.Eval(tt.res)
			g := c.Grade(tt.res, nil)
			if pass != (g.Verdict == VerdictPass) || reason != g.Reason {
				t.Fatalf("Eval = (%v, %q) disagrees with Grade(res, nil) = (%q, %q)", pass, reason, g.Verdict, g.Reason)
			}
		})
	}
}

// TestAWeakerAuthorityNeverClosesAStrongerClaim: the same evidence is graded at
// each level. What the model merely wrote closes the answer claim; it does not
// close the field claim (no field carries it) and it does not close the grounded
// claim (no document vouches for it, or there is none to ask).
func TestAWeakerAuthorityNeverClosesAStrongerClaim(t *testing.T) {
	asserted := AgentWireResult{Output: "the cache is named walrus"} // said, but not in a field and not in the documents
	for _, tt := range []struct {
		check string
		docs  []ContextDoc
		want  Verdict
	}{
		{"contains:@answer:walrus", authDocs, VerdictPass},
		{"contains:@field=summary:walrus", authDocs, VerdictUnknown},
		{"contains:@grounded:walrus", authDocs, VerdictFail},
		{"contains:@grounded:walrus", nil, VerdictUnknown},
	} {
		if got := mustParse(t, tt.check).Grade(asserted, tt.docs).Verdict; got != tt.want {
			t.Errorf("%s with %d docs = %q, want %q", tt.check, len(tt.docs), got, tt.want)
		}
	}

	// A structured field says it and the documents do not: field authority holds,
	// grounded authority does not.
	typed := AgentWireResult{Structured: json.RawMessage(`{"summary":"walrus"}`)}
	if got := mustParse(t, "contains:@field=summary:walrus").Grade(typed, authDocs).Verdict; got != VerdictPass {
		t.Errorf("field claim = %q, want pass", got)
	}
	if got := mustParse(t, "contains:@grounded:walrus").Grade(typed, authDocs).Verdict; got != VerdictFail {
		t.Errorf("grounded claim on a typed but unverified value = %q, want fail", got)
	}

	// Plain Eval has no documents, so it can never close a grounded claim, even
	// when the answer carries the text.
	pass, reason := mustParse(t, "contains:@grounded:pelican").Eval(AgentWireResult{Output: "pelican"})
	if pass || !strings.Contains(reason, "unknown") {
		t.Errorf("Eval of a grounded check = (%v, %q), want a non-pass that says unknown", pass, reason)
	}
}

// TestGroundedEvidenceIsReadInFull pins how much of the evidence the grounded level reads: every match of a pattern, every
// context document, and every scalar kind a field can hold. Each case is one way a narrower reading would still pass the
// tables above (they use one document, one match and string fields), so each is a pass that only the full reading gives.
func TestGroundedEvidenceIsReadInFull(t *testing.T) {
	twoDocs := []ContextDoc{
		{Name: "a.md", Text: "nothing relevant is written here"},
		{Name: "b.md", Text: "the cache is named pelican"},
	}
	tests := []struct {
		name  string
		check string
		res   AgentWireResult
		docs  []ContextDoc
		want  Verdict
		why   string
	}{
		// a pattern with two matches: the grounded one may be later or earlier, and one ungrounded match does not sink it
		{"regex: only the second match is in a document", "regex:@grounded:[a-z]+", AgentWireResult{Output: "walrus pelican"}, authDocs, VerdictPass, ""},
		{"regex: only the first match is in a document", "regex:@grounded:[a-z]+", AgentWireResult{Output: "pelican walrus"}, authDocs, VerdictPass, ""},
		{"regex: an alternation matching one grounded and one not", "regex:@grounded:walrus|pelican", AgentWireResult{Output: "walrus pelican"}, authDocs, VerdictPass, ""},
		{"regex: no match is in a document", "regex:@grounded:walrus|otter", AgentWireResult{Output: "walrus otter"}, authDocs, VerdictFail, "none of the 1 context documents"},
		// a claim only the second of two documents carries
		{"contains: the text is only in the second document", "contains:@grounded:pelican", AgentWireResult{Output: "the cache is named pelican"}, twoDocs, VerdictPass, ""},
		{"contains: the text is in neither document", "contains:@grounded:walrus", AgentWireResult{Output: "walrus"}, twoDocs, VerdictFail, "none of the 2 context documents"},
		{"regex: the match is only in the second document", "regex:@grounded:pel[a-z]+", AgentWireResult{Output: "pelican"}, twoDocs, VerdictPass, ""},
		{"a blank first document does not hide the second", "contains:@grounded:pelican", AgentWireResult{Output: "pelican"}, []ContextDoc{{Name: "blank.md", Text: " \n"}, twoDocs[1]}, VerdictPass, ""},
		// booleans and numbers are field values, with their literal spelling
		{"field: a boolean true", "contains:@field=flag:true", AgentWireResult{Structured: json.RawMessage(`{"flag":true}`)}, nil, VerdictPass, ""},
		{"field: a boolean false", "contains:@field=flag:false", AgentWireResult{Structured: json.RawMessage(`{"flag":false}`)}, nil, VerdictPass, ""},
		{"field: a boolean is not the other one", "contains:@field=flag:false", AgentWireResult{Structured: json.RawMessage(`{"flag":true}`)}, nil, VerdictFail, "not found"},
		{"field: a number keeps its spelling", "contains:@field=ratio:0.50", AgentWireResult{Structured: json.RawMessage(`{"ratio":0.50}`)}, nil, VerdictPass, ""},
		{"grounded: a boolean in the structured answer is evidence", "contains:@grounded:true", AgentWireResult{Structured: json.RawMessage(`{"found":true}`)}, []ContextDoc{{Name: "d", Text: "it is true that the cache exists"}}, VerdictPass, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := mustParse(t, tt.check).Grade(tt.res, tt.docs)
			if o.Verdict != tt.want {
				t.Fatalf("verdict = %q (%s), want %q", o.Verdict, o.Reason, tt.want)
			}
			if tt.why != "" && !strings.Contains(o.Reason, tt.why) {
				t.Fatalf("reason %q does not contain %q", o.Reason, tt.why)
			}
		})
	}
}

// TestClosedAtIsTheWeakestCheck: a contract closes at the authority of its
// weakest check, an unqualified check pulls it to none, and anything that did
// not pass leaves it open.
func TestClosedAtIsTheWeakestCheck(t *testing.T) {
	o := func(auth Authority, v Verdict) AcceptanceOutcome {
		return AcceptanceOutcome{Authority: auth, Verdict: v}
	}
	tests := []struct {
		name string
		in   []AcceptanceOutcome
		want Authority
		ok   bool
	}{
		{"all grounded", []AcceptanceOutcome{o(AuthGrounded, VerdictPass), o(AuthGrounded, VerdictPass)}, AuthGrounded, true},
		{"grounded and answer closes at answer", []AcceptanceOutcome{o(AuthGrounded, VerdictPass), o(AuthAnswer, VerdictPass)}, AuthAnswer, true},
		{"grounded and field closes at field", []AcceptanceOutcome{o(AuthGrounded, VerdictPass), o(AuthField, VerdictPass)}, AuthField, true},
		{"an unqualified check pulls it to none", []AcceptanceOutcome{o(AuthGrounded, VerdictPass), o(AuthNone, VerdictPass)}, AuthNone, true},
		{"an unknown leaves it open", []AcceptanceOutcome{o(AuthGrounded, VerdictPass), o(AuthAnswer, VerdictUnknown)}, AuthNone, false},
		{"a fail leaves it open", []AcceptanceOutcome{o(AuthAnswer, VerdictFail)}, AuthNone, false},
		{"nothing graded is not closed", nil, AuthNone, false},
	}
	for _, tt := range tests {
		got, ok := ClosedAt(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: ClosedAt = (%v, %v), want (%v, %v)", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// TestUnqualifiedChecksAreUnchanged pins what an unqualified check does, empty
// evidence included, so the qualified form can never leak into it. In
// particular not_contains over an empty answer still passes: it is the vacuous
// pass the qualifier exists to avoid, kept here because changing it would change
// the verdict of contracts this repository cannot see.
func TestUnqualifiedChecksAreUnchanged(t *testing.T) {
	structured := json.RawMessage(`{"digest":"three findings","findings":["a","b","c"]}`)
	tests := []struct {
		name   string
		check  string
		res    AgentWireResult
		pass   bool
		reason string
	}{
		{"contains hit", "contains:pelican", AgentWireResult{Output: "a pelican"}, true, ""},
		{"contains miss", "contains:pelican", AgentWireResult{Output: "a walrus"}, false, `contains:pelican: "pelican" not found in output`},
		{"contains on empty fails", "contains:pelican", AgentWireResult{}, false, `contains:pelican: "pelican" not found in output`},
		{"contains falls back to structured bytes", "contains:findings", AgentWireResult{Structured: structured}, true, ""},
		{"contains falls back to the KEYS too (raw bytes)", "contains:digest", AgentWireResult{Structured: structured}, true, ""},
		{"not_contains hit", "not_contains:PLACEHOLDER", AgentWireResult{Output: "PLACEHOLDER later"}, false, `not_contains:PLACEHOLDER: forbidden "PLACEHOLDER" found in output`},
		{"not_contains clean", "not_contains:PLACEHOLDER", AgentWireResult{Output: "done"}, true, ""},
		{"not_contains passes vacuously on a fully empty result", "not_contains:PLACEHOLDER", AgentWireResult{}, true, ""},
		{"regex hit", "regex:pel[a-z]+", AgentWireResult{Output: "a pelican"}, true, ""},
		{"regex miss", "regex:pel[a-z]+", AgentWireResult{Output: "a walrus"}, false, "regex:pel[a-z]+: pattern did not match output"},
		{"regex on empty fails", "regex:pel[a-z]+", AgentWireResult{}, false, "regex:pel[a-z]+: pattern did not match output"},
		{"min_items on an absent field fails closed", "min_items:findings:1", AgentWireResult{Output: "x"}, false, "min_items:findings:1: no structured output to check"},
		{"nonempty on an absent field fails closed", "nonempty:digest", AgentWireResult{}, false, "nonempty:digest: no structured output to check"},
		// a literal that merely starts with @ is still a literal
		{"literal at-sign hit", "contains:@mention:x", AgentWireResult{Output: "ping @mention:x now"}, true, ""},
		{"literal at-sign miss", "contains:@mention:x", AgentWireResult{Output: "ping x now"}, false, `contains:@mention:x: "@mention:x" not found in output`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustParse(t, tt.check)
			if c.Authority != AuthNone {
				t.Fatalf("an unqualified check parsed with authority %v", c.Authority)
			}
			pass, reason := c.Eval(tt.res)
			if pass != tt.pass || reason != tt.reason {
				t.Fatalf("Eval = (%v, %q), want (%v, %q)", pass, reason, tt.pass, tt.reason)
			}
			// Grade of an unqualified check is Eval, and it is never unknown.
			o := c.Grade(tt.res, authDocs)
			wantV := VerdictFail
			if tt.pass {
				wantV = VerdictPass
			}
			if o.Verdict != wantV || o.Reason != tt.reason || o.Authority != AuthNone {
				t.Fatalf("Grade = %+v, want verdict %q reason %q authority none", o, wantV, tt.reason)
			}
		})
	}
}

// TestFieldAuthorityIsRequiredInTheSchema: a field-level check reads a named
// field, so AcceptedFields lists it and RequireAcceptanceFields makes the
// grammar produce it, the same as min_items: and nonempty: do.
func TestFieldAuthorityIsRequiredInTheSchema(t *testing.T) {
	got := AcceptedFields([]string{"contains:@field=summary:pelican", "contains:@answer:x", "nonempty:verdict", "contains:@field=summary:y"})
	if want := "summary,verdict"; strings.Join(got, ",") != want {
		t.Fatalf("AcceptedFields = %v, want %s", got, want)
	}
}

// TestQualifiedContractValidates: Validate admits a contract that carries a
// qualified check, and still refuses a malformed one with its index.
func TestQualifiedContractValidates(t *testing.T) {
	good := AgentContract{Goal: "g", Acceptance: []string{"contains:@grounded:pelican", "regex:@answer:a+", "not_contains:@answer:PLACEHOLDER"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("a qualified contract must validate: %v", err)
	}
	bad := AgentContract{Goal: "g", Acceptance: []string{"contains:x", "contains:@field=:y"}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "acceptance[1]") {
		t.Fatalf("a malformed qualifier must be refused with its index, got %v", err)
	}
}
