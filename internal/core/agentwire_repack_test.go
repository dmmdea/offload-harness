package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The re-pack's per-attempt record is an OPTIONAL field of the wire result
// (register C-80): a result without it marshals as it always did, a record that
// was never sent omits its zero fields, and a reader that predates the field
// ignores it. Nodes deploy staggered; a new field must not be a flag day.
func TestRepackAttemptsDetailIsOptionalOnTheWire(t *testing.T) {
	plain := AgentWireResult{SchemaVersion: AgentWireSchemaVersion, NodeID: "n", Seat: "s", Output: "o", RepackAttempts: 2}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "repack_attempts_detail") {
		t.Fatalf("a result with no attempt records must not carry the key: %s", b)
	}

	with := plain
	with.RepackAttemptsDetail = []AgentRepackAttempt{
		{Attempt: 1, Lane: "grammar", MaxTokens: 1439, ClampedFrom: 8192, TokensOut: 1439, FinishReason: "length", Ms: 257000, Head: `{"a":[`, Tail: "   ", Why: "re-pack truncated at 1439 tokens"},
		{Attempt: 2, Lane: "chat", Skipped: true, Why: "re-pack skipped: 124 s left"},
	}
	b, err = json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	var back AgentWireResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.RepackAttemptsDetail, with.RepackAttemptsDetail) {
		t.Fatalf("the records did not round-trip:\n got %+v\nwant %+v", back.RepackAttemptsDetail, with.RepackAttemptsDetail)
	}

	// A skipped record carries no request fields: nothing was sent.
	skipped, _ := json.Marshal(with.RepackAttemptsDetail[1])
	for _, key := range []string{"max_tokens", "tokens_out", "finish_reason", "head", "tail", "ms", "clamped_from"} {
		if strings.Contains(string(skipped), `"`+key+`"`) {
			t.Errorf("a skipped record carries %q: %s", key, skipped)
		}
	}

	// A reader that predates the field (its struct has no such key) decodes the
	// same bytes and gets everything it knew.
	var older struct {
		Seat           string `json:"seat"`
		RepackAttempts int    `json:"repack_attempts"`
	}
	if err := json.Unmarshal(b, &older); err != nil || older.Seat != "s" || older.RepackAttempts != 2 {
		t.Fatalf("an older reader decoded %+v (%v) from %s", older, err, b)
	}
}
