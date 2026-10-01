package pipeline

import (
	"context"
	"strings"
	"testing"
)

// A coerced object is not byte for byte what the seat wrote (register C-80: a JSON
// number became the string a string list asks for, and an item at a time inside a
// list), and nothing on the wire said so: it read as a clean success, with no why
// and no note. Every lane that coerces now says which fields it re-typed.
const coercedNumbersNote = "coerced to the schema's types: numbers (3 items)"

func TestCoerceToSchemaNotedNamesTheFieldsItRetyped(t *testing.T) {
	cases := []struct {
		name, schema, in, want string
	}{
		{"a list, by its item count", digestSchemaJSON, numbersAsJSONNumbers, "numbers (3 items)"},
		{"a scalar property, by name", `{"properties":{"id":{"type":"string"}}}`, `{"id":42}`, "id"},
		{"several, in name order", `{"properties":{"b":{"type":"integer"},"a":{"type":"boolean"},"s":{"type":"array","items":{"type":"number"}}}}`,
			`{"b":"7","a":"true","s":["1","2"]}`, "a, b, s (2 items)"},
		{"one item of a list", `{"properties":{"s":{"type":"array","items":{"type":"string"}}}}`, `{"s":["a",1]}`, "s (1 item)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, changed, ok := coerceToSchemaNoted([]byte(tc.in), mustSchemaMap(t, tc.schema))
			if !ok {
				t.Fatalf("coerceToSchemaNoted refused %s", tc.in)
			}
			if got := strings.Join(changed, ", "); got != tc.want {
				t.Fatalf("changed = %q, want %q", got, tc.want)
			}
		})
	}
}

// The grammar lane: attempt 1 answers the digest with numbers where strings go.
func TestRunAgentTaskGrammarLaneCoercionIsMarked(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("Pass rate 5, latency 12.50 ms, throughput 1e3 req/s.") },
		repack:    func(int64) string { return numbersAsJSONNumbers },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, digestContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 1 || !strings.Contains(d[0].Why, coercedNumbersNote) {
		t.Fatalf("detail = %+v, want the attempt that produced the object to say what was coerced", d)
	}
	if !strings.Contains(wire.RepackNote, coercedNumbersNote) {
		t.Fatalf("repack_note = %q, want %q", wire.RepackNote, coercedNumbersNote)
	}
}

// The chat lane: the incident's own shape, the grammar-free lane answering numbers.
func TestRunAgentTaskChatLaneCoercionIsMarked(t *testing.T) {
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
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || d[2].Lane != "chat" || !strings.Contains(d[2].Why, coercedNumbersNote) || d[0].Why == "" {
		t.Fatalf("detail = %+v, want the two failed grammar attempts and a chat attempt that says what it coerced", d)
	}
	if !strings.Contains(wire.RepackNote, coercedNumbersNote) {
		t.Fatalf("repack_note = %q, want %q", wire.RepackNote, coercedNumbersNote)
	}
}

// The loop's own answer needs no re-pack and is coerced too: no attempt to carry
// the note, so the note rides on repack_note.
func TestRunAgentTaskDirectAnswerCoercionIsMarked(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(numbersAsJSONNumbers) },
		repack:    func(int64) string { return `{"wrong":"shape"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, digestContract())))
	if wire.Deferred || fake.grammarCNT.Load() != 0 {
		t.Fatalf("deferred=%v grammar requests=%d, want the loop's own answer delivered with no re-pack", wire.Deferred, fake.grammarCNT.Load())
	}
	if !strings.Contains(wire.RepackNote, coercedNumbersNote) {
		t.Fatalf("repack_note = %q, want %q", wire.RepackNote, coercedNumbersNote)
	}
}

// An object that needed no coercion says nothing: the note is for the exception.
func TestRunAgentTaskAnObjectThatNeededNoCoercionCarriesNoNote(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("Pass rate 5.") },
		repack:    func(int64) string { return `{"key_facts":["a"],"numbers":["5"],"quotes":[],"verdict":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, digestContract())))
	d := wire.RepackAttemptsDetail
	if wire.Deferred || wire.RepackNote != "" || len(d) != 1 || d[0].Why != "" {
		t.Fatalf("deferred=%v repack_note=%q detail=%+v, want a clean success with no note and no why", wire.Deferred, wire.RepackNote, d)
	}
}
