// Package gbnf generates GBNF grammars that constrain llama.cpp output to a
// fixed JSON shape. We hand-roll generation (no external dep) because Gemma-4
// crashes on --json-schema/json_schema/response_format (llama.cpp #22396) and
// only a raw grammar is a safe path.
package gbnf

import (
	"fmt"
	"strings"
)

// FieldType enumerates the value kinds we constrain.
type FieldType int

const (
	TString FieldType = iota
	TNumber
	TInteger
	TBool
	TStringArray
	TEnum   // string limited to Enum values
	TObject // a nested object: its keys are Props, in order
	TArray  // an array whose every element is Items (an object, or another array)
)

// Field is one JSON object property, emitted in declaration order.
//
// A TObject field carries its own fields in Props and a TArray field the shape of
// its elements in Items (Items.Name is unused), so a schema of any depth is a tree
// of Fields. Both are what the strings used to stand in for: before they existed
// an object and a list of objects compiled to a string and a list of strings, and
// the seat was constrained into a shape the schema then refused (register F43,
// job-error class C12).
type Field struct {
	Name  string
	Type  FieldType
	Enum  []string // used when Type == TEnum
	Props []Field  // used when Type == TObject; empty means any JSON object
	Items *Field   // used when Type == TArray; nil means strings
}

// commonRules are the shared terminals. Written as a raw string so the GBNF
// escapes (\" etc.) are preserved verbatim.
//
// ws is the rule llama.cpp's own json.gbnf uses, not `[ \t\n]*`: empty, one space,
// or one newline and at most twenty spaces or tabs. An unbounded whitespace rule
// is a legal continuation forever, so a constrained model whose preferred next
// token the grammar masks can sample whitespace until max_tokens and the output is
// still "valid". That is the suspected shape of a structured re-pack that ran to
// its token cap twice on one request (register C-80); the request was not
// replayed, so it is unconfirmed, and the per-attempt head and tail the re-pack
// records will show whether whitespace was what the seat wrote. The bound uses
// GBNF's {m,n} repetition, the operator json.gbnf uses for this same rule; this
// grammar was not compiled against a live seat by the change that introduced it,
// so the first grammar request on one is the confirmation. Every user of Object
// shares this rule.
const commonRules = "\n" + `ws ::= | " " | "\n" [ \t]{0,20}
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

// genericRules admit any JSON value. They are written out only when a grammar
// holds an object the schema left open (a TObject with no Props), which has no keys
// to constrain and must still be an object, not a string.
const genericRules = `jvalue ::= jobject | jarray | string | number | boolean | "null"
jobject ::= "{" ws ( string ws ":" ws jvalue ( ws "," ws string ws ":" ws jvalue )* )? ws "}"
jarray ::= "[" ws ( jvalue ( ws "," ws jvalue )* )? ws "]"
`

// builder collects the rules a nested grammar needs beyond root and commonRules. A
// nested object or array gets a rule of its own, named by position (`obj-3`,
// `arr-4`) and never by the property name: llama.cpp's parser takes only letters,
// digits and dashes in a rule name, and a JSON key such as `key_facts` is not one.
// Rules are numbered in the order they finish, children before the rule that uses
// them, so the same fields always yield the same text.
type builder struct {
	n       int
	rules   []string
	generic bool
}

// add records `prefix-N ::= expr` and returns the name.
func (b *builder) add(prefix, expr string) string {
	b.n++
	name := fmt.Sprintf("%s-%d", prefix, b.n)
	b.rules = append(b.rules, name+" ::= "+expr)
	return name
}

// object is the production of a JSON object with the given fields, in order:
// every field required, no other key. It is a single line.
func (b *builder) object(fields []Field) string {
	parts := []string{`"{"`, "ws"}
	for i, f := range fields {
		if i > 0 {
			parts = append(parts, `ws "," ws`)
		}
		parts = append(parts, jkey(f.Name), `ws ":" ws`, b.value(f))
	}
	parts = append(parts, `ws "}"`)
	return strings.Join(parts, " ")
}

// value is what stands for one field's value inside an object or an array: a
// terminal's name or inline alternation for a scalar, the name of a rule of its
// own for an object or an array.
func (b *builder) value(f Field) string {
	switch f.Type {
	case TObject:
		if len(f.Props) == 0 {
			b.generic = true
			return "jobject"
		}
		return b.add("obj", b.object(f.Props))
	case TArray:
		item := Field{Type: TString}
		if f.Items != nil {
			item = *f.Items
		}
		v := b.value(item)
		return b.add("arr", `"[" ws ( `+v+` (ws "," ws `+v+`)* )? ws "]"`)
	}
	return typeRule(f)
}

// text is the rules after commonRules: the generic value rules when used, then the
// nested object and array rules. Empty for a flat object, so a flat grammar is
// byte for byte what it always was.
func (b *builder) text() string {
	var sb strings.Builder
	if b.generic {
		sb.WriteString(genericRules)
	}
	for _, r := range b.rules {
		sb.WriteString(r)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// Object returns a complete GBNF grammar for a JSON object with the given
// fields in order. All fields are required and emitted in declaration order, at
// every depth: a nested object admits its own keys, in order, and nothing else.
//
// The root production stays on ONE line (`root ::= ...`), which WrapThinking
// relies on; whatever a nested shape needs follows the common rules.
func Object(fields []Field) string {
	var b builder
	root := "root ::= " + b.object(fields)
	return root + "\n" + commonRules + b.text()
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

// JSONSchema is Object's counterpart for a vLLM seat: the same Fields
// rendered as the JSON Schema vLLM's `structured_outputs.json` takes, instead
// of the GBNF llama.cpp's `grammar` takes (register D-129 — vLLM accepts and
// DISCARDS `grammar`, so a vLLM seat behind llama-swap answered
// unconstrained). Every field is required and additionalProperties is false,
// which is exactly what Object constrains: the grammar admits the listed keys
// in declaration order and nothing else.
//
// `required` carries the fields in DECLARATION order, not sorted, because
// that list is the only place the order survives — a JSON object's keys have
// none, and orderedKeys reads `required` first when FromJSONSchema turns this
// back into Fields. So JSONSchema -> FromJSONSchema round-trips, order and
// all, and the two constraint expressions of one task cannot drift apart in
// which field comes first. A nested object is rendered the same way at its own
// level, so the order survives at every depth.
func JSONSchema(fields []Field) map[string]any {
	props := make(map[string]any, len(fields))
	required := make([]any, 0, len(fields))
	for _, f := range fields {
		props[f.Name] = schemaProp(f)
		required = append(required, f.Name)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

// schemaProp is the JSON Schema for ONE field — the inverse of typeRule.
func schemaProp(f Field) map[string]any {
	switch f.Type {
	case TNumber:
		return map[string]any{"type": "number"}
	case TInteger:
		return map[string]any{"type": "integer"}
	case TBool:
		return map[string]any{"type": "boolean"}
	case TStringArray:
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case TObject:
		if len(f.Props) == 0 {
			// the grammar's open object (genericRules): any object, no keys named.
			return map[string]any{"type": "object"}
		}
		return JSONSchema(f.Props)
	case TArray:
		items := map[string]any{"type": "string"}
		if f.Items != nil {
			items = schemaProp(*f.Items)
		}
		return map[string]any{"type": "array", "items": items}
	case TEnum:
		if len(f.Enum) == 0 {
			// typeRule degrades an empty enum to a plain string; keep the two
			// expressions of the same field saying the same thing.
			return map[string]any{"type": "string"}
		}
		vals := make([]any, 0, len(f.Enum))
		for _, e := range f.Enum {
			vals = append(vals, e)
		}
		return map[string]any{"type": "string", "enum": vals}
	}
	return map[string]any{"type": "string"}
}

// FromJSONSchema builds Fields from a minimal JSON-schema object (used by the
// dynamic `extract` task and the agent re-pack). It handles type:
// string/number/integer/boolean, string enums, nested objects (their own
// "properties", to any depth) and arrays of objects or of arrays. Property order
// follows the schema's "required" list if present, then the remaining properties
// by name; a nested object is ordered the same way at its own level.
//
// What it still reads as a plain string, and the grammar therefore forces to be
// one: a type it has no field kind for (a type list such as ["string","null"], an
// anyOf/oneOf/allOf/$ref schema), and the ITEMS of an array that are not objects or
// arrays. An array of strings, numbers, booleans or enum values compiles to an array
// of strings, as it always has: the re-pack's coercion turns the quoted scalars back
// into the schema's type. Every property a schema lists is required by the grammar,
// at every depth, whether or not the schema's own "required" names it.
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
		fields = append(fields, fieldOf(name, p))
	}
	return fields
}

// fieldOf is one property's Field: its kind, and for an object or an array the
// shape underneath it.
func fieldOf(name string, p map[string]any) Field {
	f := Field{Name: name, Type: schemaType(p), Enum: enumOf(p)}
	switch f.Type {
	case TObject:
		f.Props = FromJSONSchema(p)
	case TArray:
		items, _ := p["items"].(map[string]any)
		it := fieldOf("", items)
		f.Items = &it
	}
	return f
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
		if compositeItems(p) {
			return TArray
		}
		return TStringArray
	case "object":
		return TObject
	case "":
		// a nested schema that lists properties and names no type is an object
		if _, ok := p["properties"].(map[string]any); ok {
			return TObject
		}
	}
	return TString
}

// compositeItems reports whether an array schema's items are objects or arrays,
// the elements a list of strings cannot stand in for.
func compositeItems(p map[string]any) bool {
	items, _ := p["items"].(map[string]any)
	if items == nil {
		return false
	}
	switch s, _ := items["type"].(string); s {
	case "object", "array":
		return true
	case "":
		_, ok := items["properties"].(map[string]any)
		return ok
	}
	return false
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
