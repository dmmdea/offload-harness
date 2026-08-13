// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"llamaswap-pp-cli/internal/cli"
)

// schemaGoldenDir is the committed contract. Regenerate with
// PP_UPDATE_SCHEMA_GOLDEN=1 go test ./internal/mcp/ -run Golden
const schemaGoldenDir = "../../testdata/schema"

func allOutputSchemaSpecs() []outputSchemaSpec {
	specs := append(packageToolSchemas(), commandMirrorSchemas()...)
	sort.Slice(specs, func(i, j int) bool { return specs[i].tool < specs[j].tool })
	return specs
}

// The golden gate. A schema is a PROMISE made to agents; a rename that
// silently rewrites it is a broken promise nobody noticed. Committing the
// generated document turns any envelope change into a visible diff in the
// same PR as the code change.
func TestOutputSchemaGoldenFilesAreCurrent(t *testing.T) {
	update := os.Getenv("PP_UPDATE_SCHEMA_GOLDEN") != ""
	if update {
		if err := os.MkdirAll(schemaGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	specs := allOutputSchemaSpecs()
	if len(specs) == 0 {
		t.Fatal("no output schemas were produced at all")
	}
	for _, spec := range specs {
		path := filepath.Join(schemaGoldenDir, spec.tool+".json")
		if update {
			if err := os.WriteFile(path, spec.schema, 0o644); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("no committed schema for tool %q (%s). Regenerate with PP_UPDATE_SCHEMA_GOLDEN=1 go test ./internal/mcp/ -run Golden", spec.tool, path)
			continue
		}
		if string(want) != string(spec.schema) {
			t.Errorf("the advertised outputSchema for %q no longer matches its committed contract.\n"+
				"If the envelope changed on purpose, regenerate with PP_UPDATE_SCHEMA_GOLDEN=1 go test ./internal/mcp/ -run Golden and review the diff.\npath: %s", spec.tool, path)
		}
	}
	if update {
		t.Log("golden schemas rewritten")
	}
}

// Every committed schema must be a VALID JSON Schema. A malformed one is
// worse than none: a strict host rejects the whole tool listing.
func TestOutputSchemasAreValidJSONSchema(t *testing.T) {
	for _, spec := range allOutputSchemaSpecs() {
		if _, err := compileSchema(t, spec.tool, spec.schema); err != nil {
			t.Errorf("tool %q: schema does not compile: %v\n%s", spec.tool, err, spec.schema)
		}
	}
}

func compileSchema(t *testing.T, name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	url := "mem://" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// The seed the backlog asked for: what the tools actually emit must validate
// against what they advertise. Representative values are marshalled through
// the SAME structs the handlers return, so a reflector that mistypes a field
// (int64 rendered as "string", a pointer marked required) fails here rather
// than in an agent's parser.
func TestLiveEnvelopesValidateAgainstTheirSchemas(t *testing.T) {
	cases := []struct {
		tool  string
		value any
	}{
		{"search", mcpSearchResult{
			Count:       1,
			Results:     []json.RawMessage{json.RawMessage(`{"id":"a"}`)},
			StoreStatus: "ready",
		}},
		{"search", mcpSearchResult{Count: 0, Results: []json.RawMessage{}, StoreStatus: "empty", NextStep: "run sync"}},
		{"sql", mcpSQLResult{
			Count:       2,
			Columns:     []string{"model", "n"},
			Rows:        []map[string]any{{"model": "gemma", "n": 3}},
			StoreStatus: "ready",
		}},
		{"llamaswap_search", codeOrchSearchResult{
			Count: 1,
			Results: []codeOrchEndpointRef{{
				EndpointID: "models.list", Method: "GET", Path: "/v1/models",
				Summary: "Every CONFIGURED model", Score: 7,
			}},
		}},
		{"llamaswap_get", codeOrchEndpointRef{EndpointID: "models.list", Method: "GET", Path: "/v1/models", Summary: "roster"}},
	}
	schemas := map[string]json.RawMessage{}
	for _, s := range allOutputSchemaSpecs() {
		schemas[s.tool] = s.schema
	}
	for _, c := range cases {
		raw, ok := schemas[c.tool]
		if !ok {
			t.Fatalf("no schema registered for %q", c.tool)
		}
		sch, err := compileSchema(t, c.tool, raw)
		if err != nil {
			t.Fatalf("compiling %q: %v", c.tool, err)
		}
		body, err := json.Marshal(c.value)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(doc); err != nil {
			t.Errorf("%s: a value the handler really emits does not validate against the advertised schema:\n%v\nvalue: %s", c.tool, err, body)
		}
	}
}

// Every CLI envelope schema must compile and describe an object. These are
// the ones an agent reads most, and they are reflected from structs in
// another package, so a change there lands here.
func TestCLIEnvelopeSchemasDescribeObjects(t *testing.T) {
	got := cli.ResultSchemas()
	if len(got) == 0 {
		t.Fatal("internal/cli exposes no result schemas")
	}
	for _, rs := range got {
		if rs.Description == "" {
			t.Errorf("%s: no description; the schema is the contract and the contract needs a sentence", rs.Command)
		}
		var probe struct {
			Schema     string          `json:"$schema"`
			Type       string          `json:"type"`
			Properties json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(rs.Schema, &probe); err != nil {
			t.Errorf("%s: schema is not JSON: %v", rs.Command, err)
			continue
		}
		if probe.Type != "object" {
			t.Errorf("%s: root type = %q, want object", rs.Command, probe.Type)
		}
		if !strings.Contains(probe.Schema, "2020-12") {
			t.Errorf("%s: $schema = %q, want the 2020-12 draft", rs.Command, probe.Schema)
		}
		if len(probe.Properties) == 0 {
			t.Errorf("%s: schema declares no properties", rs.Command)
		}
	}
}

// The wiring: a registered tool must actually carry the schema, and its
// handler must attach structuredContent.
func TestRegisterOutputSchemasAttachesSchemaAndStructuredContent(t *testing.T) {
	s := server.NewMCPServer("test", "0")
	s.AddTool(
		mcplib.NewTool("search", mcplib.WithDescription("stub")),
		func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return mcplib.NewToolResultText(`{"count":0,"results":[],"store_status":"empty","resumable":false}`), nil
		},
	)
	registerOutputSchemas(s)

	tool := s.GetTool("search")
	if tool == nil {
		t.Fatal("the stub tool disappeared")
	}
	if len(tool.Tool.RawOutputSchema) == 0 {
		t.Fatal("no outputSchema was attached")
	}
	if !strings.Contains(string(tool.Tool.RawOutputSchema), `"store_status"`) {
		t.Errorf("outputSchema does not describe the envelope: %s", tool.Tool.RawOutputSchema)
	}
	res, err := tool.Handler(context.Background(), mcplib.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent == nil {
		t.Fatal("the wrapped handler returned no structuredContent")
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want an object", res.StructuredContent)
	}
	if sc["store_status"] != "empty" {
		t.Errorf("structuredContent lost a field: %+v", sc)
	}
	// The text content must survive untouched: hosts that predate
	// structuredContent still read it.
	if firstTextContent(res) == "" {
		t.Error("the text content was dropped")
	}
}

// A non-JSON result must pass through unchanged rather than being forced into
// a structured shape it does not have.
func TestStructuredContentSkipsNonJSONResults(t *testing.T) {
	h := withStructuredContent(func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return mcplib.NewToolResultText("# HELP llamacpp:prompt_tokens_total ...\n"), nil
	})
	res, err := h(context.Background(), mcplib.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent != nil {
		t.Errorf("prose was coerced into structuredContent: %+v", res.StructuredContent)
	}
	if !strings.Contains(firstTextContent(res), "llamacpp:prompt_tokens_total") {
		t.Error("the text result was altered")
	}
}

func TestStructuredContentSkipsErrors(t *testing.T) {
	h := withStructuredContent(func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		return mcplib.NewToolResultError(`{"ok":false}`), nil
	})
	res, err := h(context.Background(), mcplib.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.StructuredContent != nil {
		t.Error("an error result must not advertise structured success content")
	}
}
