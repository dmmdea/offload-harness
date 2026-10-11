package gbnf

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gbnf/gbnftest"
)

// The field tree FromJSONSchema reads out of a nested schema: objects carry their
// own fields in order, an array of objects carries the object as its element.
func TestFromJSONSchemaReadsNestedShapes(t *testing.T) {
	f := FromJSONSchema(mustSchema(t, nestedSchemaJSON))
	if len(f) != 3 || f[0].Name != "title" || f[1].Name != "labels" || f[2].Name != "meta" {
		t.Fatalf("top level = %+v, want title, labels, meta", f)
	}
	labels := f[1]
	if labels.Type != TArray || labels.Items == nil || labels.Items.Type != TObject {
		t.Fatalf("labels = %+v, want an array of objects", labels)
	}
	var names []string
	for _, p := range labels.Items.Props {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "name,score,kind" {
		t.Fatalf("label keys = %s, want name,score,kind in the schema's required order", got)
	}
	if k := labels.Items.Props[2]; k.Type != TEnum || strings.Join(k.Enum, ",") != "a,b" {
		t.Errorf("label kind = %+v, want an enum a,b", k)
	}
	meta := f[2]
	if meta.Type != TObject || len(meta.Props) != 3 {
		t.Fatalf("meta = %+v, want an object with three fields", meta)
	}
	if meta.Props[1].Name != "review_notes" || meta.Props[1].Type != TStringArray {
		t.Errorf("meta.review_notes = %+v, want an array of strings", meta.Props[1])
	}
	if owner := meta.Props[2]; owner.Type != TObject || len(owner.Props) != 2 || owner.Props[0].Type != TInteger || owner.Props[1].Type != TBool {
		t.Errorf("meta.owner = %+v, want an object {id integer, active boolean}", owner)
	}
}

// JSONSchema is the vLLM rendering of the same Fields, so a nested schema has to
// survive FromJSONSchema -> JSONSchema -> FromJSONSchema with its tree and its
// order, and both renderings must say the same thing about the shape.
func TestNestedJSONSchemaRoundTrips(t *testing.T) {
	fields := FromJSONSchema(mustSchema(t, nestedSchemaJSON))
	rendered := JSONSchema(fields)

	// the nested array is an array of closed objects, with required in order
	labels := rendered["properties"].(map[string]any)["labels"].(map[string]any)
	if labels["type"] != "array" {
		t.Fatalf("labels = %v, want an array", labels)
	}
	item := labels["items"].(map[string]any)
	if item["type"] != "object" || item["additionalProperties"] != false {
		t.Errorf("labels.items = %v, want a closed object", item)
	}
	if got := item["required"]; !reflect.DeepEqual(got, []any{"name", "score", "kind"}) {
		t.Errorf("labels.items.required = %v, want [name score kind]", got)
	}

	back := FromJSONSchema(rendered)
	if !reflect.DeepEqual(back, fields) {
		t.Errorf("round trip changed the field tree:\n got: %+v\nwant: %+v", back, fields)
	}
	if a, b := Object(back), Object(fields); a != b {
		t.Errorf("Object(round trip) != Object(fields):\n got: %s\nwant: %s", a, b)
	}

	// and it survives the wire: the same schema after a JSON encode and decode
	raw, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(FromJSONSchema(wire), fields) {
		t.Errorf("the schema changed on the wire:\n%s", raw)
	}
}

// An object the schema leaves open still has to be an object, not a string, and a
// list of such objects a list of objects. Likewise an array of arrays of objects,
// and an object nested only by "properties" with no "type" written.
func TestOpenAndDeeplyNestedShapes(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","required":["extra","rows","grid","loose"],"properties":{
	  "extra":{"type":"object"},
	  "rows":{"type":"array","items":{"type":"object"}},
	  "grid":{"type":"array","items":{"type":"array","items":{"type":"object","required":["v"],"properties":{"v":{"type":"integer"}}}}},
	  "loose":{"properties":{"k":{"type":"string"}}}}}`)
	fields := FromJSONSchema(schema)
	if len(fields) != 4 {
		t.Fatalf("fields = %+v, want four", fields)
	}
	if fields[0].Type != TObject || len(fields[0].Props) != 0 {
		t.Errorf("extra = %+v, want an open object", fields[0])
	}
	if fields[1].Type != TArray || fields[1].Items.Type != TObject {
		t.Errorf("rows = %+v, want an array of objects", fields[1])
	}
	if fields[2].Type != TArray || fields[2].Items.Type != TArray || fields[2].Items.Items.Type != TObject {
		t.Errorf("grid = %+v, want an array of arrays of objects", fields[2])
	}
	if fields[3].Type != TObject || len(fields[3].Props) != 1 {
		t.Errorf("loose = %+v, want an object with one field", fields[3])
	}

	g := Object(fields)
	if err := gbnftest.Check(g); err != nil {
		t.Fatalf("grammar not well formed: %v\n%s", err, g)
	}
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"all four", `{"extra":{"a":[1,{"b":null}],"c":"d"},"rows":[{"x":1},{}],"grid":[[{"v":1},{"v":2}],[]],"loose":{"k":"s"}}`, true},
		{"empty extra", `{"extra":{},"rows":[],"grid":[],"loose":{"k":""}}`, true},
		{"extra as a string", `{"extra":"x","rows":[],"grid":[],"loose":{"k":"s"}}`, false},
		{"rows as strings", `{"extra":{},"rows":["a"],"grid":[],"loose":{"k":"s"}}`, false},
		{"grid rows as strings", `{"extra":{},"rows":[],"grid":[["a"]],"loose":{"k":"s"}}`, false},
		{"grid cell of the wrong type", `{"extra":{},"rows":[],"grid":[[{"v":"1"}]],"loose":{"k":"s"}}`, false},
		{"loose as a string", `{"extra":{},"rows":[],"grid":[],"loose":"s"}`, false},
		{"open object cut short", `{"extra":{"a":},"rows":[],"grid":[],"loose":{"k":"s"}}`, false},
	}
	for _, c := range cases {
		got, err := gbnftest.Accepts(g, c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: grammar admits = %v, want %v\n  input: %s", c.name, got, c.want, c.in)
		}
	}
}

// Rules are named by position, never by property name: llama.cpp's parser takes
// letters, digits and dashes only in a rule name, so a key such as review_notes
// (in the fixture) used as a rule name would make the whole grammar unparseable.
func TestNestedRuleNamesAreLlamaCppWords(t *testing.T) {
	g := nestedGrammar(t)
	name := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	rules := 0
	for _, line := range strings.Split(g, "\n") {
		n, _, ok := strings.Cut(line, " ::= ")
		if !ok {
			continue
		}
		rules++
		if !name.MatchString(n) {
			t.Errorf("rule name %q is not letters, digits and dashes", n)
		}
	}
	if rules < 10 {
		t.Errorf("only %d rules found in:\n%s", rules, g)
	}
	// the generator is deterministic: same fields, same text
	if again := nestedGrammar(t); again != g {
		t.Errorf("two builds of the same fields differ")
	}
}

// WrapThinking splits on the first newline and reads the root production from it,
// so the root of a nested grammar has to stay on one line, and the nested rules must
// survive the wrap.
func TestNestedGrammarKeepsRootOnOneLineAndWrapsForThinking(t *testing.T) {
	g := nestedGrammar(t)
	first, _, _ := strings.Cut(g, "\n")
	if !strings.HasPrefix(first, "root ::= ") || !strings.HasSuffix(first, `ws "}"`) {
		t.Fatalf("the first line is not the whole root production: %q", first)
	}
	wrapped := WrapThinking(g)
	if err := gbnftest.Check(wrapped); err != nil {
		t.Fatalf("wrapped grammar not well formed: %v", err)
	}
	if !strings.Contains(wrapped, "obj-1 ::=") || !strings.Contains(wrapped, "arr-2 ::=") {
		t.Errorf("the nested rules were lost in the wrap:\n%s", wrapped)
	}
	const ans = `{"title":"t","labels":[{"name":"x","score":1,"kind":"a"}],"meta":` + testMeta + `}`
	for _, in := range []string{"<think>some reasoning</think>" + ans, "<think></think> " + ans} {
		if ok, err := gbnftest.Accepts(wrapped, in); err != nil || !ok {
			t.Errorf("wrapped grammar refuses %q: ok=%v err=%v", in, ok, err)
		}
	}
	if ok, _ := gbnftest.Accepts(wrapped, `<think>x</think>{"title":"t","labels":["x"],"meta":"s"}`); ok {
		t.Errorf("wrapped grammar admits the stringified shape")
	}
}

// Nested shapes share the one bounded ws rule and bring no unbounded whitespace
// class of their own (the same pin TestGrammarWhitespaceIsBounded holds for the
// flat grammar).
func TestNestedGrammarKeepsOneBoundedWhitespaceRule(t *testing.T) {
	g := nestedGrammar(t)
	var wsRules int
	for _, line := range strings.Split(g, "\n") {
		if strings.HasPrefix(line, "ws ::=") {
			wsRules++
		}
	}
	if wsRules != 1 {
		t.Errorf("want exactly one ws rule, got %d", wsRules)
	}
	for _, bad := range []string{`[ \t\n]*`, `[ \t]*`, `[ \t\n]+`} {
		if strings.Contains(g, bad) {
			t.Errorf("an unbounded whitespace class %s survives in the grammar", bad)
		}
	}
}

// The flat kinds inside a nested object compile exactly as they do at the top
// level: a list of numbers is still the list of strings the re-pack coerces back.
func TestFlatKindsInsideNestedObjectsKeepTheirOldReading(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","required":["o"],"properties":{"o":{"type":"object","required":["nums","flags"],"properties":{
	  "nums":{"type":"array","items":{"type":"number"}},"flags":{"type":"array","items":{"type":"boolean"}}}}}}`)
	f := FromJSONSchema(schema)
	if len(f) != 1 || len(f[0].Props) != 2 {
		t.Fatalf("fields = %+v, want one object with two fields", f)
	}
	if f[0].Props[0].Type != TStringArray || f[0].Props[1].Type != TStringArray {
		t.Errorf("arrays of scalars changed reading: %+v", f[0].Props)
	}
}
