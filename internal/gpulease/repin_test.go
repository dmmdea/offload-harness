package gpulease

import (
	"strings"
	"testing"
)

// The synthetic box of cmdCards: nvidia-smi index 0 is ComfyUI device 1, index 1 is device 0, index 2 is device 2.
const (
	repinZero = "gpu-aaaa0000-x"
	repinOne  = "gpu-bbbb0000-x"
	repinTwo  = "gpu-cccc0000-x"
)

func TestPlanRepinMovesAnIndexOutsideTheLeaseToTheHeldCardsComfyIndex(t *testing.T) {
	cards := cmdCards("1,0,2")
	plan := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": " 2 "}), cards, false)
	if len(plan.Repins) != 1 {
		t.Fatalf("one pin outside the lease: %+v", plan)
	}
	r := plan.Repins[0]
	if r.Var != "COMFY_CUDA_DEVICE" || r.Old != "2" || r.New != "1" || r.Env() != "COMFY_CUDA_DEVICE=1" {
		t.Fatalf("held nvidia index 0 is ComfyUI device 1, not 0: %+v", r)
	}
	if len(r.From) != 1 || r.From[0] != repinTwo || len(r.To) != 1 || r.To[0] != repinZero || r.Unresolved != nil {
		t.Fatalf("it records where the pin pointed and where it moved: %+v", r)
	}
}

func TestPlanRepinReadsTheIndexInTheOrderTheChildRunsUnder(t *testing.T) {
	cards := cmdCards("1,0,2")
	// nvidia-smi order: 0 IS the held card, so nothing moves, although ComfyUI device 0 is another card.
	plan := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0"}), cards, true)
	if len(plan.Repins) != 0 || !plan.IsInside("COMFY_CUDA_DEVICE") {
		t.Fatalf("an index inside the lease in the child's order is inside: %+v", plan)
	}
	// And a pin outside moves to the held card's nvidia-smi index.
	plan = PlanRepin([]string{repinTwo}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0"}), cards, true)
	if len(plan.Repins) != 1 || plan.Repins[0].New != "2" {
		t.Fatalf("the held card is nvidia-smi index 2: %+v", plan)
	}
}

func TestPlanRepinMapsAListToTheFirstHeldCardsInOrderAndRefusesMoreThanHeld(t *testing.T) {
	cards := cmdCards("1,0,2")
	held := []string{repinTwo, repinZero} // lease order is not ComfyUI order
	plan := PlanRepin(held, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0,1"}), cards, false)
	if len(plan.Repins) != 1 || plan.Repins[0].New != "1,2" {
		t.Fatalf("two pinned, two held: devices 1 and 2: %+v", plan)
	}
	// A duplicate entry is one card.
	plan = PlanRepin(held, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0,0"}), cards, false)
	if len(plan.Repins) != 1 || plan.Repins[0].New != "1" {
		t.Fatalf("one distinct card pinned: the first held card in ComfyUI order: %+v", plan)
	}
	plan = PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "0,1"}), cards, false)
	if len(plan.Repins) != 0 || len(plan.NotMoved) != 1 || !strings.Contains(plan.NotMoved[0], "2 cards") {
		t.Fatalf("a pin of more cards than the lease holds is not moved, and says why: %+v", plan)
	}
}

func TestPlanRepinNeverGuessesAnIndexWithoutAKnownOrder(t *testing.T) {
	plan := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "1"}), cmdCards(""), false)
	if len(plan.Repins) != 0 || len(plan.NotMoved) != 1 || !strings.Contains(plan.NotMoved[0], "gpu_comfy_order") {
		t.Fatalf("no declared order, no index to write: %+v", plan)
	}
	// With no card table at all the same holds.
	plan = PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": "1"}), nil, false)
	if len(plan.Repins) != 0 || len(plan.NotMoved) != 1 {
		t.Fatalf("no table, no index: %+v", plan)
	}
}

func TestPlanRepinLeavesWhatItDoesNotUnderstand(t *testing.T) {
	cards := cmdCards("1,0,2")
	for _, v := range []string{"abc", "-1", "1;2", "1,,2", ","} {
		plan := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": v}), cards, false)
		if len(plan.Repins) != 0 || len(plan.Inside) != 0 {
			t.Errorf("COMFY_CUDA_DEVICE=%q is no device index list; the program reports that itself: %+v", v, plan)
		}
	}
	plan := PlanRepin(nil, envOf(map[string]string{"COMFY_CUDA_DEVICE": "2", "COMFY_CARD_UUID": "GPU-cccc0000-x"}), cards, false)
	if len(plan.Repins) != 0 {
		t.Errorf("a lease that holds no card moves nothing: %+v", plan)
	}
	plan = PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CUDA_DEVICE": " ", "COMFY_CARD_UUID": ""}), cards, false)
	if len(plan.Repins) != 0 || len(plan.Inside) != 0 || len(plan.NotMoved) != 0 {
		t.Errorf("blank values are no pin: %+v", plan)
	}
}

func TestPlanRepinMovesAUUIDByUUIDAndNeverByIndex(t *testing.T) {
	cards := cmdCards("1,0,2")
	plan := PlanRepin([]string{repinTwo, repinZero}, envOf(map[string]string{"COMFY_CARD_UUID": "GPU-bbbb0000-x"}), cards, false)
	if len(plan.Repins) != 1 {
		t.Fatalf("%+v", plan)
	}
	r := plan.Repins[0]
	// The first held card in ComfyUI order is nvidia index 0 (device 1), written as its driver uuid.
	if r.Var != "COMFY_CARD_UUID" || r.New != "GPU-aaaa0000-x" || r.Env() != "COMFY_CARD_UUID=GPU-aaaa0000-x" {
		t.Fatalf("a uuid pin becomes the held card's uuid: %+v", r)
	}
	// An index in COMFY_CARD_UUID is no identity; it is moved to a uuid too.
	plan = PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CARD_UUID": "2"}), cards, false)
	if len(plan.Repins) != 1 || plan.Repins[0].Unresolved == nil || plan.Repins[0].New != "GPU-aaaa0000-x" {
		t.Fatalf("an index where a uuid belongs is unresolved and moved: %+v", plan)
	}
	// Inside the lease: nothing moves, with or without a card table.
	if p := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CARD_UUID": "GPU-aaaa0000-x"}), cards, false); len(p.Repins) != 0 || !p.IsInside("COMFY_CARD_UUID") {
		t.Fatalf("a held uuid is inside: %+v", p)
	}
	if p := PlanRepin([]string{repinZero}, envOf(map[string]string{"COMFY_CARD_UUID": "GPU-AAAA0000-X"}), nil, false); len(p.Repins) != 0 || !p.IsInside("COMFY_CARD_UUID") {
		t.Fatalf("with no card table a lease id is still the uuid lower-cased, so a held card is recognised: %+v", p)
	}
}

func TestReadsPCIOrderAndTheArgvFlag(t *testing.T) {
	if !ReadsPCIOrder(envOf(map[string]string{"CUDA_DEVICE_ORDER": " pci_bus_id "})) || ReadsPCIOrder(envOf(map[string]string{"CUDA_DEVICE_ORDER": "FASTEST_FIRST"})) || ReadsPCIOrder(envOf(nil)) {
		t.Error("only PCI_BUS_ID (any case) makes an index nvidia-smi's")
	}
	if !HasCudaDeviceFlag([]string{"python", "main.py", "--cuda-device", "1"}) || !HasCudaDeviceFlag([]string{"main.py", "--cuda-device=1"}) ||
		HasCudaDeviceFlag([]string{"main.py", "--cuda-device"}) || HasCudaDeviceFlag([]string{"main.py"}) {
		t.Error("the flag, in both spellings, with a value")
	}
	if got := DriverUUID("gpu-dddd1111-y", nil); got != "GPU-dddd1111-y" {
		t.Errorf("rebuilt from the lease id: %q", got)
	}
}
