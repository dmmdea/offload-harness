package delegate

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// Register C-80: the node records one entry per structured re-pack attempt
// (repack_attempts_detail: lane, max_tokens, tokens generated, finish reason, a clip
// of what the seat wrote, why it failed or was skipped), and the row an
// agent_delegate caller reads dropped it at WireResponse, as it once dropped the
// per-call record: the corpus kept it and the answer to "what did the re-pack do"
// stayed on the node. The row carries the first four (bounded; the node sends at most
// three and a skip), and publishes nothing for a node that recorded none, so a row
// from a node that predates the field is byte-identical to before.
func TestWireResponseCarriesTheRepackAttemptRecords(t *testing.T) {
	attempts := []core.AgentRepackAttempt{
		{Attempt: 1, Lane: "grammar", MaxTokens: 1439, TokensOut: 1439, FinishReason: "length", Ms: 257000, Head: `{"key_facts":["`, Tail: "          ", Why: "re-pack truncated at 1439 tokens"},
		{Attempt: 2, Lane: "chat", Skipped: true, Why: "re-pack skipped: 154 s left to the wall + 30 s grace at 5.6 tok/s buys 862 tokens < the answer's 991"},
	}
	many := make([]core.AgentRepackAttempt, 0, 6)
	for i := 1; i <= 6; i++ {
		many = append(many, core.AgentRepackAttempt{Attempt: i, Lane: "grammar", TokensOut: 10 * i})
	}
	results := []PlacedResult{
		{Node: "node-a", Seat: "seat-a", Result: core.AgentWireResult{Output: "x", RepackAttempts: 1, RepackAttemptsDetail: attempts}},
		{Node: "node-b", Seat: "seat-b", Result: core.AgentWireResult{Output: "y"}},
		{Node: "node-c", Seat: "seat-c", Result: core.AgentWireResult{Output: "z", RepackAttemptsDetail: many}},
	}
	raw, err := json.Marshal(WireResponse(results, Summary{Succeeded: 3}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 3 {
		t.Fatalf("results: got %d, want 3", len(got.Results))
	}
	rawDetail, ok := got.Results[0]["repack_attempts_detail"]
	if !ok {
		t.Fatalf("results[0] carries no \"repack_attempts_detail\" key; keys: %v", keysOf(got.Results[0]))
	}
	var wire []core.AgentRepackAttempt
	if err := json.Unmarshal(rawDetail, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire, attempts) {
		t.Fatalf("records did not survive WireResponse:\n got %+v\nwant %+v", wire, attempts)
	}
	if _, has := got.Results[1]["repack_attempts_detail"]; has {
		t.Fatalf("results[1] recorded no attempts but publishes the key: omitempty must hold so a node that predates the field publishes a byte-identical row")
	}
	var bounded []core.AgentRepackAttempt
	if err := json.Unmarshal(got.Results[2]["repack_attempts_detail"], &bounded); err != nil {
		t.Fatal(err)
	}
	if len(bounded) != wireRepackAttemptsMax || bounded[0].Attempt != 1 || bounded[len(bounded)-1].Attempt != wireRepackAttemptsMax {
		t.Fatalf("a long record = %+v, want the first %d attempts: the earliest one shows what went wrong", bounded, wireRepackAttemptsMax)
	}
}
