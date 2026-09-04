package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestGroundedContractIsOneWithAttachedText: a contract that carries a context
// document is document-grounded (runs without few-shot exemplars); a bare goal or
// an empty doc is not.
func TestGroundedContractIsOneWithAttachedText(t *testing.T) {
	if groundedContract(core.AgentContract{Goal: "summarise"}) {
		t.Fatal("bare goal must not count as grounded")
	}
	if groundedContract(core.AgentContract{Goal: "summarise", Context: []core.ContextDoc{{Name: "x"}}}) {
		t.Fatal("an empty doc must not count as grounded")
	}
	if !groundedContract(core.AgentContract{Goal: "summarise", Context: []core.ContextDoc{{Name: "page", Text: "body"}}}) {
		t.Fatal("a doc with text must count as grounded")
	}
}
