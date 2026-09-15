package delegate

import (
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Register D-99: the agent_delegate tool description promises results[].calls
// (finish reasons, token counts, sampling, thinking_off) but the wire row a
// delegator publishes dropped the node's per-call record at WireResponse — a
// caller could read it only from the node's own GET /fleet/jobs/{id} with the
// fleet token. The row now carries the LAST eight calls (bounded; the loop
// records one per planner completion and a 12-step run with a re-pack can
// record more than a reader needs). The assertions read the JSON, not the
// struct, so the test compiled — and was red — before the field existed.
func TestWireResponseCarriesTheLastEightCalls(t *testing.T) {
	calls := make([]core.AgentCallRecord, 0, 11)
	for i := 1; i <= 11; i++ {
		calls = append(calls, core.AgentCallRecord{Step: i, MaxTokens: 1024, FinishReason: "stop", CompletionTokens: 10 * i, Sampling: "temperature=0"})
	}
	results := []PlacedResult{
		{Node: "lenovo", Seat: "qwen3.5-4b-vllm", Result: core.AgentWireResult{Output: "x", Calls: calls}},
		{Node: "Qube", Seat: "agent-pool", Result: core.AgentWireResult{Output: "y"}},
	}
	raw, err := json.Marshal(WireResponse(results, Summary{Succeeded: 2}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results: got %d, want 2", len(got.Results))
	}
	rawCalls, ok := got.Results[0]["calls"]
	if !ok {
		t.Fatalf("results[0] carries no \"calls\" key; keys: %v", keysOf(got.Results[0]))
	}
	var wire []core.AgentCallRecord
	if err := json.Unmarshal(rawCalls, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 8 {
		t.Fatalf("calls: got %d records, want the last 8 of 11", len(wire))
	}
	if wire[0].Step != 4 || wire[7].Step != 11 {
		t.Fatalf("calls window: got steps %d..%d, want 4..11 (the LAST eight, the final answer included)", wire[0].Step, wire[7].Step)
	}
	if wire[7].Sampling != "temperature=0" || wire[7].CompletionTokens != 110 {
		t.Fatalf("calls[7] lost its fields: %+v", wire[7])
	}
	if _, has := got.Results[1]["calls"]; has {
		t.Fatalf("results[1] recorded no calls but publishes a \"calls\" key — omitempty must hold so a pre-D-99 node's row is byte-identical")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
