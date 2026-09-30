package tasks

import (
	"encoding/json"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
)

// Caps is what the seat a prompt is built for can do. The zero value is every seat that takes a
// grammar, and BuildFor(req, Caps{}) is byte-identical to Build(req).
type Caps struct {
	// Unconstrained: the seat's runtime cannot constrain decoding (the RKLLM runtime refuses any
	// grammar / json_schema / response_format with HTTP 400 and ignores logprobs). The JSON shape
	// the grammar would have forced is then stated in the prompt, and the reply is validated
	// strictly against Built.Strict instead.
	Unconstrained bool
}

// BuildFor is Build for a seat with the given capabilities. For a seat that takes a grammar the
// result is Build's, unchanged, byte for byte. For an unconstrained seat a task that carries a
// field list (classify, extract; summarize and triage also build one) comes back with no grammar,
// the exact shape in the system prompt, and Strict set.
func BuildFor(req core.Request, caps Caps) (Built, error) {
	b, err := Build(req)
	if err != nil {
		return Built{}, err
	}
	if caps.Unconstrained {
		b = b.ForUnconstrained()
	}
	return b, nil
}

// ForUnconstrained rewrites an already-built (and possibly exemplar-decorated or re-packed) task
// for a seat that cannot constrain decoding: Grammar is cleared, the shape instruction is appended
// to System only (User, and so any injected exemplars and the packed input, are untouched), and
// Strict is the schema every field of the grammar would have enforced — all keys required, their
// types, a classify label inside the allowed set, and no other key. A build with no field list
// (free-text tasks) is returned as it is.
func (b Built) ForUnconstrained() Built {
	if len(b.Fields) == 0 {
		return b
	}
	b.Grammar = ""
	b.System = strings.TrimRight(b.System, " \n") + "\n\n" + ShapeInstruction(b.Fields)
	b.Strict = gbnf.JSONSchema(b.Fields)
	return b
}

// ShapeInstruction states the exact JSON object a grammar would have forced: the keys in order,
// each one's type (an enum's allowed values), the rule that no other key may appear, and one
// compact example. It is deterministic, because it rides in the system prompt and so in the
// prompt-prefix fingerprint.
func ShapeInstruction(fields []gbnf.Field) string {
	var keys, ex []string
	for _, f := range fields {
		k := jsonString(f.Name)
		keys = append(keys, k+" ("+typeWords(f)+")")
		ex = append(ex, k+":"+exampleValue(f))
	}
	return "OUTPUT FORMAT: reply with exactly ONE JSON object and nothing else: no prose before or after it, no markdown fences. " +
		"It has exactly these keys, each once, in this order: " + strings.Join(keys, ", ") + ". " +
		"Do not add any other key. Example of the shape: {" + strings.Join(ex, ",") + "}"
}

func typeWords(f gbnf.Field) string {
	switch f.Type {
	case gbnf.TNumber:
		return "a number"
	case gbnf.TInteger:
		return "an integer"
	case gbnf.TBool:
		return "true or false"
	case gbnf.TStringArray:
		return "an array of strings"
	case gbnf.TEnum:
		if len(f.Enum) > 0 {
			vals := make([]string, len(f.Enum))
			for i, e := range f.Enum {
				vals[i] = jsonString(e)
			}
			return "a string, exactly one of " + strings.Join(vals, ", ")
		}
	}
	return "a string"
}

func exampleValue(f gbnf.Field) string {
	switch f.Type {
	case gbnf.TNumber:
		return "0.5"
	case gbnf.TInteger:
		return "1"
	case gbnf.TBool:
		return "true"
	case gbnf.TStringArray:
		return `["..."]`
	case gbnf.TEnum:
		if len(f.Enum) > 0 {
			return jsonString(f.Enum[0])
		}
	}
	return `"..."`
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
