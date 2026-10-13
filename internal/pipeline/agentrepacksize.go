package pipeline

import (
	"math"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// How much a structured re-pack can have to WRITE, from the schema it writes to.
//
// The skip decision (repackOpts.fit) asks whether the time left can buy the re-pack's output. It
// used to size that output from the ANSWER: about a token per three characters of it, because the
// re-pack was thought of as re-writing the answer as JSON. That is the cap, not the need, for a
// schema made of many short slots: a title, at most ten strings, a flag and a note bound what the
// extraction can emit whatever the answer's length. The recorded re-pack of a 7,126 character
// answer was skipped because its 2,439-token "size" was more than the 2,330 tokens the call's
// last 476 seconds buy at 4.9 tokens a second (a comparable re-pack that did run, six items on a
// faster seat, wrote 228 tokens). The finished answer was lost to a budget defer for a re-pack
// the schema bounds well under the time the call had left.
//
// It is the cap for a schema of one or two free-text fields. {answer: string} is the flat shape
// every remote agent_run carries, and its one string IS the answer restated; a digest schema's
// summary is most of it. Their output tracks the answer, so they keep the answer-based figure.
//
// repackSchemaTokens reads the bound off the schema, and repackLeast takes it when it is smaller
// than the answer-based figure. It never raises the figure, so a schema that bounds nothing (a list
// with no maxItems, an object that names no fields) or that is mostly one free field keeps the
// answer-based one.

const (
	// repackStringTokens is what a free string slot (no maxLength, no enum) is estimated at: about
	// 400 characters. CHOSEN, not measured: an extracted title, quote or note is shorter.
	repackStringTokens = 128
	// repackRestatingSlots is how many free string slots a schema may have and still be read as a
	// restating one: its strings are the answer in another shape, and each may hold all of it.
	// CHOSEN, not measured. Three or more slots are an extraction into parts.
	repackRestatingSlots = 2
	// repackScalarTokens is a boolean or a number: `false`, `-12345.678`.
	repackScalarTokens = 8
	// repackKeyTokens is what a key costs beside its name: the quotes, the colon and the comma.
	repackKeyTokens = 6
	// repackOutputSlack covers the braces, a fence the seat may add and the tokenizer's spread.
	repackOutputSlack = 16
	// repackUnbounded marks a schema that bounds the output by nothing.
	repackUnbounded = math.MaxInt
)

// specSize is the estimate for one value of a spec: its tokens (repackUnbounded when the spec
// leaves them open) and how many free string slots it holds (a list of n such strings holds n).
type specSize struct {
	tokens int
	slots  int
}

// repackSchemaTokens estimates the most tokens an object of this schema can take, or
// repackUnbounded when the schema leaves it open (a list without maxItems, an object that names no
// fields) or reads as a restating one (one or two free string slots: repackRestatingSlots).
func repackSchemaTokens(schema map[string]any) int {
	z := specTokens(schema, 0)
	if z.tokens == repackUnbounded || (z.slots > 0 && z.slots <= repackRestatingSlots) {
		return repackUnbounded
	}
	return satAdd(z.tokens, repackOutputSlack)
}

// specTokens sizes one value of the given spec. depth bounds the walk of a schema that nests
// deeper than any real one does.
func specTokens(spec map[string]any, depth int) specSize {
	open := specSize{tokens: repackUnbounded, slots: repackUnbounded}
	if depth > 8 {
		return open
	}
	switch typeName(spec) {
	case "boolean":
		return specSize{tokens: repackScalarTokens}
	case "integer", "number":
		return specSize{tokens: repackScalarTokens + 4}
	case "array":
		bound, ok := spec["maxItems"].(float64)
		if !ok || bound < 0 || bound > 1e6 {
			return open
		}
		items, _ := spec["items"].(map[string]any)
		item := specTokens(items, depth+1) // an item with no spec reads as a string, the grammar's reading
		if item.tokens == repackUnbounded {
			return open
		}
		return specSize{tokens: satAdd(2, satMul(int(bound), satAdd(item.tokens, 2))), slots: satMul(int(bound), item.slots)}
	case "object":
		props, _ := spec["properties"].(map[string]any)
		if len(props) == 0 {
			return open
		}
		total := specSize{tokens: 2}
		for name, raw := range props {
			sub, _ := raw.(map[string]any)
			v := specTokens(sub, depth+1)
			if v.tokens == repackUnbounded {
				return open
			}
			total.tokens = satAdd(total.tokens, satAdd(repackKeyTokens+len(name)/3, v.tokens))
			total.slots = satAdd(total.slots, v.slots)
		}
		return total
	}
	// A string. A declared bound is the bound; an enum is its longest value; otherwise a free slot.
	if n, ok := spec["maxLength"].(float64); ok && n >= 0 && n <= 1e6 {
		return specSize{tokens: int(n)/2 + 2}
	}
	if raw, ok := spec["enum"].([]any); ok && len(raw) > 0 {
		longest := 0
		for _, v := range raw {
			if s, isStr := v.(string); isStr && len(s) > longest {
				longest = len(s)
			}
		}
		return specSize{tokens: longest/2 + 2}
	}
	return specSize{tokens: repackStringTokens, slots: 1}
}

// repackLeast is the smallest an attempt may be sized to: what the re-pack has to write, which is
// the answer-based figure (expectedRepackTokens) unless the schema bounds it lower. needBySchema
// says it was the schema, for the note a skip carries.
func repackLeast(schema map[string]any, output string) (least int, needBySchema bool) {
	least = expectedRepackTokens(output)
	if schema == nil {
		return least, false
	}
	if bound := repackSchemaTokens(schema); bound < least {
		return bound, true
	}
	return least, false
}

// satAdd and satMul saturate at repackUnbounded instead of wrapping.
func satAdd(a, b int) int {
	if a == repackUnbounded || b == repackUnbounded || a > repackUnbounded-b {
		return repackUnbounded
	}
	return a + b
}

func satMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a == repackUnbounded || b == repackUnbounded || a > repackUnbounded/b {
		return repackUnbounded
	}
	return a * b
}

// repackOwedTokens is the time a re-pack of the given size in tokens is owed at the seat's decode
// rate, whatever the loop left of the wall: the tokens at that rate, padded by the factor the
// re-pack's own stall allowance pads its generation estimate with (agent.RepackMargin). 0 when no
// rate is known: the wall bound then does no arithmetic at all, so there is nothing to extend.
func repackOwedTokens(tokens int, tokS float64) time.Duration {
	if tokS <= 0 {
		return 0
	}
	return time.Duration(float64(tokens) / tokS * agent.RepackMargin * float64(time.Second))
}
