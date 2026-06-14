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
