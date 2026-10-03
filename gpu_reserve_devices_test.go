package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// planCards is a synthetic box where the index spaces disagree (nvidia index 1 is
// ComfyUI position 0) and nvidia index 1 is the display card.
func planCards(order string) []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}
	cards, _ := gpuprobe.BuildCards(devs, order)
	return cards
}

func loaderOf(cards []gpuprobe.Card) func() ([]gpuprobe.Card, string, error) {
	return func() ([]gpuprobe.Card, string, error) { return cards, "", nil }
}

func noEnv(string) string { return "" }

func envMap(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// FLAG OFF = NO CHANGE (the P2 acceptance, carried into P3): a host that has not enabled
// card-scoped leases must not read the card table (an nvidia-smi call on every reserve)
// nor derive a device set from the command (which would be refused as a device lease).
func TestPlanFlagOffHostNeitherReadsCardsNorDerivesDevices(t *testing.T) {
	loader := func() ([]gpuprobe.Card, string, error) {
		t.Fatal("a flag-off host must not read the card table for a reserve with no --devices")
		return nil, "", nil
	}
	p, err := planReserveDevices(reserveDeviceFlags{}, []string{"python", "main.py", "--cuda-device", "2"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "1"}), false, loader)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.IDs) != 0 || p.Auto {
		t.Fatalf("flag off: the lease is whole-node whatever the command says: %+v", p)
	}
}

func TestPlanExplicitDevicesOnAFlagOffHostIsAHardError(t *testing.T) {
	for _, f := range []reserveDeviceFlags{{devices: "0"}, {cards: "2"}} {
		_, err := planReserveDevices(f, nil, noEnv, false, loaderOf(planCards("")))
		if !errors.Is(err, gpulease.ErrCardScopedOff) {
			t.Fatalf("%+v on a flag-off host must be refused with ErrCardScopedOff, got %v", f, err)
		}
		if !strings.Contains(err.Error(), "--whole-node") {
			t.Errorf("the refusal must name the way out: %v", err)
		}
	}
}

func TestPlanDevicesResolveIndexAndUUID(t *testing.T) {
	p, err := planReserveDevices(reserveDeviceFlags{devices: "2, GPU-aaaa"}, nil, noEnv, true, loaderOf(planCards("")))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.IDs) != 2 || p.IDs[0] != "gpu-cccc0000-x" || p.IDs[1] != "gpu-aaaa0000-x" || p.Source != "--devices" {
		t.Fatalf("explicit devices are nvidia-smi indices or UUID prefixes, in the order given: %+v", p)
	}
	// An explicit word naming the display card is honoured (the operator's choice).
	if p, err = planReserveDevices(reserveDeviceFlags{devices: "1"}, nil, noEnv, true, loaderOf(planCards(""))); err != nil || len(p.IDs) != 1 || p.IDs[0] != "gpu-bbbb0000-x" {
		t.Fatalf("display card named explicitly: %+v %v", p, err)
	}
	for _, bad := range []string{"9", "GPU-nope", " "} {
		if _, err := planReserveDevices(reserveDeviceFlags{devices: bad}, nil, noEnv, true, loaderOf(planCards(""))); err == nil {
			t.Errorf("--devices %q must not resolve", bad)
		}
	}
}

func TestPlanDevicesWithNoCardTableIsAnErrorNotAWholeNodeLease(t *testing.T) {
	loader := func() ([]gpuprobe.Card, string, error) { return nil, "", errors.New("nvidia-smi: not on PATH") }
	_, err := planReserveDevices(reserveDeviceFlags{devices: "0"}, nil, noEnv, true, loader)
	if err == nil || !strings.Contains(err.Error(), "nvidia-smi") {
		t.Fatalf("naming cards without a card table cannot silently widen to the whole node: %v", err)
	}
}

func TestPlanCardsCountParses(t *testing.T) {
	for in, want := range map[string][2]int{"1": {1, 1}, "2": {2, 2}, "1..3": {1, 3}, " 2..2 ": {2, 2}} {
		min, max, err := parseCardCount(in)
		if err != nil || min != want[0] || max != want[1] {
			t.Errorf("%q: %d..%d %v, want %v", in, min, max, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "3..1", "a", "1..", "..2", "1...2"} {
		if _, _, err := parseCardCount(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	p, err := planReserveDevices(reserveDeviceFlags{cards: "1..2"}, nil, noEnv, true, loaderOf(planCards("")))
	if err != nil || !p.Auto || p.Min != 1 || p.Max != 2 {
		t.Fatalf("--cards 1..2: %+v %v", p, err)
	}
}

func TestPlanFlagsAreMutuallyExclusive(t *testing.T) {
	for _, f := range []reserveDeviceFlags{
		{devices: "0", cards: "1"}, {devices: "0", wholeNode: true}, {cards: "1", wholeNode: true},
	} {
		if _, err := planReserveDevices(f, nil, noEnv, true, loaderOf(planCards(""))); err == nil || !strings.Contains(err.Error(), "only one") {
			t.Errorf("%+v: want a mutual-exclusion error, got %v", f, err)
		}
	}
}

func TestPlanWholeNodeFlagBeatsDerivation(t *testing.T) {
	p, err := planReserveDevices(reserveDeviceFlags{wholeNode: true}, nil, envMap(map[string]string{"COMFY_CUDA_DEVICE": "0"}), true, loaderOf(planCards("1,0,2")))
	if err != nil || len(p.IDs) != 0 || p.Auto || p.Source != "--whole-node" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestPlanDerivesTheCardSetFromTheWrappedCommand(t *testing.T) {
	p, err := planReserveDevices(reserveDeviceFlags{}, nil, envMap(map[string]string{"COMFY_CUDA_DEVICE": "2"}), true, loaderOf(planCards("1,0,2")))
	if err != nil || len(p.IDs) != 1 || p.IDs[0] != "gpu-cccc0000-x" || !strings.Contains(p.Source, "COMFY_CUDA_DEVICE") {
		t.Fatalf("derived: %+v %v", p, err)
	}
	// No evidence: whole node, and quietly (a command that names no card is the common case).
	p, err = planReserveDevices(reserveDeviceFlags{}, []string{"python", "bench.py"}, noEnv, true, loaderOf(planCards("1,0,2")))
	if err != nil || len(p.IDs) != 0 || p.Note != "" {
		t.Fatalf("no evidence: %+v %v", p, err)
	}
}

// Evidence that cannot be turned into a card must degrade to the whole node (the wider
// fence, the safe direction) and SAY so; it must not fail a reserve that worked before.
func TestPlanDerivationFailureFallsBackToWholeNodeLoudly(t *testing.T) {
	// The order is not declared, so ComfyUI device 2 is unknowable.
	p, err := planReserveDevices(reserveDeviceFlags{}, nil, envMap(map[string]string{"COMFY_CUDA_DEVICE": "2"}), true, loaderOf(planCards("")))
	if err != nil || len(p.IDs) != 0 {
		t.Fatalf("whole node expected: %+v %v", p, err)
	}
	if !strings.Contains(p.Note, "whole-node") || !strings.Contains(p.Note, "gpu_comfy_order") {
		t.Fatalf("the note must say what happened and the fix: %q", p.Note)
	}
	// No card table at all: the same fallback.
	loader := func() ([]gpuprobe.Card, string, error) { return nil, "", errors.New("nvidia-smi: not on PATH") }
	p, err = planReserveDevices(reserveDeviceFlags{}, nil, envMap(map[string]string{"COMFY_CUDA_DEVICE": "0"}), true, loader)
	if err != nil || len(p.IDs) != 0 || !strings.Contains(p.Note, "nvidia-smi") {
		t.Fatalf("no card table: %+v %v", p, err)
	}
}
