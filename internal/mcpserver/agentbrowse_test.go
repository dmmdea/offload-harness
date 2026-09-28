package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func TestBrowseDoorRefusalIsTheOneAdmissionRule(t *testing.T) {
	ok := browseCfg()
	ok.AgentAllowBrowse = true
	for _, route := range []string{"", "local"} {
		if r := browseDoorRefusal(ok, route, true, []string{"example.com"}); r != "" {
			t.Fatalf("route %q on an opted-in, configured node with hosts must admit, got %q", route, r)
		}
	}
	for name, tc := range map[string]struct {
		route string
		hosts []string
		want  string
	}{
		"auto route":   {"auto", []string{"example.com"}, "route"},
		"spread route": {"spread", []string{"example.com"}, "route"},
		"queue route":  {"queue", []string{"example.com"}, "route"},
		"no hosts":     {"local", nil, "browse_hosts"},
	} {
		if r := browseDoorRefusal(ok, tc.route, true, tc.hosts); !strings.Contains(r, tc.want) {
			t.Errorf("%s: refusal %q does not name %s", name, r, tc.want)
		}
	}
	if r := browseDoorRefusal(browseCfg(), "local", true, []string{"example.com"}); !strings.Contains(r, "agent_allow_browse") {
		t.Errorf("a node without the opt-in must refuse naming agent_allow_browse, got %q", r)
	}
	unconf := browseCfg()
	unconf.AgentAllowBrowse, unconf.BrowseDecisionURL = true, ""
	if r := browseDoorRefusal(unconf, "local", true, []string{"example.com"}); !strings.Contains(r, "not configured") {
		t.Errorf("an unconfigured lane must refuse, got %q", r)
	}
}

func browseCallText(t *testing.T, h func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error), args any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: b}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

// agent_delegate admits a browse subtask only on route local: auto (the default)
// could place it on another node's browser.
func TestAgentDelegateRefusesBrowseOffTheLocalRoute(t *testing.T) {
	cfg := browseCfg()
	cfg.AgentAllowBrowse = true
	s := New(pipeline.New(cfg, nil, nil, nil))
	text := browseCallText(t, s.handleAgentDelegate, map[string]any{"route": "auto", "subtasks": []any{
		map[string]any{"goal": "read it", "allow_browse": true, "browse_hosts": []string{"example.com"}}}})
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "route") || !strings.Contains(text, "subtask 0") {
		t.Errorf("want a route refusal for subtask 0, got %s", text)
	}
}

// agent_delegate reads an omitted route as auto (delegate.RunWith), which may place
// the contract on another node's browser: a browse subtask must name local.
func TestAgentDelegateRefusesBrowseOnAnOmittedRoute(t *testing.T) {
	cfg := browseCfg()
	cfg.AgentAllowBrowse = true
	s := New(pipeline.New(cfg, nil, nil, nil))
	text := browseCallText(t, s.handleAgentDelegate, map[string]any{"subtasks": []any{
		map[string]any{"goal": "read it", "allow_browse": true, "browse_hosts": []string{"example.com"}}}})
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "omitted route means auto") {
		t.Errorf("an omitted delegation route must be refused for browse, got %s", text)
	}
	if r := browseDoorRefusal(cfg, "", false, []string{"example.com"}); r == "" {
		t.Error("emptyIsLocal=false must refuse an omitted route")
	}
	if r := browseDoorRefusal(cfg, "", true, []string{"example.com"}); r != "" {
		t.Errorf("emptyIsLocal=true (agent_run) must admit an omitted route, got %q", r)
	}
}

func TestAgentRunRefusesBrowseWithoutOptInOrOffLocal(t *testing.T) {
	s := New(pipeline.New(browseCfg(), nil, nil, nil)) // lane configured, node NOT opted in
	text := browseCallText(t, s.handleAgentRun, map[string]any{"goal": "g", "allow_browse": true, "browse_hosts": []string{"example.com"}})
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "agent_allow_browse") {
		t.Errorf("want an opt-in refusal, got %s", text)
	}
	cfg := browseCfg()
	cfg.AgentAllowBrowse = true
	s = New(pipeline.New(cfg, nil, nil, nil))
	text = browseCallText(t, s.handleAgentRun, map[string]any{"goal": "g", "route": "remote", "allow_browse": true, "browse_hosts": []string{"example.com"}})
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "route") {
		t.Errorf("want a route refusal, got %s", text)
	}
	text = browseCallText(t, s.handleAgentRun, map[string]any{"goal": "g", "browse_hosts": []string{"example.com"}})
	if !strings.Contains(text, "allow_browse is not") {
		t.Errorf("hosts without allow_browse must be refused, got %s", text)
	}
}
