package pipeline

import (
	"fmt"
	"strings"
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
