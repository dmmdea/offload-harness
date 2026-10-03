package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// ModelPins is what the load gate arms from the layers (plan P4): the device pins of the
// layer seat(s) that serve a model name, or "unknown" for a model no layer declares.

func TestModelPinsFromDeclaredLayerSeats(t *testing.T) {
	c := CompositeFixture()
	cases := []struct {
		model string
		want  []string
	}{
		{"gemma-4-26b-agent", []string{"0"}},
		{"qwen3-vl-8b", []string{"2"}},
		{"agent-pool", []string{"0", "2"}},
		{"qwen3.8-27b-262k", []string{"0", "2"}},
		{"qwen3.8-flash-next-262k", []string{"0", "1", "2"}},
		// A router twin named in model_map is pinned where its seat is.
		{"gemma-4-e4b-display", []string{"1"}},
		{"GEMMA-4-26B-AGENT", []string{"0"}}, // names compare case-insensitively
	}
	for _, tc := range cases {
		got, ok := c.ModelPins(tc.model)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ModelPins(%q) = %v, %v; want %v", tc.model, got, ok, tc.want)
		}
	}
}

func TestModelPinsUnknownModelAndPlainBox(t *testing.T) {
	c := CompositeFixture()
	for _, m := range []string{"", "  ", "not-a-layer-seat", "vision"} { // an alias is not a name a layer declares
		if pins, ok := c.ModelPins(m); ok || len(pins) != 0 {
			t.Errorf("ModelPins(%q) = %v, %v; an undeclared model is UNKNOWN, which the gate reads as every card", m, pins, ok)
		}
	}
	if pins, ok := Default().ModelPins("gemma-4-26b-agent"); ok || len(pins) != 0 {
		t.Errorf("a box with no layers declares no pins, got %v %v", pins, ok)
	}
}

// A model declared on two seats sits on the union of their cards: the fence has to cover
// every card the model can land on.
func TestModelPinsUnionWhenAModelIsDeclaredTwice(t *testing.T) {
	c := CompositeFixture()
	c.Layers[0].Seats[1].Model = "shared-seat"
	c.Layers[1].Seats[0].Model = "shared-seat"
	got, ok := c.ModelPins("shared-seat")
	if !ok || !reflect.DeepEqual(got, []string{"0", "2"}) {
		t.Fatalf("union of device 0 and device 0,2 = %v, %v; want [0 2]", got, ok)
	}
}

// The single layer's router seat declares no model: it stands for the cascade's rungs, so
// those model names carry its pin.
func TestModelPinsRouterSeatCoversTheCascadeRungs(t *testing.T) {
	c := CompositeFixture()
	c.Model, c.TriageModel, c.EscalationModel = "workhorse-12b", "triage-4b", "escalate-26b"
	for _, m := range []string{"workhorse-12b", "triage-4b", "escalate-26b"} {
		got, ok := c.ModelPins(m)
		if !ok || !reflect.DeepEqual(got, []string{"0"}) {
			t.Errorf("ModelPins(%q) = %v, %v; the single layer's router rung sits on device 0", m, got, ok)
		}
	}
}

// Load arms the gate's seat pins in the same act that arms its lease directory.
func TestLoadArmsTheSeatPins(t *testing.T) {
	root := t.TempDir()
	c := CompositeFixture()
	body, err := json.Marshal(map[string]any{"state_dir": root, "layers": c.Layers, "tier_profile": c.TierProfile, "tiers": c.Tiers})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := modelaffinity.PinsFor("qwen3-vl-8b")
	if !ok || !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("after Load the gate sees %v, %v for qwen3-vl-8b, want [2]", got, ok)
	}
}
