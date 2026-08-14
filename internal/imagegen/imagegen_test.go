package imagegen

import (
	"reflect"
	"strings"
	"testing"
)

// argv renders args as a single space-joined string for readable assertions.
func argv(a []string) string { return strings.Join(a, " ") }

// has reports whether args contains the flag followed by the expected value.
func has(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Fatalf("%s has no value in: %s", flag, argv(args))
			}
			if args[i+1] != want {
				t.Fatalf("%s = %q, want %q in: %s", flag, args[i+1], want, argv(args))
			}
			return
		}
	}
	t.Fatalf("missing %s %s in: %s", flag, want, argv(args))
}

func hasNot(t *testing.T, args []string, flag string) {
	t.Helper()
	for _, a := range args {
		if a == flag {
			t.Fatalf("unexpected %s in: %s", flag, argv(args))
		}
	}
}

// TestBuildArgs_FamilyBinding: the family selects the model-correct graph in
// comfy-render.mjs (e.g. hidream-o1 = pixel-space DiT graph with ModelNoiseScale +
// patch-seam smoothing at native 2048). Set -> --family passed; unset -> absent
// (SDXL machines unchanged).
func TestBuildArgs_FamilyBinding(t *testing.T) {
	m := Model{Ckpt: "hidream_o1_image_bf16.safetensors", VAE: "builtin", Family: "hidream-o1"}
	args := buildArgs("out.png", "p", map[string]any{}, m)
	has(t, args, "--family", "hidream-o1")
	hasNot(t, buildArgs("out.png", "p", map[string]any{}, Model{Ckpt: "RealVisXL_V5.0_fp16.safetensors"}), "--family")
}

// TestBuildArgs_QwenPresetBinding: the qwen-image family's remaining knobs thread
// through as flags (preset picks the steps+cfg+LoRA pairing; clip/lora/strength/
// shift override its fields). Unset -> absent, so every existing binding renders
// byte-for-byte the same command.
func TestBuildArgs_QwenPresetBinding(t *testing.T) {
	m := Model{Ckpt: "qwen-image-2512-Q5_1.gguf", Family: "qwen-image",
		Preset: "lightning4", CLIP: "qwen_2.5_vl_7b_fp8_scaled.safetensors",
		LoRA: "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors",
		LoRAStrength: 0.9, Shift: 3.1}
	args := buildArgs("out.png", "p", map[string]any{}, m)
	has(t, args, "--preset", "lightning4")
	has(t, args, "--clip", "qwen_2.5_vl_7b_fp8_scaled.safetensors")
	has(t, args, "--lora", "Qwen-Image-2512-Lightning-4steps-V1.0-fp32.safetensors")
	has(t, args, "--lora-strength", "0.9")
	has(t, args, "--shift", "3.1")
	for _, f := range []string{"--preset", "--clip", "--lora", "--lora-strength", "--shift"} {
		hasNot(t, buildArgs("out.png", "p", map[string]any{}, Model{Ckpt: "x.gguf", Family: "qwen-image"}), f)
	}
}

// TestBuildArgs_Krea2PoolBinding: the 32GB-pool doctrine knobs thread through as
// flags; unset (the single-GPU fleet shape) emits none, so every existing
// binding renders byte-for-byte the same command.
func TestBuildArgs_Krea2PoolBinding(t *testing.T) {
	m := Model{Ckpt: "krea2_turbo_bf16.safetensors", Family: "krea2",
		VAE: "qwen_image_vae.safetensors",
		PoolVvramGB: 12, PoolCompute: "cuda:0", PoolDonor: "cuda:1"}
	args := buildArgs("out.png", "p", map[string]any{}, m)
	has(t, args, "--family", "krea2")
	has(t, args, "--pool-vvram", "12")
	has(t, args, "--pool-compute", "cuda:0")
	has(t, args, "--pool-donor", "cuda:1")
	for _, f := range []string{"--pool-vvram", "--pool-compute", "--pool-donor"} {
		hasNot(t, buildArgs("out.png", "p", map[string]any{}, Model{Ckpt: "krea2_turbo_bf16.safetensors", Family: "krea2"}), f)
	}
}

// TestBuildArgs_ZeroModelIsUnchanged is the compatibility guard for every machine
// that has NOT set an image-model binding (e.g. the 8GB laptop on SDXL). A zero
// Model must add no flags at all, so comfy-render.mjs keeps its own defaults and the
// rendered command is byte-for-byte what it was before the binding existed.
func TestBuildArgs_ZeroModelIsUnchanged(t *testing.T) {
	args := buildArgs("out.png", "a cat", map[string]any{
		"negative": "text, watermark",
		"width":    1024,
		"height":   768,
		"steps":    30,
		"seed":     7,
	}, Model{})

	want := "out.png a cat --negative text, watermark --width 1024 --height 768 --steps 30 --seed 7"
	if got := argv(args); got != want {
		t.Fatalf("zero Model must pass no model flags.\ngot:  %s\nwant: %s", got, want)
	}
	for _, f := range []string{"--ckpt", "--vae", "--cfg", "--sampler", "--scheduler"} {
		hasNot(t, args, f)
	}
}

// TestBuildArgs_DiTBinding covers the workstation's HiDream binding: a DiT checkpoint
// decoded with the VAE the checkpoint LOADER supplies. "builtin" must be forwarded
// verbatim — comfy-render.mjs turns it into a VAEDecode wired to the checkpoint loader
// instead of a standalone (4-channel, incompatible) sdxl_vae.
func TestBuildArgs_DiTBinding(t *testing.T) {
	args := buildArgs("o.png", "a keyboard", nil, Model{
		Ckpt:      "hidream_o1_image_dev_mxfp8.safetensors",
		VAE:       "builtin",
		Steps:     20,
		CFG:       5,
		Sampler:   "euler",
		Scheduler: "simple",
	})

	has(t, args, "--ckpt", "hidream_o1_image_dev_mxfp8.safetensors")
	has(t, args, "--vae", "builtin")
	has(t, args, "--steps", "20")
	has(t, args, "--cfg", "5")
	has(t, args, "--sampler", "euler")
	has(t, args, "--scheduler", "simple")
}

// TestBuildArgs_SDXLBinding proves the same mechanism binds an SDXL machine, with a
// standalone VAE and SDXL's own sampler tuning. Nothing about either model is
// special-cased in Go — the roster is data.
func TestBuildArgs_SDXLBinding(t *testing.T) {
	args := buildArgs("o.png", "a cat", nil, Model{
		Ckpt:      "RealVisXL_V5.0_fp16.safetensors",
		VAE:       "sdxl_vae.safetensors",
		CFG:       7,
		Scheduler: "karras",
	})

	has(t, args, "--ckpt", "RealVisXL_V5.0_fp16.safetensors")
	has(t, args, "--vae", "sdxl_vae.safetensors")
	has(t, args, "--cfg", "7")
	has(t, args, "--scheduler", "karras")
	hasNot(t, args, "--steps")   // Steps unset -> script default
	hasNot(t, args, "--sampler") // Sampler unset -> script default
}

// TestBuildArgs_RequestStepsOverrideMachineDefault: the machine's steps is a DEFAULT,
// not a ceiling. A caller tuning one render must win, and --steps must not be passed
// twice (the renderer's flag parser takes the last occurrence, but emitting a
// duplicate is a bug waiting to bite).
func TestBuildArgs_RequestStepsOverrideMachineDefault(t *testing.T) {
	args := buildArgs("o.png", "p", map[string]any{"steps": 8}, Model{Steps: 20})

	has(t, args, "--steps", "8")
	n := 0
	for _, a := range args {
		if a == "--steps" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("--steps emitted %d times, want 1: %s", n, argv(args))
	}
}

func TestBatchArgs_BindingAndPaths(t *testing.T) {
	m := Model{Ckpt: "hidream.safetensors", VAE: "builtin", Steps: 20, CFG: 5, Sampler: "euler", Scheduler: "simple", Family: "hidream-o1"}
	got := batchArgs(`D:\jobs.jsonl`, `D:\jobs.results.jsonl`, m)
	want := []string{"--batch", `D:\jobs.jsonl`, "--results", `D:\jobs.results.jsonl`,
		"--ckpt", "hidream.safetensors", "--vae", "builtin", "--steps", "20",
		"--cfg", "5", "--sampler", "euler", "--scheduler", "simple", "--family", "hidream-o1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batchArgs mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestBatchArgs_ZeroModelEmitsNoBindingFlags(t *testing.T) {
	got := batchArgs("j.jsonl", "r.jsonl", Model{})
	want := []string{"--batch", "j.jsonl", "--results", "r.jsonl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero model must add no flags: got %v", got)
	}
}

func TestEditArgs_FullBinding(t *testing.T) {
	m := EditModel{Unet: "qwen-edit.gguf", Preset: "lightning8", LoRA: "light8.safetensors",
		LoRAStrength: 0.8, CLIP: "qwen_vl.safetensors", VAE: "qwen_vae.safetensors",
		Steps: 8, CFG: 1, Sampler: "euler", Scheduler: "simple", Megapixels: 2}
	got := editArgs("o.png", "in.png", "make it snow", map[string]any{"seed": 9, "negative": "blurry"}, m)
	want := []string{"o.png", "in.png", "make it snow",
		"--negative", "blurry", "--seed", "9", "--unet", "qwen-edit.gguf",
		"--preset", "lightning8", "--clip", "qwen_vl.safetensors", "--vae", "qwen_vae.safetensors",
		"--lora", "light8.safetensors", "--lora-strength", "0.8",
		"--steps", "8", "--cfg", "1", "--sampler", "euler", "--scheduler", "simple",
		"--megapixels", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("editArgs:\n got %v\nwant %v", got, want)
	}
}

// An UNSET canvas must stay unset on the command line. The runner's default is
// "measure the source and keep its resolution, capped"; passing a number here — any
// number — would overwrite that with a fixed size for every edit on the machine.
func TestEditArgs_UnsetMegapixelsStaysOffTheCommandLine(t *testing.T) {
	joined := strings.Join(editArgs("o.png", "in.png", "p", nil, EditModel{Unet: "u.gguf"}), " ")
	if strings.Contains(joined, "--megapixels") {
		t.Fatalf("an unset canvas must not reach the runner: %s", joined)
	}
	joined = strings.Join(editArgs("o.png", "in.png", "p", nil,
		EditModel{Unet: "u.gguf", Megapixels: 1.5}), " ")
	if !strings.Contains(joined, "--megapixels 1.5") {
		t.Fatalf("a configured canvas must reach the runner: %s", joined)
	}
}

func TestEditArgs_ZeroModelAddsNoFlags(t *testing.T) {
	got := editArgs("o.png", "in.png", "p", nil, EditModel{})
	want := []string{"o.png", "in.png", "p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero model must add no flags: got %v", got)
	}
}

// A per-request preset is the supported speed/fidelity dial and must beat the
// machine binding — otherwise a caller asking for `full` silently gets Lightning.
func TestEditArgs_RequestPresetAndStepsWin(t *testing.T) {
	m := EditModel{Unet: "u.gguf", Preset: "lightning8", Steps: 8}
	joined := strings.Join(editArgs("o.png", "in.png", "p",
		map[string]any{"preset": "full", "steps": 40, "cfg": 3.0}, m), " ")
	if !strings.Contains(joined, "--preset full") || strings.Contains(joined, "--preset lightning8") {
		t.Fatalf("request preset must win: %s", joined)
	}
	if !strings.Contains(joined, "--steps 40") || strings.Contains(joined, "--steps 8") {
		t.Fatalf("request steps must win: %s", joined)
	}
	if !strings.Contains(joined, "--cfg 3") {
		t.Fatalf("request cfg must reach the runner: %s", joined)
	}
}

// An explicit empty lora means "base model, no distillation" and has to override a
// configured LoRA. Falling through to the binding would silently keep distilling.
func TestEditArgs_ExplicitEmptyLoRAOverridesBinding(t *testing.T) {
	m := EditModel{Unet: "u.gguf", LoRA: "light8.safetensors"}
	joined := strings.Join(editArgs("o.png", "in.png", "p", map[string]any{"lora": ""}, m), " ")
	if strings.Contains(joined, "light8.safetensors") {
		t.Fatalf("explicit empty lora must override the binding: %s", joined)
	}
	if !strings.Contains(joined, "--lora ") {
		t.Fatalf("explicit empty lora must still reach the runner: %s", joined)
	}
	// absent (not explicit) falls through to the binding
	fallthroughJoined := strings.Join(editArgs("o.png", "in.png", "p", nil, m), " ")
	if !strings.Contains(fallthroughJoined, "--lora light8.safetensors") {
		t.Fatalf("absent lora must fall through to the binding: %s", fallthroughJoined)
	}
}

func TestInpaintArgs_FullBinding(t *testing.T) {
	m := InpaintModel{Ckpt: "sdxl.safetensors", VAE: "builtin", Steps: 34, CFG: 6.5, Sampler: "dpmpp_2m", Scheduler: "karras"}
	got := inpaintArgs("o.png", "in.png", "m.png", "clean it", map[string]any{"seed": 9, "denoise": 0.85, "grow_mask": 24, "negative": "text"}, m)
	want := []string{"o.png", "in.png", "m.png", "clean it",
		"--negative", "text", "--seed", "9", "--grow-mask", "24", "--denoise", "0.85",
		"--ckpt", "sdxl.safetensors", "--vae", "builtin", "--steps", "34",
		"--cfg", "6.5", "--sampler", "dpmpp_2m", "--scheduler", "karras"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inpaintArgs:\n got %v\nwant %v", got, want)
	}
}

func TestInpaintArgs_RequestStepsWin(t *testing.T) {
	m := InpaintModel{Ckpt: "sdxl.safetensors", Steps: 34}
	got := inpaintArgs("o.png", "in.png", "m.png", "p", map[string]any{"steps": 12}, m)
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--steps 12") || strings.Contains(joined, "--steps 34") {
		t.Fatalf("request steps must win: %v", got)
	}
}

// Review fix: an explicit grow_mask of 0 must reach the runner (--grow-mask 0), not
// silently fall back to the node default of 16; an ABSENT grow_mask emits nothing.
func TestInpaintArgsGrowMaskZeroVsAbsent(t *testing.T) {
	withZero := inpaintArgs("o.png", "i.png", "m.png", "p", map[string]any{"grow_mask": 0}, InpaintModel{})
	found := false
	for i, a := range withZero {
		if a == "--grow-mask" && i+1 < len(withZero) && withZero[i+1] == "0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit grow_mask 0 must emit --grow-mask 0, got %v", withZero)
	}
	absent := inpaintArgs("o.png", "i.png", "m.png", "p", map[string]any{}, InpaintModel{})
	for _, a := range absent {
		if a == "--grow-mask" {
			t.Fatalf("absent grow_mask must emit nothing, got %v", absent)
		}
	}
}

// TestSdcppArgs_FullBinding: the sdcpp argv carries every configured binding flag,
// positionals first, extra args as repeated --extra tokens (the runner owns the
// mapping to sd.cpp's real CLI — no sd.cpp flag names appear at this layer).
func TestSdcppArgs_FullBinding(t *testing.T) {
	m := SdcppModel{
		Bin:       `C:\sd\sd.exe`,
		Model:     `D:\models\z-image-turbo-Q8_0.gguf`,
		ModelKind: "diffusion",
		VAE:       `D:\models\zimage_vae.safetensors`,
		ClipL:     `D:\models\clip_l.safetensors`,
		ClipG:     `D:\models\clip_g.safetensors`,
		T5:        `D:\models\t5xxl_fp16.safetensors`,
		Steps:     8,
		CFG:       1,
		Sampler:   "euler",
		ExtraArgs: []string{"--vae-on-cpu", "--clip-on-cpu"},
	}
	args := sdcppArgs("out.png", "a red fox", map[string]any{"negative": "blurry", "width": 1024, "height": 768, "seed": 42}, m)
	if args[0] != "out.png" || args[1] != "a red fox" {
		t.Fatalf("positionals wrong: %s", argv(args))
	}
	has(t, args, "--negative", "blurry")
	has(t, args, "--width", "1024")
	has(t, args, "--height", "768")
	has(t, args, "--seed", "42")
	has(t, args, "--steps", "8")
	has(t, args, "--model", `D:\models\z-image-turbo-Q8_0.gguf`)
	has(t, args, "--model-kind", "diffusion")
	has(t, args, "--vae", `D:\models\zimage_vae.safetensors`)
	has(t, args, "--clip-l", `D:\models\clip_l.safetensors`)
	has(t, args, "--clip-g", `D:\models\clip_g.safetensors`)
	has(t, args, "--t5xxl", `D:\models\t5xxl_fp16.safetensors`)
	has(t, args, "--cfg", "1")
	has(t, args, "--sampler", "euler")
	// both extra args present as --extra pairs, in order
	n := 0
	for i, a := range args {
		if a == "--extra" {
			want := m.ExtraArgs[n]
			if args[i+1] != want {
				t.Fatalf("--extra[%d] = %q, want %q in: %s", n, args[i+1], want, argv(args))
			}
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want 2 --extra pairs, got %d in: %s", n, argv(args))
	}
}

// TestSdcppArgs_RequestStepsWinAndZeroBinding: a per-request steps overrides the
// binding's steps (same contract as the ComfyUI path), and a zero binding passes
// no binding flags at all.
func TestSdcppArgs_RequestStepsWinAndZeroBinding(t *testing.T) {
	m := SdcppModel{Model: "m.gguf", Steps: 20}
	args := sdcppArgs("o.png", "p", map[string]any{"steps": 4}, m)
	has(t, args, "--steps", "4")
	zero := SdcppModel{Model: "m.gguf"}
	zargs := sdcppArgs("o.png", "p", nil, zero)
	if want := "o.png p --model m.gguf"; argv(zargs) != want {
		t.Fatalf("zero binding argv = %q, want %q", argv(zargs), want)
	}
	hasNot(t, zargs, "--model-kind")
	hasNot(t, zargs, "--vae")
	hasNot(t, zargs, "--extra")
}
