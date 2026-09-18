package placement

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// oneCardLayers is the ampere-16 Lenovo as it declares layers (register
// A-100): one card, the planner default 27B GSQ under the single layer and the
// 35B fast digest seat under a second layer named `fast`. Neither is a pair.
func oneCardLayers() []config.LayerSpec {
	return []config.LayerSpec{
		{Name: LayerSingle, Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: RoleAgent, Model: "qwen38-27b-gsq-vllm", Device: "0", CtxTokens: 32768, FootprintGiB: 13.9},
		}},
		{Name: "fast", Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: RoleAgent, Model: "qwen36-35b-a3b-gsq-vllm", Device: "0", CtxTokens: 32768, MaxInflight: 8, FootprintGiB: 11.7},
		}},
	}
}

func TestRequestedLayerTakesItsAgentSeat(t *testing.T) {
	d := DecideOnLayer(agentReq(1000), oneCardLayers(), "fast", admitting().live())
	if d.Defer || d.Wait {
		t.Fatalf("a requested layer with an agent seat must place, got defer=%v wait=%v reason=%q", d.Defer, d.Wait, d.Reason)
	}
	if d.Layer != "fast" || d.Role != RoleAgent || d.Seat != "qwen36-35b-a3b-gsq-vllm" || d.CtxTokens != 32768 {
		t.Fatalf("placed = %+v, want the fast layer's agent seat", d.Placed)
	}
	if !strings.Contains(d.Reason, "fast") {
		t.Fatalf("reason must name the requested layer: %q", d.Reason)
	}
}

func TestRequestedLayerOverflowDefersNamingTheLayer(t *testing.T) {
	d := DecideOnLayer(agentReq(40000), oneCardLayers(), "fast", admitting().live())
	if !d.Defer || d.DeferClass != core.DeferClassContract {
		t.Fatalf("a contract past the requested seat's window is a contract defer, got %+v", d)
	}
	if d.Layer != "fast" || !strings.Contains(d.Reason, "32768") {
		t.Fatalf("the defer must name the layer and its window: %+v", d.Placed)
	}
}

func TestRequestedLayerWithoutAnAgentSeatStillDefers(t *testing.T) {
	layers := oneCardLayers()
	layers[1].Seats = []config.LayerSeat{{Role: RoleOCR, Model: "ocr", Device: "0", CtxTokens: 4096}}
	d := DecideOnLayer(agentReq(1000), layers, "fast", admitting().live())
	if !d.Defer || !strings.Contains(d.Reason, "restricted to layer fast") {
		t.Fatalf("a requested layer with no agent seat must defer naming the restriction, got %+v", d)
	}
}

func TestFreeChoiceOnAOneCardBoxLandsOnTheSingleAgentSeat(t *testing.T) {
	d := Decide(agentReq(1000), oneCardLayers(), admitting().live())
	if d.Defer || d.Wait {
		t.Fatalf("a box with no pair must still place the free choice, got defer=%v reason=%q", d.Defer, d.Reason)
	}
	if d.Layer != LayerSingle || d.Seat != "qwen38-27b-gsq-vllm" || d.Role != RoleAgent {
		t.Fatalf("placed = %+v, want the single layer's agent seat (the planner default)", d.Placed)
	}
}

func TestFreeChoiceNeverTakesTheFastLayer(t *testing.T) {
	// The second layer is reachable by name only: the free choice must not
	// drift onto a seat whose blind coverage is 4.65 against the default's 8.53.
	for i := 0; i < 3; i++ {
		d := Decide(agentReq(1000), oneCardLayers(), admitting().live())
		if d.Seat == "qwen36-35b-a3b-gsq-vllm" {
			t.Fatalf("free choice landed on the fast seat: %+v", d.Placed)
		}
	}
}

func TestFreeChoiceOnTheReferenceBoxStillPrefersThePair(t *testing.T) {
	d := Decide(agentReq(1000), layers(), admitting().live())
	if d.Defer || d.Layer != LayerPair {
		t.Fatalf("the reference box's free choice is the pair, got %+v", d.Placed)
	}
}
