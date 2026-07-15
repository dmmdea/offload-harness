package imagegen

import (
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
