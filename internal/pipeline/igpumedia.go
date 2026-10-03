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
	meta.ErrClass = errClassCPUBackendRefused
	return p.deferGen(req, meta, start, len(req.Input), what+" refused: "+err.Error())
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

// timeoutArgs arms the runner's own deadline a margin BEFORE gpugen's. gpugen kills only the
// node process on a non-Windows host, so a runner killed from outside leaves its engine
// running on the iGPU; a runner that times itself out kills the engine's whole tree first.
// The margin is 15 s (a quarter of the budget when that is under a minute); no timeout, no flag.
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

// runIGPU takes the media lease, runs the script under gpugen (process-tree-killed on
// timeout, no ComfyUI /free) and returns the produced file, or the defer result.
func (p *Pipeline) runIGPU(ctx context.Context, req core.Request, meta *core.Meta, start time.Time, r igpuRun) (string, *core.Result) {
	leaseEnv, release, lerr := p.acquireMediaLease(ctx, r.leaseReason, r.timeout, p.gpuWait())
	if lerr != nil {
		res := p.deferForLease(lerr, req.Task, *meta, len(req.Input), start)
		return "", &res
	}
	defer release()
	spec := gpugen.Spec{
		Exe:           p.cfg.NodePath,
		Script:        r.script,
		Args:          r.args,
		Env:           append(p.igpuEnv(), leaseEnv...),
		Out:           r.out,
		Timeout:       r.timeout,
		SkipFreeComfy: true,
	}
	p.footprintSampling(r.fpFamily, r.fpQuant, r.fpTask).ApplyTo(&spec)
	outPath, gerr := gpugen.Generate(ctx, spec)
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

// runGenerateVideoSdcpp renders generate_video through render/sdcpp-video.mjs.
// params as the ComfyUI route: still/out/negative/seed/steps/frames/width/height; a
// per-request value wins over the family binding's default.
func (p *Pipeline) runGenerateVideoSdcpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time, renderFamily string, fb config.VideoFamilyBinding) core.Result {
	if renderFamily == "" {
		renderFamily = "sdcpp"
	}
	meta.Model = "sdcpp-video:" + renderFamily
	meta.License = fb.License
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty video prompt")
	}
	if err := config.CPUBackendRefusal(fb.SdcppBackend); err != nil {
		return p.deferCPUBackend(req, meta, start, "video generation", err)
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
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "video-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}
	timeout := time.Duration(p.cfg.VideoGenTimeoutSec) * time.Second

	args := []string{out}
	if still != "" {
		args = append(args, still)
	}
	args = append(args, prompt,
		"--sd-bin", fb.SdcppBin, "--model", fb.SdcppModel, "--vae", fb.SdcppVAE, "--t5xxl", fb.SdcppT5xxl,
		"--backend", fb.SdcppBackend)
	if fb.SdcppHighNoiseModel != "" {
		args = append(args, "--high-noise-model", fb.SdcppHighNoiseModel)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		args = append(args, "--negative", n)
	}
	pick := func(k string, def int) int {
		if v := paramIntOr(req.Params, k, 0); v > 0 {
			return v
		}
		return def
	}
	if v := normalizeVideoFrames(pick("frames", fb.Frames)); v > 0 {
		args = append(args, "--frames", strconv.Itoa(v))
	}
	if v := floorTo32(pick("width", fb.Width)); v > 0 {
		args = append(args, "--width", strconv.Itoa(v))
	}
	if v := floorTo32(pick("height", fb.Height)); v > 0 {
		args = append(args, "--height", strconv.Itoa(v))
	}
	if fb.FPS > 0 {
		args = append(args, "--fps", strconv.Itoa(fb.FPS))
	}
	if v := pick("steps", fb.Steps); v > 0 {
		args = append(args, "--steps", strconv.Itoa(v))
	}
	if fb.CFG > 0 {
		args = append(args, "--cfg", fmtFloat(fb.CFG))
	}
	if fb.FlowShift > 0 {
		args = append(args, "--flow-shift", fmtFloat(fb.FlowShift))
	}
	if fb.Sampler != "" {
		args = append(args, "--sampler", fb.Sampler)
	}
	args = append(args, "--seed", strconv.Itoa(seed))
	args = append(args, extraArgsFlag(fb.SdcppExtraArgs)...)
	args = append(args, timeoutArgs(timeout)...)

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "video-gen (sdcpp)", failVerb: "video generation failed",
		fpFamily: videoFootprintFamily(renderFamily), fpQuant: quantFromModelFile(fb.SdcppModel), fpTask: "video-gen",
		script: script, args: args, out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{"video_path": outPath, "seed": seed}
	addLicenseData(payload, config.FamilyInfo{License: fb.License, CommercialUse: fb.CommercialUse})
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// ---------------------------------------------------------------- animate

// runAnimateCharacterSdcpp animates through render/sdcpp-animate.mjs (animategen_engine
// sdcpp): ffmpeg frames -> depth-anything.cpp -> sd.cpp Wan2.1 VACE. Same params as the
// ComfyUI route; pose_strength/ref_strength/motion_prompt are WAN-Animate-2 knobs that
// VACE does not read, so they are accepted and ignored.
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
		_ = os.MkdirAll(cfg.MediaDir, 0o755)
		out = filepath.Join(cfg.MediaDir, "animate-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}
	timeout := time.Duration(cfg.AnimateGenTimeoutSec) * time.Second

	args := []string{out, ref, driver, prompt,
		"--sd-bin", cfg.AnimateGenSdcppBin, "--model", cfg.AnimateGenSdcppModel, "--vae", cfg.AnimateGenSdcppVAE,
		"--t5xxl", cfg.AnimateGenSdcppT5xxl, "--backend", cfg.AnimateGenSdcppBackend,
		"--depth-bin", cfg.AnimateGenDepthBin, "--depth-model", cfg.AnimateGenDepthModel}
	if n := paramStr(req.Params, "negative"); n != "" {
		args = append(args, "--negative", n)
	}
	pick := func(k string, def int) int {
		if v := paramIntOr(req.Params, k, 0); v > 0 {
			return v
		}
		return def
	}
	if v := normalizeVideoFrames(pick("frames", 0)); v > 0 {
		args = append(args, "--frames", strconv.Itoa(v))
	}
	if v := floorTo32(pick("width", cfg.AnimateGenWidth)); v > 0 {
		args = append(args, "--width", strconv.Itoa(v))
	}
	if v := floorTo32(pick("height", cfg.AnimateGenHeight)); v > 0 {
		args = append(args, "--height", strconv.Itoa(v))
	}
	if v := pick("steps", cfg.AnimateGenSteps); v > 0 {
		args = append(args, "--steps", strconv.Itoa(v))
	}
	if cfg.AnimateGenCFG > 0 {
		args = append(args, "--cfg", fmtFloat(cfg.AnimateGenCFG))
	}
	if cfg.AnimateGenFlowShift > 0 {
		args = append(args, "--flow-shift", fmtFloat(cfg.AnimateGenFlowShift))
	}
	args = append(args, "--seed", strconv.Itoa(seed))
	args = append(args, extraArgsFlag(cfg.AnimateGenSdcppExtraArgs)...)
	args = append(args, timeoutArgs(timeout)...)
	if len(cfg.AnimateGenDepthExtraArgs) > 0 {
		b, _ := json.Marshal(cfg.AnimateGenDepthExtraArgs)
		args = append(args, "--depth-extra-args", string(b))
	}

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "animate (sdcpp)", failVerb: "character animation failed",
		fpFamily: "wan-vace", fpQuant: quantFromModelFile(cfg.AnimateGenSdcppModel), fpTask: "animate",
		script: script, args: args, out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"video_path": outPath, "seed": seed})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// ---------------------------------------------------------------- audio

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
// params: clone/lang (voice), seconds/lyrics (music), out, seed. Output is a .wav (music is
// loudness-normalized by the runner when ffmpeg is present).
func (p *Pipeline) runGenerateAudioAudiocpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time, kind string) core.Result {
	cfg := p.cfg
	family, model := cfg.AudiocppVoiceFamilyName(), cfg.AudiocppVoiceModel
	if kind == "music" {
		family, model = cfg.AudiocppMusicFamilyName(), cfg.AudiocppMusicModel
	}
	meta.Model = "audiocpp:" + family
	text := strings.TrimSpace(req.Input)
	if text == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty audio prompt")
	}
	if err := config.CPUBackendRefusal(cfg.AudiocppBackend); err != nil {
		return p.deferCPUBackend(req, meta, start, "audio generation", err)
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
		_ = os.MkdirAll(cfg.MediaDir, 0o755)
		out = filepath.Join(cfg.MediaDir, kind+"-"+sha256hex(text + tasks.StableParamsKey(req.Params))[:8]+".wav")
	}
	timeout := time.Duration(cfg.AudioGenTimeoutSec) * time.Second

	args := []string{out, text, "--kind", kind,
		"--bin", cfg.AudiocppBin, "--family", family, "--model", model, "--backend", cfg.AudiocppBackend}
	if d := strings.TrimSpace(cfg.AudiocppDevice); d != "" {
		args = append(args, "--device", d)
	}
	if kind == "voice" {
		ref := paramStr(req.Params, "clone")
		if ref == "" {
			ref = cfg.VoiceGenRef
		}
		if ref != "" {
			args = append(args, "--clone", ref)
		}
		if lang := paramStr(req.Params, "lang"); lang != "" {
			args = append(args, "--lang", lang)
		}
	} else {
		if s := paramIntOr(req.Params, "seconds", 0); s > 0 {
			args = append(args, "--seconds", strconv.Itoa(s))
		}
		if l := paramStr(req.Params, "lyrics"); l != "" {
			args = append(args, "--lyrics", l)
		}
	}
	args = append(args, "--seed", strconv.Itoa(seed))
	args = append(args, extraArgsFlag(cfg.AudiocppExtraArgs)...)
	args = append(args, timeoutArgs(timeout)...)

	outPath, dres := p.runIGPU(ctx, req, &meta, start, igpuRun{
		leaseReason: "audio-gen (" + kind + ", audiocpp)", failVerb: "audio generation failed",
		fpFamily: family, fpQuant: quantFromModelFile(model), fpTask: "audio-gen",
		script: script, args: args, out: out, timeout: timeout,
	})
	if dres != nil {
		return *dres
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"audio_path": outPath, "kind": kind, "seed": seed})
	p.record(req.Task, meta, len(text))
	return core.Result{OK: true, Data: data, Meta: meta}
}
