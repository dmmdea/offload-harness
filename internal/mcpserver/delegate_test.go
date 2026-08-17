// Task 6 (multi-node agent delegation): the agent_delegate MCP surface.
// Two load-bearing pins: (1) roast delta 13 — the tool registers ONLY when
// agent_delegation_enabled, and the flag adds EXACTLY that one tool (tools/list
// byte-identical otherwise); (2) roast delta 14 — the response leads with the
// summary (the raw JSON literally starts with "summary"), so eight quiet
// defers read as a loud outcome.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// listTools connects a real MCP client to the built server over an in-memory
// transport and returns the advertised tool list — the same view Claude gets.
func listTools(t *testing.T, cfg config.Config) []*mcp.Tool {
	t.Helper()
	srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "pin", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	return res.Tools
}

func TestAgentDelegateRegistrationGated(t *testing.T) {
	off := listTools(t, config.Default()) // agent_delegation_enabled defaults false
	for _, tool := range off {
		if tool.Name == "agent_delegate" {
			t.Fatal("agent_delegate advertised with agent_delegation_enabled OFF")
		}
	}

	cfgOn := config.Default()
	cfgOn.AgentDelegationEnabled = true
	on := listTools(t, cfgOn)

	// The flag must add EXACTLY agent_delegate and change nothing else:
	// strip it from the on-list and byte-compare the remainder to the
	// off-list (the delta-13 "tools/list byte-identical when off" pin).
	var stripped []*mcp.Tool
	found := false
	for _, tool := range on {
		if tool.Name == "agent_delegate" {
			found = true
			continue
		}
		stripped = append(stripped, tool)
	}
	if !found {
		t.Fatal("agent_delegate not advertised with agent_delegation_enabled ON")
	}
	offJSON, err := json.Marshal(off)
	if err != nil {
		t.Fatal(err)
	}
	strippedJSON, err := json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("enabling agent_delegation changed the tool list beyond adding agent_delegate")
	}
}

// delegateTestServer builds a Server with the delegation flag on, every side
// effect rooted in a temp dir, and a fake local runner injected through the
// localAgent seam.
func delegateTestServer(t *testing.T, local func(context.Context, core.AgentContract) (core.AgentWireResult, error)) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

func TestAgentDelegateHandlerLocalHappyPath(t *testing.T) {
	var gotContract core.AgentContract
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		gotContract = c
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			NodeID:        "this-box",
			Seat:          "fake-seat",
			Output:        "done on qube",
			Structured:    json.RawMessage(`{"answer":"42"}`),
			Steps:         1,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"answer it","output_schema":{"properties":{"answer":{"type":"string"}}},"acceptance":["contains:qube","nonempty:answer"],"max_steps":99}],"route":"local"}`))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}

	// Summary-first pin (roast delta 14): the raw JSON leads with the tally.
	raw := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(raw, `{"summary"`) {
		t.Fatalf("response must lead with the summary, got %q…", raw[:min(len(raw), 40)])
	}

	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary == nil || summary["succeeded"] != float64(1) {
		t.Fatalf("summary = %v, want one success", m["summary"])
	}
	results, _ := m["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", m["results"])
	}
	r0, _ := results[0].(map[string]any)
	if r0["node"] != "this-box" || r0["seat"] != "fake-seat" || r0["output"] != "done on qube" {
		t.Errorf("result = %v", r0)
	}
	if r0["deferred"] == true || r0["failed"] == true {
		t.Errorf("happy path marked deferred/failed: %v", r0)
	}

	// The handler's intake must have minted version/depth and clamped steps.
	if gotContract.SchemaVersion != core.AgentWireSchemaVersion || gotContract.Depth != 0 {
		t.Errorf("contract version/depth = %d/%d", gotContract.SchemaVersion, gotContract.Depth)
	}
	if gotContract.MaxSteps != core.AgentMaxStepsCap {
		t.Errorf("max_steps = %d, want clamped to %d", gotContract.MaxSteps, core.AgentMaxStepsCap)
	}
}

// TestAgentDelegateHandlerAcceptanceFailure: a schema-valid local result that
// fails acceptance surfaces as failed_verification with the check named —
// through the FULL handler path, not just the engine.
func TestAgentDelegateHandlerAcceptanceFailure(t *testing.T) {
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
			Output: "wrong content", Structured: json.RawMessage(`{"answer":"x"}`), StopReason: "done"}, nil
	})
	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"answer it","acceptance":["contains:qube"]}],"route":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["failed_verification"] != float64(1) || summary["succeeded"] != float64(0) {
		t.Fatalf("summary = %v, want the acceptance failure counted", summary)
	}
}

func TestAgentDelegateHandlerBadInputsDefer(t *testing.T) {
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		t.Error("local runner must not run on a refused request")
		return core.AgentWireResult{}, nil
	})
	nine := strings.Repeat(`{"goal":"g"},`, 8) + `{"goal":"g"}`
	cases := []struct{ name, args, want string }{
		{"no subtasks", `{"subtasks":[]}`, "subtask"},
		{"nine subtasks", `{"subtasks":[` + nine + `]}`, "8"},
		{"bad route", `{"subtasks":[{"goal":"g"}],"route":"sideways"}`, "route"},
		{"public remote", `{"subtasks":[{"goal":"g"}],"remotes":["https://api.evil.com"]}`, "api.evil.com"},
		{"invalid acceptance", `{"subtasks":[{"goal":"g","acceptance":["contains:"]}]}`, "acceptance"},
		{"missing goal", `{"subtasks":[{"context":[{"name":"a","text":"b"}]}]}`, "goal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleAgentDelegate(context.Background(), callReq(tc.args))
			if err != nil {
				t.Fatalf("MCP error (want deferred-shape): %v", err)
			}
			m := decodeResult(t, res)
			if m["deferred"] != true {
				t.Fatalf("want deferred:true, got %v", m)
			}
			if reason, _ := m["reason"].(string); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason %q must mention %q", reason, tc.want)
			}
		})
	}
}
