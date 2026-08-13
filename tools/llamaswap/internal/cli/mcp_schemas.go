// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave LS-1): the MCP outputSchema source of truth.
// Not a command: no pp:data-source marker.
//
// The novel commands' JSON envelopes are the most valuable thing this CLI
// hands an agent, and until now they were advertised over MCP as "text". An
// agent had to parse an undocumented shape and hope. These specs reflect the
// ACTUAL Go result structs, so the schema an agent is given and the JSON it
// receives cannot disagree: rename a field and the schema renames with it.

package cli

import (
	"encoding/json"

	"llamaswap-pp-cli/internal/schemaref"
)

// ResultSchema pairs a command path with the JSON Schema of the envelope it
// emits under --json.
type ResultSchema struct {
	// Command is the CLI command path, space separated, exactly as the
	// command-mirror names it.
	Command string
	// Description is the one-line contract summary attached to the schema.
	Description string
	// Schema is the reflected JSON Schema document.
	Schema json.RawMessage
}

// ResultSchemas returns the reflected envelope schema for every novel command
// with a stable typed contract.
//
// Commands NOT listed here emit either free-form text or a generated
// endpoint-mirror envelope; advertising a schema for those would be a promise
// this CLI does not keep. Absence is deliberate, not an oversight.
func ResultSchemas() []ResultSchema {
	specs := []struct {
		command     string
		description string
		schema      *schemaref.Schema
	}{
		{"gguf", "GGUF header: architecture, GQA geometry, quantization profile with measured bits-per-weight, shard membership, MoE total-vs-active parameters, and RoPE context scaling.", schemaref.Of[ggufReport]()},
		{"fit", "Will this model at this context fit the cards, as an INTERVAL with a refuse-to-answer band. Verdict is fits | does-not-fit | UNCERTAIN.", schemaref.Of[fitReport]()},
		{"ctx", "Real tokens (the model's own tokenizer) against the seat's live n_ctx, with the KV cost at a target context.", schemaref.Of[ctxReport]()},
		{"bench", "Prompt-processing and generation rates measured separately at each KV depth, each with a sample standard deviation, plus the comparability key identifying the serving configuration.", schemaref.Of[benchReport]()},
		{"bench compare", "Diff of two recorded bench rows, or a refusal naming the comparability-key fields that differ.", schemaref.Of[benchCompareReport]()},
		{"metrics", "Parsed llama-server Prometheus telemetry with windowed counter rates and capacity findings.", schemaref.Of[metricsReport]()},
		{"version drift", "The llama-swap surface this CLI was verified against versus the live server, plus which backend actually answered.", schemaref.Of[surfaceDriftReport]()},
		{"ps", "Models currently holding VRAM, with the source of every derived column.", schemaref.Of[spinePsReport]()},
	}
	out := make([]ResultSchema, 0, len(specs))
	for _, s := range specs {
		raw, err := schemaref.JSON(s.schema)
		if err != nil {
			// A schema that will not marshal is a programming error, not a
			// runtime condition; skipping it advertises nothing rather than
			// advertising something wrong.
			continue
		}
		out = append(out, ResultSchema{Command: s.command, Description: s.description, Schema: raw})
	}
	return out
}
