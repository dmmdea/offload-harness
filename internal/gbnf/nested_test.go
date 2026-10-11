package gbnf

import (
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gbnf/gbnftest"
)

// flatGrammarPin is the exact grammar the flat field kinds compile to. The nested
// shapes are built on top of it, and a change to the shapes that were already right
// (the extract lane, the classifiers, every flat output_schema) must show up here as
// a diff, not as a silent change to what every seat is constrained by.
const flatGrammarPin = `root ::= "{" ws "\"summary\"" ws ":" ws string ws "," ws "\"score\"" ws ":" ws number ws "," ws "\"count\"" ws ":" ws integer ws "," ws "\"ok\"" ws ":" ws boolean ws "," ws "\"bullets\"" ws ":" ws stringarray ws "," ws "\"label\"" ws ":" ws ("\"yes\"" | "\"no\"") ws "}"

ws ::= | " " | "\n" [ \t]{0,20}
string ::= "\"" ( [^"\\] | "\\" ["\\/bfnrt] | "\\u" [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] [0-9a-fA-F] )* "\""
number ::= "-"? ("0" | [1-9] [0-9]*) ("." [0-9]+)? ([eE] [-+]? [0-9]+)?
integer ::= "-"? ("0" | [1-9] [0-9]*)
boolean ::= "true" | "false"
stringarray ::= "[" ws ( string (ws "," ws string)* )? ws "]"
`

func TestFlatGrammarIsByteIdentical(t *testing.T) {
	got := Object([]Field{
		{Name: "summary", Type: TString},
		{Name: "score", Type: TNumber},
		{Name: "count", Type: TInteger},
		{Name: "ok", Type: TBool},
		{Name: "bullets", Type: TStringArray},
		{Name: "label", Type: TEnum, Enum: []string{"yes", "no"}},
	})
	if got != flatGrammarPin {
		t.Errorf("the flat grammar changed:\n got: %q\nwant: %q", got, flatGrammarPin)
	}
}

// nestedSchemaJSON is the shape register F43 / C12 is about: an object whose
// fields include an array of objects and an object, the object holding a list of
// strings and an object of its own. Keys are in declaration (required) order, one
// of them snake_case on purpose: a property name is not a legal GBNF rule name.
const nestedSchemaJSON = `{"type":"object","required":["title","labels","meta"],"properties":{
  "title":{"type":"string"},
  "labels":{"type":"array","items":{"type":"object","required":["name","score","kind"],"properties":{
    "name":{"type":"string"},"score":{"type":"number"},"kind":{"type":"string","enum":["a","b"]}}}},
  "meta":{"type":"object","required":["source","review_notes","owner"],"properties":{
    "source":{"type":"string"},
    "review_notes":{"type":"array","items":{"type":"string"}},
    "owner":{"type":"object","required":["id","active"],"properties":{"id":{"type":"integer"},"active":{"type":"boolean"}}}}}}}`

func mustSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func nestedGrammar(t *testing.T) string {
	t.Helper()
	return Object(FromJSONSchema(mustSchema(t, nestedSchemaJSON)))
}

const (
	testOwner = `{"id":7,"active":true}`
	testMeta  = `{"source":"s","review_notes":["n1","n2"],"owner":` + testOwner + `}`
)

// TestNestedSchemaGrammarAdmitsTheNestedAnswer is the defect as a behaviour test:
// the grammar built from a nested schema has to admit the object the schema
// describes and refuse the strings it used to force in their place. Before the fix
// an array of objects compiled to `stringarray` and an object to `string`, so a seat
// was constrained to write `"labels":["..."]` and `"meta":"..."`, which the
// schema's validator then rejected ("got string, want object") on every attempt.
func TestNestedSchemaGrammarAdmitsTheNestedAnswer(t *testing.T) {
	g := nestedGrammar(t)
	if err := gbnftest.Check(g); err != nil {
		t.Fatalf("the grammar is not well formed: %v\n%s", err, g)
	}
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"one label", `{"title":"t","labels":[{"name":"x","score":0.5,"kind":"a"}],"meta":` + testMeta + `}`, true},
		{"three labels", `{"title":"t","labels":[{"name":"x","score":1,"kind":"a"},{"name":"y","score":-2.5e3,"kind":"b"},{"name":"z","score":0,"kind":"a"}],"meta":` + testMeta + `}`, true},
		{"no labels", `{"title":"t","labels":[],"meta":` + testMeta + `}`, true},
		{"pretty printed", "{\n \"title\": \"t\",\n \"labels\": [ {\"name\": \"x\", \"score\": 1, \"kind\": \"b\"} ],\n \"meta\": " + testMeta + "\n}", true},
		{"no review notes", `{"title":"t","labels":[],"meta":{"source":"s","review_notes":[],"owner":` + testOwner + `}}`, true},

		// What the old grammar forced, and the schema rejects.
		{"labels as strings", `{"title":"t","labels":["x","y"],"meta":` + testMeta + `}`, false},
		{"meta as a string", `{"title":"t","labels":[],"meta":"s"}`, false},
		{"owner as a string", `{"title":"t","labels":[],"meta":{"source":"s","review_notes":[],"owner":"7"}}`, false},

		// Inside the nested shapes the grammar still constrains.
		{"label missing a key", `{"title":"t","labels":[{"name":"x","score":1}],"meta":` + testMeta + `}`, false},
		{"label with an extra key", `{"title":"t","labels":[{"name":"x","score":1,"kind":"a","extra":1}],"meta":` + testMeta + `}`, false},
		{"label keys out of order", `{"title":"t","labels":[{"score":1,"name":"x","kind":"a"}],"meta":` + testMeta + `}`, false},
		{"label score as a string", `{"title":"t","labels":[{"name":"x","score":"1","kind":"a"}],"meta":` + testMeta + `}`, false},
		{"label kind outside the enum", `{"title":"t","labels":[{"name":"x","score":1,"kind":"c"}],"meta":` + testMeta + `}`, false},
		{"owner id as a string", `{"title":"t","labels":[],"meta":{"source":"s","review_notes":[],"owner":{"id":"7","active":true}}}`, false},
		{"owner active as a string", `{"title":"t","labels":[],"meta":{"source":"s","review_notes":[],"owner":{"id":7,"active":"yes"}}}`, false},
		{"trailing comma in labels", `{"title":"t","labels":[{"name":"x","score":1,"kind":"a"},],"meta":` + testMeta + `}`, false},
		{"labels missing", `{"title":"t","meta":` + testMeta + `}`, false},
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
