package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blackwell-3x16 is the only tier whose device map contains a card the harness must
// not schedule onto: the RTX 5070 Ti that drives the display. Starving it is not
// theoretical — on 2026-09-04 a three-card engine at util 0.87-0.90 left it under
// 1 GB, Windows dropped to a 720p-class mode and the box needed a reboot (clean event
// log, no TDR: starvation, not a crash).
//
// TWO DEVICE ORDERINGS, AND THE DISPLAY CARD HAS A DIFFERENT INDEX IN EACH. This is
// the trap that made the shipped config wrong and then made a first attempt at fixing
// it wrong in the opposite direction:
//
//	gpu_env / CUDA_VISIBLE_DEVICES -> PCI_BUS_ID order (the tier pins
//	  CUDA_DEVICE_ORDER=PCI_BUS_ID): 0 = 5060 Ti @17:00.0, 1 = 5070 Ti @65:00.0
//	  (DISPLAY), 2 = 5060 Ti @B5:00.0.
//	ComfyUI *_pool_* keys ("cuda:N") -> ComfyUI's own FASTEST-FIRST order:
//	  0 = 5070 Ti (DISPLAY, ~896 GB/s), 1 and 2 = the 5060 Ti pair.
//
// So "device 1" is the card to avoid in gpu_env and a perfectly good 5060 Ti in a
// pool key. The live box states it in its llama-swap header ("media pools compute on
// the pair (config imagegen/videogen pool = cuda:1/cuda:2 in ComfyUI's fastest-first
// order)") and the 48 GB ledger's G4 incident names cuda:0 "5070 Ti compute".
//
// 0.113.32 shipped this tier with its ENTIRE media block copied byte-for-byte from
// blackwell-2x16, where the gpu_env map is 0 = utility / 1 = FAST. That copy pinned
// the vision seat (11,751 MiB, 2026-09-05 CUDA-X baseline) onto the display card and
// never mentioned card 2 at all — which is also why the three-card box measured no
// media difference from the two-card one. It was an unadapted copy, not a hardware
// result.
//
// Operator decision 2026-09-07: the 5070 Ti is CONTEXT/KV ONLY. It may grow the KV
// pool for long-context seats (78,506 -> 137,898 tokens across three cards) and the
// named opt-in over-2-card seats may span it, but no tier seat is pinned to it.
func TestTripleBlackwellNeverSchedulesOntoTheDisplayCard(t *testing.T) {
	const tier = "blackwell-3x16"
	// PCI_BUS_ID order, for gpu_env.
	const displayByPCI = "1"
	// ComfyUI fastest-first order, for the pool keys.
	const displayByComfy = "cuda:0"

	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			ConfigSeed map[string]json.RawMessage `json:"config_seed"`
			MediaSeats []struct {
				Kind      string   `json:"kind"`
				Name      string   `json:"name"`
				Residency string   `json:"residency"`
				GPUEnv    []string `json:"gpu_env"`
			} `json:"media_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}
	if len(p.MediaSeats) == 0 {
		t.Fatalf("tier %q declares no media seats — this gate went blind", tier)
	}

	// 1. No seat on the display card, and every seat must name a card explicitly:
	//    an unpinned seat inherits the tier gpu_env and can land anywhere.
	for _, s := range p.MediaSeats {
		pin := ""
		for _, e := range s.GPUEnv {
			if v, ok := strings.CutPrefix(strings.TrimSpace(e), "CUDA_VISIBLE_DEVICES="); ok {
				pin = v
			}
		}
		switch pin {
		case "":
			t.Errorf("%s seat %q (%s) declares no CUDA_VISIBLE_DEVICES pin — on a three-card box an unpinned "+
				"seat can land on the display card", tier, s.Name, s.Kind)
		case displayByPCI:
			t.Errorf("%s seat %q (%s) is pinned to CUDA_VISIBLE_DEVICES=%s — that is the RTX 5070 Ti driving the "+
				"display; starving it dropped the desktop to 720p and forced a reboot on 2026-09-04",
				tier, s.Name, s.Kind, displayByPCI)
		}
	}

	// 2. Image generation must pool across the 5060 Ti pair. Its checkpoint is bf16
	//    (krea2_turbo_bf16), so nothing forces it onto ComfyUI's default device, and
	//    the reference box renders it on cuda:1/cuda:2 — re-verified after the
	//    2026-09-01 revert with two 1568x880 Krea 2 Turbo stills.
	for _, key := range []string{"imagegen_pool_compute", "imagegen_pool_donor"} {
		if v, ok := p.ConfigSeed[key]; ok && strings.Contains(string(v), displayByComfy) {
			t.Errorf("%s seeds %s = %s — in ComfyUI's fastest-first order %s IS the 5070 Ti display card, and "+
				"image generation has no reason to sit there (bf16 checkpoint, unaffected by the int8 constraint)",
				tier, key, string(v), displayByComfy)
		}
	}

	// 3. Video generation is the ONE documented exception and must stay one: ComfyUI-
	//    MultiGPU #220 means an int8 DiT cannot compute on a non-default CUDA device,
	//    so videogen_pool_compute is cuda:0 by necessity. Its DONOR still must not be.
	//    Asserted rather than merely allowed, so nobody "tidies" the exception away
	//    into a config that throws cudaErrorIllegalAddress mid-render.
	if v, ok := p.ConfigSeed["videogen_pool_compute"]; ok && !strings.Contains(string(v), displayByComfy) {
		t.Errorf("%s seeds videogen_pool_compute = %s — an int8 DiT cannot compute on a non-default CUDA device "+
			"(ComfyUI-MultiGPU #220); the 2026-09-01 attempt to move it threw cudaErrorIllegalAddress and was "+
			"reverted to %s as a documented law exception", tier, string(v), displayByComfy)
	}
	if v, ok := p.ConfigSeed["videogen_pool_donor"]; ok && strings.Contains(string(v), displayByComfy) {
		t.Errorf("%s seeds videogen_pool_donor = %s — compute already sits on the display card; donating from it "+
			"too leaves the desktop nothing", tier, string(v))
	}

	// 4. The three-card tier must not be a silent copy of the two-card one. That copy
	//    is the whole defect: identical media placement is what made the third card
	//    measure no different from two.
	two, ok := doc.Profiles["blackwell-2x16"]
	if !ok {
		return
	}
	same := true
	for _, key := range []string{"imagegen_pool_compute", "imagegen_pool_donor", "videogen_pool_donor"} {
		if string(p.ConfigSeed[key]) != string(two.ConfigSeed[key]) {
			same = false
		}
	}
	if same {
		t.Errorf("%s's media pool bindings are byte-identical to blackwell-2x16's. The two tiers have DIFFERENT "+
			"device maps, so identical bindings mean the three-card tier was never adapted — the state that put "+
			"the vision seat and both pools on the display card and left card 2 named nowhere", tier)
	}
}
