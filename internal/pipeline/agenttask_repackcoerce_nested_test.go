package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/gbnf/gbnftest"
	"github.com/dmmdea/offload-harness/internal/validator"
)

// F43 compiles a list of numbers, integers or booleans to a list of QUOTED scalars at every depth
// (the grammar's `stringarray`), on the promise that the re-pack's coercion turns them back into
// the schema's type. The coercion only read the top level, so a nested numeric list could never
// validate on a seat that obeys the grammar: every grammar attempt failed `got string, want
// number`, and the delegator's one-completion rescue could not deliver such a schema at all.

func TestCoerceToSchemaReadsEveryDepth(t *testing.T) {
	cases := []struct {
		name, schema, in, want, note string
	}{
		{"a number list in an object",
			`{"properties":{"meta":{"type":"object","properties":{"scores":{"type":"array","items":{"type":"number"}}}}}}`,
			`{"meta":{"scores":["1","2.50"]}}`, `{"meta":{"scores":[1,2.50]}}`, "meta.scores (2 items)"},
		{"a boolean list in an object",
			`{"properties":{"meta":{"type":"object","properties":{"flags":{"type":"array","items":{"type":"boolean"}}}}}}`,
			`{"meta":{"flags":["true","False"]}}`, `{"meta":{"flags":[true,false]}}`, "meta.flags (2 items)"},
		{"an integer list in every object of a list",
			`{"properties":{"rows":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"},"scores":{"type":"array","items":{"type":"integer"}}}}}}}`,
			`{"rows":[{"name":"a","scores":["1","2"]},{"name":"b","scores":["3"]}]}`, `{"rows":[{"name":"a","scores":[1,2]},{"name":"b","scores":[3]}]}`, "rows[].scores (3 items)"},
		{"scalars of an object two levels down",
			`{"properties":{"meta":{"type":"object","properties":{"owner":{"type":"object","properties":{"id":{"type":"integer"},"active":{"type":"boolean"}}}}}}}`,
			`{"meta":{"owner":{"id":"7","active":"true"}}}`, `{"meta":{"owner":{"active":true,"id":7}}}`, "meta.owner.active, meta.owner.id"},
		{"a list of lists of numbers",
			`{"properties":{"grid":{"type":"array","items":{"type":"array","items":{"type":"number"}}}}}`,
			`{"grid":[["1","2"],["3"]]}`, `{"grid":[[1,2],[3]]}`, "grid[] (3 items)"},
		{"a bare number for a string list at depth",
			`{"properties":{"meta":{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"}}}}}}`,
			`{"meta":{"ids":[5,"six",7.0]}}`, `{"meta":{"ids":["5","six","7.0"]}}`, "meta.ids (2 items)"},
		// The top level keeps its names: nothing but a dotted path is new.
		{"the top level is named as before",
			`{"properties":{"b":{"type":"integer"},"s":{"type":"array","items":{"type":"number"}}}}`,
			`{"b":"7","s":["1","2"]}`, `{"b":7,"s":[1,2]}`, "b, s (2 items)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixed, changed, ok := coerceToSchemaNoted([]byte(tc.in), mustSchemaMap(t, tc.schema))
			if !ok {
				t.Fatalf("coerceToSchemaNoted refused %s", tc.in)
			}
			if string(fixed) != tc.want {
				t.Fatalf("coerced %s\n got %s\nwant %s", tc.in, fixed, tc.want)
			}
			if got := strings.Join(changed, ", "); got != tc.note {
				t.Fatalf("changed = %q, want %q", got, tc.note)
			}
		})
	}
}

// Depth does not loosen the rule: lossless or not at all, and the whole object must validate.
func TestCoerceToSchemaRefusesAtDepthWhatItRefusesAtTheTop(t *testing.T) {
	meta := `{"properties":{"meta":{"type":"object","required":["scores"],"properties":{"scores":{"type":"array","items":{"type":"number"}}}}}}`
	cases := []struct{ name, schema, in string }{
		{"one bad item sinks the nested list", meta, `{"meta":{"scores":["1","two"]}}`},
		{"a missing nested field is not invented", meta, `{"meta":{"other":["1"]}}`},
		{"an exponent out of range", meta, `{"meta":{"scores":["1e999"]}}`},
		{"an object in a nested scalar list", meta, `{"meta":{"scores":[{"x":1}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fixed, ok := coerceToSchema([]byte(tc.in), mustSchemaMap(t, tc.schema)); ok {
				t.Fatalf("coerceToSchema converted %s into %s; it must refuse", tc.in, fixed)
			}
		})
	}
}

const nestedScalarListSchema = `{"type":"object","required":["title","meta"],"properties":{
  "title":{"type":"string"},
  "meta":{"type":"object","required":["scores","flags"],"properties":{
    "scores":{"type":"array","items":{"type":"number"}},
    "flags":{"type":"array","items":{"type":"boolean"}}}}}}`

const nestedScalarListAnswer = `{"title":"t","meta":{"scores":[1,2.5],"flags":[true,false]}}`

// What a seat that obeys the grammar F43 writes for the schema above can only write: the quoted lists.
const nestedScalarListQuoted = `{"title":"t","meta":{"scores":["1","2.5"],"flags":["true","false"]}}`

// The premise, shown and not assumed: the grammar the re-pack sends admits the quoted lists and
// refuses the real ones, so a seat held to it writes the quoted form.
func TestTheGrammarOfANestedNumericListQuotesIt(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(nestedScalarListSchema), &schema); err != nil {
		t.Fatal(err)
	}
	grammar := gbnf.Object(gbnf.FromJSONSchema(schema))
	for text, want := range map[string]bool{nestedScalarListQuoted: true, nestedScalarListAnswer: false} {
		got, err := gbnftest.Accepts(grammar, text)
		if err != nil {
			t.Fatalf("grammar not well formed: %v", err)
		}
		if got != want {
			t.Errorf("grammar admits %s = %v, want %v", text, got, want)
		}
	}
	if validator.Validate([]byte(nestedScalarListQuoted), schema) == nil {
		t.Fatal("the quoted form must fail the contract's schema, or the coercion has nothing to do")
	}
}

// End to end, on a seat that obeys the grammar: the first attempt delivers the real numbers and
// booleans, and says what it re-typed. Before the fix every attempt failed at '/meta/scores/0'.
func TestRepackOnALlamaCppSeatDeliversANestedNumericList(t *testing.T) {
	fake := groundedGrammarSeat(t, nestedScalarListAnswer, nestedScalarListQuoted)
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)

	structured, _, _, attempts, err := p.repackStructured(context.Background(), agentTestSeat, json.RawMessage(nestedScalarListSchema), "Scores 1 and 2.5; flags true and false.", 0)
	if err != nil {
		t.Fatalf("repackStructured on a llama.cpp seat with a nested numeric list: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: the coercion recovers the first grammar attempt", attempts)
	}
	var got, want any
	_ = json.Unmarshal(structured, &got)
	_ = json.Unmarshal([]byte(nestedScalarListAnswer), &want)
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("structured = %s, want %s", gb, wb)
	}
}

// The same through the run, so the delivered object carries the note that says it was re-typed.
func TestARunWithANestedNumericListSaysWhatItRetyped(t *testing.T) {
	fake := groundedGrammarSeat(t, nestedScalarListAnswer, nestedScalarListQuoted)
	fake.loop = func(int64) string { return doneChat("Scores 1 and 2.5; flags true and false.") }
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.OutputSchema = json.RawMessage(nestedScalarListSchema)

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	if want := "coerced to the schema's types: meta.flags (2 items), meta.scores (2 items)"; !strings.Contains(wire.RepackNote, want) {
		t.Fatalf("repack_note = %q, want it to contain %q", wire.RepackNote, want)
	}
	if !strings.Contains(string(wire.Structured), `"scores":[1,2.5]`) {
		t.Fatalf("structured = %s, want real numbers", wire.Structured)
	}
}
