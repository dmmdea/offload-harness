package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// Register C-46 (diagnosis S-27): agent_run and offload_ask always ran local,
// so a remote seat could not be named from this box's door. With `route` the
// call becomes one contract through the delegator's single-contract path, and
// the response names where it ran.

func routeServer(t *testing.T, local delegate.LocalRunner) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = t.TempDir()
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

func decodeRouteResult(t *testing.T, res any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(res)
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &env); err != nil || len(env.Content) == 0 {
		t.Fatalf("result has no text content: %s", b)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(env.Content[0].Text), &out); err != nil {
		t.Fatalf("result text is not JSON: %v\n%s", err, env.Content[0].Text)
	}
	return out
}

func TestAgentRunWithARouteRidesTheFleetAndNamesTheNode(t *testing.T) {
	localCalls := 0
	s := routeServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "local"}, nil
	})
	var gotRoute string
	var got []core.AgentContract
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, subtasks []core.AgentContract, route string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		gotRoute, got = route, subtasks
		return []delegate.PlacedResult{{Node: "node-b", Seat: "seat-b", PlacementReason: "route=remote forced → node-b",
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "from the fleet", Steps: 3, Seat: "seat-b", StopReason: "final"}}}, delegate.Summary{Succeeded: 1}, nil
	}
	res, err := s.handleAgentRun(context.Background(), callReq(`{"goal":"summarize the standing rules in one line","route":"remote","max_steps":4,"profile":"research"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRouteResult(t, res)
	if localCalls != 0 {
		t.Fatalf("route=remote must not run the local loop (%d calls)", localCalls)
	}
	if gotRoute != "remote" || len(got) != 1 || got[0].Goal != "summarize the standing rules in one line" || got[0].MaxSteps != 4 || got[0].Profile != "research" || got[0].Door != "agent_run" || !got[0].TimeoutAuto {
		t.Fatalf("the contract must carry the call's goal, steps, profile, door and an auto wall: route=%q %+v", gotRoute, got)
	}
	if out["output"] != "from the fleet" || out["node"] != "node-b" || out["seat"] != "seat-b" || out["route"] != "remote" || out["executed_on"] != "fleet" || !strings.Contains(out["placement"].(string), "node-b") {
		t.Fatalf("the response must name where it ran: %v", out)
	}
}

func TestOffloadAskWithARouteRidesTheFleet(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rules.md"), []byte("# Rules\n\nThe seat_idle_unload_ttl_seconds value is 300: the seat unloads after that many idle seconds. The gpu_lease_queue_rule says a held card is a place in line.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	localCalls := 0
	s := routeServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "local"}, nil
	})
	var gotRoute string
	var got []core.AgentContract
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, subtasks []core.AgentContract, route string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		gotRoute, got = route, subtasks
		return []delegate.PlacedResult{{Node: "node-b", Seat: "seat-b", PlacementReason: "route=auto → node-b",
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "300 seconds", Steps: 1, Seat: "seat-b",
				Structured: json.RawMessage(`{"answer":"300 seconds","evidence":"The seat unloads after 300 seconds idle."}`)}}}, delegate.Summary{Succeeded: 1}, nil
	}
	res, err := s.handleAsk(context.Background(), callReq(`{"question":"after how many idle seconds does the seat unload?","paths":["rules.md"],"read_root":`+jsonString(root)+`,"route":"auto"}`))
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRouteResult(t, res)
	if localCalls != 0 {
		t.Fatalf("a routed ask must not run the local seat (%d calls)", localCalls)
	}
	if gotRoute != "auto" || len(got) != 1 || got[0].Door != "offload_ask" || len(got[0].Context) == 0 {
		t.Fatalf("the ask contract must ride the route with its files inline: route=%q %+v out=%v", gotRoute, got, out)
	}
	if out["answer"] != "300 seconds" || out["node"] != "node-b" || out["executed_on"] != "fleet" {
		t.Fatalf("the response must carry the answer and where it ran: %v", out)
	}
	// Without a route the local seat answers, as before.
	res, err = s.handleAsk(context.Background(), callReq(`{"question":"what is the lease?","paths":["rules.md"],"read_root":`+jsonString(root)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if localCalls != 1 {
		t.Fatalf("no route = the local seat, got %d local calls", localCalls)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
