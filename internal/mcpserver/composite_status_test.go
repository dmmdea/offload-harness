package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// shippedComposite is the composite box as the TIER TABLE ships it: the
// fixture's layers minus the three-card `triple` layer, which blackwell-3x16
// stopped declaring when the 2026-09-10 rule (RAM is overflow only) removed
// its only seat. Status must be tested against what a box actually seeds, and
// this shape is also hermetic: no layer with a live guard is placeable, so a
// status call reads seats over HTTP and never execs nvidia-smi.
func shippedComposite() config.Config {
	c := config.CompositeFixture()
	kept := c.Layers[:0:0]
	for _, l := range c.Layers {
		if l.Name != "triple" {
			kept = append(kept, l)
		}
	}
	c.Layers = kept
	return c
}

// coldSwap is a llama-swap that lists every seat and reports NOTHING running.
// It fails the test by name on any path that could start a model.
func coldSwap(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			data := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				data = append(data, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.URL.Path == "/running":
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			if strings.HasPrefix(p, "/upstream/") || strings.Contains(p, "/chat/completions") {
				t.Errorf("a status call hit %q — a path that can auto-start a seat; layer rows must never load a model", p)
			}
		}
	})
	return srv
}

// TestStatusReportsLayersWithoutLoadingAnySeat is the composite half of the
// status contract: a box that seeds layers publishes its identity (tier_profile,
// tiers) and one row per layer with the live occupancy of every seat — and
// still costs no model load. The dormant display layer reports itself
// inadmissible for the reason that actually holds it closed (the operator),
// never a guard it never reached.
func TestStatusReportsLayersWithoutLoadingAnySeat(t *testing.T) {
	swap := coldSwap(t, "agent-pool", "qwen3.8-27b-262k", "gemma-4-26b-agent", "qwen3-vl-8b", "qwen3-vl-32b")
	cfg := shippedComposite()
	cfg.Endpoint = swap.URL
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	m := decodeResult(t, res)
	local, _ := m["local"].(map[string]any)
	if local == nil {
		t.Fatalf("no local section: %v", m)
	}
	if local["tier_profile"] != "blackwell-3x16" {
		t.Fatalf("local.tier_profile = %v", local["tier_profile"])
	}
	tiers, _ := local["tiers"].([]any)
	if len(tiers) != 3 || tiers[0] != "blackwell-16" || tiers[2] != "blackwell-3x16" {
		t.Fatalf("local.tiers = %v", local["tiers"])
	}
	layers, _ := local["layers"].([]any)
	if len(layers) != len(cfg.Layers) {
		t.Fatalf("local.layers = %d rows, want %d: %v", len(layers), len(cfg.Layers), local["layers"])
	}
	rows := map[string]map[string]any{}
	for _, raw := range layers {
		row, _ := raw.(map[string]any)
		name, _ := row["name"].(string)
		rows[name] = row
	}
	pair := rows["pair"]
	if pair == nil {
		t.Fatalf("no pair row: %v", layers)
	}
	if pair["admissible"] != true {
		t.Fatalf("an unguarded layer is admissible: %v", pair)
	}
	seats, _ := pair["seats"].([]any)
	var agent map[string]any
	for _, raw := range seats {
		s, _ := raw.(map[string]any)
		if s["role"] == "agent" {
			agent = s
		}
	}
	if agent == nil {
		t.Fatalf("pair row has no agent seat: %v", pair)
	}
	// Known (the roster answered) but NOT loaded (nothing is running): the two
	// states a cold seat must be distinguishable in.
	if agent["known"] != true || agent["loaded"] == true {
		t.Fatalf("cold pair/agent must report known=true, loaded=false: %v", agent)
	}
	display := rows["display"]
	if display == nil || display["admissible"] != false {
		t.Fatalf("the dormant display layer must publish admissible=false: %v", display)
	}
	if reason, _ := display["reason"].(string); !strings.Contains(reason, "dormant") {
		t.Fatalf("the dormant layer's reason must name the operator's decision, got %q", reason)
	}
}

// TestStatusOnAPlainBoxCarriesNoLayerKeys is the byte-identity half: a box that
// seeds no layers publishes not one new key, so a pre-composite reader sees the
// payload it always saw.
func TestStatusOnAPlainBoxCarriesNoLayerKeys(t *testing.T) {
	swap := coldSwap(t, "offload-e4b")
	cfg := config.Default()
	cfg.Endpoint = swap.URL
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	local, _ := decodeResult(t, res)["local"].(map[string]any)
	for _, k := range []string{"tier_profile", "tiers", "layers"} {
		if _, present := local[k]; present {
			t.Fatalf("a plain box must not publish local.%s: %v", k, local[k])
		}
	}
}

// TestAgentRunPublishesPlacedOnACompositeBoxAndGuardsAnExplicitOptInModel pins
// the one thing an explicit `model` must never do: walk past a layer's guards.
// A seat that belongs to an opt-in layer is admitted only if that layer would
// admit it right now — the dormant display twin is refused, with the placement
// block saying which layer and why, before any seat is touched.
func TestAgentRunPublishesPlacedOnACompositeBoxAndGuardsAnExplicitOptInModel(t *testing.T) {
	swap := coldSwap(t, "agent-pool", "gemma-4-e4b-display")
	cfg := shippedComposite()
	cfg.Endpoint = swap.URL
	s := New(pipeline.New(cfg, nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(
		`{"goal":"summarize this","model":"gemma-4-e4b-display"}`))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a dormant layer's seat must defer, got %v", m)
	}
	placed, _ := m["placed"].(map[string]any)
	if placed == nil {
		t.Fatalf("the defer must carry the placement block: %v", m)
	}
	if placed["layer"] != "display" {
		t.Fatalf("placed.layer = %v, want the layer that refused", placed["layer"])
	}
	if reason, _ := placed["reason"].(string); !strings.Contains(reason, "dormant") {
		t.Fatalf("placed.reason must say why, got %q", reason)
	}

	// Awake but guarded: operator_presence defaults to present, so the display
	// card refuses by NAME — the guard, not a generic capacity message.
	//
	// The layer is narrowed to the PRESENCE guard alone for this case, and that
	// is not a convenience: display_floor is evaluated first and reads the live
	// cards, so on a machine with no nvidia-smi (CI) it refuses as unreadable
	// and on a machine with cards it depends on what is loaded right now. A
	// test that asserts which guard refused must not depend on the host it runs
	// on — the fail-closed floor has its own tests over injected readings.
	awake := shippedComposite()
	awake.Endpoint = swap.URL
	for i := range awake.Layers {
		if awake.Layers[i].Name == "display" {
			awake.Layers[i].Dormant = false
			awake.Layers[i].Guards = []string{"presence"}
		}
	}
	s = New(pipeline.New(awake, nil, nil, nil))
	res, err = s.handleAgentRun(context.Background(), callReq(
		`{"goal":"summarize this","model":"gemma-4-e4b-display"}`))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m = decodeResult(t, res)
	placed, _ = m["placed"].(map[string]any)
	if m["deferred"] != true || placed == nil || placed["guard"] != "presence" {
		t.Fatalf("with the operator present the display card refuses on the presence guard, got %v", m)
	}

	// A closed vocabulary, checked on this door too.
	res, err = s.handleAgentRun(context.Background(), callReq(`{"goal":"g","context_class":"huge"}`))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m = decodeResult(t, res)
	if m["deferred"] != true || !strings.Contains(m["reason"].(string), "context_class") {
		t.Fatalf("an unknown context_class must defer naming the field, got %v", m)
	}

	// The guard is COMPOSITE-only: a plain box places nothing and the same
	// call falls through to the roster check it has always hit. Its own
	// llama-swap does not serve the twin, so the run dies there — proving the
	// refusal came from the roster and not from a layer.
	plain := config.Default()
	plain.Endpoint = coldSwap(t, "offload-e4b").URL
	s = New(pipeline.New(plain, nil, nil, nil))
	res, err = s.handleAgentRun(context.Background(), callReq(
		`{"goal":"summarize this","model":"gemma-4-e4b-display"}`))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m = decodeResult(t, res)
	if _, present := m["placed"]; present {
		t.Fatalf("a plain box must publish no placed block: %v", m)
	}
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "served roster") {
		t.Fatalf("the plain box's refusal is the roster's, got %q", reason)
	}
}

// TestAgentDelegateAcceptsContextClass: the field reaches the contract the
// local runner executes (so a long ask can be placed), and an unknown value
// defers naming the field instead of being dropped on the floor.
func TestAgentDelegateAcceptsContextClass(t *testing.T) {
	var got core.AgentContract
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		got = c
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "ok", Structured: json.RawMessage(`{"answer":"ok"}`), Steps: 1, StopReason: "done",
		}, nil
	})
	if _, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"read it","context_class":"long","output_schema":{"properties":{"answer":{"type":"string"}}}}],"route":"local"}`)); err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	if got.ContextClass != core.ContextClassLong {
		t.Fatalf("context_class must ride the contract, got %q", got.ContextClass)
	}

	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"read it","context_class":"huge","output_schema":{"properties":{"answer":{"type":"string"}}}}],"route":"local"}`))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, "context_class") {
		t.Fatalf("an unknown context_class must defer naming the field, got %v", m)
	}
}

// TestCompositeDoorsAdmitTheBoxesOwnContextCap: a composite box raises the
// per-contract context ceiling to its largest window (chars/3 would otherwise
// make its 262k seat unreachable — a 256 KiB contract estimates ~87k tokens),
// and a plain box keeps the 256 KiB cap exactly.
func TestCompositeDoorsAdmitTheBoxesOwnContextCap(t *testing.T) {
	big := strings.Repeat("x", 300<<10)
	args := func() string {
		b, err := json.Marshal(map[string]any{
			"subtasks": []any{map[string]any{
				"goal":          "read it",
				"context":       []any{map[string]any{"name": "big.txt", "text": big}},
				"output_schema": map[string]any{"properties": map[string]any{"answer": map[string]any{"type": "string"}}},
			}},
			"route": "local",
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}()

	var ran int
	s := delegateTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		ran++
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "ok", Structured: json.RawMessage(`{"answer":"ok"}`), Steps: 1, StopReason: "done",
		}, nil
	})
	res, err := s.handleAgentDelegate(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || !strings.Contains(m["reason"].(string), "exceeds") {
		t.Fatalf("a plain box must refuse 300 KiB of context, got %v", m)
	}
	if ran != 0 {
		t.Fatalf("the refused contract must never reach the seat (ran %d times)", ran)
	}

	home := t.TempDir()
	cfg := shippedComposite()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	s2 := New(pipeline.New(cfg, nil, nil, nil))
	s2.localAgent = func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		ran++
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
			Output: "ok", Structured: json.RawMessage(`{"answer":"ok"}`), Steps: 1, StopReason: "done",
		}, nil
	}
	if _, err := s2.handleAgentDelegate(context.Background(), callReq(args)); err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	if ran != 1 {
		t.Fatalf("a composite box admits the contract at its own cap (ran %d times)", ran)
	}
}
