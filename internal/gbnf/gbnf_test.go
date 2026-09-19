package gbnf

import (
	"strings"
	"testing"
)

func TestObjectEnumAndArray(t *testing.T) {
	g := Object([]Field{
		{Name: "summary", Type: TString},
		{Name: "bullets", Type: TStringArray},
	})
	for _, want := range []string{"root ::=", `"\"summary\""`, `"\"bullets\""`, "stringarray"} {
		if !strings.Contains(g, want) {
			t.Errorf("grammar missing %q\n%s", want, g)
		}
	}
}

func TestObjectEnum(t *testing.T) {
	g := Object([]Field{{Name: "decision", Type: TEnum, Enum: []string{"yes", "no", "unsure"}}})
	for _, want := range []string{`"\"yes\""`, `"\"no\""`, `"\"unsure\""`, "|"} {
		if !strings.Contains(g, want) {
			t.Errorf("enum grammar missing %q\n%s", want, g)
		}
	}
}

func TestFromJSONSchemaOrderAndTypes(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"company", "price_usd"},
		"properties": map[string]any{
			"company":   map[string]any{"type": "string"},
			"price_usd": map[string]any{"type": "integer"},
		},
	}
	f := FromJSONSchema(schema)
	if len(f) != 2 {
		t.Fatalf("want 2 fields, got %d", len(f))
	}
	if f[0].Name != "company" {
		t.Errorf("required order not honored: want company first, got %s", f[0].Name)
	}
	if f[1].Type != TInteger {
		t.Errorf("price_usd should be TInteger, got %v", f[1].Type)
	}
}

func TestWrapThinking(t *testing.T) {
	base := Object([]Field{{Name: "label", Type: TEnum, Enum: []string{"a", "b"}}})
	g := WrapThinking(base)
	// a <think>...</think> span is prepended to the root, with a `think` rule added
	for _, want := range []string{`"<think>"`, `"</think>"`, "think ::=", `"\"a\""`} {
		if !strings.Contains(g, want) {
			t.Errorf("think-wrapped grammar missing %q\n%s", want, g)
		}
	}
	// the original common rules survive so JSON terminals still resolve
	if !strings.Contains(g, "ws ::=") {
		t.Errorf("think-wrapped grammar lost common rules:\n%s", g)
	}
	// the root production must come AFTER the think span (think then JSON object)
	if !strings.Contains(g, `root ::= "<think>" think "</think>" ws "{"`) {
		t.Errorf("think span not prepended to root:\n%s", g)
	}
	// input that isn't a `root ::=` grammar passes through unchanged (safety guard)
	if got := WrapThinking("not a grammar"); got != "not a grammar" {
		t.Errorf("non-root input should pass through unchanged, got %q", got)
	}
}

// TestJSONSchemaRoundTripsThroughFromJSONSchema pins the invariant the two
// send sites depend on (register D-129): the JSON Schema a vLLM seat is
// constrained by and the GBNF a llama.cpp seat is constrained by are two
// renderings of ONE field list — same fields, same types, same ORDER. If they
// could drift, the same task would constrain differently per engine and the
// answers would stop being comparable.
func TestJSONSchemaRoundTripsThroughFromJSONSchema(t *testing.T) {
	// Every FieldType, and deliberately NOT in alphabetical order: sorted
	// order would hide an order bug, since orderedKeys falls back to sorting
	// whatever "required" does not name.
	fields := []Field{
		{Name: "zeta", Type: TString},
		{Name: "alpha", Type: TNumber},
		{Name: "middle", Type: TInteger},
		{Name: "flag", Type: TBool},
		{Name: "bullets", Type: TStringArray},
		{Name: "label", Type: TEnum, Enum: []string{"yes", "no", "unsure"}},
	}

	got := FromJSONSchema(JSONSchema(fields))
	if len(got) != len(fields) {
		t.Fatalf("round trip returned %d fields, want %d: %+v", len(got), len(fields), got)
	}
	for i, want := range fields {
		if got[i].Name != want.Name {
			t.Errorf("field %d: name %q, want %q — declaration order did not survive the round trip", i, got[i].Name, want.Name)
		}
		if got[i].Type != want.Type {
			t.Errorf("field %d (%s): type %v, want %v", i, want.Name, got[i].Type, want.Type)
		}
		if strings.Join(got[i].Enum, ",") != strings.Join(want.Enum, ",") {
			t.Errorf("field %d (%s): enum %v, want %v", i, want.Name, got[i].Enum, want.Enum)
		}
	}

	// ...and therefore the grammar the round trip produces is the grammar the
	// original fields produce: one field list, two engines.
	if a, b := Object(got), Object(fields); a != b {
		t.Errorf("Object(round trip) != Object(fields):\n got: %s\nwant: %s", a, b)
	}
}

// TestJSONSchemaShape pins the three structural claims the schema makes: an
// object, every field required, and no extra keys — the same closed shape
// Object's grammar admits.
func TestJSONSchemaShape(t *testing.T) {
	s := JSONSchema([]Field{
		{Name: "summary", Type: TString},
		{Name: "bullets", Type: TStringArray},
	})
	if s["type"] != "object" {
		t.Errorf("type = %v, want \"object\"", s["type"])
	}
	if s["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", s["additionalProperties"])
	}
	req, ok := s["required"].([]any)
	if !ok || len(req) != 2 || req[0] != "summary" || req[1] != "bullets" {
		t.Errorf("required = %v, want [summary bullets] in declaration order", s["required"])
	}
	props, ok := s["properties"].(map[string]any)
	if !ok || len(props) != 2 {
		t.Fatalf("properties = %v, want 2 entries", s["properties"])
	}
	arr, ok := props["bullets"].(map[string]any)
	if !ok || arr["type"] != "array" {
		t.Fatalf("bullets property = %v, want an array", props["bullets"])
	}
	items, ok := arr["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Errorf("bullets items = %v, want {\"type\":\"string\"}", arr["items"])
	}
}
