package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/seatguard"
)

// An ask that names no route ran on this box's agent seat even when loading it
// would unload another vLLM seat holding the cards (on the reference box,
// opencode's three-card seat, 2026-10-06). The ask is self-contained, so it now
// takes route=auto in that case; an explicit route:"local" still runs here.
func TestOffloadAskWithoutARouteSparesAnOccupiedLocalSeat(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rules.md"), []byte("# Rules\n\nThe seat_idle_unload_ttl_seconds value is 300: the seat unloads after that many idle seconds.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = t.TempDir()
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentModel = "agent-pool"
	localCalls := 0
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "local",
			Structured: json.RawMessage(`{"answer":"300","evidence":"The seat unloads after 300 idle seconds."}`)}, nil
	}
	verdict := seatguard.Verdict{Protect: true, Seat: "opencode-seat"}
	var asked string
	s.askSeatGuard = func(_ context.Context, model string) seatguard.Verdict {
		asked = model
		return verdict
	}
	fleetCalls := 0
	var gotRoute string
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, _ []core.AgentContract, route string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		fleetCalls++
		gotRoute = route
		return []delegate.PlacedResult{{Node: "node-b", Seat: "seat-b", PlacementReason: "route=auto → node-b",
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "300 seconds", Steps: 1, Seat: "seat-b",
				Structured: json.RawMessage(`{"answer":"300 seconds","evidence":"The seat unloads after 300 idle seconds."}`)}}}, delegate.Summary{Succeeded: 1}, nil
	}
	// Each arm asks its own question: offload_ask caches an identical repeat, which would
	// answer the later arms without running anything.
	ask := func(question, extra string) map[string]any {
		t.Helper()
		res, err := s.handleAsk(context.Background(), callReq(`{"question":`+jsonString(question)+`,"paths":["rules.md"],"read_root":`+jsonString(root)+extra+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return decodeRouteResult(t, res)
	}

	// Occupied, no route named: the fleet answers and the response says why it moved.
	out := ask("after how many idle seconds does the seat unload?", "")
	if asked != "agent-pool" {
		t.Fatalf("the guard must be asked about the box's agent seat, got %q", asked)
	}
	if localCalls != 0 || fleetCalls != 1 || gotRoute != "auto" {
		t.Fatalf("local=%d fleet=%d route=%q: an occupied seat must send the ask through route=auto", localCalls, fleetCalls, gotRoute)
	}
	if note, _ := out["route_note"].(string); !strings.Contains(note, "would evict the loaded vLLM seat opencode-seat") || out["node"] != "node-b" {
		t.Fatalf("the response must name the occupant and where it ran: %v", out)
	}

	// An explicit route:"local" is the caller's choice and still runs here.
	localOut := ask("what number does the idle unload setting hold?", `,"route":"local"`)
	if localCalls != 1 || fleetCalls != 1 {
		t.Fatalf("route=local: local=%d fleet=%d, want 1/1: %v", localCalls, fleetCalls, localOut)
	}

	// Nothing occupied, and an unknown reading (no seat named): local, as before.
	for i, v := range []seatguard.Verdict{{}, {Protect: true, Stale: true}} {
		verdict = v
		before := localCalls
		out := ask(fmt.Sprintf("arm %d: how long is the idle unload ttl in seconds?", i), "")
		if localCalls != before+1 || fleetCalls != 1 {
			t.Fatalf("verdict %+v: local=%d fleet=%d, want the local seat", v, localCalls, fleetCalls)
		}
		if _, ok := out["route_note"]; ok {
			t.Fatalf("verdict %+v: a local ask carries no route_note: %v", v, out)
		}
	}
}
