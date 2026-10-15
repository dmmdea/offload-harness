package core

import (
	"encoding/json"
	"testing"
)

// The pager object is additive on the agent wire result. These pin the two shapes a
// reader of the delegation log has to tell apart: the object present (a run whose node recorded
// the pager gate, zeros included) and the key absent (a node that predates the field).

// TestAgentPagerKeysAreTheLoopReportsKeys: the wire object reuses the key names of the loop's
// own pager report, so one decoder reads both the standalone trace file and the delegation log.
func TestAgentPagerKeysAreTheLoopReportsKeys(t *testing.T) {
	b, err := json.Marshal(AgentWireResult{SchemaVersion: 1, Pager: &AgentPager{Basis: "insufficient_data"}})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	raw, ok := top["pager"]
	if !ok {
		t.Fatalf("pager key missing: %s", b)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	want := []string{"basis", "evictions", "evicted_bytes", "distinct_evicted", "refetched_distinct", "compaction_passes"}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			t.Errorf("pager object lacks %q (got %s): a zero here is a measurement and must serialize", k, raw)
		}
	}
	if len(obj) != len(want) {
		t.Errorf("pager object has %d keys, want %d: %s", len(obj), len(want), raw)
	}
}

// TestAWireResultWithoutAPagerOmitsTheKey: a node that predates the field, or a deferred run
// that never reached the loop, writes no pager key at all; it must not become an object of zeros.
func TestAWireResultWithoutAPagerOmitsTheKey(t *testing.T) {
	b, err := json.Marshal(AgentWireResult{SchemaVersion: 1, Seat: "seat-x"})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["pager"]; ok {
		t.Fatalf("a result with no pager serialized one: %s", b)
	}
}
