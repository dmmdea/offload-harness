package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gbnf/gbnftest"
)

// nestedRepackSchema is a contract schema that nests: an array of objects, and an
// object holding a list of strings and an object of its own (register F43 / C12:
// about twenty jobs in 48 hours deferred "output failed schema ... got string, want
// object" on the llama.cpp seats).
const nestedRepackSchema = `{"type":"object","required":["title","labels","meta"],"properties":{
  "title":{"type":"string"},
  "labels":{"type":"array","items":{"type":"object","required":["name","score","kind"],"properties":{
    "name":{"type":"string"},"score":{"type":"number"},"kind":{"type":"string","enum":["a","b"]}}}},
  "meta":{"type":"object","required":["source","review_notes","owner"],"properties":{
    "source":{"type":"string"},
    "review_notes":{"type":"array","items":{"type":"string"}},
    "owner":{"type":"object","required":["id","active"],"properties":{"id":{"type":"integer"},"active":{"type":"boolean"}}}}}}}`

const nestedRepackAnswer = `{"title":"t","labels":[{"name":"x","score":0.5,"kind":"a"},{"name":"y","score":2,"kind":"b"}],"meta":{"source":"s","review_notes":["n1"],"owner":{"id":7,"active":true}}}`

// nestedRepackStringified is what a seat held to the OLD grammar had to write: an
// array of objects compiled to a list of strings and an object to a string.
const nestedRepackStringified = `{"title":"t","labels":["x","y"],"meta":"s"}`

// groundedGrammarSeat is a llama.cpp seat that honours the grammar it is sent: it
// writes the answer it was scripted with when that text is one the grammar admits,
// and what the grammar forces instead when it is not. That is the whole property of
// a constrained decoder this lane relies on, and the one a fake that ignores
// `grammar` cannot show: a canned valid answer passes the validator whatever the
// grammar said, which is how the defect stayed green.
func groundedGrammarSeat(t *testing.T, scripted, forced string) *agentFake {
	t.Helper()
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			grammar, _ := body["grammar"].(string)
			out := scripted
			admitted, err := gbnftest.Accepts(grammar, scripted)
			if err != nil {
				t.Errorf("the re-pack sent a grammar that is not well formed: %v\n%s", err, grammar)
			}
			if !admitted {
				out = forced
			}
			content, _ := json.Marshal(out)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		},
	}
}

// TestRepackOnALlamaCppSeatConstrainsANestedSchemaToItsOwnShape drives the
// node-side re-pack with the nested schema above against a seat that obeys the
// grammar it is sent. Before the fix the grammar forced `"labels":["x","y"]` and
// `"meta":"s"`, so all three attempts failed the contract's schema ("got string,
// want object") and the finished run deferred; now the first attempt delivers the
// nested object.
func TestRepackOnALlamaCppSeatConstrainsANestedSchemaToItsOwnShape(t *testing.T) {
	fake := groundedGrammarSeat(t, nestedRepackAnswer, nestedRepackStringified)
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)

	structured, _, _, attempts, err := p.repackStructured(context.Background(), agentTestSeat, json.RawMessage(nestedRepackSchema), "Two labels: x (a) and y (b). Source s.", 0)
	if err != nil {
		t.Fatalf("repackStructured on a llama.cpp seat with a nested schema: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: the grammar should admit the nested answer on the first request", attempts)
	}
	var got, want any
	if jerr := json.Unmarshal(structured, &got); jerr != nil {
		t.Fatalf("structured is not JSON: %v: %s", jerr, structured)
	}
	_ = json.Unmarshal([]byte(nestedRepackAnswer), &want)
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("structured = %s, want %s", gb, wb)
	}
	if n := fake.grammarCNT.Load(); n != 1 {
		t.Errorf("grammar requests = %d, want 1", n)
	}
	if strings.Contains(string(structured), `"labels":["`) {
		t.Errorf("labels came back as strings: %s", structured)
	}
}

// wantNestedFieldTypes is what every lane's prompt says about the nested schema: the
// keys of each nested object, with their types, at every depth. The grammar fixes the
// keys, but only the prompt tells the seat which value goes under which, and the
// grammar-free chat lane has nothing else: a chat re-pack of an array of objects came
// back with the items present and their required keys missing.
var wantNestedFieldTypes = []string{
	`"title" (string)`,
	`"labels" (array of objects, each with fields "name" (string), "score" (number), "kind" (string, one of "a", "b"))`,
	`"meta" (object with fields "source" (string), "review_notes" (array of strings), "owner" (object with fields "id" (integer), "active" (boolean)))`,
}

func TestRepackPromptsNameTheFieldsOfNestedObjects(t *testing.T) {
	contract := testContract()
	contract.OutputSchema = json.RawMessage(nestedRepackSchema)

	t.Run("grammar lane", func(t *testing.T) {
		fake := groundedGrammarSeat(t, nestedRepackAnswer, nestedRepackStringified)
		fake.loop = func(int64) string { return doneChat("Two labels, x and y.") }
		fake.repackBodies = make(chan map[string]any, 4)
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
		}
		_, user := promptOf(t, <-fake.repackBodies)
		for _, want := range wantNestedFieldTypes {
			if !strings.Contains(user, want) {
				t.Errorf("grammar lane: the user prompt does not say %s:\n%s", want, user)
			}
		}
	})

	t.Run("chat lane", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:    []string{agentTestSeat},
			loop:         func(int64) string { return doneChat("Two labels, x and y.") },
			repack:       func(int64) string { return `{"wrong":"shape"}` },
			chatFallback: func(int64) string { return doneChat(nestedRepackAnswer) },
			chatBodies:   make(chan map[string]any, 4),
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
		}
		system, user := promptOf(t, <-fake.chatBodies)
		for _, want := range wantNestedFieldTypes {
			if !strings.Contains(user, want) {
				t.Errorf("chat lane: the user prompt does not say %s:\n%s", want, user)
			}
		}
		if !strings.Contains(system, "an object field holds a JSON object with exactly the fields listed for it") {
			t.Errorf("chat lane: the system prompt does not say what an object field holds:\n%s", system)
		}
	})
}
