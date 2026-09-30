package core

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The literal keys are the contract between node and delegator versions (the same
// argument the queued_ms pin makes): the fake nodes in the delegate tests marshal and
// unmarshal the SAME struct, so renaming a tag there changes both ends at once and
// every round-trip test stays green while a real mixed-version fleet reads zero — and
// the delegator then credits a re-placed run nothing for the wait its dead seat cost.
func TestAgentWireResultSeatDownFieldsKeepTheirWireNames(t *testing.T) {
	var w AgentWireResult
	if err := json.Unmarshal([]byte(`{"seat_recoveries":2,"seat_down_wait_sec":41.5}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.SeatRecoveries != 2 || w.SeatDownWaitSec != 41.5 {
		t.Fatalf("decoded seat_recoveries=%d seat_down_wait_sec=%v from the wire literal", w.SeatRecoveries, w.SeatDownWaitSec)
	}
	b, err := json.Marshal(AgentWireResult{SeatRecoveries: 1, SeatDownWaitSec: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"seat_recoveries":1`)) || !bytes.Contains(b, []byte(`"seat_down_wait_sec":2.5`)) {
		t.Fatalf("wire = %s, want the literal keys seat_recoveries and seat_down_wait_sec", b)
	}
	if b0, _ := json.Marshal(AgentWireResult{}); bytes.Contains(b0, []byte("seat_")) {
		t.Fatalf("zero values must be omitted: %s", b0)
	}
}
