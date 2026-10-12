package imagegen

import (
	"reflect"
	"testing"
)

// The card-only placement (config imagegen_placement / gen_edit_placement: multicard) reaches the runners as --placement multicard and as nothing
// else: the two cards come from the lease (GPU_LEASE_DEVICES), never from a flag the harness makes up.

func TestGenerateArgsCarryPlacementMulticard(t *testing.T) {
	m := Model{Ckpt: "qwen-image-2512-Q5_1.gguf", Family: "qwen-image", Placement: "multicard"}
	args := buildArgs("out.png", "p", map[string]any{}, m)
	has(t, args, "--placement", "multicard")
	has(t, args, "--ckpt", "qwen-image-2512-Q5_1.gguf")
	// the batch carries the binding too: every job of the session renders on the same two cards
	has(t, batchArgs("jobs.jsonl", "results.jsonl", m), "--placement", "multicard")
	// the other flags a multicard call must not invent
	for _, f := range []string{"--cards", "--resident-gb", "--pool-vvram", "--pool-compute", "--pool-donor"} {
		hasNot(t, args, f)
	}
}

func TestNoPlacementAddsNothingToTheArgv(t *testing.T) {
	base := Model{Ckpt: "qwen-image-2512-Q5_1.gguf", Family: "qwen-image", Steps: 20, CFG: 4}
	want := buildArgs("out.png", "p", map[string]any{"seed": 7}, base)
	for _, placement := range []string{"", "single", " "} {
		m := base
		m.Placement = placement
		if got := buildArgs("out.png", "p", map[string]any{"seed": 7}, m); !reflect.DeepEqual(got, want) {
			t.Errorf("placement %q changes the argv: %v vs %v", placement, got, want)
		}
		hasNot(t, buildArgs("out.png", "p", nil, m), "--placement")
		hasNot(t, batchArgs("j.jsonl", "r.jsonl", m), "--placement")
	}
}

func TestEditArgsCarryPlacementMulticard(t *testing.T) {
	m := EditModel{Unet: "qwen_image_edit_2511_fp8mixed.safetensors", Placement: "multicard"}
	args := editArgs("out.png", "in.png", "make it orange", map[string]any{}, m)
	has(t, args, "--placement", "multicard")
	has(t, args, "--unet", "qwen_image_edit_2511_fp8mixed.safetensors")
	for _, placement := range []string{"", "single"} {
		m.Placement = placement
		hasNot(t, editArgs("out.png", "in.png", "p", nil, m), "--placement")
	}
	// byte for byte what it was
	plain := EditModel{Unet: "u.gguf", Preset: "lightning8", Megapixels: 1.5}
	withSingle := plain
	withSingle.Placement = "single"
	if a, b := editArgs("o.png", "i.png", "p", nil, plain), editArgs("o.png", "i.png", "p", nil, withSingle); !reflect.DeepEqual(a, b) {
		t.Errorf("single changes the edit argv: %v vs %v", a, b)
	}
}
