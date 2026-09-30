package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// requiredFieldContract is the contract of a caller that names a field in its
// acceptance but never declared it required in the schema: the "current
// schema" of register C-74. The digest carries a list the acceptance reads and
// a verdict it does not.
func requiredFieldContract() core.AgentContract {
	c := testContract()
	c.OutputSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"key_facts":{"type":"array","items":{"type":"string"}},` +
		`"verdict":{"type":"string"}}}`)
	c.Acceptance = []string{"min_items:key_facts:1"}
	return c
}

// A seat that answers the loop with a JSON object that leaves out the field the
// acceptance reads used to be delivered as the structured answer (the object
// validates: nothing declared the field required), skip the re-pack, and fail
// acceptance at the delegator after a whole run — 36 of the 59 min_items
// failures on 2026-09-29 took exactly this path. The field the acceptance
// names is required for the answer, so the object goes to the re-pack.
func TestRunAgentTaskObjectMissingAnAcceptanceFieldGoesToRepack(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(`{"verdict":"the page covers transfer modes"}`) },
		repack: func(int64) string {
			return `{"key_facts":["pinned staging memory"],"verdict":"the page covers transfer modes"}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, requiredFieldContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s / %s", wire.DeferClass, wire.Reason)
	}
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("re-pack completions = %d, want 1: an object missing the acceptance field must go to the re-pack, not be delivered", got)
	}
	var got struct {
		KeyFacts []string `json:"key_facts"`
	}
	if err := json.Unmarshal(wire.Structured, &got); err != nil || len(got.KeyFacts) != 1 {
		t.Fatalf("structured = %s (%v), want the re-packed object carrying key_facts", wire.Structured, err)
	}
}

// The guard on the other side: PRESENCE is what the answer owes, not
// non-emptiness. A direct object that carries the field — empty, for a page
// with nothing to say — is delivered as it is; sending it to the re-pack would
// spend a second generation to rewrite a faithful answer.
func TestRunAgentTaskObjectCarryingTheAcceptanceFieldIsDeliveredDirectly(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(`{"key_facts":[],"verdict":"nothing on this page"}`) },
		repack:    func(int64) string { t.Error("re-pack ran for an object that already carries the field"); return `{}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, requiredFieldContract())))
	if wire.Deferred || fake.grammarCNT.Load() != 0 {
		t.Fatalf("deferred=%v re-packs=%d, want the direct object delivered as is", wire.Deferred, fake.grammarCNT.Load())
	}
	if string(wire.Structured) != `{"key_facts":[],"verdict":"nothing on this page"}` {
		t.Fatalf("structured = %s", wire.Structured)
	}
}

// vLLM's structured_outputs leaves an optional field out of its own answer, so
// the re-pack on a vLLM-declared seat must send the schema WITH the fields the
// acceptance reads declared required (the mechanism behind the re-pack rows
// that still failed min_items after a re-pack).
func TestRepackOnAVLLMSeatDeclaresTheAcceptanceFieldsRequired(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The page covers pinned staging memory.") },
		chatFallback: func(int64) string { return doneChat(`{"key_facts":["pinned staging memory"],"verdict":"ok"}`) },
		chatBodies:   make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	cfg := p.Cfg()
	cfg.VLLMSeats = []string{agentTestSeat}
	p.cfg = cfg

	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, requiredFieldContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s / %s", wire.DeferClass, wire.Reason)
	}
	select {
	case body := <-fake.chatBodies:
		so, _ := body["structured_outputs"].(map[string]any)
		js, _ := so["json"].(map[string]any)
		req, _ := js["required"].([]any)
		if len(req) != 1 || req[0] != "key_facts" {
			t.Fatalf("structured_outputs.json.required = %v, want [key_facts]: the seat's grammar may otherwise leave the field the acceptance reads out (body %v)", req, body)
		}
	default:
		t.Fatal("the vLLM seat never received a structured re-pack request")
	}
}
