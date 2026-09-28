package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func browseCfg() config.Config {
	c := config.Default()
	c.BrowsePython = "/opt/offload/browse/.venv/bin/python"
	c.BrowseScript = "/opt/offload/browse/runner.py"
	c.BrowseDecisionURL = "http://127.0.0.1:18720/v1/systemone"
	return c
}

// offload_browse is an opt-in surface that drives the operator's browser: it must be
// absent from a box that never configured it, and adding it must change nothing else.
func TestBrowseRegistrationGated(t *testing.T) {
	off := listTools(t, config.Default())
	for _, tool := range off {
		if tool.Name == "offload_browse" {
			t.Fatal("offload_browse advertised on a box that never configured the lane")
		}
	}
	provider := browseCfg()
	provider.BrowseDecisionURL = "https://api.typesafe.ai/v1/systemone"
	for _, tool := range listTools(t, provider) {
		if tool.Name == "offload_browse" {
			t.Fatal("offload_browse advertised with a non-loopback decision endpoint")
		}
	}
	on := listTools(t, browseCfg())
	var stripped []*mcp.Tool
	found := false
	for _, tool := range on {
		if tool.Name == "offload_browse" {
			found = true
			if !strings.Contains(tool.Description, "LOOPBACK decision endpoint") || !strings.Contains(tool.Description, "not proof") {
				t.Errorf("the description must name the loopback endpoint and that DONE is not proof: %q", tool.Description)
			}
			continue
		}
		stripped = append(stripped, tool)
	}
	if !found {
		t.Fatal("offload_browse missing on a configured box")
	}
	a, _ := json.Marshal(stripped)
	b, _ := json.Marshal(off)
	if string(a) != string(b) {
		t.Error("configuring the browse lane changed tools other than offload_browse")
	}
}

// A route the lane cannot honour is refused at the door, before the pipeline runs.
func TestBrowseDoorRefusesNonLocalRoute(t *testing.T) {
	s := New(pipeline.New(browseCfg(), nil, nil, nil))
	args, _ := json.Marshal(map[string]any{"url": "https://example.com/", "goal": "x", "route": "remote"})
	res, err := s.handleBrowse(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "route must be local") {
		t.Errorf("want a route defer, got %s", text)
	}
}

// The door never lets a caller set `unattended`: that flag belongs to the agent doors,
// and forwarding it here would let an MCP caller dodge nothing but also confuse audit.
func TestBrowseDoorDoesNotForwardUnattended(t *testing.T) {
	s := New(pipeline.New(browseCfg(), nil, nil, nil))
	args, _ := json.Marshal(map[string]any{"url": "file:///x", "goal": "x", "unattended": true})
	res, err := s.handleBrowse(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "BAD_INPUT") || strings.Contains(text, "allow_hosts") {
		t.Errorf("an MCP call must be judged as attended (bad URL, not a missing host list): %s", text)
	}
}
