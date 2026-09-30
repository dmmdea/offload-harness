package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// `schema_miss` is a cross-version wire name: a 0.140.x node never sends it
// and an older delegator must ignore it, so the spelling is the contract. Every
// other test builds the flag through the struct, so a renamed json tag kept the
// suite green while breaking the node-to-delegator handshake.
func TestSchemaMissKeepsItsWireName(t *testing.T) {
	b, err := json.Marshal(AgentWireResult{Deferred: true, SchemaMiss: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"schema_miss":true`) {
		t.Fatalf("wire = %s, want the documented schema_miss key", b)
	}
	var w AgentWireResult
	if err := json.Unmarshal([]byte(`{"deferred":true,"schema_miss":true}`), &w); err != nil || !w.SchemaMiss {
		t.Fatalf("decode of the documented key: %+v (%v)", w, err)
	}
	if b, _ := json.Marshal(AgentWireResult{Deferred: true}); strings.Contains(string(b), "schema_miss") {
		t.Fatalf("an unflagged result carries the key: %s", b)
	}
}
