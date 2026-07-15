// Package imagegen generates an image from a text prompt by shelling out to the repo's
// render/comfy-generate.mjs (Node), which wraps the proven comfy-render.mjs with the
// GPU-lock + ComfyUI start/stop lifecycle. The render runs on the LOCAL ComfyUI (SDXL/
// RealVisXL) — free, no cloud. It is now a THIN caller of internal/gpugen, which owns
// the shared exec lifecycle (process-tree-kill on timeout, WaitDelay, defer
// freeComfyVRAM) so image/video/audio all get the SAME guards. Behavior is preserved
// byte-for-byte; only the lifecycle was lifted into gpugen.
package imagegen

import (
	"context"
	"strconv"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpugen"
)

// Model is THIS machine's image-model binding, read from config. The harness is
// hardware- and model-agnostic: an 8GB laptop runs SDXL, a 16GB workstation may run
// a DiT (HiDream). No model name belongs in shared code, so every field here is
// optional — a zero Model passes no flags at all and comfy-render.mjs keeps its own
// defaults, leaving an existing SDXL machine byte-for-byte unchanged.
type Model struct {
	Ckpt string // CheckpointLoaderSimple filename ("" = script default)
	// VAE is a standalone VAE filename, or "builtin" to decode with the VAE the
	// checkpoint loader supplies (required for HiDream: it ships no VAE weights, so a
	// standalone 4-channel sdxl_vae cannot decode its output). "" = script default.
	VAE       string
	Steps     int     // 0 = script default
	CFG       float64 // 0 = script default
	Sampler   string  // "" = script default
	Scheduler string  // "" = script default
}

// Generate runs `node <script> <out> <prompt> [--negative ..] [--width ..] ...` and
// returns out on success. node is the node executable ("" => "node"); script is the
// absolute path to comfy-generate.mjs; comfyDir is exported as COMFY_DIR for the script.
// params may carry negative (string) and width/height/steps/seed (int-ish); m carries
// the machine's image-model binding. A per-request steps param overrides m.Steps.
// extraEnv appends additional "K=V" entries (LO-1: the pipeline threads a configured
// GPU_LOCK override so the runner contends on the same lock the Go vision gate
// watches). A non-zero exit, a timeout, or a missing/empty output file returns an
// error (the caller defers).
func Generate(ctx context.Context, node, script, comfyDir, out, prompt string, params map[string]any, m Model, timeout time.Duration, extraEnv ...string) (string, error) {
	args := buildArgs(out, prompt, params, m)
	return gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    args,
		Env:     append([]string{"COMFY_DIR=" + comfyDir}, extraEnv...),
		Out:     out,
		Timeout: timeout,
	})
}

// buildArgs assembles the comfy-generate.mjs argv. Split out from Generate so the
// model binding is unit-testable without spawning ComfyUI.
func buildArgs(out, prompt string, params map[string]any, m Model) []string {
	args := []string{out, prompt}
	if n, ok := params["negative"].(string); ok && n != "" {
		args = append(args, "--negative", n)
	}
	for _, k := range []string{"width", "height", "steps", "seed"} {
		if v := gpugen.AsInt(params[k]); v > 0 {
			args = append(args, "--"+k, strconv.Itoa(v))
		}
	}
	// This machine's model binding. Steps is applied only when the request did not
	// set it (the request wins), so a caller can still tune a single render.
	if m.Ckpt != "" {
		args = append(args, "--ckpt", m.Ckpt)
	}
	if m.VAE != "" {
		args = append(args, "--vae", m.VAE)
	}
	if m.Steps > 0 && gpugen.AsInt(params["steps"]) <= 0 {
		args = append(args, "--steps", strconv.Itoa(m.Steps))
	}
	if m.CFG > 0 {
		args = append(args, "--cfg", strconv.FormatFloat(m.CFG, 'g', -1, 64))
	}
	if m.Sampler != "" {
		args = append(args, "--sampler", m.Sampler)
	}
	if m.Scheduler != "" {
		args = append(args, "--scheduler", m.Scheduler)
	}
	return args
}
