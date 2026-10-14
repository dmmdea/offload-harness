package mcpserver

// toolsprofile.go: the compact tools/list profile (ENH-13a, config key tools_schema_profile).
//
// A client that sends every MCP tool schema up front on every call (opencode has no deferred tool search) pays the whole
// advertised list as prompt tokens on every turn. Most of that list is prose: a description of several hundred tokens per
// tool and a sentence or three per parameter. "compact" keeps the structure a call needs and cuts the prose:
//
//   - every tool stays, in the same order, under the same name;
//   - every parameter keeps its name, type, enum, items, required entry and numeric bounds, at every depth, so a call that
//     is valid against the full list is valid against the compact one;
//   - a description (a tool's, and each parameter's) is cut to its first sentence plus every later sentence that carries a
//     safety word (safetyWords below): the rule that keeps "never pass third-party pages" and "ADVISORY ONLY" in the list;
//   - examples, example, title and $comment keywords are dropped.
//
// Only the advertised list is rewritten, by a receiving middleware on tools/list that is installed ONLY when the profile is
// compact, so the default configuration has no middleware and its list is byte-identical to the list before the key existed.
// The registered tools are never modified (the result's *mcp.Tool values alias the registry, so each is copied before it is
// changed), and the handlers and the call-time validation read the full schema. Anything the rewrite cannot parse is left as
// it was: the failure mode of this file is a full list, never a broken one.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// safetyWords marks a sentence the compact profile must keep, whatever its position in the description: a prohibition (never,
// do not, may not, forbidden, refuse, denied), an obligation (must), an inability a caller relies on (cannot, read-only), a trust
// boundary (trusted, untrusted, unattended, sandbox), a risk (destructive, irreversible, safety) or the advisory status of a
// result (advisory). A sentence is kept on one hit and kept whole.
//
// "only" is deliberately not on the list. It is plain description in a large share of the prose here ("the only candidate", "Only
// a fleet node whose largest card is at least this many GB"), and keeping every sentence that holds it was measured to cost 1,801
// tokens on the 31-tool list (19,674 become 21,475; see docs/systems/opencode-integration.md), enough to put the compact list back
// above the 0.137.0 figures it exists to beat. A real "only" prohibition is caught by its companion word ("TRUSTED CODE ONLY ...
// never pass", "ADVISORY ONLY"). TestCompactKeepsNamedSafetySentences pins named sentences and TestCompactNeverLosesASafetyWord
// counts the safety words of every advertised description, so narrowing this list shows up as a red test.
var safetyWords = regexp.MustCompile(`(?i)\b(never|do not|don't|must|may not|cannot|refus\w*|denied|trusted|untrusted|unattended|sandbox\w*|safety|advisory|read-only|forbidden|irreversible|destructive)\b`)

// compactLeadMin is the shortest first "sentence" the cut accepts: a description that opens "OPTIONAL." or "Required." has
// said nothing yet, so the next sentence is kept with it.
const compactLeadMin = 28

// The longest lead the cut keeps before it looks for a clause boundary to stop at: a tool's description, and a parameter's.
const (
	compactToolLead  = 320
	compactParamLead = 200
)

// abbreviationEnds are the lower-case tails that end a sentence-looking run without ending the sentence ("e.g. Foo").
var abbreviationEnds = []string{"e.g", "i.e", " vs", "etc", " cf", "approx", "incl", "resp"}

// splitSentences cuts prose into sentences at a '.', '!' or '?' (plus any closing quote or bracket) that is followed by
// white space and then a sentence-opening character, outside any bracket pair, and not after an abbreviation. Version numbers
// (0.137.0), file names, "section:\"brief\"" and "e.g. Foo" do not split. Text that never splits is one sentence, so an
// unbalanced bracket keeps more text, never less.
func splitSentences(s string) []string {
	var out []string
	rs := []rune(s)
	depth, start := 0, 0
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		case '.', '!', '?':
		default:
			continue
		}
		if depth > 0 {
			continue
		}
		j := i + 1
		for j < len(rs) && strings.ContainsRune(`"')]}*`, rs[j]) {
			j++
		}
		k := j
		for k < len(rs) && unicode.IsSpace(rs[k]) {
			k++
		}
		if k == j || k >= len(rs) || !sentenceOpener(rs[k]) {
			continue
		}
		if rs[i] == '.' && endsWithAbbreviation(string(rs[start:i])) {
			continue
		}
		out = append(out, strings.TrimSpace(string(rs[start:j])))
		start, i = k, k-1
	}
	if tail := strings.TrimSpace(string(rs[start:])); tail != "" {
		out = append(out, tail)
	}
	return out
}

// sentenceOpener reports whether r can begin a sentence. Lower-case letters count: the prose here opens sentences with
// identifiers ("results[].ran_on says ...", "context_paths inlines ..."), and treating those as continuations would glue most
// of a long description into one sentence that is kept whole.
func sentenceOpener(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("\"'(`[{<", r)
}

func endsWithAbbreviation(prefix string) bool {
	p := strings.ToLower(prefix)
	for _, a := range abbreviationEnds {
		if strings.HasSuffix(p, a) {
			return true
		}
	}
	return false
}

// compactDescription cuts prose to its first sentence (grown to compactLeadMin, shortened to maxLead at a clause boundary
// when the part it drops carries no safety word) plus every later sentence that carries a safety word. Prose of one sentence,
// or none, comes back as it was, unless that one sentence is over maxLead.
func compactDescription(s string, maxLead int) string {
	sents := splitSentences(s)
	if len(sents) == 0 {
		return s
	}
	var keep []string
	lead := 0
	for i, sent := range sents {
		switch {
		case lead < compactLeadMin:
			keep = append(keep, sent)
			lead += len(sent)
		case i > 0 && safetyWords.MatchString(sent):
			keep = append(keep, sent)
		}
	}
	keep[0] = shortenLead(keep[0], maxLead)
	return strings.Join(keep, " ")
}

// shortenLead cuts a long lead sentence at a top-level clause boundary: "; ", a spaced em dash, or the comma that follows a
// closing bracket (the end of one "name (explanation), name (explanation)" item). It takes the last boundary inside maxLead,
// else the first one inside twice that; the dropped tail must carry no safety word and a lead with no such boundary stays whole,
// so the cut can shorten the prose but never remove a safety clause or break a bracket.
func shortenLead(lead string, maxLead int) string {
	if maxLead <= 0 || len(lead) <= maxLead {
		return lead
	}
	rs := []rune(lead)
	depth, within, beyond := 0, -1, -1
	for i := 0; i < len(rs) && i <= 2*maxLead; i++ {
		switch rs[i] {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth > 0 {
			continue
		}
		cut := -1
		switch rs[i] {
		case ';':
			if i+1 < len(rs) && rs[i+1] == ' ' {
				cut = i
			}
		case '—': // em dash
			if i > 0 && rs[i-1] == ' ' {
				cut = i - 1
			}
		case ',':
			if i > 0 && strings.ContainsRune(")]}", rs[i-1]) && i+1 < len(rs) && rs[i+1] == ' ' {
				cut = i
			}
		}
		switch {
		case cut <= 0:
		case i <= maxLead:
			within = cut
		case beyond < 0:
			beyond = cut
		}
	}
	cut := within
	if cut <= 0 {
		cut = beyond
	}
	if cut <= 0 || safetyWords.MatchString(string(rs[cut:])) {
		return lead
	}
	return strings.TrimRight(string(rs[:cut]), " ,:") + "."
}

// --- order-preserving JSON, so a compacted schema lists its parameters in the order the full one did ---

type ordObj struct {
	keys []string
	vals map[string]any
}

func (o *ordObj) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *ordObj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshalNoEscape(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := marshalNoEscape(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// decodeOrdered reads one JSON value; objects become *ordObj, arrays []any, numbers json.Number (so a bound such as
// 262144 is written back digit for digit).
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		o := &ordObj{vals: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, io.ErrUnexpectedEOF
}

// droppedKeywords are schema keywords that carry no structure a call depends on.
var droppedKeywords = map[string]bool{"examples": true, "example": true, "title": true, "$comment": true}

// schemaMapKeywords hold a map of name -> schema; schemaKeywords hold one schema (or an array of them).
var (
	schemaMapKeywords = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}
	schemaKeywords    = map[string]bool{"items": true, "additionalProperties": true, "not": true, "if": true, "then": true, "else": true, "oneOf": true, "anyOf": true, "allOf": true, "prefixItems": true, "contains": true, "propertyNames": true}
)

// compactSchemaNode rewrites one schema node: its description is cut, droppedKeywords go, and schema-bearing keywords are
// walked. Every other keyword (type, enum, required, const, default, bounds, ...) is copied verbatim, and the VALUES of enum,
// const and default are never walked, so a property that is itself named "description" is never mistaken for the keyword.
func compactSchemaNode(v any) any {
	switch n := v.(type) {
	case *ordObj:
		out := &ordObj{vals: map[string]any{}}
		for _, k := range n.keys {
			val := n.vals[k]
			switch {
			case droppedKeywords[k]:
				continue
			case k == "description":
				if s, ok := val.(string); ok {
					out.set(k, compactDescription(s, compactParamLead))
					continue
				}
				out.set(k, val)
			case schemaMapKeywords[k]:
				if m, ok := val.(*ordObj); ok {
					cm := &ordObj{vals: map[string]any{}}
					for _, name := range m.keys {
						cm.set(name, compactSchemaNode(m.vals[name]))
					}
					out.set(k, cm)
					continue
				}
				out.set(k, val)
			case schemaKeywords[k]:
				out.set(k, compactSchemaNode(val))
			default:
				out.set(k, val)
			}
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, e := range n {
			out[i] = compactSchemaNode(e)
		}
		return out
	}
	return v
}

// compactInputSchema returns the compact form of a tool's input schema, or the schema untouched when it cannot be parsed.
func compactInputSchema(schema any) any {
	var raw []byte
	switch s := schema.(type) {
	case nil:
		return schema
	case json.RawMessage:
		raw = s
	case []byte:
		raw = s
	default:
		b, err := json.Marshal(s)
		if err != nil {
			return schema
		}
		raw = b
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tree, err := decodeOrdered(dec)
	if err != nil {
		return schema
	}
	b, err := marshalNoEscape(compactSchemaNode(tree))
	if err != nil {
		return schema
	}
	return json.RawMessage(b)
}

// compactTool returns a copy of t with its description and input schema compacted. t itself is the registry's value and is
// never modified.
func compactTool(t *mcp.Tool) *mcp.Tool {
	if t == nil {
		return nil
	}
	c := *t
	c.Description = compactDescription(t.Description, compactToolLead)
	c.InputSchema = compactInputSchema(t.InputSchema)
	return &c
}

// compactToolsMiddleware rewrites the result of every tools/list the server answers. It sits outside the handlers and never
// touches a tools/call.
func compactToolsMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lt, ok := res.(*mcp.ListToolsResult)
		if !ok || lt == nil {
			return res, err
		}
		out := *lt
		out.Tools = make([]*mcp.Tool, len(lt.Tools))
		for i, t := range lt.Tools {
			out.Tools[i] = compactTool(t)
		}
		return &out, nil
	}
}
