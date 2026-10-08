package core

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAcceptedFieldsReadsOnlyTheFieldVerbs(t *testing.T) {
	got := AcceptedFields([]string{
		"regex:(?i)(?P<docanchor>alpha|beta)", // reads the text, not a field
		"min_items:key_facts:1",
		"contains:42",
		"nonempty:verdict",
		"min_items:key_facts:3", // the same field again: listed once
		"min_items:broken",      // does not parse: skipped, never a panic
		"diff_touches:internal/",
	})
	if want := []string{"key_facts", "verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AcceptedFields = %v, want %v", got, want)
	}
	if got := AcceptedFields(nil); got != nil {
		t.Fatalf("AcceptedFields(nil) = %v, want nil", got)
	}
}

// The any-of nonempty reads every field it names, so each is a field the schema
// must declare required; min_items takes a pipe in its field name literally.
func TestAcceptedFieldsSplitsTheNonemptyAlternation(t *testing.T) {
	got := AcceptedFields([]string{
		"nonempty:key_facts|numbers|quotes|verdict",
		"min_items:key_facts:2",  // already listed by the alternation: not repeated
		"nonempty:verdict|extra", // verdict is a repeat, extra is new
		"nonempty:a||b",          // does not parse: skipped, never a panic
		"min_items:left|right:1", // a literal field name
	})
	want := []string{"key_facts", "numbers", "quotes", "verdict", "extra", "left|right"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AcceptedFields = %v, want %v", got, want)
	}
}

// The any-of check declares each name it lists, presence only (a field the
// schema never declares is left out, as for every other check), and a schema
// that already requires them all comes back byte for byte: the default research
// digest, which carries this check, must not be reshaped by it.
func TestRequireAcceptanceFieldsDeclaresEachAlternativeAndIsANoOpWhenAllAreRequired(t *testing.T) {
	check := []string{"nonempty:key_facts|numbers|quotes|verdict"}

	allRequired := json.RawMessage(`{"type":"object","properties":{"key_facts":{"type":"array","items":{"type":"string"}},"numbers":{"type":"array","items":{"type":"string"}},"quotes":{"type":"array","items":{"type":"string"}},"verdict":{"type":"string"}},"required":["key_facts","numbers","quotes","verdict"]}`)
	if got := RequireAcceptanceFields(allRequired, check); !bytes.Equal(got, allRequired) {
		t.Fatalf("a schema that already requires every alternative was reshaped:\n got %s\nwant %s", got, allRequired)
	}

	partial := json.RawMessage(`{"type":"object","properties":{"key_facts":{"type":"array"},"verdict":{"type":"string"},"other":{"type":"string"}},"required":["other"]}`)
	got := requiredList(t, RequireAcceptanceFields(partial, check))
	if want := []string{"other", "key_facts", "verdict"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v (numbers and quotes are not declared, so they are not required)", got, want)
	}
}

func requiredList(t *testing.T, schema json.RawMessage) []string {
	t.Helper()
	var s struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("not JSON: %v: %s", err, schema)
	}
	return s.Required
}

// The caller's own required entries stay first (the gbnf builder reads
// `required` first for field order), the new ones follow in the order asked,
// and a field the schema never declares is never required.
func TestRequireFieldsKeepsTheCallersOrderAndSkipsUndeclaredFields(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"array"},"c":{"type":"string"}},"required":["b"]}`)
	got := requiredList(t, RequireFields(schema, []string{"c", "ghost", "a", "b"}))
	if want := []string{"b", "c", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
	// No required list at all: one is created.
	bare := json.RawMessage(`{"properties":{"x":{"type":"string"}}}`)
	if got := requiredList(t, RequireFields(bare, []string{"x"})); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("required = %v, want [x]", got)
	}
}

// Nothing to add, or nothing safe to rewrite: the input comes back byte for
// byte, so a contract that needed no change is not reshaped.
func TestRequireFieldsIsTheIdentityWhenThereIsNothingToAdd(t *testing.T) {
	cases := map[string]struct {
		schema string
		fields []string
	}{
		"no fields":        {`{"properties":{"a":{}},"required":["a"]}`, nil},
		"already required": {`{"properties":{"a":{}},"required":["a"]}`, []string{"a"}},
		"undeclared":       {`{"properties":{"a":{}}}`, []string{"ghost"}},
		"no properties":    {`{"type":"object"}`, []string{"a"}},
		"not an object":    {`[1,2]`, []string{"a"}},
		"malformed":        {`{"properties":`, []string{"a"}},
		"bad required":     {`{"properties":{"a":{}},"required":"a"}`, []string{"a"}},
		"empty properties": {`{"properties":{}}`, []string{"a"}},
	}
	for name, tc := range cases {
		got := RequireFields(json.RawMessage(tc.schema), tc.fields)
		if !bytes.Equal(got, []byte(tc.schema)) {
			t.Errorf("%s: schema changed: %s", name, got)
		}
	}
}

// The rest of the schema is carried through untouched: nested keywords, numbers
// and text that carries < > & (json.Marshal would rewrite those to \u003c).
func TestRequireFieldsCarriesTheRestOfTheSchemaThrough(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","description":"rows <b>&amp; more","properties":{` +
		`"rows":{"type":"array","items":{"type":"string","maxLength":18},"maxItems":6,"description":"a < b"}}}`)
	out := RequireFields(schema, []string{"rows"})
	for _, want := range []string{`"maxItems":6`, `"maxLength":18`, `"description":"rows <b>&amp; more"`, `"description":"a < b"`, `"required":["rows"]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %s:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), `\u003c`) {
		t.Errorf("HTML characters were escaped:\n%s", out)
	}
	// Still a schema the wire decode gate admits.
	if err := validateOutputSchema(out); err != nil {
		t.Fatalf("the augmented schema no longer compiles for the seat grammar: %v", err)
	}
}
