package delegate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A node's structured object is checked against the contract's acceptance checks, and those
// read FIELDS: `contains:` reads the finished answer's text and `nonempty:doc_title` reads one
// scalar. Neither can say the object kept the lists the answer held. A node that re-packed an
// answer into an all-empty object ("stale": [], "up_to_date": true) passed both, and a
// document with eleven stale sentences was reported as having none. EvalAcceptance now also
// reads the object against its answer, so a hollow object from a node that predates the node
// side check is a failed verification (which the delegator retries on another node), not a
// success.

const fidelitySchema = `{"type":"object","properties":{"doc_title":{"type":"string"},"stale":{"type":"array","items":{"type":"string"},"maxItems":10},"up_to_date":{"type":"boolean"},"notes":{"type":"string"}},"required":["doc_title","stale","up_to_date"]}`

func fidelityContract() core.AgentContract {
	c := remoteContract()
	c.OutputSchema = json.RawMessage(fidelitySchema)
	c.Acceptance = []string{"contains:Intake guide", "nonempty:doc_title"}
	return c
}

// invertedAnswer is a final answer that is a JSON object with a list under a key the schema
// does not have, as a fenced block.
func invertedAnswer(items int) string {
	var b strings.Builder
	b.WriteString("```json\n{\n  \"doc_title\": \"Intake guide\",\n  \"outdated_sentences\": [\n")
	for i := 1; i <= items; i++ {
		sep := ","
		if i == items {
			sep = ""
		}
		fmt.Fprintf(&b, "    {\"quote\": \"Sentence %d of the guide.\", \"release_note_says\": \"Release note %d.\"}%s\n", i, i, sep)
	}
	b.WriteString("  ]\n}\n```")
	return b.String()
}

const hollowStructured = `{"doc_title":"Intake guide","stale":[],"up_to_date":true,"notes":"No stale sentences were identified in the provided text."}`

func TestEvalAcceptanceFailsAnObjectThatLostTheListsOfItsAnswer(t *testing.T) {
	contract := fidelityContract()
	wire := core.AgentWireResult{Output: invertedAnswer(11), Structured: json.RawMessage(hollowStructured)}
	fails := EvalAcceptance(contract, wire)
	if len(fails) == 0 {
		t.Fatalf("a hollow object over an answer holding 11 list items passed acceptance")
	}
	joined := strings.Join(fails, " | ")
	if !strings.Contains(joined, `"stale"`) || !strings.Contains(joined, "11") {
		t.Fatalf("failures = %q, want one that names the emptied list and the 11 items", joined)
	}
}

func TestEvalAcceptancePassesAnObjectThatKeptItsAnswersLists(t *testing.T) {
	contract := fidelityContract()
	kept := `{"doc_title":"Intake guide","stale":["Sentence 1 of the guide.","Sentence 2 of the guide."],"up_to_date":false}`
	if fails := EvalAcceptance(contract, core.AgentWireResult{Output: invertedAnswer(2), Structured: json.RawMessage(kept)}); len(fails) != 0 {
		t.Fatalf("a faithful object failed acceptance: %v", fails)
	}
	// Legitimately empty: the answer itself says there is nothing stale.
	empty := `{"doc_title":"Intake guide","stale":[],"up_to_date":true}`
	answer := `{"doc_title":"Intake guide","stale":[],"checked_files":["a.md","b.md"],"up_to_date":true}`
	if fails := EvalAcceptance(contract, core.AgentWireResult{Output: answer, Structured: json.RawMessage(empty)}); len(fails) != 0 {
		t.Fatalf("a legitimately empty list failed acceptance: %v", fails)
	}
	// A prose answer has no list to count.
	if fails := EvalAcceptance(contract, core.AgentWireResult{Output: "Intake guide: nothing is stale.", Structured: json.RawMessage(empty)}); len(fails) != 0 {
		t.Fatalf("a prose answer failed acceptance: %v", fails)
	}
}

// The check needs a schema and a structured object; a contract without either is judged by its
// acceptance checks alone, as before (the single-seat ask lane's schema has no list at all).
func TestEvalAcceptanceFidelityNeedsASchemaAndAStructuredObject(t *testing.T) {
	contract := fidelityContract()
	// No object: the nonempty check fails on the missing field, and no fidelity finding rides on top.
	for _, f := range EvalAcceptance(contract, core.AgentWireResult{Output: invertedAnswer(3)}) {
		if strings.Contains(f, "list content") {
			t.Fatalf("a fidelity finding without an object: %q", f)
		}
	}
	// No schema: nothing says which lists the object was to hold.
	contract.OutputSchema = nil
	wire := core.AgentWireResult{Output: invertedAnswer(3), Structured: json.RawMessage(hollowStructured)}
	for _, f := range EvalAcceptance(contract, wire) {
		if strings.Contains(f, "list content") {
			t.Fatalf("a fidelity finding without a schema: %q", f)
		}
	}
}

// A rescued object is held to the same reading before the delegator delivers it: a hollow
// object from a rescue leaves the node's defer standing, with the finished answer attached.
func TestValidateRescuedRejectsAnObjectThatLostTheListsOfItsAnswer(t *testing.T) {
	contract := fidelityContract()
	err := validateRescued(contract, json.RawMessage(hollowStructured), invertedAnswer(11))
	if err == nil || !strings.Contains(err.Error(), `"stale"`) {
		t.Fatalf("validateRescued = %v, want a rejection naming the emptied list", err)
	}
	kept := `{"doc_title":"Intake guide","stale":["Sentence 1 of the guide."],"up_to_date":false}`
	if err := validateRescued(contract, json.RawMessage(kept), invertedAnswer(1)); err != nil {
		t.Fatalf("validateRescued rejected a faithful object: %v", err)
	}
}
