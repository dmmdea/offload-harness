package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Authority-graded acceptance (ENH-13b).
//
// A check says WHAT must hold; an authority says how much the evidence behind
// it may be trusted. The idea is the one reverse-engineering tools use for a
// finding: evidence carries an authority level, a claim that needs a stronger
// level is not closed by a weaker one, and ABSENT evidence is "unknown", never a
// pass. The DSL verb set is closed (reviewedAcceptanceVerbs in the root package
// pins it), so the authority is not a verb: it is an optional qualifier at the
// head of the argument of contains:, regex: and not_contains:.
//
//	contains:@answer:<s>          <s> is in the answer text the model wrote (output)
//	contains:@field=<name>:<s>    <s> is in the value of structured field <name>
//	contains:@grounded:<s>        <s> is in the answer AND verbatim in a context document
//	regex:@answer:<re>            the same three levels over a Go regexp; at the
//	regex:@field=<name>:<re>      grounded level the TEXT it matched must be in a
//	regex:@grounded:<re>          context document
//	not_contains:@answer:<s>      <s> is absent from the answer text
//	not_contains:@field=<name>:<s> <s> is absent from structured field <name>
//
// The qualifier lives in the argument tail so the verb spelling is unchanged: a
// node one release behind parses the line as a plain substring (a node only
// PARSES acceptance when it ACKs a contract and never evaluates it), and an older
// delegator reads the same line as a literal that no answer carries, so it fails
// closed. An unqualified check is untouched by any of this.
//
// The levels are ordered, weakest first:
//
//	AuthNone     no authority claimed (an unqualified check; today's behaviour)
//	AuthAnswer   what the model wrote. Nothing but the model vouches for it.
//	AuthField    a value in the structured result. The contract's own schema
//	             shaped the slot, so it is a commitment to a requested shape.
//	AuthGrounded text the answer carries that the delegator's own context
//	             documents also carry. Corroborated mention, not proof of truth.
//
// Each level reads exactly the evidence it names. A weaker level's evidence never
// closes a stronger level's claim: an answer that says a thing closes the answer
// claim and not the field claim (no field carries it) or the grounded claim (no
// document vouches for it). An evidence set that is empty or absent grades as
// UNKNOWN, which is not a pass: the merge gate counts it as a failed check and
// the reason says unknown, so an empty answer is told apart from a wrong one.
//
// Unqualified not_contains over an empty answer still passes. That is the vacuous
// pass this qualifier exists to avoid, and it is kept for unqualified checks
// because a write-door leg and a schema-only result legitimately reach
// acceptance with an empty output, and the documented contracts that pair
// not_contains with a write-set verb would otherwise stop passing. A contract
// that cannot afford the vacuous pass writes the qualified form.

// Authority is the strength of the evidence a check reads, ordered weakest
// first. The zero value is "no authority claimed".
type Authority int

const (
	AuthNone Authority = iota
	AuthAnswer
	AuthField
	AuthGrounded
)

// String is the name the qualifier and the reasons use.
func (a Authority) String() string {
	switch a {
	case AuthNone:
		return "none"
	case AuthAnswer:
		return "answer"
	case AuthField:
		return "field"
	case AuthGrounded:
		return "grounded"
	}
	return "authority(" + strconv.Itoa(int(a)) + ")"
}

// AtLeast reports whether a is as strong as b or stronger.
func (a Authority) AtLeast(b Authority) bool { return a >= b }

// Verdict is the three-valued result of a graded check. Unknown is not a pass.
type Verdict string

const (
	VerdictPass    Verdict = "pass"
	VerdictFail    Verdict = "fail"
	VerdictUnknown Verdict = "unknown"
)

// AcceptanceOutcome is one graded check. Reason is empty exactly when Verdict is
// VerdictPass and always opens with the check itself, because failures are
// classified by that prefix.
type AcceptanceOutcome struct {
	Check     string    // the DSL string as written
	Verdict   Verdict   // pass, fail or unknown
	Authority Authority // the level the check demanded; AuthNone for an unqualified check
	Reason    string
}

// ClosedAt is the authority a set of graded checks closes at: the weakest
// authority among them, and only when every one passed. An unqualified check
// counts as AuthNone, so a single one pulls the whole set down to none; a fail, an
// unknown, or an empty set leaves it open (ok false). It is how a caller states
// "this contract was closed at grounded authority" and has the claim refused
// when its weakest check was only an answer.
func ClosedAt(outcomes []AcceptanceOutcome) (closed Authority, ok bool) {
	if len(outcomes) == 0 {
		return AuthNone, false
	}
	closed = AuthGrounded
	for _, o := range outcomes {
		if o.Verdict != VerdictPass {
			return AuthNone, false
		}
		if o.Authority < closed {
			closed = o.Authority
		}
	}
	return closed, true
}

const (
	qualAnswer   = "@answer:"
	qualGrounded = "@grounded:"
	qualField    = "@field"
)

// splitAuthority strips an authority qualifier off the head of a check's
// argument. It only reads the three reserved spellings, on the three verbs that
// take a qualifier; anything else (an argument that merely starts with "@", a
// verb that reads a field name or a path) comes back untouched, so every string
// that parsed before parses to the same check.
func splitAuthority(verb AcceptanceKind, rest string) (tail string, auth Authority, field string, err error) {
	if !strings.HasPrefix(rest, "@") {
		return rest, AuthNone, "", nil
	}
	switch verb {
	case AccContains, AccRegex, AccNotContains:
	default:
		return rest, AuthNone, "", nil
	}
	switch {
	case strings.HasPrefix(rest, qualAnswer):
		tail, auth = rest[len(qualAnswer):], AuthAnswer
	case strings.HasPrefix(rest, qualGrounded):
		tail, auth = rest[len(qualGrounded):], AuthGrounded
	case strings.HasPrefix(rest, qualField+"=") || strings.HasPrefix(rest, qualField+":"):
		name, after, ok := strings.Cut(rest[len(qualField)+1:], ":")
		if rest[len(qualField)] != '=' || !ok || name == "" {
			return "", AuthNone, "", fmt.Errorf("the field qualifier is @field=<name>:<%s> (a structured field to read)", verbArgWord(verb))
		}
		tail, auth, field = after, AuthField, name
	default:
		return rest, AuthNone, "", nil
	}
	if verb == AccNotContains && auth == AuthGrounded {
		return "", AuthNone, "", fmt.Errorf("not_contains cannot be grounded (the context documents cannot corroborate an absence); use @answer: or @field=<name>:")
	}
	return tail, auth, field, nil
}

func verbArgWord(verb AcceptanceKind) string {
	if verb == AccRegex {
		return "re"
	}
	return "s"
}

// Grade runs the check against a result and the contract's context documents
// and returns pass, fail or unknown. An unqualified check is graded exactly as
// Eval grades it (so it is never unknown). A qualified check reads only the
// evidence its level names, and an empty or absent evidence set is unknown.
// docs is only read at the grounded level; Eval passes none, so a grounded
// check can never be closed by it.
func (c AcceptanceCheck) Grade(res AgentWireResult, docs []ContextDoc) AcceptanceOutcome {
	if c.Authority == AuthNone {
		pass, reason := c.Eval(res)
		v := VerdictFail
		if pass {
			v = VerdictPass
		}
		return AcceptanceOutcome{Check: c.raw, Verdict: v, Authority: AuthNone, Reason: reason}
	}
	return c.gradeQualified(res, docs)
}

func (c AcceptanceCheck) gradeQualified(res AgentWireResult, docs []ContextDoc) AcceptanceOutcome {
	out := AcceptanceOutcome{Check: c.raw, Authority: c.Authority}
	unknown := func(why string) AcceptanceOutcome {
		out.Verdict = VerdictUnknown
		out.Reason = fmt.Sprintf("%s: unknown at %s authority: %s", c.raw, c.Authority, why)
		return out
	}
	fail := func(format string, a ...any) AcceptanceOutcome {
		out.Verdict = VerdictFail
		out.Reason = c.raw + ": " + fmt.Sprintf(format, a...)
		return out
	}
	pass := func() AcceptanceOutcome { out.Verdict = VerdictPass; return out }

	var segs, docTexts []string
	var where string
	switch c.Authority {
	case AuthAnswer:
		if strings.TrimSpace(res.Output) == "" {
			return unknown("the answer text is empty, so there is no evidence either way")
		}
		segs, where = []string{res.Output}, "the answer text"
	case AuthField:
		texts, why := fieldTexts(res.Structured, c.field)
		if why != "" {
			return unknown(why)
		}
		segs, where = texts, fmt.Sprintf("structured field %q", c.field)
	case AuthGrounded:
		segs, where = answerTexts(res), "the answer"
		if len(segs) == 0 {
			return unknown("the answer is empty (no answer text and no structured field text), so there is no evidence either way")
		}
		for _, d := range docs {
			if strings.TrimSpace(d.Text) != "" {
				docTexts = append(docTexts, d.Text)
			}
		}
		if len(docTexts) == 0 {
			return unknown("the contract carries no context documents to verify against")
		}
	default:
		return fail("authority %v is not a level a check can demand", c.Authority)
	}

	switch c.Kind {
	case AccContains:
		if !anyContains(segs, c.Arg) {
			return fail("%q not found in %s", c.Arg, where)
		}
		if c.Authority == AuthGrounded && !anyContains(docTexts, c.Arg) {
			return fail("%q is in the answer but in none of the %d context documents (not verified)", c.Arg, len(docTexts))
		}
		return pass()
	case AccNotContains:
		if anyContains(segs, c.Arg) {
			return fail("forbidden %q found in %s", c.Arg, where)
		}
		return pass()
	case AccRegex:
		if c.re == nil {
			return fail("the pattern was not compiled (not built by ParseAcceptanceCheck?)")
		}
		matched := false
		for _, seg := range segs {
			if c.Authority != AuthGrounded {
				if c.re.MatchString(seg) {
					return pass()
				}
				continue
			}
			for _, m := range c.re.FindAllString(seg, -1) {
				if m == "" {
					continue // an empty match carries no evidence
				}
				matched = true
				if anyContains(docTexts, m) {
					return pass()
				}
			}
		}
		if matched {
			return fail("the pattern matched in the answer, but the matched text is in none of the %d context documents (not verified)", len(docTexts))
		}
		return fail("pattern did not match %s", where)
	}
	return fail("a %s check does not take an authority", c.Kind)
}

func anyContains(texts []string, sub string) bool {
	for _, t := range texts {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

// fieldTexts is the evidence set of a field-level check: the text of every
// scalar in the named top-level field (string values as written, numbers and
// booleans as their literal; keys and nulls are not evidence). A reason comes
// back instead when there is no such evidence: no structured result, a result
// that is not an object, an absent field, or a field that is empty by the
// emptiness rules nonempty: uses.
func fieldTexts(structured json.RawMessage, name string) (texts []string, why string) {
	if len(bytes.TrimSpace(structured)) == 0 {
		return nil, fmt.Sprintf("there is no structured output to read field %q from", name)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(structured, &obj); err != nil {
		return nil, "the structured output is not a JSON object"
	}
	field, ok := obj[name]
	if !ok {
		return nil, fmt.Sprintf("field %q is absent from the structured output", name)
	}
	if empty := emptiness(field); empty != "" {
		return nil, fmt.Sprintf("field %q is %s", name, empty)
	}
	texts = scalarTexts(field)
	if len(texts) == 0 {
		return nil, fmt.Sprintf("field %q holds no text", name)
	}
	return texts, ""
}

// answerTexts is the evidence set of a grounded check: the answer text, then the
// scalars of the structured result (an answer that arrived only as a schema
// object is still an answer). Empty when the result carries neither.
func answerTexts(res AgentWireResult) []string {
	var out []string
	if strings.TrimSpace(res.Output) != "" {
		out = append(out, res.Output)
	}
	if len(bytes.TrimSpace(res.Structured)) > 0 {
		out = append(out, scalarTexts(res.Structured)...)
	}
	return out
}

// scalarTexts flattens a JSON value into the text of its scalars, object keys in
// sorted order so the result is deterministic. Blank strings and nulls are
// skipped; numbers keep their literal spelling.
func scalarTexts(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		case json.Number:
			out = append(out, t.String())
		case bool:
			out = append(out, strconv.FormatBool(t))
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(v)
	return out
}
