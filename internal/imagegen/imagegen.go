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
	// Family selects the MODEL-CORRECT graph in comfy-render.mjs ("" = the generic
	// SDXL-shaped graph). "hidream-o1" / "hidream-o1-dev" render the official
	// pixel-space DiT graph (ModelNoiseScale, patch-seam smoothing, SamplerCustom,
	// native 2048) — driving a DiT through the generic graph produces measurable
	// 32px patch blocking. Which family a checkpoint belongs to is per-machine
	// config, never shared code.
	Family string
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
	// COMFY_WAIT_SEC aligns the render script's poll budget with the harness timeout
	// (quality-first: a 2048 bf16 O1 render legitimately runs tens of minutes; the
	// script must not give up before the Go timeout — which stays the hard stop).
	env := []string{"COMFY_DIR=" + comfyDir}
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	return gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    args,
		Env:     append(env, extraEnv...),
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
	b := bindingArgs(m)
	if m.Steps > 0 && gpugen.AsInt(params["steps"]) > 0 {
		// request set steps: strip the binding's "--steps N" pair from the shared args
		for i := 0; i < len(b)-1; i++ {
			if b[i] == "--steps" {
				b = append(b[:i], b[i+2:]...)
				break
			}
		}
	}
	args = append(args, b...)
	return args
}

// bindingArgs emits this machine's model-binding flags (shared by single and batch
// argv builders). NOTE the difference from buildArgs: batch has no per-request steps
// param at this layer (per-job steps live in the jobs JSONL and win inside
// batch-jobs.mjs), so the binding's Steps is always emitted when set.
func bindingArgs(m Model) []string {
	var args []string
	if m.Ckpt != "" {
		args = append(args, "--ckpt", m.Ckpt)
	}
	if m.VAE != "" {
		args = append(args, "--vae", m.VAE)
	}
	if m.Steps > 0 {
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
	if m.Family != "" {
		args = append(args, "--family", m.Family)
	}
	return args
}

// InpaintModel is the machine's inpaint binding (SDXL-class; see config). Inpainting
// is masked latent re-denoise (VAEEncodeForInpaint) — a pixel-space DiT (HiDream)
// cannot drive it, so this binding is separate from Model even on a HiDream box.
type InpaintModel struct {
	Ckpt, VAE, Sampler, Scheduler string
	Steps                         int
	CFG                           float64
}

// inpaintArgs assembles the comfy-inpaint.mjs argv. Pure; unit-tested. Request
// steps wins over m.Steps (same rule as buildArgs).
func inpaintArgs(out, image, mask, prompt string, params map[string]any, m InpaintModel) []string {
	args := []string{out, image, mask, prompt}
	if n, ok := params["negative"].(string); ok && n != "" {
		args = append(args, "--negative", n)
	}
	if v := gpugen.AsInt(params["seed"]); v > 0 {
		args = append(args, "--seed", strconv.Itoa(v))
	}
	// Presence-gated, not >0: an explicit grow_mask of 0 (tight mask, no latent
	// dilation) is a valid request and must reach the runner as --grow-mask 0
	// rather than silently falling back to the node default of 16.
	if _, present := params["grow_mask"]; present {
		if v := gpugen.AsInt(params["grow_mask"]); v >= 0 {
			args = append(args, "--grow-mask", strconv.Itoa(v))
		}
	}
	if f, ok := params["denoise"].(float64); ok && f > 0 && f <= 1 {
		args = append(args, "--denoise", strconv.FormatFloat(f, 'g', -1, 64))
	}
	if m.Ckpt != "" {
		args = append(args, "--ckpt", m.Ckpt)
	}
	if m.VAE != "" {
		args = append(args, "--vae", m.VAE)
	}
	reqSteps := gpugen.AsInt(params["steps"])
	if reqSteps > 0 {
		args = append(args, "--steps", strconv.Itoa(reqSteps))
	} else if m.Steps > 0 {
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

// Inpaint re-renders ONLY the masked region of image on the LOCAL ComfyUI (free).
// Same lifecycle guards as Generate: gpugen tree-kill on timeout + deferred /free.
func Inpaint(ctx context.Context, node, script, comfyDir, out, image, mask, prompt string, params map[string]any, m InpaintModel, timeout time.Duration, extraEnv ...string) (string, error) {
	env := []string{"COMFY_DIR=" + comfyDir}
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	return gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    inpaintArgs(out, image, mask, prompt, params, m),
		Env:     append(env, extraEnv...),
		Out:     out,
		Timeout: timeout,
	})
}

// batchArgs assembles the comfy-generate.mjs --batch argv. Pure; unit-tested.
func batchArgs(jobsPath, resultsPath string, m Model) []string {
	return append([]string{"--batch", jobsPath, "--results", resultsPath}, bindingArgs(m)...)
}

// GenerateBatch renders every job in jobsPath (JSONL: {"prompt","out",...} per line)
// through ONE warm ComfyUI session and writes one result line per job to resultsPath.
// The results file is the gpugen success gate: the script exits 0 with a complete
// results file even when individual renders failed (the caller reads per-job status),
// while a crash/timeout/GPU-busy exits non-zero and errors here. timeout bounds the
// WHOLE batch.
func GenerateBatch(ctx context.Context, node, script, comfyDir, jobsPath, resultsPath string, m Model, timeout time.Duration, extraEnv ...string) error {
	env := []string{"COMFY_DIR=" + comfyDir}
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	_, err := gpugen.Generate(ctx, gpugen.Spec{
		Exe:     node,
		Script:  script,
		Args:    batchArgs(jobsPath, resultsPath, m),
		Env:     append(env, extraEnv...),
		Out:     resultsPath,
		Timeout: timeout,
	})
	return err
}
