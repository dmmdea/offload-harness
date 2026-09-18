package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// oneCardRows is the ampere-16 Lenovo's advertised rows once it declares a
// `single` layer (the 27B GSQ planner default) and a `fast` layer (the 35B
// digest seat) on its one card — register A-100.
func oneCardRows(t *testing.T) []placetable.LayerRow {
	t.Helper()
	cfg := config.Default()
	cfg.TierProfile = "ampere-16"
	cfg.Layers = []config.LayerSpec{
		{Name: placetable.LayerSingle, Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: placetable.RoleAgent, Model: "qwen38-27b-gsq-vllm", Device: "0", CtxTokens: 32768},
		}},
		{Name: "fast", Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: placetable.RoleAgent, Model: "qwen36-35b-a3b-gsq-vllm", Device: "0", CtxTokens: 32768, MaxInflight: 8},
		}},
	}
	if err := cfg.ValidateLayers(); err != nil {
		t.Fatalf("one-card layers must validate: %v", err)
	}
	live := placetable.Live{
		Seat:        func(string, string) placetable.SeatState { return placetable.SeatState{} },
		DeviceFree:  func(string) (float64, bool) { return 15, true },
		DeviceIndex: func(d string) (string, bool) { return d, true },
	}
	rows := placetable.RowsFromConfig(cfg, live)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	return rows
}

func TestRemoteDecisionHonoursTheContractsLayer(t *testing.T) {
	r := eligibleRemote()
	r.Layers = oneCardRows(t)

	st := schemaSubtask()
	st.Contract.Layer = "fast"
	dec, ok := remoteDecision(st, r)
	if !ok || dec.Defer || dec.Wait {
		t.Fatalf("a contract naming a layer the node declares must place there, got ok=%v dec=%+v", ok, dec)
	}
	if dec.Layer != "fast" || dec.Seat != "qwen36-35b-a3b-gsq-vllm" {
		t.Fatalf("placed = %+v, want the fast layer's seat", dec.Placed)
	}
	if !remoteEligible(st, r) {
		t.Fatalf("the node declaring the requested layer must be eligible")
	}

	// Without a layer the same node runs the same contract on its planner
	// default: declaring layers never changes where yesterday's contracts land.
	plain := schemaSubtask()
	dec, ok = remoteDecision(plain, r)
	if !ok || dec.Defer || dec.Seat != "qwen38-27b-gsq-vllm" || dec.Layer != placetable.LayerSingle {
		t.Fatalf("free choice on the one-card node = %+v, want the single layer's 27B seat", dec.Placed)
	}
}

func TestRemoteDecisionRefusesANodeWithoutTheRequestedLayer(t *testing.T) {
	r := eligibleRemote()
	r.Layers = pairOnlyRows(t)
	st := schemaSubtask()
	st.Contract.Layer = "fast"
	dec, ok := remoteDecision(st, r)
	if !ok || !dec.Defer || !strings.Contains(dec.Reason, "fast") {
		t.Fatalf("a node without the requested layer must defer naming it, got ok=%v dec=%+v", ok, dec)
	}
	eligible, word, detail := eligibilityVerdict(st, r)
	if eligible || word != "layer" || !strings.Contains(detail, "fast") {
		t.Fatalf("verdict = (%v, %q, %q), want ineligible on the layer word", eligible, word, detail)
	}
}

func TestPlaceSendsANamedLayerToTheNodeThatDeclaresIt(t *testing.T) {
	st := schemaSubtask()
	st.Contract.Layer = "fast"
	lenovo := eligibleRemote()
	lenovo.Layers = oneCardRows(t)

	// Idle local, plain box: the idle-local rule would keep it and run the
	// planner seat; a named layer goes to the node that declares it.
	if got := Place("seed", st, localNode(), []NodeView{lenovo}, false); got.NodeID != "lenovo" {
		t.Fatalf("idle plain local kept a contract naming a layer it does not declare: placed on %q", got.NodeID)
	}
	// No node declares it: local still, where runner.decide defers by name.
	plainRemote := eligibleRemote()
	if got := Place("seed", st, localNode(), []NodeView{plainRemote}, false); !got.Local {
		t.Fatalf("with no node declaring the layer the contract must land local (to defer by name), got %q", got.NodeID)
	}
	// An unnamed contract keeps the idle-local rule byte for byte.
	if got := Place("seed", schemaSubtask(), localNode(), []NodeView{lenovo}, false); !got.Local {
		t.Fatalf("idle local must still win an unnamed contract, got %q", got.NodeID)
	}
	// A composite local that declares the layer keeps it.
	local := localNode()
	local.Layers = oneCardRows(t)
	if got := Place("seed", st, local, []NodeView{lenovo}, false); !got.Local {
		t.Fatalf("an idle local that declares the layer must keep it, got %q", got.NodeID)
	}
}

func TestLocalDecideDefersANamedLayerOnAPlainBox(t *testing.T) {
	r := &runner{cfg: config.Default()}
	c := schemaSubtask().Contract
	c.Layer = "fast"
	dec := r.decide(context.Background(), c, schemaSubtask())
	if !dec.Defer || dec.DeferClass != core.DeferClassContract || dec.Layer != "fast" || !strings.Contains(dec.Reason, "declares no layers") {
		t.Fatalf("a plain box must defer a named layer by name, got %+v", dec)
	}
	if dec := r.decide(context.Background(), schemaSubtask().Contract, schemaSubtask()); dec.Defer || dec.Layer != "" {
		t.Fatalf("an unnamed contract on a plain box is the zero decision, got %+v", dec)
	}
}
