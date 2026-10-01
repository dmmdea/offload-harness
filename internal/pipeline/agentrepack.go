package pipeline

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// Helpers of the node-side structured re-pack (repackStructuredWith): what its
// prompts say about types, and, further down, how it is bounded and observed.

// repackTypeRule is the one sentence both re-pack prompts carry about types.
// The prompts used to list field NAMES only, and the chat prompt added "numbers
// unquoted": a seat that read the name of a `numbers` list answered bare JSON
// numbers where the schema wants strings, on every lane that has no grammar
// (register C-80). The rule says what each kind of field holds, so the schema's
// own types are what the seat reads.
const repackTypeRule = `Respect each field's type exactly: a string, or a list of strings, holds quoted text only, even when that text is a number (write "42", never 42); a number or integer field holds a bare JSON number; a boolean field holds true or false.`

// repackFieldList renders the fields of a schema for a re-pack prompt, one
// `"name" (type)` each, in the order given. A property with no usable type reads
// as a string, which is what the grammar compiles it to.
func repackFieldList(names []string, props map[string]any) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		spec, _ := props[name].(map[string]any)
		parts = append(parts, fmt.Sprintf("%q (%s)", name, fieldTypeText(spec)))
	}
	return strings.Join(parts, ", ")
}

// fieldTypeText spells one schema property's type for a prompt: "string",
// "number", "integer", "boolean", `string, one of "a", "b"` for an enum, and for a
// list the type of its items too ("array of strings"): the item type is exactly
// what a prompt that says only "array" leaves a seat to guess.
func fieldTypeText(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ == "array" {
		items, _ := spec["items"].(map[string]any)
		if len(items) == 0 {
			return "array"
		}
		return "array of " + pluralType(typeName(items)) + enumTail(items, ", each one of ")
	}
	return typeName(spec) + enumTail(spec, ", one of ")
}

// typeName is a property's declared scalar type, "string" when it names none.
func typeName(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ != "" {
		return typ
	}
	return "string"
}

// enumTail renders a property's string enum as `<lead>"a", "b"`, or "" when it has none.
func enumTail(spec map[string]any, lead string) string {
	raw, _ := spec["enum"].([]any)
	var quoted []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			quoted = append(quoted, fmt.Sprintf("%q", s))
		}
	}
	if len(quoted) == 0 {
		return ""
	}
	return lead + strings.Join(quoted, ", ")
}

func pluralType(name string) string {
	switch name {
	case "string", "number", "integer", "boolean", "object", "array":
		return name + "s"
	}
	return name
}

// repackOpts is what one structured re-pack call is handed beside its context.
// The zero value is the call every caller made before: no bound, no record.
type repackOpts struct {
	// Trace, when set, receives one record per attempt, for the wire.
	Trace *repackTrace
}

// repackTrace collects what each attempt of one re-pack call did.
type repackTrace struct {
	Attempts []core.AgentRepackAttempt
}

// repackClipBytes is how much of what an attempt wrote rides on the wire at each
// end: enough to see a whitespace tail, a loop or the shape of a prefix, never
// the answer.
const repackClipBytes = 80

// add records one attempt: what was asked (lane, max_tokens), what came back
// (tokens, finish reason, a clip of the content), how long it took and why it
// failed ("" when it produced the object). A nil trace records nothing.
func (tr *repackTrace) add(attemptNum int, lane string, maxTokens int, g llamaclient.GenResult, took time.Duration, why string) {
	if tr == nil {
		return
	}
	a := core.AgentRepackAttempt{
		Attempt: attemptNum, Lane: lane, MaxTokens: maxTokens,
		TokensOut: g.TokensOut, FinishReason: g.FinishReason, Ms: took.Milliseconds(), Why: why,
	}
	a.Head, a.Tail = clipEnds(g.Content)
	tr.Attempts = append(tr.Attempts, a)
}

// clipEnds returns the first and the last repackClipBytes bytes of s, each cut on
// a character boundary. Content that fits in both clips is returned whole as the
// head, with no tail, so a short answer is not shown twice.
func clipEnds(s string) (head, tail string) {
	if len(s) <= 2*repackClipBytes {
		return s, ""
	}
	h := repackClipBytes
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	t := len(s) - repackClipBytes
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h], s[t:]
}

// repackWhy is an error as one clipped line for an attempt's record: the
// validator's message spans lines, and the record is a note, not a log.
func repackWhy(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) <= 240 {
		return s
	}
	cut := 240
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
