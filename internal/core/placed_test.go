package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContextClassAndLayerAreClosedVocabularies: the two placement inputs a
// caller may put on a contract are closed shapes — an unknown context_class
// would silently fall back to the default layer, and a free-form layer string
// is dispatched onto the wire as a seat lookup key on the receiving node.
func TestContextClassAndLayerAreClosedVocabularies(t *testing.T) {
	if err := (AgentContract{Goal: "g", ContextClass: "long", Layer: "triple"}).Validate(); err != nil {
		t.Fatalf("long/triple must validate: %v", err)
	}
	if err := (AgentContract{Goal: "g", ContextClass: "huge"}).Validate(); err == nil || !strings.Contains(err.Error(), "context_class") {
		t.Fatalf("want context_class error, got %v", err)
	}
	if err := (AgentContract{Goal: "g", Layer: "../etc"}).Validate(); err == nil || !strings.Contains(err.Error(), "layer") {
		t.Fatalf("want layer error, got %v", err)
	}
}

// TestValidateWithCapAdmitsALargerContextOnlyWhenAsked: the 256 KiB transport
// bound stays the default; a composite box passes its own, larger cap through
// ValidateWithCap so a long-window seat can actually receive a long contract.
func TestValidateWithCapAdmitsALargerContextOnlyWhenAsked(t *testing.T) {
	big := AgentContract{Goal: "g", Context: []ContextDoc{
		{Name: "a.txt", Text: strings.Repeat("x", 100<<10)},
		{Name: "b.txt", Text: strings.Repeat("x", 100<<10)},
		{Name: "c.txt", Text: strings.Repeat("x", 100<<10)},
	}}
	if err := big.Validate(); err == nil {
		t.Fatal("300 KiB must fail the default 256 KiB cap")
	}
	if err := big.ValidateWithCap(1 << 20); err != nil {
		t.Fatalf("300 KiB must pass a 1 MiB cap: %v", err)
	}
}

// TestDecodeAgentContractWithCapAdmitsWhatTheBoxAdmits: the node-side decoder
// is the LAST validator a cross-node contract meets, so it must take the box's
// cap too — a composite delegator admits a 300 KiB contract at its own cap,
// and a remote decoding at the fixed 256 KiB would 400 it at ACK and leave
// its long seat dead over the wire. The default entry point keeps the fixed
// cap; a non-positive cap falls back to it rather than to "unbounded".
func TestDecodeAgentContractWithCapAdmitsWhatTheBoxAdmits(t *testing.T) {
	big := AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "g", Context: []ContextDoc{
		{Name: "a.txt", Text: strings.Repeat("x", 100<<10)},
		{Name: "b.txt", Text: strings.Repeat("x", 100<<10)},
		{Name: "c.txt", Text: strings.Repeat("x", 100<<10)},
	}}
	body, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAgentContract(strings.NewReader(string(body))); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("300 KiB must fail the default decoder's 256 KiB cap, got %v", err)
	}
	if _, err := DecodeAgentContractWithCap(strings.NewReader(string(body)), 0); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("a non-positive cap must fall back to the default, not admit everything; got %v", err)
	}
	got, err := DecodeAgentContractWithCap(strings.NewReader(string(body)), 1<<20)
	if err != nil {
		t.Fatalf("300 KiB must pass a 1 MiB cap: %v", err)
	}
	if len(got.Context) != 3 || got.MaxSteps != AgentMaxStepsDefault || got.TimeoutSec != AgentTimeoutSecDefault {
		t.Fatalf("cap-taking decode must keep every other decoder rule (docs=%d steps=%d timeout=%d)", len(got.Context), got.MaxSteps, got.TimeoutSec)
	}
	// Everything but the cap is unchanged: the schema gate still runs first.
	if _, err := DecodeAgentContractWithCap(strings.NewReader(`{"schema_version":99,"goal":"g"}`), 1<<20); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("schema gate must survive the cap parameter, got %v", err)
	}
}

// TestPlacedAndContextClassAreOmittedWhenUnset: a non-composite box must stay
// byte-identical on every wire and result surface, so none of the new keys may
// leak when unset.
func TestPlacedAndContextClassAreOmittedWhenUnset(t *testing.T) {
	if b, _ := json.Marshal(AgentContract{Goal: "g"}); strings.Contains(string(b), "context_class") || strings.Contains(string(b), `"layer"`) {
		t.Fatalf("leak: %s", b)
	}
	if r, _ := json.Marshal(AgentWireResult{SchemaVersion: 1}); strings.Contains(string(r), "placed") {
		t.Fatalf("leak: %s", r)
	}
	if m, _ := json.Marshal(Meta{Model: "x"}); strings.Contains(string(m), "placed") {
		t.Fatalf("leak: %s", m)
	}
}
