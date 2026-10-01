package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/validator"
)

// digestSchemaJSON is the shape of the research digest's output_schema: every
// list holds STRINGS, `numbers` included (register C-80: a seat that read the
// field name and answered JSON numbers failed the whole re-pack on
// "/numbers/0: got number, want string").
const digestSchemaJSON = `{"type":"object","properties":{` +
	`"key_facts":{"type":"array","items":{"type":"string"}},` +
	`"numbers":{"type":"array","items":{"type":"string"}},` +
	`"quotes":{"type":"array","items":{"type":"string"}},` +
	`"verdict":{"type":"string"}},` +
	`"required":["key_facts","numbers","quotes","verdict"]}`

// numbersAsJSONNumbers is attempt 3 of the incident: valid JSON, right keys,
// but `numbers` carries JSON numbers where the schema wants strings.
const numbersAsJSONNumbers = `{"key_facts":["the pass rate was measured"],"numbers":[5,12.50,1e3],"quotes":[],"verdict":"ok"}`

func mustSchemaMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture schema: %v", err)
	}
	return m
}

func digestContract() core.AgentContract {
	c := testContract()
	c.OutputSchema = json.RawMessage(digestSchemaJSON)
	return c
}

// TestCoerceToSchemaTurnsJSONNumbersIntoStringsKeepingTheirText: the incident's
// attempt-3 shape must coerce and validate. The literal text survives: 12.50 is
// the string "12.50", never "12.5", and 1e3 is "1e3".
func TestCoerceToSchemaTurnsJSONNumbersIntoStringsKeepingTheirText(t *testing.T) {
	schema := mustSchemaMap(t, digestSchemaJSON)
	if validator.Validate([]byte(numbersAsJSONNumbers), schema) == nil {
		t.Fatal("the fixture must fail validation before coercion, or the test proves nothing")
	}
	fixed, ok := coerceToSchema([]byte(numbersAsJSONNumbers), schema)
	if !ok {
		t.Fatalf("coerceToSchema refused the incident's shape: %s", numbersAsJSONNumbers)
	}
	if err := validator.Validate(fixed, schema); err != nil {
		t.Fatalf("the coerced object does not validate: %v\n%s", err, fixed)
	}
	var got struct {
		Numbers []string `json:"numbers"`
	}
	if err := json.Unmarshal(fixed, &got); err != nil {
		t.Fatalf("coerced object is not JSON: %v", err)
	}
	if strings.Join(got.Numbers, "|") != "5|12.50|1e3" {
		t.Fatalf("numbers = %q, want the literal texts [5 12.50 1e3]", got.Numbers)
	}
}

// A property whose schema type is string takes a bare number the same way, and
// the other direction (string to number, now inside an array too) keeps working.
func TestCoerceToSchemaProperties(t *testing.T) {
	cases := []struct {
		name, schema, in, want string
	}{
		{"number into a string property", `{"properties":{"id":{"type":"string"}}}`, `{"id":42}`, `{"id":"42"}`},
		{"large integer keeps every digit", `{"properties":{"id":{"type":"string"}}}`, `{"id":12345678901234567890}`, `{"id":"12345678901234567890"}`},
		{"string into number items", `{"properties":{"s":{"type":"array","items":{"type":"number"}}}}`, `{"s":["1","2.5"]}`, `{"s":[1,2.5]}`},
		{"string into integer property", `{"properties":{"n":{"type":"integer"}}}`, `{"n":"7"}`, `{"n":7}`},
		{"string into boolean items", `{"properties":{"b":{"type":"array","items":{"type":"boolean"}}}}`, `{"b":["true","False"]}`, `{"b":[true,false]}`},
		{"mixed items in one list", `{"properties":{"s":{"type":"array","items":{"type":"string"}}}}`, `{"s":["a",1,"b",2.50]}`, `{"s":["a","1","b","2.50"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixed, ok := coerceToSchema([]byte(tc.in), mustSchemaMap(t, tc.schema))
			if !ok {
				t.Fatalf("coerceToSchema refused %s", tc.in)
			}
			if string(fixed) != tc.want {
				t.Fatalf("coerced %s\n got %s\nwant %s", tc.in, fixed, tc.want)
			}
		})
	}
}

// An object nothing coerced is not rewritten, and a number the coercion did not
// touch comes back with every digit even when ANOTHER field forced a change: the
// old float64 round trip turned 12345678901234567890 into 12345678901234567000.
func TestCoerceToSchemaLeavesUntouchedNumbersAlone(t *testing.T) {
	schema := mustSchemaMap(t, `{"properties":{"id":{"type":"integer"},"flag":{"type":"boolean"}}}`)
	fixed, ok := coerceToSchema([]byte(`{"id":12345678901234567890,"flag":"true"}`), schema)
	if !ok {
		t.Fatal("coerceToSchema refused a boolean that needed coercing")
	}
	if !strings.Contains(string(fixed), `"id":12345678901234567890`) || !strings.Contains(string(fixed), `"flag":true`) {
		t.Fatalf("fixed = %s, want the integer's digits intact and the flag a boolean", fixed)
	}
}

// Coercion is lossless or it does not happen: nothing that is not a scalar of
// the wanted type's text is converted, and one item that cannot be converted
// leaves the whole object as it came (the validator still decides).
func TestCoerceToSchemaRefusesWhatItCannotConvertLosslessly(t *testing.T) {
	cases := []struct {
		name, schema, in string
	}{
		{"nothing to coerce", `{"properties":{"id":{"type":"string"}}}`, `{"id":"42"}`},
		{"a number for an untyped property", `{"properties":{"id":{}}}`, `{"id":42}`},
		{"a number for an object property", `{"properties":{"id":{"type":"object"}}}`, `{"id":42}`},
		{"a boolean for a string property", `{"properties":{"id":{"type":"string"}}}`, `{"id":true}`},
		{"null in a string list", `{"properties":{"s":{"type":"array","items":{"type":"string"}}}}`, `{"s":["a",null]}`},
		{"an object in a string list", `{"properties":{"s":{"type":"array","items":{"type":"string"}}}}`, `{"s":["a",{"x":1}]}`},
		{"a nested list is left alone", `{"properties":{"s":{"type":"array","items":{"type":"string"}}}}`, `{"s":[[1,2]]}`},
		{"text that is not a number", `{"properties":{"n":{"type":"number"}}}`, `{"n":"12 apples"}`},
		{"an exponent out of range", `{"properties":{"n":{"type":"number"}}}`, `{"n":"1e999"}`},
		{"one bad item sinks the list", `{"properties":{"s":{"type":"array","items":{"type":"number"}}}}`, `{"s":["1","two"]}`},
		{"trailing data after the object", `{"properties":{"id":{"type":"string"}}}`, `{"id":42} {"id":43}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fixed, ok := coerceToSchema([]byte(tc.in), mustSchemaMap(t, tc.schema)); ok {
				t.Fatalf("coerceToSchema converted %s into %s; it must refuse", tc.in, fixed)
			}
		})
	}
}

// The incident, end to end: the grammar lane answers a wrong shape twice, the
// chat lane answers the digest with JSON numbers in `numbers`, and the run
// delivers the object instead of deferring "got number, want string".
func TestRunAgentTaskChatLaneNumbersForStringsAreCoercedNotDeferred(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("Pass rate 5, latency 12.50 ms, throughput 1e3 req/s.") },
		repack:       func(int64) string { return `{"wrong":"shape"}` },
		chatFallback: func(int64) string { return doneChat(numbersAsJSONNumbers) },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, digestContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	if err := validator.Validate(wire.Structured, mustSchemaMap(t, digestSchemaJSON)); err != nil {
		t.Fatalf("structured does not validate: %v\n%s", err, wire.Structured)
	}
	if !strings.Contains(string(wire.Structured), `"numbers":["5","12.50","1e3"]`) {
		t.Fatalf("structured = %s, want the numbers as their literal texts", wire.Structured)
	}
	if fake.chatFallbackCNT.Load() != 1 {
		t.Fatalf("chat completions = %d, want the one that answered", fake.chatFallbackCNT.Load())
	}
}

// directStructured applies the same coercion: a loop whose own answer is the
// digest with numbers in `numbers` needs no re-pack at all.
func TestRunAgentTaskDirectAnswerWithNumbersForStringsNeedsNoRepack(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(numbersAsJSONNumbers) },
		repack:    func(int64) string { return `{"wrong":"shape"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, digestContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatalf("re-pack completions = %d, want 0: the loop's answer was already the object once its numbers are strings", fake.grammarCNT.Load())
	}
	if !strings.Contains(string(wire.Structured), `"numbers":["5","12.50","1e3"]`) {
		t.Fatalf("structured = %s", wire.Structured)
	}
}
