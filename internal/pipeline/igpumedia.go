package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/mediaops"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// The iGPU media engines (CT-49): generate_video on an sdcpp family, animate_character
// with animategen_engine sdcpp, and generate_audio voice/music with the audiocpp engines.
// Each is a spawn-per-job native CLI behind a render/*.mjs runner, under the same media
// lease, timeout, footprint sampling and result shape as the ComfyUI route it replaces on
// a box that binds the engine. A box that binds none of the engine keys never reaches this
// file: every seam below is gated on the engine key.
//
// No model runs on CPU on these engines. A cpu (or unset) backend is a typed defer here
// (error class cpu_backend_refused) even though config.Load refuses it too: an in-process
// Config never went through Load. The runners add the CPU_PLACEMENT log guard.

const (
	errClassCPUBackendRefused = "cpu_backend_refused"
	errClassExtraArgsRefused  = "extra_args_refused"
	errClassTokenCapExceeded  = "token_cap_exceeded"
	errClassDeviceInvalid     = "device_invalid" // audiocpp_device is not a device index: a config error, not a backend refusal

	// What the runners render when the caller names no size or length (render/sdcpp-video.mjs
	// and sdcpp-animate.mjs DEFAULT_*): the token cap is computed on what the runner will
	// actually render. render/testdata/token-cap-table.json pins the Go and Node sides together.
	defaultIGPUFrames = 49
	defaultIGPUWidth  = 832
	defaultIGPUHeight = 480

	defaultSdcppVideoScript   = "render/sdcpp-video.mjs"
	defaultSdcppAnimateScript = "render/sdcpp-animate.mjs"
	defaultAudiocppScript     = "render/audiocpp-generate.mjs"
)

// igpuEnv is the env for an iGPU runner: the shared gen env (MEMORY_STACK for the
// llama-swap unload, FFMPEG_PATH for the mp4/loudness steps) without COMFY_DIR, which
// none of these engines has any use for.
func (p *Pipeline) igpuEnv() []string {
	var out []string
	for _, e := range p.genEnv() {
		if strings.HasPrefix(e, "COMFY_DIR=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// deferCPUBackend is the typed defer for a CPU or unset backend.
func (p *Pipeline) deferCPUBackend(req core.Request, meta core.Meta, start time.Time, what string, err error) core.Result {
	return p.deferRefused(req, meta, start, errClassCPUBackendRefused, what, err)
}

// deferRefused is the typed, non-retryable defer for a request an iGPU lane refuses before it
// takes the media lease or spawns anything: err_class names why (cpu_backend_refused,
// extra_args_refused, token_cap_exceeded).
func (p *Pipeline) deferRefused(req core.Request, meta core.Meta, start time.Time, class, what string, err error) core.Result {
	meta.ErrClass = class
	return p.deferGen(req, meta, start, len(req.Input), what+" refused: "+err.Error())
}

// orDefault is n when it is set, else the runner's own default.
func orDefault(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

// resolveIGPUScript resolves a configured runner (relative to the executable dir like every
// render script), falling back to its default name when the key is empty.
func resolveIGPUScript(configured, def string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		configured = def
	}
	return gpugen.ResolveScript(configured)
}

// quantFromModelFile reads a GGUF quant out of a model file's basename (longest token first,
// so BF16 never reads as F16 and Q4_K_M never as Q4_K), "" when it names none. The footprint
// store keys on it, as it does for the sdcpp image route.
func quantFromModelFile(path string) string {
	up := strings.ToUpper(filepath.Base(path))
	for _, q := range []string{"Q8_0", "Q6_K", "Q5_K_M", "Q5_K_S", "Q5_K", "Q5_1", "Q4_K_M", "Q4_K_S", "Q4_K", "Q4_1", "Q4_0", "Q3_K", "Q2_K", "BF16", "F16"} {
		if strings.Contains(up, q) {
			return strings.ToLower(q)
		}
	}
	return ""
}

// normalizeVideoFrames is the runner's own rule applied up front, so the argv this process
// sends is the argv sd-cli gets: the NEAREST 4k+1 (a tie goes up), minimum 5. 0 (unset) stays
// 0 so the runner's default applies.
func normalizeVideoFrames(n int) int {
	if n <= 0 {
		return 0
	}
	if n <= 5 {
		return 5
	}
	down := (n-1)/4*4 + 1
	up := down + 4
	if n-down < up-n {
		return down
	}
	return up
}

// floorTo32 floors a width/height to a multiple of 32 (minimum 32); 0 stays 0.
func floorTo32(n int) int {
	if n <= 0 {
		return 0
	}
	if n < 32 {
		return 32
	}
	return n / 32 * 32
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// timeoutArgs arms the runner's own deadline a margin BEFORE gpugen's, so a run that is too slow
// ends with the runner's own typed timeout after it has killed the engine's whole tree and removed
// its temp dirs, instead of being cut down from outside (gpugen SIGTERMs the runner's process
// group and SIGKILLs it after a grace). The margin is 15 s (a quarter of the budget when that is
// under a minute); no timeout, no flag.
func timeoutArgs(timeout time.Duration) []string {
	if timeout <= 0 {
		return nil
	}
	margin := 15 * time.Second
	if timeout < time.Minute {
		margin = timeout / 4
	}
	secs := int((timeout - margin) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return []string{"--timeout-sec", strconv.Itoa(secs)}
}

// extraArgsFlag is the JSON-array form the runners parse (a token with spaces survives).
func extraArgsFlag(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	b, _ := json.Marshal(extra)
	return []string{"--extra-args", string(b)}
}

// runnerArgs builds a runner's argv in the shape every iGPU runner parses: the flags first, then a
// bare `--`, then the positionals. The terminator is what keeps a prompt, TTS text or lyrics that
// starts with "--" (a lyrics section marker such as "--- Intro ---") a positional instead of being
// read as a flag that swallows the next token (render/igpu-engine.mjs parseArgs).
func runnerArgs(flags []string, positionals ...string) []string {
	out := make([]string, 0, len(flags)+1+len(positionals))
	out = append(out, flags...)
	out = append(out, "--")
	return append(out, positionals...)
}

// resolveEngineBin is the ONE binary-resolution rule for the iGPU engines: the bound value is
// resolved the way the spawned child resolves it (mediaops.ResolveBinary: an explicit path is
// stat'd, a bare name is looked up on PATH, as mediacap reports it) and the ABSOLUTE path is what
// the runner gets. The runners refuse a non-absolute path, so a bare "sd-cli" that doctor shows
// CONFIGURED can never pass doctor and then fail every call in the runner.
func resolveEngineBin(key, bound string) (string, error) {
	p, ok := mediaops.ResolveBinary(strings.TrimSpace(bound))
	if !ok {
		return "", fmt.Errorf("%s=%s not found (no such file, and not on PATH)", key, bound)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%s=%s: %v", key, bound, err)
	}
	return abs, nil
}

// ensureOutDir makes the directory a result will land in BEFORE the lease is taken, so a
// minutes-long render never ends in "cannot write the output"; the error is reported, not
// dropped (the runner checks again, for a hand run).
func ensureOutDir(out string) error {
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create the output directory %s: %v", dir, err)
	}
	return nil
}

// igpuNotes accumulates the notes an iGPU lane puts on its result ("notes" in the payload, only
// when there is one): what the request asked for that the lane could not honor.
type igpuNotes []string

func (n igpuNotes) addTo(payload map[string]any) {
	if len(n) > 0 {
		payload["notes"] = []string(n)
	}
}

// igpuRun is one runner invocation's identity: how it is leased, tagged and worded.
type igpuRun struct {
	leaseReason string
	failVerb    string // "video generation failed"
	fpFamily    string
	fpQuant     string
	fpTask      string
	script      string
	args        []string
	out         string
	timeout     time.Duration
}

// generateIGPU is gpugen.Generate; a var so a test can capture the Spec a lane hands it (the flags
// that make a cancel reach the engine are only visible there).
var generateIGPU = gpugen.Generate

// runIGPU takes the media lease, runs the script under gpugen (process-tree-killed on
// timeout, no ComfyUI /free) and returns the produced file, or the defer result.
func (p *Pipeline) runIGPU(ctx context.Context, req core.Request, meta *core.Meta, start time.Time, r igpuRun) (string, *core.Result) {
	// an iGPU engine has no ComfyUI instance to bind to a card: the whole node, as the sdcpp image lane
	grant, lerr := p.acquireMediaLease(ctx, r.leaseReason, r.timeout, p.gpuWait(), wholeNeed(paramStr(req.Params, "waiter_token")).resumableBy(req))
	if lerr != nil {
		res := p.deferForLease(lerr, req.Task, *meta, len(req.Input), start)
		return "", &res
	}
	defer grant.Release()
	spec := gpugen.Spec{
		Exe:           p.cfg.NodePath,
		Script:        r.script,
		Args:          r.args,
		Env:           append(p.igpuEnv(), grant.Env...),
		Out:           r.out,
		Timeout:       r.timeout,
		SkipFreeComfy: true,
		// the engine is a native binary under the runner: signal the runner's whole group so a
		// cancel or timeout reaches it (and its temp dirs) instead of killing node alone
		OwnProcessGroup: true,
	}
	p.footprintSampling(r.fpFamily, r.fpQuant, r.fpTask).ApplyTo(&spec)
	outPath, gerr := generateIGPU(ctx, spec)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		res := p.deferGen(req, *meta, start, len(req.Input), r.failVerb+": "+gerr.Error())
		return "", &res
	}
	return outPath, nil
}

// ---------------------------------------------------------------- video

// sdcppVideoBinding reports the resolved sdcpp binding a generate_video request renders
// with, when its family is bound to the sdcpp engine. Pure: resolveVideoFamily and
// ResolveVideoFamilyBinding are, so asking it up front leaves the ComfyUI path untouched.
func (p *Pipeline) sdcppVideoBinding(req core.Request) (renderFamily string, fb config.VideoFamilyBinding, ok bool) {
	_, renderFamily = resolveVideoFamily(p.cfg, paramStr(req.Params, "model"))
	fb = p.cfg.ResolveVideoFamilyBinding(renderFamily)
	return renderFamily, fb, fb.UsesSdcpp()
}

// sdcppRenderFamily is the family an sdcpp video render is recorded under. A request that names
// no model on a box whose default family is an sdcpp one that is spelled like a ComfyUI family
// (wan22, ...) arrives with renderFamily "": the ledger and the footprint store key on THAT
// family, the one config.DefaultVideoSdcppFamily (and mediacap) name. The fleet advertises the
// same family (fleetnode.familyFor; TestEngineLaneFamiliesMatchTheAdvertisedOnes pins the two).
func sdcppRenderFamily(cfg config.Config, renderFamily string) string {
	if renderFamily != "" {
		return renderFamily
	}
	if def, ok := cfg.DefaultVideoSdcppFamily(); ok {
		return def
	}
	return "sdcpp"
}

// runGenerateVideoSdcpp renders generate_video through render/sdcpp-video.mjs.
// params as the ComfyUI route: still/out/negative/seed/steps/frames/width/height; a
// per-request value wins over the family binding's default. fast=true decodes with the family's
// tiny autoencoder (sdcpp_tae, opt-in); without that key fast is a no-op on this lane and the
// result's notes say so.
func (p *Pipeline) runGenerateVideoSdcpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time, renderFamily string, fb config.VideoFamilyBinding) core.Result {
	renderFamily = sdcppRenderFamily(p.cfg, renderFamily)
	meta.Model = "sdcpp-video:" + renderFamily
	meta.License = fb.License
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty video prompt")
	}
	if err := config.CPUBackendRefusal(fb.SdcppBackend); err != nil {
		return p.deferCPUBackend(req, meta, start, "video generation", err)
	}
	if err := config.ExtraArgsRefusal("sdcpp_extra_args", config.ExtraArgsSdcpp, fb.SdcppExtraArgs); err != nil {
		return p.deferRefused(req, meta, start, errClassExtraArgsRefused, "video generation", err)
	}
	for _, m := range []struct{ key, v string }{{"sdcpp_bin", fb.SdcppBin}, {"sdcpp_model", fb.SdcppModel}, {"sdcpp_vae", fb.SdcppVAE}, {"sdcpp_t5xxl", fb.SdcppT5xxl}} {
		if strings.TrimSpace(m.v) == "" {
			return p.deferGen(req, meta, start, len(req.Input), fmt.Sprintf("video family %q is bound to sdcpp but %s is not configured", renderFamily, m.key))
		}
	}
	script, serr := resolveIGPUScript(p.cfg.VideoGenSdcppScript, defaultSdcppVideoScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	pick := func(k string, def int) int {
		if v := paramIntOr(req.Params, k, 0); v > 0 {
			return v
		}
		return def
	}
	frames := normalizeVideoFrames(pick("frames", fb.Frames))
	width := floorTo32(pick("width", fb.Width))
	height := floorTo32(pick("height", fb.Height))
	// The token cap keeps one GPU dispatch inside the amdgpu 2 s lockup timeout. It is
	// computed here, on what the runner will render, BEFORE the media lease is taken; the
	// runner computes it again from the same flags. No cap configured = no check.
	if fb.SdcppMaxTokens > 0 {
		if err := config.TokenCapRefusal("sdcpp_max_tokens", orDefault(width, defaultIGPUWidth), orDefault(height, defaultIGPUHeight),
			orDefault(frames, defaultIGPUFrames), fb.SdcppVAEStride, 0, fb.SdcppMaxTokens); err != nil {
			return p.deferRefused(req, meta, start, errClassTokenCapExceeded, "video generation", err)
		}
	}
	// One binary-resolution rule: the runner gets the ABSOLUTE path the bound name resolves to.
	bin, berr := resolveEngineBin("sdcpp_bin", fb.SdcppBin)
	if berr != nil {
		return p.deferGen(req, meta, start, len(req.Input), "video generation refused: "+berr.Error())
	}
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	still := paramStr(req.Params, "still")
	if still == "" {
		still = req.Image
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		out = filepath.Join(p.cfg.MediaDir, "video-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}
	if err := ensureOutDir(out); err != nil {
		return p.deferGen(req, meta, start, len(req.Input), "video generation refused: "+err.Error())
	}
	timeout := time.Duration(p.cfg.VideoGenTimeoutSec) * time.Second

	var notes igpuNotes
	tae := ""
	if paramBool(req.Params, "fast") {
		switch {
		case strings.TrimSpace(fb.SdcppTAE) == "":
			notes = append(notes, fmt.Sprintf("fast=true is a no-op on the sdcpp video lane: family %q binds no sdcpp_tae, so the full VAE decoded", renderFamily))
		case !fileExistsAt(fb.SdcppTAE):
			notes = append(notes, fmt.Sprintf("fast=true could not use the tiny autoencoder: sdcpp_tae=%s does not exist, so the full VAE decoded", fb.SdcppTAE))
		default:
			tae = fb.SdcppTAE
			notes = append(notes, "fast=true decoded with the tiny autoencoder "+filepath.Base(tae)+" (approximate against the full VAE)")
		}
	}

	flags := []string{"--sd-bin", bin, "--model", fb.SdcppModel, "--vae", fb.SdcppVAE, "--t5xxl", fb.SdcppT5xxl,
		"--backend", fb.SdcppBackend}
	if fb.SdcppHighNoiseModel != "" {
		flags = append(flags, "--high-noise-model", fb.SdcppHighNoiseModel)
		// the high-noise expert's own recipe (A14B pair); sd-cli's own default there is cfg 7.0
		if fb.HighNoiseCFG > 0 {
			flags = append(flags, "--high-noise-cfg", fmtFloat(fb.HighNoiseCFG))
		}
		if fb.HighNoiseSteps > 0 {
			flags = append(flags, "--high-noise-steps", strconv.Itoa(fb.HighNoiseSteps))
		}
		if fb.HighNoiseSampler != "" {
			flags = append(flags, "--high-noise-sampler", fb.HighNoiseSampler)
		}
	}
	if tae != "" {
		flags = append(flags, "--tae", tae)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		flags = append(flags, "--negative", n)
	}
	if frames > 0 {
		flags = append(flags, "--frames", strconv.Itoa(frames))
	}
	if width > 0 {
		flags = append(flags, "--width", strconv.Itoa(width))
	}
	if height > 0 {
		flags = append(flags, "--height", strconv.Itoa(height))
	}
	if fb.SdcppMaxTokens > 0 {
		flags = append(flags, "--max-tokens", strconv.Itoa(fb.SdcppMaxTokens), "--vae-stride", strconv.Itoa(fb.SdcppVAEStride))
	}
	if fb.FPS > 0 {
		flags = append(flags, "--fps", strconv.Itoa(fb.FPS))
	}
	if v := pick("steps", fb.Steps); v > 0 {
		flags = append(flags, "--steps", strconv.Itoa(v))
	}
	if fb.CFG > 0 {
		flags = append(flags, "--cfg", fmtFloat(fb.CFG))
	}
	if fb.FlowShift > 0 {
		flags = append(flags, "--flow-shift", fmtFloat(fb.FlowShift))
	}
	if fb.Sampler != "" {
		flags = append(flags, "--sampler", fb.Sampler)
	}
	flags = append(flags, "--seed", strconv.Itoa(seed))
	flags = append(flags, extraArgsFlag(fb.SdcppExtraArgs)...)
	flags = append(flags, timeoutArgs(timeout)...)
	positionals := []string{out}
	if still != "" {
		positionals = append(positionals, still)
	}
	positionals = append(positionals, prompt)

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "video-gen (sdcpp)", failVerb: "video generation failed",
		fpFamily: videoFootprintFamily(renderFamily), fpQuant: quantFromModelFile(fb.SdcppModel), fpTask: "video-gen",
		script: script, args: runnerArgs(flags, positionals...), out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{"video_path": outPath, "seed": seed}
	addLicenseData(payload, config.FamilyInfo{License: fb.License, CommercialUse: fb.CommercialUse})
	notes.addTo(payload)
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// fileExistsAt reports whether path names an existing regular file or directory.
func fileExistsAt(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---------------------------------------------------------------- animate

// runAnimateCharacterSdcpp animates through render/sdcpp-animate.mjs (animategen_engine
// sdcpp): ffmpeg frames -> depth-anything.cpp -> sd.cpp Wan2.1 VACE. Same params as the
// ComfyUI route; pose_strength/ref_strength/motion_prompt are WAN-Animate-2 knobs that
// VACE does not read, so they are accepted and ignored. fast=true decodes with
// animategen_sdcpp_tae when it is bound (opt-in tiny autoencoder); otherwise fast is a no-op here
// and the result's notes say so. The VACE model must be a .safetensors checkpoint: the public GGUFs
// lack vace_patch_embedding.weight and sd-cli refuses them (the runner reports MODEL_INCOMPATIBLE).
func (p *Pipeline) runAnimateCharacterSdcpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	meta.Model = "sdcpp-animate:wan2.1-vace"
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty character/background prompt")
	}
	ref := paramStr(req.Params, "ref")
	if ref == "" {
		ref = req.Image
	}
	driver := paramStr(req.Params, "driver")
	if driver == "" {
		driver = req.Video
	}
	if ref == "" || driver == "" {
		return p.deferGen(req, meta, start, len(req.Input), "animate needs both a reference image and a driver video")
	}
	cfg := p.cfg
	if err := config.CPUBackendRefusal(cfg.AnimateGenSdcppBackend); err != nil {
		return p.deferCPUBackend(req, meta, start, "character animation", err)
	}
	if err := config.ExtraArgsRefusal("animategen_sdcpp_extra_args", config.ExtraArgsSdcpp, cfg.AnimateGenSdcppExtraArgs); err != nil {
		return p.deferRefused(req, meta, start, errClassExtraArgsRefused, "character animation", err)
	}
	if err := config.ExtraArgsRefusal("animategen_depth_extra_args", config.ExtraArgsDepth, cfg.AnimateGenDepthExtraArgs); err != nil {
		return p.deferRefused(req, meta, start, errClassExtraArgsRefused, "character animation", err)
	}
	for _, m := range []struct{ key, v string }{
		{"animategen_sdcpp_bin", cfg.AnimateGenSdcppBin}, {"animategen_sdcpp_model", cfg.AnimateGenSdcppModel},
		{"animategen_sdcpp_vae", cfg.AnimateGenSdcppVAE}, {"animategen_sdcpp_t5xxl", cfg.AnimateGenSdcppT5xxl},
		{"animategen_depth_bin", cfg.AnimateGenDepthBin}, {"animategen_depth_model", cfg.AnimateGenDepthModel},
	} {
		if strings.TrimSpace(m.v) == "" {
			return p.deferGen(req, meta, start, len(req.Input), "animategen_engine is sdcpp but "+m.key+" is not configured")
		}
	}
	script, serr := resolveIGPUScript(cfg.AnimateGenSdcppScript, defaultSdcppAnimateScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	pick := func(k string, def int) int {
		if v := paramIntOr(req.Params, k, 0); v > 0 {
			return v
		}
		return def
	}
	// A request that names no frames renders animategen_frames (0 = the runner's own default). The
	// cap check below and the runner's --frames flag both read this one value, so a box whose cap
	// fits only a short clip (the amd-gcn seed: 33 frames at 288x512) is not refused for the
	// longer default the runner would otherwise render.
	frames := normalizeVideoFrames(pick("frames", cfg.AnimateGenFrames))
	width := floorTo32(pick("width", cfg.AnimateGenWidth))
	height := floorTo32(pick("height", cfg.AnimateGenHeight))
	// The VACE reference image occupies one latent frame on top of the clip's (see the video
	// lane's note on the token cap).
	if cfg.AnimateGenSdcppMaxTokens > 0 {
		if err := config.TokenCapRefusal("animategen_sdcpp_max_tokens", orDefault(width, defaultIGPUWidth), orDefault(height, defaultIGPUHeight),
			orDefault(frames, defaultIGPUFrames), cfg.AnimateGenSdcppVAEStride, 1, cfg.AnimateGenSdcppMaxTokens); err != nil {
			return p.deferRefused(req, meta, start, errClassTokenCapExceeded, "character animation", err)
		}
	}
	sdBin, berr := resolveEngineBin("animategen_sdcpp_bin", cfg.AnimateGenSdcppBin)
	if berr != nil {
		return p.deferGen(req, meta, start, len(req.Input), "character animation refused: "+berr.Error())
	}
	depthBin, derr := resolveEngineBin("animategen_depth_bin", cfg.AnimateGenDepthBin)
	if derr != nil {
		return p.deferGen(req, meta, start, len(req.Input), "character animation refused: "+derr.Error())
	}
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		out = filepath.Join(cfg.MediaDir, "animate-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}
	if err := ensureOutDir(out); err != nil {
		return p.deferGen(req, meta, start, len(req.Input), "character animation refused: "+err.Error())
	}
	timeout := time.Duration(cfg.AnimateGenTimeoutSec) * time.Second

	var notes igpuNotes
	tae := ""
	if paramBool(req.Params, "fast") {
		switch {
		case strings.TrimSpace(cfg.AnimateGenSdcppTAE) == "":
			notes = append(notes, "fast=true is a no-op on the sdcpp animate lane: animategen_sdcpp_tae is not bound, so the full VAE decoded")
		case !fileExistsAt(cfg.AnimateGenSdcppTAE):
			notes = append(notes, fmt.Sprintf("fast=true could not use the tiny autoencoder: animategen_sdcpp_tae=%s does not exist, so the full VAE decoded", cfg.AnimateGenSdcppTAE))
		default:
			tae = cfg.AnimateGenSdcppTAE
			notes = append(notes, "fast=true decoded with the tiny autoencoder "+filepath.Base(tae)+" (approximate against the full VAE)")
		}
	}

	flags := []string{"--sd-bin", sdBin, "--model", cfg.AnimateGenSdcppModel, "--vae", cfg.AnimateGenSdcppVAE,
		"--t5xxl", cfg.AnimateGenSdcppT5xxl, "--backend", cfg.AnimateGenSdcppBackend,
		"--depth-bin", depthBin, "--depth-model", cfg.AnimateGenDepthModel}
	if tae != "" {
		flags = append(flags, "--tae", tae)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		flags = append(flags, "--negative", n)
	}
	if frames > 0 {
		flags = append(flags, "--frames", strconv.Itoa(frames))
	}
	if width > 0 {
		flags = append(flags, "--width", strconv.Itoa(width))
	}
	if height > 0 {
		flags = append(flags, "--height", strconv.Itoa(height))
	}
	if cfg.AnimateGenSdcppMaxTokens > 0 {
		flags = append(flags, "--max-tokens", strconv.Itoa(cfg.AnimateGenSdcppMaxTokens), "--vae-stride", strconv.Itoa(cfg.AnimateGenSdcppVAEStride))
	}
	if v := pick("steps", cfg.AnimateGenSteps); v > 0 {
		flags = append(flags, "--steps", strconv.Itoa(v))
	}
	if cfg.AnimateGenCFG > 0 {
		flags = append(flags, "--cfg", fmtFloat(cfg.AnimateGenCFG))
	}
	if cfg.AnimateGenFlowShift > 0 {
		flags = append(flags, "--flow-shift", fmtFloat(cfg.AnimateGenFlowShift))
	}
	flags = append(flags, "--seed", strconv.Itoa(seed))
	flags = append(flags, extraArgsFlag(cfg.AnimateGenSdcppExtraArgs)...)
	flags = append(flags, timeoutArgs(timeout)...)
	if len(cfg.AnimateGenDepthExtraArgs) > 0 {
		b, _ := json.Marshal(cfg.AnimateGenDepthExtraArgs)
		flags = append(flags, "--depth-extra-args", string(b))
	}

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "animate (sdcpp)", failVerb: "character animation failed",
		fpFamily: config.AnimateSdcppFootprintFamily, fpQuant: quantFromModelFile(cfg.AnimateGenSdcppModel), fpTask: "animate",
		script: script, args: runnerArgs(flags, out, ref, driver, prompt), out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{"video_path": outPath, "seed": seed}
	notes.addTo(payload)
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// ---------------------------------------------------------------- audio

// audiocppFootprintFamily is the audio.cpp --family a kind runs, which is also the footprint-store
// family it is recorded under and the family the fleet advertises for audio-gen (fleetnode.audioFamilies).
func audiocppFootprintFamily(cfg config.Config, kind string) string {
	if kind == "music" {
		return cfg.AudiocppMusicFamilyName()
	}
	return cfg.AudiocppVoiceFamilyName()
}

// audiocppServes reports whether this generate_audio request is served by audio.cpp: kind
// music with musicgen_engine audiocpp, or kind voice with voicegen_engine audiocpp on the
// default (generalist) voice. voice=finetuned and voice=endpoint keep their own lanes.
func audiocppServes(cfg config.Config, kind, voice string) bool {
	switch kind {
	case "music":
		return cfg.MusicGenEngine == config.EngineAudiocpp
	case "voice":
		return cfg.VoiceGenEngine == config.EngineAudiocpp && (voice == "" || voice == "generalist")
	}
	return false
}

// runGenerateAudioAudiocpp renders voice or music through render/audiocpp-generate.mjs.
// params: clone/lang (voice), seconds/lyrics (music), out, seed. Output is a .wav: voice is the
// engine's file as it wrote it, music is trimmed of its trailing silence, faded and
// loudness-normalized to -14 LUFS by the runner; both pass the dead-air gate (DEAD_AIR). The
// voice clone reference falls back to voicegen_ref.
func (p *Pipeline) runGenerateAudioAudiocpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time, kind string) core.Result {
	cfg := p.cfg
	family, model := audiocppFootprintFamily(cfg, kind), cfg.AudiocppVoiceModel
	if kind == "music" {
		model = cfg.AudiocppMusicModel
	}
	meta.Model = "audiocpp:" + family
	text := strings.TrimSpace(req.Input)
	if text == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty audio prompt")
	}
	// audio.cpp's backend value (vulkan only: the runner's GPU-evidence guard reads Vulkan buffers) and
	// a separate device index: "vulkan0" is sd.cpp's spelling and the CLI would reject it at run time
	if err := config.AudiocppBackendRefusal(cfg.AudiocppBackend); err != nil {
		return p.deferCPUBackend(req, meta, start, "audio generation", err)
	}
	if err := config.AudiocppDeviceRefusal(cfg.AudiocppDevice); err != nil {
		return p.deferRefused(req, meta, start, errClassDeviceInvalid, "audio generation", err)
	}
	if err := config.ExtraArgsRefusal("audiocpp_extra_args", config.ExtraArgsAudiocpp, cfg.AudiocppExtraArgs); err != nil {
		return p.deferRefused(req, meta, start, errClassExtraArgsRefused, "audio generation", err)
	}
	engineKey := map[string]string{"voice": "voicegen_engine", "music": "musicgen_engine"}[kind]
	if strings.TrimSpace(cfg.AudiocppBin) == "" {
		return p.deferGen(req, meta, start, len(req.Input), engineKey+" is audiocpp but audiocpp_bin is not configured")
	}
	if strings.TrimSpace(model) == "" {
		return p.deferGen(req, meta, start, len(req.Input), fmt.Sprintf("%s is audiocpp but audiocpp_%s_model is not configured", engineKey, kind))
	}
	script, serr := resolveIGPUScript(cfg.AudiocppScript, defaultAudiocppScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	bin, berr := resolveEngineBin("audiocpp_bin", cfg.AudiocppBin)
	if berr != nil {
		return p.deferGen(req, meta, start, len(req.Input), "audio generation refused: "+berr.Error())
	}
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		out = filepath.Join(cfg.MediaDir, kind+"-"+sha256hex(text + tasks.StableParamsKey(req.Params))[:8]+".wav")
	}
	if err := ensureOutDir(out); err != nil {
		return p.deferGen(req, meta, start, len(req.Input), "audio generation refused: "+err.Error())
	}
	timeout := time.Duration(cfg.AudioGenTimeoutSec) * time.Second

	flags := []string{"--kind", kind,
		"--bin", bin, "--family", family, "--model", model, "--backend", cfg.AudiocppBackend}
	if d := strings.TrimSpace(cfg.AudiocppDevice); d != "" {
		flags = append(flags, "--device", d)
	}
	if kind == "voice" {
		ref := paramStr(req.Params, "clone")
		if ref == "" {
			ref = cfg.VoiceGenRef
		}
		if ref != "" {
			flags = append(flags, "--clone", ref)
		}
		if lang := paramStr(req.Params, "lang"); lang != "" {
			flags = append(flags, "--lang", lang)
		}
	} else {
		if s := paramIntOr(req.Params, "seconds", 0); s > 0 {
			flags = append(flags, "--seconds", strconv.Itoa(s))
		}
		if l := paramStr(req.Params, "lyrics"); l != "" {
			flags = append(flags, "--lyrics", l)
		}
	}
	flags = append(flags, "--seed", strconv.Itoa(seed))
	flags = append(flags, extraArgsFlag(cfg.AudiocppExtraArgs)...)
	flags = append(flags, timeoutArgs(timeout)...)

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "audio-gen (" + kind + ", audiocpp)", failVerb: "audio generation failed",
		fpFamily: family, fpQuant: quantFromModelFile(model), fpTask: "audio-gen",
		script: script, args: runnerArgs(flags, out, text), out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"audio_path": outPath, "kind": kind, "seed": seed})
	p.record(req.Task, meta, len(text))
	return core.Result{OK: true, Data: data, Meta: meta}
}
