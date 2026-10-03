package gpulease

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// cmdCards is a synthetic box where the three index spaces DISAGREE: nvidia-smi index 1
// is ComfyUI's position 0, index 0 is position 1, index 2 is position 2.
func cmdCards(order string) []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T2", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}
	cards, _ := gpuprobe.BuildCards(devs, order)
	return cards
}

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestEnvDerivedDevicesFromComfyCudaDevice(t *testing.T) {
	cards := cmdCards("1,0,2")
	// COMFY_CUDA_DEVICE counts in ComfyUI's order: "0" is nvidia-smi index 1, not 0.
	got, err := DevicesFromCommand(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0"}), cards)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.IDs) != 1 || got.IDs[0] != "gpu-bbbb0000-x" || got.Source != "COMFY_CUDA_DEVICE" {
		t.Fatalf("COMFY_CUDA_DEVICE=0 must resolve through the ComfyUI order to the card at nvidia index 1: %+v", got)
	}
	// A comma list names several cards.
	got, err = DevicesFromCommand(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0, 2"}), cards)
	if err != nil || len(got.IDs) != 2 || got.IDs[0] != "gpu-bbbb0000-x" || got.IDs[1] != "gpu-cccc0000-x" {
		t.Fatalf("list: %+v %v", got, err)
	}
	// An explicit --cuda-device on the command line beats the env var.
	got, err = DevicesFromCommand([]string{"python", "main.py", "--cuda-device", "2"}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0"}), cards)
	if err != nil || len(got.IDs) != 1 || got.IDs[0] != "gpu-cccc0000-x" || got.Source != "--cuda-device" {
		t.Fatalf("--cuda-device: %+v %v", got, err)
	}
	got, err = DevicesFromCommand([]string{"python", "main.py", "--cuda-device=1"}, envOf(nil), cards)
	if err != nil || len(got.IDs) != 1 || got.IDs[0] != "gpu-aaaa0000-x" {
		t.Fatalf("--cuda-device=1: %+v %v", got, err)
	}
}

// The same card named twice is one card (a lease records each card once).
func TestDerivedDevicesNameEachCardOnce(t *testing.T) {
	got, err := DevicesFromCommand(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0,0, 2"}), cmdCards("1,0,2"))
	if err != nil || len(got.IDs) != 2 || got.IDs[0] != "gpu-bbbb0000-x" || got.IDs[1] != "gpu-cccc0000-x" {
		t.Fatalf("duplicates collapse: %+v %v", got, err)
	}
}

func TestDerivedDevicesNoEvidenceIsWholeNode(t *testing.T) {
	got, err := DevicesFromCommand([]string{"python", "bench.py"}, envOf(nil), cmdCards("1,0,2"))
	if err != nil || len(got.IDs) != 0 || got.Source != "" {
		t.Fatalf("a command naming no card is a whole-node lease: %+v %v", got, err)
	}
}

// The order is not declared: a ComfyUI index cannot be turned into a card. That is an
// ERROR the caller turns into "whole node, and here is why", never a guess.
func TestDerivedDevicesRefuseAnUnknownComfyOrder(t *testing.T) {
	_, err := DevicesFromCommand(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "2"}), cmdCards(""))
	if !errors.Is(err, ErrDeviceUnresolved) {
		t.Fatalf("want ErrDeviceUnresolved, got %v", err)
	}
	if !strings.Contains(err.Error(), "gpu_comfy_order") || !strings.Contains(err.Error(), "--devices") {
		t.Fatalf("the refusal must name both fixes: %v", err)
	}
	// A single-card box has one possible order, so it needs no declaration.
	one := cmdCards("")[:1]
	one[0].ComfyOrder = 0
	got, err := DevicesFromCommand(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0"}), one)
	if err != nil || len(got.IDs) != 1 {
		t.Fatalf("single card: %+v %v", got, err)
	}
}

func TestDerivedDevicesFromCudaVisibleDevices(t *testing.T) {
	cards := cmdCards("")
	// UUIDs are direct and need no order.
	got, err := DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-cccc0000-x,GPU-aaaa0000-x"}), cards)
	if err != nil || len(got.IDs) != 2 || got.Source != "CUDA_VISIBLE_DEVICES" {
		t.Fatalf("uuid list: %+v %v", got, err)
	}
	// A bare index under PCI_BUS_ID is nvidia-smi's index.
	got, err = DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "1", "CUDA_DEVICE_ORDER": "PCI_BUS_ID"}), cards)
	if err != nil || len(got.IDs) != 1 || got.IDs[0] != "gpu-bbbb0000-x" {
		t.Fatalf("PCI_BUS_ID index: %+v %v", got, err)
	}
	// A bare index otherwise is FASTEST_FIRST: ambiguous unless the order is declared.
	if _, err = DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "1"}), cards); !errors.Is(err, ErrDeviceUnresolved) {
		t.Fatalf("bare index with the order unknown must refuse, got %v", err)
	}
	got, err = DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "1"}), cmdCards("1,0,2"))
	if err != nil || got.IDs[0] != "gpu-aaaa0000-x" {
		t.Fatalf("FASTEST_FIRST index 1 with the order declared is nvidia 0: %+v %v", got, err)
	}
	// It outranks --cuda-device: the process can only see what it lists.
	got, err = DevicesFromCommand([]string{"x", "--cuda-device", "0"}, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-cccc0000-x"}), cards)
	if err != nil || len(got.IDs) != 1 || got.IDs[0] != "gpu-cccc0000-x" {
		t.Fatalf("CUDA_VISIBLE_DEVICES wins: %+v %v", got, err)
	}
	// A UUID no card carries is an error, never a silent whole-node widening.
	if _, err = DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-zzzz9999"}), cards); !errors.Is(err, ErrDeviceUnresolved) {
		t.Fatalf("unknown uuid: %v", err)
	}
}

// An empty or "-1" CUDA_VISIBLE_DEVICES hides every card; it names none, so it is not
// evidence of which card a job uses.
func TestDerivedDevicesIgnoreAnEmptyCudaVisibleDevices(t *testing.T) {
	for _, v := range []string{"", "  ", "-1"} {
		got, err := DevicesFromCommand(nil, envOf(map[string]string{"CUDA_VISIBLE_DEVICES": v}), cmdCards(""))
		if err != nil || len(got.IDs) != 0 {
			t.Errorf("CUDA_VISIBLE_DEVICES=%q: %+v %v", v, got, err)
		}
	}
}

func TestWouldDeriveSaysWhetherTheCommandNamesACard(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{"nothing", []string{"python", "x.py"}, nil, false},
		{"flag", []string{"python", "main.py", "--cuda-device", "1"}, nil, true},
		{"flag with equals", []string{"main.py", "--cuda-device=1"}, nil, true},
		{"comfy env", nil, map[string]string{"COMFY_CUDA_DEVICE": "1"}, true},
		{"visible devices", nil, map[string]string{"CUDA_VISIBLE_DEVICES": "0"}, true},
		{"visible devices hiding everything", nil, map[string]string{"CUDA_VISIBLE_DEVICES": "-1"}, false},
		{"empty visible devices", nil, map[string]string{"CUDA_VISIBLE_DEVICES": " "}, false},
	}
	for _, c := range cases {
		if got := WouldDerive(c.args, envOf(c.env)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
