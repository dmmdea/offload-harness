// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"comfyui-pp-cli/internal/mcp/bound"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// codeOrchSchemaGoldens maps each code-orchestration tool to the committed
// copy of its output schema. The committed file — not the Go constant — is the
// fixture every assertion below runs against, so renaming a property in
// output_schema.go, or dropping WithRawOutputSchema off a registration, fails
// here instead of silently shipping a tool whose advertised contract no longer
// matches what it returns.
var codeOrchSchemaGoldens = map[string]string{
	"comfyui_search":  filepath.Join("testdata", "schema", "comfyui_search.output.json"),
	"comfyui_get":     filepath.Join("testdata", "schema", "comfyui_get.output.json"),
	"comfyui_execute": filepath.Join("testdata", "schema", "comfyui_execute.output.json"),
}

func readSchemaGolden(t *testing.T, toolName string) []byte {
	t.Helper()
	path, ok := codeOrchSchemaGoldens[toolName]
	if !ok {
		t.Fatalf("no committed schema registered for tool %q", toolName)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- fixed testdata path from a package-local map.
	if err != nil {
		t.Fatalf("reading committed schema %s: %v", path, err)
	}
	return data
}

func decodeJSON(t *testing.T, label string, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", label, err, data)
	}
	return v
}

// TestCodeOrchAdvertisedOutputSchemasMatchCommittedGoldens compares what the
// server actually advertises for each tool against the committed schema files.
func TestCodeOrchAdvertisedOutputSchemasMatchCommittedGoldens(t *testing.T) {
	s := server.NewMCPServer("comfyui", "test")
	RegisterCodeOrchestrationTools(s)

	tools := s.ListTools()
	for toolName, goldenPath := range codeOrchSchemaGoldens {
		entry, ok := tools[toolName]
		if !ok {
			t.Fatalf("tool %q was not registered: %v", toolName, toolNames(tools))
		}
		if len(entry.Tool.RawOutputSchema) == 0 {
			t.Fatalf("tool %q advertises no outputSchema; the WithRawOutputSchema option is missing", toolName)
		}
		advertised := decodeJSON(t, "advertised schema for "+toolName, entry.Tool.RawOutputSchema)
		committed := decodeJSON(t, goldenPath, readSchemaGolden(t, toolName))
		if !reflect.DeepEqual(advertised, committed) {
			t.Fatalf("tool %q advertises a schema that differs from %s.\nadvertised: %s\ncommitted:  %s",
				toolName, goldenPath, entry.Tool.RawOutputSchema, readSchemaGolden(t, toolName))
		}
	}
}

// TestCodeOrchOutputSchemasReachTheWire proves the schema survives the
// tools/list serialization an MCP host actually reads, not just the in-memory
// Tool value.
func TestCodeOrchOutputSchemasReachTheWire(t *testing.T) {
	s := server.NewMCPServer("comfyui", "test")
	RegisterCodeOrchestrationTools(s)

	entry := s.GetTool("comfyui_search")
	if entry == nil {
		t.Fatal("comfyui_search was not registered")
	}
	wire, err := json.Marshal(entry.Tool)
	if err != nil {
		t.Fatalf("marshalling comfyui_search: %v", err)
	}
	var envelope struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("decoding marshalled tool: %v", err)
	}
	if len(envelope.OutputSchema) == 0 {
		t.Fatalf("marshalled comfyui_search carries no outputSchema: %s", wire)
	}
	if !reflect.DeepEqual(decodeJSON(t, "wire outputSchema", envelope.OutputSchema),
		decodeJSON(t, "committed schema", readSchemaGolden(t, "comfyui_search"))) {
		t.Fatalf("wire outputSchema differs from the committed schema: %s", envelope.OutputSchema)
	}
}

// validateAgainstCommittedSchema runs handler through a server configured with
// WithOutputSchemaValidation, declaring the COMMITTED schema bytes as the
// tool's output schema. mcp-go compiles that JSON Schema and checks the
// handler's structuredContent against it, so the assertion is "the real
// payload satisfies the file we shipped", not "a Go value round-trips".
func validateAgainstCommittedSchema(t *testing.T, toolName string, handler server.ToolHandlerFunc, args map[string]any) mcplib.CallToolResult {
	t.Helper()
	s := server.NewMCPServer("comfyui", "test", server.WithOutputSchemaValidation())
	s.AddTool(
		mcplib.NewTool(toolName, mcplib.WithRawOutputSchema(readSchemaGolden(t, toolName))),
		handler,
	)

	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": toolName, "arguments": args},
	})
	if err != nil {
		t.Fatalf("encoding tools/call request: %v", err)
	}
	response := s.HandleMessage(context.Background(), request)
	if response == nil {
		t.Fatal("HandleMessage returned no response")
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshalling JSON-RPC response: %v", err)
	}
	var envelope struct {
		Result mcplib.CallToolResult `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decoding JSON-RPC response: %v\n%s", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("tools/call %s failed at the protocol level: %s", toolName, envelope.Error.Message)
	}
	return envelope.Result
}

func requireConformingResult(t *testing.T, toolName string, result mcplib.CallToolResult) {
	t.Helper()
	if result.IsError {
		t.Fatalf("%s returned an error result (output-schema validation rejects a non-conforming payload here): %s",
			toolName, resultText(t, result))
	}
	if len(result.RawStructuredContent) == 0 {
		t.Fatalf("%s declares an outputSchema but returned no structuredContent; an MCP host reading the schema gets nothing", toolName)
	}
}

func resultText(t *testing.T, result mcplib.CallToolResult) string {
	t.Helper()
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestCodeOrchSearchStructuredContentSatisfiesCommittedSchema(t *testing.T) {
	result := validateAgainstCommittedSchema(t, "comfyui_search", handleCodeOrchSearch, map[string]any{
		"query": "how long did the render take",
	})
	requireConformingResult(t, "comfyui_search", result)

	var payload struct {
		Count   int `json:"count"`
		Results []struct {
			EndpointID string `json:"endpoint_id"`
			Method     string `json:"method"`
			Path       string `json:"path"`
			Summary    string `json:"summary"`
			Score      int    `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(result.RawStructuredContent, &payload); err != nil {
		t.Fatalf("decoding structuredContent: %v\n%s", err, result.RawStructuredContent)
	}
	if payload.Count == 0 || len(payload.Results) != payload.Count {
		t.Fatalf("search returned count=%d with %d results: %s", payload.Count, len(payload.Results), result.RawStructuredContent)
	}
	// The timing question must reach the history endpoints, whose summaries
	// carry the execution_start/execution_success duration rule.
	var ids []string
	for _, r := range payload.Results {
		ids = append(ids, r.EndpointID)
	}
	if !containsString(ids, "history.get") && !containsString(ids, "history.list") {
		t.Fatalf("timing query surfaced no history endpoint: %v", ids)
	}
}

func TestCodeOrchGetStructuredContentSatisfiesCommittedSchema(t *testing.T) {
	result := validateAgainstCommittedSchema(t, "comfyui_get", handleCodeOrchGet, map[string]any{
		"endpoint_id": "objectinfo.get",
	})
	requireConformingResult(t, "comfyui_get", result)

	var payload struct {
		EndpointID string `json:"endpoint_id"`
		Method     string `json:"method"`
		Path       string `json:"path"`
		Summary    string `json:"summary"`
	}
	if err := json.Unmarshal(result.RawStructuredContent, &payload); err != nil {
		t.Fatalf("decoding structuredContent: %v\n%s", err, result.RawStructuredContent)
	}
	if payload.EndpointID != "objectinfo.get" || payload.Path != "/object_info/{class_type}" {
		t.Fatalf("comfyui_get returned the wrong endpoint: %s", result.RawStructuredContent)
	}
	// The hard-won detail must survive the port, not be paraphrased away.
	if !strings.Contains(payload.Summary, "index 1") || !strings.Contains(payload.Summary, "extra_model_paths.yaml") {
		t.Fatalf("objectinfo.get summary lost its COMBO/registration detail: %q", payload.Summary)
	}
}

// TestCodeOrchExecuteStructuredContentSatisfiesCommittedSchema drives
// comfyui_execute against an httptest stand-in for ComfyUI (the local server is
// never contacted) and asserts both that the path placeholder was substituted
// and that the envelope satisfies the committed schema.
func TestCodeOrchExecuteStructuredContentSatisfiesCommittedSchema(t *testing.T) {
	const promptID = "9f1c2f7a-0d3b-4b21-9c55-3a0a1f4e77bd"
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + promptID + `":{"status":{"status_str":"success","completed":true,` +
			`"messages":[["execution_start",{"timestamp":1755102000000}],["execution_success",{"timestamp":1755102070000}]]},` +
			`"outputs":{"9":{"images":[{"filename":"ComfyUI_00021_.png","subfolder":"","type":"output"}]}}}}`))
	}))
	defer srv.Close()

	resetMCPPathEnv(t)
	t.Setenv("COMFYUI_BASE_URL", srv.URL)

	result := validateAgainstCommittedSchema(t, "comfyui_execute", handleCodeOrchExecute, map[string]any{
		"endpoint_id": "history.get",
		"params":      map[string]any{"prompt_id": promptID},
	})
	requireConformingResult(t, "comfyui_execute", result)

	// The sibling implementation left Positional empty, which shipped the
	// literal "{prompt_id}" segment to the server. Assert the substitution.
	if want := "/history/" + promptID; gotPath != want {
		t.Fatalf("request path = %q, want %q (path placeholder was not substituted)", gotPath, want)
	}

	var payload struct {
		EndpointID string          `json:"endpoint_id"`
		Method     string          `json:"method"`
		Path       string          `json:"path"`
		BodyBytes  int             `json:"body_bytes"`
		Body       json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(result.RawStructuredContent, &payload); err != nil {
		t.Fatalf("decoding structuredContent: %v\n%s", err, result.RawStructuredContent)
	}
	if payload.EndpointID != "history.get" || payload.Method != "GET" {
		t.Fatalf("execute envelope misreports the call: %s", result.RawStructuredContent)
	}
	if payload.Path != "/history/"+promptID {
		t.Fatalf("execute envelope path = %q, want the substituted path", payload.Path)
	}
	if payload.BodyBytes == 0 || len(payload.Body) == 0 {
		t.Fatalf("execute envelope carried no body: %s", result.RawStructuredContent)
	}
	if !strings.Contains(string(payload.Body), "execution_success") {
		t.Fatalf("execute envelope body lost the timing messages: %s", payload.Body)
	}
}

// TestCodeOrchExecuteBinaryBodyRoundTripsAsBase64 covers the branch nothing in
// the fake-server suite reached before a live run: an endpoint that answers
// with file bytes. The client layer must hand the code-orchestration path a
// self-describing base64 envelope — not a 406, not an empty body, not raw
// bytes smuggled into body_text — with no per-endpoint Accept override.
func TestCodeOrchExecuteBinaryBodyRoundTripsAsBase64(t *testing.T) {
	payloadBytes := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("p", 512))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payloadBytes)
	}))
	defer srv.Close()

	resetMCPPathEnv(t)
	t.Setenv("COMFYUI_BASE_URL", srv.URL)

	result := validateAgainstCommittedSchema(t, "comfyui_execute", handleCodeOrchExecute, map[string]any{
		"endpoint_id": "view.get",
		"params":      map[string]any{"filename": "render.png", "subfolder": "", "type": "output"},
	})
	requireConformingResult(t, "comfyui_execute", result)

	var payload struct {
		Path string `json:"path"`
		Body struct {
			PPBinary    bool   `json:"_pp_binary"`
			ContentType string `json:"content_type"`
			Encoding    string `json:"encoding"`
			Bytes       int    `json:"bytes"`
			Data        string `json:"data"`
		} `json:"body"`
		BodyText string `json:"body_text"`
	}
	if err := json.Unmarshal(result.RawStructuredContent, &payload); err != nil {
		t.Fatalf("decoding structuredContent: %v\n%s", err, result.RawStructuredContent)
	}
	if payload.BodyText != "" {
		t.Fatalf("binary payload leaked into body_text: %q", payload.BodyText)
	}
	if !payload.Body.PPBinary || payload.Body.Encoding != "base64" {
		t.Fatalf("binary response did not arrive as a base64 envelope: %s", result.RawStructuredContent)
	}
	if payload.Body.ContentType != "image/png" || payload.Body.Bytes != len(payloadBytes) {
		t.Fatalf("binary envelope misreports the payload: %s", result.RawStructuredContent)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload.Body.Data)
	if err != nil {
		t.Fatalf("envelope data is not decodable base64: %v", err)
	}
	if !bytes.Equal(decoded, payloadBytes) {
		t.Fatalf("decoded payload differs from the bytes the server sent (%d vs %d bytes)", len(decoded), len(payloadBytes))
	}
}

// TestCodeOrchExecuteOversizedBinaryRefusesInsteadOfTruncating pins the fix for
// what a live /view fetch of a real render exposed. A multi-megabyte PNG
// base64-encodes past the MCP budget, and the generic bounding turned it into a
// `preview` holding a base64 string cut mid-value: unparseable, and corrupt if
// an agent decoded it anyway. The note attached to that preview advised filters
// and --select, which cannot shrink an image. Refusing with a route to the
// bytes is the only honest answer.
func TestCodeOrchExecuteOversizedBinaryRefusesInsteadOfTruncating(t *testing.T) {
	payloadBytes := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("q"), 3*bound.MaxBytes)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payloadBytes)
	}))
	defer srv.Close()

	resetMCPPathEnv(t)
	t.Setenv("COMFYUI_BASE_URL", srv.URL)

	result := validateAgainstCommittedSchema(t, "comfyui_execute", handleCodeOrchExecute, map[string]any{
		"endpoint_id": "view.get",
		"params":      map[string]any{"filename": "render.png", "subfolder": "", "type": "output"},
	})
	if !result.IsError {
		t.Fatalf("oversized binary returned a success result instead of refusing: %s", result.RawStructuredContent)
	}
	msg := resultText(t, result)
	if strings.Contains(msg, "_pp_truncated") || strings.Contains(msg, "\"preview\"") {
		t.Fatalf("oversized binary was truncated into a preview rather than refused: %s", msg)
	}
	for _, want := range []string{"too large", "image/png", "comfyui-pp-cli view", "--deliver file:", "base64"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal message is missing %q, so the caller has no route to the bytes: %s", want, msg)
		}
	}
}

// TestCodeOrchExecuteNonJSONBodyUsesBodyText covers the other branch of the
// envelope: a textual, non-JSON response body.
func TestCodeOrchExecuteNonJSONBodyUsesBodyText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("interrupted"))
	}))
	defer srv.Close()

	resetMCPPathEnv(t)
	t.Setenv("COMFYUI_BASE_URL", srv.URL)

	result := validateAgainstCommittedSchema(t, "comfyui_execute", handleCodeOrchExecute, map[string]any{
		"endpoint_id": "queue.interrupt",
	})
	requireConformingResult(t, "comfyui_execute", result)

	var payload struct {
		BodyText string          `json:"body_text"`
		Body     json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(result.RawStructuredContent, &payload); err != nil {
		t.Fatalf("decoding structuredContent: %v\n%s", err, result.RawStructuredContent)
	}
	if len(payload.Body) != 0 {
		t.Fatalf("non-JSON response should not populate body: %s", result.RawStructuredContent)
	}
	if !strings.Contains(payload.BodyText, "interrupted") {
		t.Fatalf("body_text = %q, want the plain-text response", payload.BodyText)
	}
}

// TestCodeOrchExecuteRoutesQueryParamsOnWriteMethods proves QueryParams is
// wired: userdata.put's `overwrite` must land in the query string, and its
// `file` placeholder must be substituted, leaving only the real body behind.
func TestCodeOrchExecuteRoutesQueryParamsOnWriteMethods(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	resetMCPPathEnv(t)
	t.Setenv("COMFYUI_BASE_URL", srv.URL)

	result := validateAgainstCommittedSchema(t, "comfyui_execute", handleCodeOrchExecute, map[string]any{
		"endpoint_id": "userdata.put",
		"params": map[string]any{
			"file":      "workflows/hidream-2048.json",
			"overwrite": true,
			"contents":  "{}",
		},
	})
	requireConformingResult(t, "comfyui_execute", result)

	if !strings.HasPrefix(gotPath, "/userdata/") || strings.Contains(gotPath, "{file}") {
		t.Fatalf("request path = %q, want the substituted /userdata/<file> path", gotPath)
	}
	if gotQuery != "overwrite=true" {
		t.Fatalf("query string = %q, want overwrite=true routed out of the JSON body", gotQuery)
	}
	if strings.Contains(gotBody, "overwrite") {
		t.Fatalf("request body still carries the query param: %s", gotBody)
	}
	if !strings.Contains(gotBody, "contents") {
		t.Fatalf("request body lost the non-query params: %s", gotBody)
	}
}

// TestCommittedSchemaRejectsNonConformingPayload is the negative control for
// every assertion above. Without it a silently disabled validator would let
// each conformance test pass unconditionally.
func TestCommittedSchemaRejectsNonConformingPayload(t *testing.T) {
	broken := func(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		// count as a string, and the item key renamed — exactly the silent
		// drift the committed schema exists to catch.
		return mcplib.NewToolResultStructured(map[string]any{
			"count": "1",
			"results": []map[string]any{{
				"endpointId": "history.get",
				"method":     "GET",
				"path":       "/history/{prompt_id}",
				"summary":    "…",
				"score":      2,
			}},
		}, "{}"), nil
	}
	result := validateAgainstCommittedSchema(t, "comfyui_search", broken, map[string]any{"query": "timing"})
	if !result.IsError {
		t.Fatalf("committed schema accepted a payload with a renamed field and a wrong type: %s", result.RawStructuredContent)
	}
}

// TestCodeOrchPositionalCoversEveryPathPlaceholder is the structural guard for
// the execute path: handleCodeOrchExecute only substitutes `{name}` segments
// that are listed in Positional, so an endpoint whose placeholders and
// Positional disagree ships a literal template to the server.
func TestCodeOrchPositionalCoversEveryPathPlaceholder(t *testing.T) {
	for _, ep := range codeOrchEndpoints {
		placeholders := pathPlaceholders(ep.Path)
		if !reflect.DeepEqual(placeholders, ep.Positional) {
			t.Errorf("%s: path %q has placeholders %v but Positional is %v", ep.ID, ep.Path, placeholders, ep.Positional)
		}
		for _, q := range ep.QueryParams {
			if q.WireName == "" {
				t.Errorf("%s: query binding %+v has no wire name", ep.ID, q)
			}
		}
	}
}

// TestCodeOrchRegistryMatchesToolsManifest keeps the registry honest against
// the CLI's own endpoint manifest — a new or renamed endpoint that never
// reaches codeOrchEndpoints is invisible to comfyui_search, which is the only
// discovery surface left once endpoint-mirror tools are hidden.
func TestCodeOrchRegistryMatchesToolsManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "tools-manifest.json"))
	if err != nil {
		t.Fatalf("reading tools-manifest.json: %v", err)
	}
	var manifest struct {
		Tools []struct {
			Name   string `json:"name"`
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decoding tools-manifest.json: %v", err)
	}
	if len(manifest.Tools) != len(codeOrchEndpoints) {
		t.Fatalf("manifest declares %d endpoints, registry carries %d", len(manifest.Tools), len(codeOrchEndpoints))
	}
	registry := map[string]codeOrchEndpoint{}
	for _, ep := range codeOrchEndpoints {
		registry[ep.Method+" "+ep.Path] = ep
	}
	for _, tool := range manifest.Tools {
		ep, ok := registry[tool.Method+" "+tool.Path]
		if !ok {
			t.Errorf("manifest endpoint %s %s (%s) is missing from codeOrchEndpoints", tool.Method, tool.Path, tool.Name)
			continue
		}
		// history_get -> history.get: the manifest's underscore name and the
		// registry's dotted id must describe the same endpoint.
		if want := strings.Replace(tool.Name, "_", ".", 1); ep.ID != want {
			t.Errorf("endpoint %s %s has id %q, want %q", tool.Method, tool.Path, ep.ID, want)
		}
	}
}

func pathPlaceholders(path string) []string {
	var out []string
	rest := path
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			break
		}
		shut := strings.Index(rest[open:], "}")
		if shut < 0 {
			break
		}
		out = append(out, rest[open+1:open+shut])
		rest = rest[open+shut+1:]
	}
	if out == nil {
		return []string{}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func toolNames(tools map[string]*server.ServerTool) []string {
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	return out
}
