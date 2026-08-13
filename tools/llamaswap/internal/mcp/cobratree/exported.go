// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored: a stable public name for the generated tool-name derivation.
//
// The MCP output-schema layer needs to map a CLI command path onto the tool
// name RegisterAll produced for it. Deriving that name a second time in
// another package would be a duplicate of the rule that could drift; calling
// through to the generated function keeps ONE derivation. A reprint of
// names.go leaves this wrapper working.

package cobratree

// ToolNameForPath returns the MCP tool name RegisterAll registers for a
// command path, e.g. ["bench","compare"] -> "bench_compare".
func ToolNameForPath(parts []string) string { return toolNameForPath(parts) }
