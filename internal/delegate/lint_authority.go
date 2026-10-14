package delegate

import (
	"encoding/json"
	"fmt"

	"github.com/dmmdea/offload-harness/internal/core"
)

// authorityWarnings flags an authority-qualified check whose level the contract
// cannot reach: the check would grade unknown on every run, so the contract could
// never be accepted. It is the static twin of the unknown verdict, and warn-only
// like the rest of LintAcceptance (the run still happens).
func authorityWarnings(c core.AgentContract, raw string, chk core.AcceptanceCheck) []string {
	switch chk.Authority {
	case core.AuthGrounded:
		for _, d := range c.Context {
			if d.Text != "" {
				return nil
			}
		}
		return []string{fmt.Sprintf(
			"acceptance %q is AUTHORITY-UNREACHABLE: it demands grounded authority but the contract carries no context documents to verify against, so it can only ever grade unknown and the contract can never be accepted — add the documents or lower the qualifier to @answer:", raw)}
	case core.AuthField:
		name := ""
		if f := chk.Fields(); len(f) == 1 {
			name = f[0]
		}
		if len(c.OutputSchema) == 0 {
			return []string{fmt.Sprintf(
				"acceptance %q is AUTHORITY-UNREACHABLE: it reads structured field %q but the contract has no output_schema, so there is never a structured result to read and the check can only ever grade unknown — add an output_schema declaring the field or use @answer:", raw, name)}
		}
		if !schemaDeclares(c.OutputSchema, name) {
			return []string{fmt.Sprintf(
				"acceptance %q is AUTHORITY-UNREACHABLE: the output_schema does not declare a property %q, so the seat is never asked for the field and the check can only ever grade unknown", raw, name)}
		}
	}
	return nil
}

// schemaDeclares reports whether the JSON Schema lists name under its top-level
// properties. A schema that does not parse is treated as declaring nothing:
// Validate refuses it before a run, so this only keeps the lint total.
func schemaDeclares(schema json.RawMessage, name string) bool {
	var top struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &top) != nil {
		return false
	}
	_, ok := top.Properties[name]
	return ok
}
