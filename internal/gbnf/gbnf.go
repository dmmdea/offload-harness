// Package gbnf generates GBNF grammars that constrain llama.cpp output to a
// fixed JSON shape. We hand-roll generation (no external dep) because Gemma-4
// crashes on --json-schema/json_schema/response_format (llama.cpp #22396) and
// only a raw grammar is a safe path.
package gbnf

import "strings"

// FieldType enumerates the value kinds we constrain.
type FieldType int

const (
	TString FieldType = iota
	TNumber
	TInteger
	TBool
	TStringArray
	TEnum // string limited to Enum values
)

// Field is one JSON object property, emitted in declaration order.
type Field struct {
	Name string
	Type FieldType
	Enum []string // used when Type == TEnum
}

// commonRules are the shared terminals. Written as a raw string so the GBNF
// escapes (\" etc.) are preserved verbatim.
const commonRules = "\n" + `ws ::= [ \t\n]*
string ::= "\"" ( [^"\\] | "\\" ["\\/bfnrt] | "\\u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] )* "\""
number ::= "-"? ("0" | [1-9] [0-9]*) ("." [0-9]+)? ([eE] [-+]? [0-9]+)?
integer ::= "-"? ("0" | [1-9] [0-9]*)
boolean ::= "true" | "false"
stringarray ::= "[" ws ( string (ws "," ws string)* )? ws "]"
`

// jkey emits the GBNF literal that matches a quoted JSON key: "name" -> "\"name\"".
func jkey(name string) string { return `"\"` + name + `\""` }

func typeRule(f Field) string {
	switch f.Type {
	case TString:
		return "string"
	case TNumber:
		return "number"
	case TInteger:
		return "integer"
	case TBool:
		return "boolean"
	case TStringArray:
		return "stringarray"
	case TEnum:
		alts := make([]string, 0, len(f.Enum))
		for _, e := range f.Enum {
			alts = append(alts, jkey(e))
		}
		if len(alts) == 0 {
			return "string"
		}
		return "(" + strings.Join(alts, " | ") + ")"
	}
	return "string"
}

// Object returns a complete GBNF grammar for a JSON object with the given
// fields in order. All fields are required and emitted in declaration order.
func Object(fields []Field) string {
	parts := []string{`"{"`, "ws"}
	for i, f := range fields {
		if i > 0 {
			parts = append(parts, `ws "," ws`)
		}
		parts = append(parts, jkey(f.Name), `ws ":" ws`, typeRule(f))
	}
	parts = append(parts, `ws "}"`)
	root := "root ::= " + strings.Join(parts, " ")
	return root + "\n" + commonRules
}

// WrapThinking turns a grammar G (whose root is "root ::= <prod>") into a reasoning grammar
// "root ::= \"<think>\" think \"</think>\" ws <prod>" plus a `think` rule. A thinking model
// then reasons freely inside the <think> span and emits exactly G's structured output after
// it. `think` matches any run of NON-'<' characters, so the first '<' after the span must
// begin the literal </think> — reasoning can never derail on stray markup (e.g. </div>). The
// reasoning span itself therefore cannot contain a literal '<'; the JSON answer still can.
// Pair with a thinking-OFF server (the grammar — not the chat template — supplies the think
// tags, so the JSON lands in `content`) and StripThink() on the response. Input that is not a
// "root ::= " grammar is returned unchanged so the caller can fall back to the plain grammar.
func WrapThinking(grammar string) string {
	const thinkRule = "\nthink ::= [^<]*\n"
	nl := strings.IndexByte(grammar, '\n')
	if nl < 0 || !strings.HasPrefix(grammar, "root ::= ") {
		return grammar
	}
	prod := strings.TrimPrefix(grammar[:nl], "root ::= ")
	rest := grammar[nl:]
	return `root ::= "<think>" think "</think>" ws ` + prod + thinkRule + rest
}

// FromJSONSchema builds Fields from a minimal JSON-schema object (used by the
// dynamic `extract` task). It handles type: string/number/integer/boolean,
// arrays of strings, and string enums. Property order follows the schema's
// "required" list if present, else map iteration is sorted by required then name.
func FromJSONSchema(schema map[string]any) []Field {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return nil
	}
	order := orderedKeys(schema, props)
	fields := make([]Field, 0, len(order))
	for _, name := range order {
		p, _ := props[name].(map[string]any)
		if p == nil {
			continue
		}
		fields = append(fields, Field{Name: name, Type: schemaType(p), Enum: enumOf(p)})
	}
	return fields
}

func schemaType(p map[string]any) FieldType {
	if enumOf(p) != nil {
		return TEnum
	}
	switch s, _ := p["type"].(string); s {
	case "number":
		return TNumber
	case "integer":
		return TInteger
	case "boolean":
		return TBool
	case "array":
		return TStringArray
	default:
		return TString
	}
}

func enumOf(p map[string]any) []string {
	raw, ok := p["enum"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func orderedKeys(schema map[string]any, props map[string]any) []string {
	seen := map[string]bool{}
	var order []string
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				if _, exists := props[s]; exists && !seen[s] {
					order = append(order, s)
					seen[s] = true
				}
			}
		}
	}
	// Append any remaining props in stable sorted order.
	var rest []string
	for k := range props {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	return append(order, rest...)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
