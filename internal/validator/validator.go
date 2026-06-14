// Package validator checks model output JSON against a JSON schema.
package validator

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validate checks that data conforms to schema (a parsed JSON-schema object).
// A nil/empty schema passes (structure was already grammar-constrained).
func Validate(data []byte, schema map[string]any) error {
	if len(schema) == 0 {
		// Still confirm it is a JSON object.
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("invalid json: %w", err)
		}
		return nil
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://schema", schema); err != nil {
		return fmt.Errorf("add schema: %w", err)
	}
	sch, err := c.Compile("mem://schema")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	var inst any
	if err := json.Unmarshal(data, &inst); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}
	return nil
}
