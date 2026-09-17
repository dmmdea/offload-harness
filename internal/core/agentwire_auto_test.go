package core

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestTimeoutAutoRidesTheWireAndDefaultsFalse (register D-03): the marker that
// says "the caller never set timeout_sec" must survive the wire — that is the
// whole reason it is a field and not a sentinel — and a contract from an older
// delegator, which never sends it, decodes as an explicit 300 exactly as today.
func TestTimeoutAutoRidesTheWireAndDefaultsFalse(t *testing.T) {
	in := AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "g", TimeoutSec: AgentTimeoutSecDefault, TimeoutAuto: true}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"timeout_auto":true`) {
		t.Fatalf("wire = %s, want timeout_auto:true on it", buf.String())
	}
	got, err := DecodeAgentContract(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TimeoutAuto || got.TimeoutSec != AgentTimeoutSecDefault {
		t.Fatalf("decoded auto=%v timeout=%d, want true/%d", got.TimeoutAuto, got.TimeoutSec, AgentTimeoutSecDefault)
	}
	// An explicit contract carries no marker at all: omitempty keeps the bytes
	// of every existing caller unchanged.
	buf.Reset()
	_ = json.NewEncoder(&buf).Encode(AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "g", TimeoutSec: 120})
	if strings.Contains(buf.String(), "timeout_auto") {
		t.Fatalf("an explicit contract must not carry the marker: %s", buf.String())
	}
	old, err := DecodeAgentContract(strings.NewReader(`{"schema_version":` + itoa(AgentWireSchemaVersion) + `,"goal":"g"}`))
	if err != nil {
		t.Fatal(err)
	}
	if old.TimeoutAuto || old.TimeoutSec != AgentTimeoutSecDefault {
		t.Fatalf("an older delegator's contract decoded auto=%v timeout=%d, want false/%d (today's behaviour)", old.TimeoutAuto, old.TimeoutSec, AgentTimeoutSecDefault)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
