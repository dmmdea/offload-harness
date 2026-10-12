package delegate

import (
	"encoding/json"
	"log"

	"github.com/dmmdea/offload-harness/internal/core"
)

// listFidelityFailure is the delegator's reading of a structured object against the finished
// answer it was written from (core.ListsEmptied): "" when the object kept the answer's lists, else
// the finding. The contract's acceptance checks read fields, and a field check cannot see that a
// node's re-pack emptied every list it was asked for; a hollow object from a node that predates
// the node-side check is therefore a failed verification (retried once on another node), not a
// success. It needs a schema and an object; with either missing it says nothing.
func listFidelityFailure(contract core.AgentContract, structured json.RawMessage, answer string) string {
	if len(structured) == 0 || len(contract.OutputSchema) == 0 {
		return ""
	}
	var schema map[string]any
	if err := json.Unmarshal(contract.OutputSchema, &schema); err != nil {
		// Not a finding (a broken contract is not the node's fault), but not silent either: the
		// result goes on without having been read against its answer.
		log.Printf("delegate: the contract's output_schema does not parse (%v); the structured result was not read against its answer", err)
		return ""
	}
	if why := core.ListsEmptied(structured, schema, answer); why != "" {
		return "the structured result lost the list content of its own answer: " + why
	}
	return ""
}
