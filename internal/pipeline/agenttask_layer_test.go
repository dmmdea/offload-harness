// Composite tier (ADR 0039, Task 6): the node-side agent task runs the seat a
// placement DECIDED, not the planner default — through the seat override a
// delegator hands it (RunAgentContract's options) or through the layer a
// dispatched contract names (contract.layer), which the node re-runs through
// placement.DecideOnLayer with its OWN live readers so the display-card
// guards are evaluated where the card is (council R5/R6). Every result stamps
// `placed`; a plain box stamps nothing (the byte-identical constraint).
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// compositeTestPipeline is agentContractPipeline over config.CompositeFixture:
// the pair's agent seat is the scripted test seat and its long seat is
// "long-seat", so window overflow inside the pair layer can be asserted by
// name. Live readers are injected through the placementLive seam: the seat
// reader answers "unknown" (nothing loaded, nothing to wait on), the display
// card has 16 GiB free, host RAM 100 GiB, and presence follows the config's
// mode exactly as the production probe would (present is the default).
func compositeTestPipeline(t *testing.T, base string) *Pipeline {
	t.Helper()
	cfg := config.CompositeFixture()
	cfg.Home = t.TempDir()
	cfg.Endpoint = base
	cfg.Model = "workhorse"
	cfg.AgentModel = agentTestSeat
	cfg.FleetNodeID = "node-t"
	cfg.Temperature = 0.1
	for i := range cfg.Layers {
		if cfg.Layers[i].Name != placement.LayerPair {
			continue
		}
		for j := range cfg.Layers[i].Seats {
			switch cfg.Layers[i].Seats[j].Role {
			case placement.RoleAgent:
				cfg.Layers[i].Seats[j].Model = agentTestSeat
			case placement.RoleLong:
				cfg.Layers[i].Seats[j].Model = "long-seat"
			}
		}
	}
	if err := cfg.ValidateLayers(); err != nil {
		t.Fatalf("fixture must validate: %v", err)
	}
	p := New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
	p.placementLive = func() placement.Live {
		return placement.Live{
			Seat:        func(string, string) placement.SeatState { return placement.SeatState{} },
			DeviceFree:  func(string) (float64, bool) { return 16, true },
			DeviceIndex: func(d string) (string, bool) { return d, true },
			HostFree:    func() (float64, bool) { return 100, true },
			Presence:    func() placement.Presence { return placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle()) },
		}
	}
	return p
}

func TestRunAgentTaskHonoursTheSeatOverrideAndStampsPlaced(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat, "long-seat"},
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, _ := agentContractPipeline(t, srv.URL)
	want := &core.Placed{Layer: "pair", Role: "long", Seat: "long-seat", Reason: "test"}
	wire, err := p.RunAgentContract(context.Background(), testContract(), AgentContractOptions{Seat: "long-seat", Placed: want})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.Seat != "long-seat" || wire.Placed == nil || wire.Placed.Layer != "pair" || wire.Placed.Seat != "long-seat" {
		t.Fatalf("wire = %+v (placed %+v)", wire, wire.Placed)
	}
}

func TestRunAgentTaskWithoutOptionsIsThePlannerSeatAndNoPlaced(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, _ := agentContractPipeline(t, srv.URL)
	wire, err := p.RunAgentContract(context.Background(), testContract(), AgentContractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.Seat != agentTestSeat || wire.Placed != nil {
		t.Fatalf("a plain box must run the planner seat and publish no placed block; wire = %+v", wire)
	}
}

// TestNodeHonoursARequestedLayerThroughDecideAndItsOwnGuards: a contract
// dispatched with `layer` is re-decided on the node. A pair contract whose
// estimate overflows the agent window lands on the pair's LONG seat (row 6);
// a triple contract with context_class long is refused by the presence guard
// because this box's operator_presence is unset (= present) — the delegator's
// word is not enough for the display card.
func TestNodeHonoursARequestedLayerThroughDecideAndItsOwnGuards(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat, "long-seat", "qwen3.8-flash-next-262k"},
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := compositeTestPipeline(t, srv.URL)

	t.Run("pair overflow → the pair's long seat", func(t *testing.T) {
		contract := testContract()
		contract.Layer = placement.LayerPair
		// ~200k tokens at chars/3: past the agent seat's 163,840 window,
		// inside the long seat's 262,144.
		contract.Context = []core.ContextDoc{{Name: "big.md", Text: strings.Repeat("lorem ", 100_000)}}
		res := p.Run(context.Background(), agentTestRequest(t, contract))
		wire := decodeWire(t, res)
		if wire.Deferred {
			t.Fatalf("deferred: %s", wire.Reason)
		}
		if wire.Seat != "long-seat" {
			t.Fatalf("seat = %q, want the pair's long seat (placed %+v)", wire.Seat, wire.Placed)
		}
		if wire.Placed == nil || wire.Placed.Layer != placement.LayerPair || wire.Placed.Role != placement.RoleLong || wire.Placed.Seat != "long-seat" {
			t.Fatalf("placed = %+v", wire.Placed)
		}
		if res.Meta.Placed == nil || res.Meta.Placed.Layer != placement.LayerPair {
			t.Fatalf("meta.Placed = %+v; the ledger row must carry the layer", res.Meta.Placed)
		}
	})

	t.Run("triple long → refused by the node's own presence guard", func(t *testing.T) {
		// A real ledger: the row a guard refusal writes is the surface council
		// R8 sums by `layer`, so its shape is pinned here, not inferred from
		// the wire.
		ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
		led, err := ledger.Open(ledgerPath)
		if err != nil {
			t.Fatal(err)
		}
		p.led = led
		defer func() { p.led = nil; _ = led.Close() }()

		contract := testContract()
		contract.Layer = placement.LayerTriple
		contract.ContextClass = core.ContextClassLong
		contract.TimeoutSec = 600 // the feasibility rule must pass so the GUARDS are what refuse
		res := p.Run(context.Background(), agentTestRequest(t, contract))
		wire := decodeWire(t, res)
		if !wire.Deferred {
			t.Fatalf("the display card must stay closed while operator_presence is unset; wire = %+v", wire)
		}
		if wire.DeferClass != core.DeferClassCapacity {
			t.Fatalf("defer_class = %q, want capacity (a guard refused right now)", wire.DeferClass)
		}
		if wire.Placed == nil || wire.Placed.Guard != "presence" || wire.Placed.Layer != placement.LayerTriple {
			t.Fatalf("placed = %+v, want the presence guard named on the triple layer", wire.Placed)
		}
		if fake.loopCalls.Load() != 1 {
			t.Fatalf("the refused contract must never reach the planner (loop calls = %d, want the 1 from the pair case)", fake.loopCalls.Load())
		}
		// The defer is about the seat the guard refused, on the wire and in the
		// row alike — never the planner default, which never saw the contract.
		const refusedSeat = "qwen3.8-flash-next-262k"
		if wire.Seat != refusedSeat || wire.Placed.Seat != refusedSeat {
			t.Fatalf("wire.seat = %q / placed.seat = %q, want the refused triple seat %q", wire.Seat, wire.Placed.Seat, refusedSeat)
		}
		if res.Meta.Placed == nil || res.Meta.Placed.Layer != placement.LayerTriple || res.Meta.Placed.Guard != "presence" || res.Meta.Model != refusedSeat {
			t.Fatalf("meta.Placed = %+v / meta.Model = %q: the refusal must reach the ledger row with its layer, guard and seat", res.Meta.Placed, res.Meta.Model)
		}
		raw, err := os.ReadFile(ledgerPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
		var row struct {
			Layer     string `json:"layer"`
			ModelTier string `json:"model_tier"`
			Deferred  bool   `json:"deferred"`
			Reason    string `json:"reason"`
		}
		if err := json.Unmarshal(lines[len(lines)-1], &row); err != nil {
			t.Fatalf("ledger row: %v (%s)", err, lines[len(lines)-1])
		}
		if !row.Deferred || row.Layer != placement.LayerTriple || row.ModelTier != refusedSeat || !strings.Contains(row.Reason, "presence") {
			t.Fatalf("ledger row = %+v, want deferred=true layer=triple model_tier=%s naming the presence guard", row, refusedSeat)
		}
	})

	t.Run("an undeclared layer is a contract defer", func(t *testing.T) {
		contract := testContract()
		contract.Layer = "quad"
		wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
		if !wire.Deferred || wire.DeferClass != core.DeferClassContract {
			t.Fatalf("wire = %+v", wire)
		}
	})
}

// TestPlainBoxIgnoresAContractLayer: a node whose config declares no layers
// has one implicit layer — a dispatched `layer` changes nothing and no placed
// block appears (byte-identical to the pre-layer build).
func TestPlainBoxIgnoresAContractLayer(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("ok") },
		repack:    func(int64) string { return `{"answer":"ok"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.Layer = placement.LayerTriple
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred || wire.Seat != agentTestSeat || wire.Placed != nil {
		t.Fatalf("wire = %+v", wire)
	}
}
