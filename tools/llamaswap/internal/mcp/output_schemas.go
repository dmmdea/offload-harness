// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored (wave LS-1): MCP outputSchema + structuredContent.
//
// Until now every tool this server exposed advertised its result as TEXT. An
// agent receiving `{"schema_version":1,"header":{...}}` had to discover that
// shape by calling the tool and reading what came back, and had no way to know
// whether a field was optional, an integer, or renamed last week.
//
// The 2025-06-18 MCP revision added `outputSchema` on the tool definition and
// `structuredContent` on the result; mcp-go v0.57 supports both. This file
// attaches them to the tools worth attaching them to:
//
//   - the three registry tools (search / get / execute), and the local
//     search / sql tools, whose envelopes are defined right here;
//   - every command-mirror tool whose CLI command has a typed result envelope,
//     with the schema REFLECTED from the Go struct the command actually
//     marshals (internal/cli.ResultSchemas).
//
// Tools without a stable typed envelope are deliberately left alone. An
// advertised schema is a promise, and a promise made for free-form text is a
// promise broken on the first call.
//
// Attachment happens AFTER registration by reading each tool back, adding the
// schema and a structured-content wrapper, and re-adding it. That keeps the
// tool definitions in one place (the generated registrations) instead of
// duplicating descriptions and parameters here.

package mcp

import (
	"context"
	"encoding/json"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"llamaswap-pp-cli/internal/cli"
	"llamaswap-pp-cli/internal/mcp/cobratree"
	"llamaswap-pp-cli/internal/schemaref"
)

// ---------------------------------------------------------------------------
// typed envelopes for the tools defined in this package
// ---------------------------------------------------------------------------

// mcpSearchResult is the envelope handleSearch emits. Kept beside the handler
// so the schema and the map literal are edited together.
type mcpSearchResult struct {
	Count       int               `json:"count"`
	Results     []json.RawMessage `json:"results"`
	StoreStatus string            `json:"store_status"`
	Resumable   bool              `json:"resumable"`
	NextStep    string            `json:"next_step,omitempty"`
}

// mcpSQLResult is the envelope handleSQL emits.
type mcpSQLResult struct {
	Count       int              `json:"count"`
	Columns     []string         `json:"columns"`
	Rows        []map[string]any `json:"rows"`
	StoreStatus string           `json:"store_status"`
	Resumable   bool             `json:"resumable"`
	NextStep    string           `json:"next_step,omitempty"`
}

// codeOrchEndpointRef is one endpoint as the registry tools describe it.
type codeOrchEndpointRef struct {
	EndpointID string `json:"endpoint_id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Summary    string `json:"summary"`
	Score      int    `json:"score,omitempty"`
}

// codeOrchSearchResult is llamaswap_search's envelope.
type codeOrchSearchResult struct {
	Count   int                   `json:"count"`
	Results []codeOrchEndpointRef `json:"results"`
}

// outputSchemaSpec is one tool's advertised result contract.
type outputSchemaSpec struct {
	tool   string
	schema json.RawMessage
}

// packageToolSchemas are the tools defined in this package.
func packageToolSchemas() []outputSchemaSpec {
	specs := []struct {
		tool   string
		schema *schemaref.Schema
	}{
		{"search", schemaref.Of[mcpSearchResult]()},
		{"sql", schemaref.Of[mcpSQLResult]()},
		{"llamaswap_search", schemaref.Of[codeOrchSearchResult]()},
		{"llamaswap_get", schemaref.Of[codeOrchEndpointRef]()},
		// llamaswap_execute returns the upstream API's own body, whose shape
		// is per-endpoint. An open object is the honest contract; a
		// fabricated one would reject valid responses.
		{"llamaswap_execute", &schemaref.Schema{Type: "object"}},
	}
	out := make([]outputSchemaSpec, 0, len(specs))
	for _, s := range specs {
		raw, err := schemaref.JSON(s.schema)
		if err != nil {
			continue
		}
		out = append(out, outputSchemaSpec{tool: s.tool, schema: raw})
	}
	return out
}

// commandMirrorSchemas maps each typed CLI envelope onto its mirror tool.
func commandMirrorSchemas() []outputSchemaSpec {
	var out []outputSchemaSpec
	for _, rs := range cli.ResultSchemas() {
		name := cobratree.ToolNameForPath(strings.Fields(rs.Command))
		if name == "" {
			continue
		}
		out = append(out, outputSchemaSpec{tool: name, schema: rs.Schema})
	}
	return out
}

// registerOutputSchemas attaches every schema it can and wraps the matching
// handler so the result carries structuredContent alongside its text.
//
// A tool that is not registered (a command suppressed by classification, say)
// is skipped silently: this layer decorates what exists, it never invents a
// tool to hang a schema on.
func registerOutputSchemas(s *server.MCPServer) {
	if s == nil {
		return
	}
	for _, spec := range append(packageToolSchemas(), commandMirrorSchemas()...) {
		existing := s.GetTool(spec.tool)
		if existing == nil {
			continue
		}
		tool := existing.Tool
		tool.RawOutputSchema = spec.schema
		s.AddTool(tool, withStructuredContent(existing.Handler))
	}
}

// withStructuredContent parses a tool's text result as JSON and republishes it
// as structuredContent. Parsing is best-effort BY DESIGN: a result that was
// truncated to fit the MCP byte budget, or a command that fell back to prose,
// still returns its text unchanged rather than failing. structuredContent is
// added only when the text is a complete JSON object, so a consumer that finds
// the field can trust it.
func withStructuredContent(inner server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		res, err := inner(ctx, req)
		if err != nil || res == nil || res.IsError || res.StructuredContent != nil {
			return res, err
		}
		raw := firstTextContent(res)
		if raw == "" {
			return res, nil
		}
		trimmed := strings.TrimSpace(raw)
		if !strings.HasPrefix(trimmed, "{") {
			return res, nil
		}
		var probe map[string]any
		if json.Unmarshal([]byte(trimmed), &probe) != nil {
			return res, nil
		}
		res.StructuredContent = probe
		res.RawStructuredContent = json.RawMessage(trimmed)
		return res, nil
	}
}

func firstTextContent(res *mcplib.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
