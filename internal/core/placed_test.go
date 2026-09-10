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
