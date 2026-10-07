package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A twin the render pins by GPU UUID (this board re-enumerates CUDA indices on a power loss, so the box
// pins by UUID) against a layer that declares the card by index cannot be compared from text. The gate
// compares them through this machine's card table when it can read one, and REFUSES when it cannot,
// naming the way out: a pin that cannot be shown to be the declared one is not passed.

const (
	pinUUIDDisplay = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff"
	pinUUIDOther   = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func uuidPinnedTwinResult(deviceDeclared string) renderResult {
	cfg := "models:\n  gemma-4-e4b-display:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + pinUUIDDisplay + "\"]\n    cmd: llama-server -ngl 99 -m twin.gguf\n"
	return renderResult{
		TierID: "blackwell-3x16",
		Config: cfg,
		Layers: []config.LayerSpec{{Name: "display", Seats: []config.LayerSeat{{Role: "router", Device: deviceDeclared,
			ModelMap: map[string]string{"workhorse": "gemma-4-e4b-display"}}}}},
	}
}

func withCardTable(t *testing.T, devs []gpuprobe.Device, err error) {
	t.Helper()
	prev := renderCardTable
	renderCardTable = func() ([]gpuprobe.Device, error) { return devs, err }
	t.Cleanup(func() { renderCardTable = prev })
}

func TestRenderGateComparesAUUIDPinWithAnIndexDeclarationThroughTheCardTable(t *testing.T) {
	table := []gpuprobe.Device{{Index: 0, UUID: pinUUIDOther}, {Index: 1, UUID: pinUUIDDisplay}}
	withCardTable(t, table, nil)
	if err := renderGate(uuidPinnedTwinResult("1")); err != nil {
		t.Fatalf("the UUID is card 1 on this box and the layer declares 1: %v", err)
	}
	err := renderGate(uuidPinnedTwinResult("0"))
	if err == nil || !strings.Contains(err.Error(), `declared device pin "0"`) {
		t.Fatalf("the UUID is card 1 and the layer declares 0: refused as a plain mismatch, got %v", err)
	}
}

func TestRenderGateRefusesAUUIDPinItCannotCompareAndSaysHowToFixIt(t *testing.T) {
	for name, tc := range map[string]struct {
		devs []gpuprobe.Device
		err  error
	}{
		"no card table (nvidia-smi absent)":   {nil, errors.New("nvidia-smi: not found")},
		"a table that does not list the card": {[]gpuprobe.Device{{Index: 0, UUID: pinUUIDOther}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			withCardTable(t, tc.devs, tc.err)
			err := renderGate(uuidPinnedTwinResult("1"))
			if err == nil {
				t.Fatal("a UUID pin that cannot be compared with the declared index must be refused, not passed")
			}
			for _, want := range []string{"gemma-4-e4b-display", "GPU UUID", "Declare the seat's device as the GPU UUID in layers"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must mention %q: %v", want, err)
				}
			}
		})
	}
}

// A render with no UUID pin never reads the card table: nvidia-smi is not run for a comparison that
// text settles.
func TestRenderGateDoesNotReadTheCardTableWhenTextSettlesThePins(t *testing.T) {
	prev := renderCardTable
	renderCardTable = func() ([]gpuprobe.Device, error) {
		t.Error("the card table was read for a render that pins by index only")
		return nil, nil
	}
	t.Cleanup(func() { renderCardTable = prev })
	res := uuidPinnedTwinResult("1")
	res.Config = strings.Replace(res.Config, pinUUIDDisplay, "1", 1)
	if err := renderGate(res); err != nil {
		t.Fatalf("an index pin against an index declaration needs no table: %v", err)
	}
}
