package config

import (
	"fmt"
	"regexp"
	"strings"
)

// iGPU media engines (CT-49): the video, animate, voice and music lanes on a box whose
// only GPU is a Vulkan iGPU. All four are spawn-per-job native CLIs (stable-diffusion.cpp
// `sd-cli -M vid_gen`, depth-anything.cpp, audio.cpp `audiocpp_cli`), zero-warm under the
// media lease. The operator rule behind this file: NO model runs on CPU on these engines.
// A CPU backend is refused at three layers: here (config load, so `doctor` fails), in
// mediacap (the route verdict is BOUND-BUT-MISSING), and in the pipeline (a typed defer
// for an in-process config that never went through Load). The render scripts add a
// fourth, reading their engine's own log for POSITIVE evidence of the GPU (CPU_PLACEMENT
// when it is missing or a model sits on the CPU). Extra args that change the backend or the
// placement are refused at the same doors (ExtraArgsRefusal), and the video/animate token cap
// (TokenCapRefusal) keeps one GPU dispatch inside the amdgpu 2 s lockup timeout.

// Engine names. "" and "comfy" are today's ComfyUI/python paths on every route.
const (
	EngineComfy    = "comfy"
	EngineSdcpp    = "sdcpp"
	EngineAudiocpp = "audiocpp"
)

// Defaults an audio.cpp family key falls back to when it is unset (the families the
// reference node's models were verified for).
const (
	DefaultAudiocppVoiceFamily = "chatterbox"
	DefaultAudiocppMusicFamily = "ace_step"
)

// vulkanBackendRe is the only backend value these engines accept: "vulkan" or "vulkanN".
// An allowlist, so "cpu", "best", "auto", "blas", "opencl", "rpc" and a typo such as
// "vulcan" are all refused instead of being left to the binary to resolve.
var vulkanBackendRe = regexp.MustCompile(`^vulkan\d*$`)

// CPUBackendRefusal returns an error when backend is not purely Vulkan devices, and nil
// for a backend that names only Vulkan devices. The empty string is refused too: an engine
// with no backend would let the binary pick its own, and the binary's own choice is CPU on a
// box whose GPU it cannot open.
//
// sd-cli's --backend takes either one value ("vulkan0") or per-module assignments
// ("diffusion=vulkan0,vae=cpu", with "&" joining devices of one module), so every
// assignment is checked, not just the bare word.
func CPUBackendRefusal(backend string) error {
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" {
		return fmt.Errorf("backend is unset (a GPU backend such as \"vulkan0\" is required; no model runs on CPU on this engine)")
	}
	for _, part := range strings.FieldsFunc(b, func(r rune) bool { return r == ',' || r == '&' }) {
		v := strings.TrimSpace(part)
		if i := strings.LastIndex(v, "="); i >= 0 {
			v = strings.TrimSpace(v[i+1:])
		}
		if !vulkanBackendRe.MatchString(v) {
			return fmt.Errorf("backend %q is not a Vulkan device (%q; refused: no model runs on CPU on this engine, so only \"vulkan\" or \"vulkanN\" such as \"vulkan0\" is accepted)", backend, v)
		}
	}
	return nil
}

// audiocppBackends is the allowlist of audio.cpp --backend values: the GPU backends of
// `audiocpp_cli --backend cpu|cuda|hip|rocm|vulkan|metal|best` (rocm is an alias of hip).
// audio.cpp takes the device index SEPARATELY (--device N, the audiocpp_device key), so
// "vulkan0" is not a valid audio.cpp backend even though sd.cpp's --backend wants exactly that.
// cpu and best (which may pick the CPU) are not on the list: no model runs on CPU.
var audiocppBackends = map[string]bool{"vulkan": true, "cuda": true, "hip": true, "rocm": true, "metal": true}

// AudiocppBackendRefusal returns an error unless backend is one of audio.cpp's GPU backends
// (vulkan, cuda, hip, rocm, metal). It is the audiocpp_backend twin of CPUBackendRefusal,
// which validates sd.cpp's vulkanN spelling.
func AudiocppBackendRefusal(backend string) error {
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" {
		return fmt.Errorf("backend is unset (an audio.cpp GPU backend such as \"vulkan\" is required; no model runs on CPU on this engine)")
	}
	if !audiocppBackends[b] {
		hint := ""
		if vulkanBackendRe.MatchString(b) {
			hint = fmt.Sprintf(" - audio.cpp takes the device index separately: set audiocpp_backend \"vulkan\" and audiocpp_device %q", strings.TrimPrefix(b, "vulkan"))
		}
		return fmt.Errorf("backend %q is not an audio.cpp GPU backend (want vulkan, cuda, hip, rocm or metal; cpu and best are refused: no model runs on CPU on this engine)%s", backend, hint)
	}
	return nil
}

var deviceIndexRe = regexp.MustCompile(`^\d+$`)

// AudiocppDeviceRefusal validates audiocpp_device: empty (= device 0) or a non-negative index.
func AudiocppDeviceRefusal(device string) error {
	d := strings.TrimSpace(device)
	if d == "" || deviceIndexRe.MatchString(d) {
		return nil
	}
	return fmt.Errorf("audiocpp_device %q is not a device index (a non-negative integer such as \"0\")", device)
}

// Engines whose extra args are screened (ExtraArgsRefusal): they differ only in which flags
// the runner owns (audio.cpp's --device is the audiocpp_device key's).
const (
	ExtraArgsSdcpp    = "sdcpp"
	ExtraArgsDepth    = "da3"
	ExtraArgsAudiocpp = "audiocpp"
)

// placementFlags change the backend or where a model lives. The runner supplies the backend
// itself, so an extra-args element naming any of these is never legitimate: whatever follows
// it overrides the GPU backend the runner put in the argv. Mirrors render/igpu-engine.mjs.
var placementFlags = map[string]bool{
	"--backend": true, "-b": true, "--params-backend": true, "--offload-to-cpu": true,
	"--clip-on-cpu": true, "--vae-on-cpu": true, "--control-net-cpu": true, "--rpc": true,
}

var extraArgSplitRe = regexp.MustCompile(`[=,&:\s]+`)
var cpuTokenRe = regexp.MustCompile(`^cpu\d*$`)

// ScreenExtraArgs returns the first *_extra_args element that changes the backend or the
// placement: its index, the element, why, and true; false when the list is clean. engine is
// one of the ExtraArgs* constants.
func ScreenExtraArgs(engine string, args []string) (index int, arg, why string, found bool) {
	for i, a := range args {
		low := strings.ToLower(strings.TrimSpace(a))
		name := strings.TrimSpace(strings.SplitN(low, "=", 2)[0])
		switch {
		case placementFlags[name]:
			return i, a, name + " sets the backend or where a model lives", true
		case engine == ExtraArgsAudiocpp && name == "--device":
			return i, a, "--device is chosen by the audiocpp_device key", true
		case strings.HasPrefix(name, "-") && strings.Contains(name, "cpu"):
			return i, a, "a cpu-named flag places a model on the CPU", true
		}
		for _, p := range extraArgSplitRe.Split(low, -1) {
			if cpuTokenRe.MatchString(p) {
				return i, a, "cpu as a backend value", true
			}
		}
	}
	return 0, "", "", false
}

// ExtraArgsRefusal is the typed refusal for an *_extra_args list (key names it in the
// error): nil for a clean list. The message starts EXTRA_ARGS_REFUSED, the class the runner
// and the pipeline both report.
func ExtraArgsRefusal(key, engine string, args []string) error {
	i, a, why, found := ScreenExtraArgs(engine, args)
	if !found {
		return nil
	}
	return fmt.Errorf("EXTRA_ARGS_REFUSED: %s[%d] %q: %s (no model runs on CPU on this engine; backends and devices are set by the *_backend / *_device keys only)", key, i, a, why)
}

// LatentTokens is the number of latent tokens one attention pass of a Wan-family DiT sees:
// the VAE downsamples stride times per side, the DiT patch is 2x2, time is /4 with the first
// frame kept, and a VACE reference image adds refLatentFrames latent frames:
//
//	ceil(W/(stride*2)) * ceil(H/(stride*2)) * (floor((frames-1)/4) + 1 + refLatentFrames)
//
// render/igpu-engine.mjs latentTokens is the Node twin; render/testdata/token-cap-table.json
// pins both.
func LatentTokens(width, height, frames, stride, refLatentFrames int) int {
	cell := stride * 2
	if cell <= 0 {
		return 0
	}
	if frames < 1 {
		frames = 1
	}
	return ((width + cell - 1) / cell) * ((height + cell - 1) / cell) * ((frames-1)/4 + 1 + refLatentFrames)
}

// tokenFitAdvice says how a request over the cap can be made to fit (frames at this size
// first). Same wording as the runner's.
func tokenFitAdvice(width, height, stride, refLatentFrames, capTokens int) string {
	cell := stride * 2
	perLatentFrame := ((width + cell - 1) / cell) * ((height + cell - 1) / cell)
	latentFrames := capTokens/perLatentFrame - refLatentFrames
	if latentFrames >= 2 {
		return fmt.Sprintf("at %dx%d up to %d frames fit; or lower width/height", width, height, (latentFrames-1)*4+1)
	}
	return fmt.Sprintf("even 5 frames do not fit at %dx%d: lower width and height", width, height)
}

// TokenCapRefusal is the typed, non-retryable refusal for a request over the configured
// latent-token cap, or nil: no cap configured (capTokens <= 0) is no check. key names the
// config key (sdcpp_max_tokens / animategen_sdcpp_max_tokens) in the message. The message
// starts TOKEN_CAP_EXCEEDED, the class the runner reports as well.
func TokenCapRefusal(key string, width, height, frames, stride, refLatentFrames, capTokens int) error {
	if capTokens <= 0 {
		return nil
	}
	tokens := LatentTokens(width, height, frames, stride, refLatentFrames)
	if tokens <= capTokens {
		return nil
	}
	ref := ""
	if refLatentFrames > 0 {
		ref = " + reference"
	}
	return fmt.Errorf("TOKEN_CAP_EXCEEDED: %dx%dx%d%s needs %d latent tokens (VAE stride %d, patch 2) but the cap is %d (%s); one GPU dispatch that long would hit the amdgpu 2 s lockup timeout and reset the GPU. To fit: %s. Not retried.",
		width, height, frames, ref, tokens, stride, capTokens, key, tokenFitAdvice(width, height, stride, refLatentFrames, capTokens))
}

// validateTokenCap checks one cap/stride pair: both non-negative, the stride 8 or 16 (the
// Wan2.1 and Wan2.2 VAEs), and a stride REQUIRED when a cap is set.
func validateTokenCap(where, capKey, strideKey string, capTokens, stride int) error {
	if capTokens < 0 || stride < 0 {
		return fmt.Errorf("%s: %s and %s must not be negative", where, capKey, strideKey)
	}
	if stride != 0 && stride != 8 && stride != 16 {
		return fmt.Errorf("%s: %s is %d, want 8 or 16 (the VAE's spatial downsampling)", where, strideKey, stride)
	}
	if capTokens > 0 && stride == 0 {
		return fmt.Errorf("%s: %s is set but %s is not: the token count needs the VAE stride (8 or 16)", where, capKey, strideKey)
	}
	return nil
}

// validateVideoFamilyEngine checks one videogen_families entry's engine contract.
func validateVideoFamilyEngine(name string, b VideoFamilyBinding) error {
	switch b.Engine {
	case "", EngineComfy:
		if b.SdcppBin != "" || b.SdcppModel != "" || b.SdcppHighNoiseModel != "" || b.SdcppVAE != "" ||
			b.SdcppT5xxl != "" || b.SdcppBackend != "" || len(b.SdcppExtraArgs) > 0 || b.SdcppMaxTokens != 0 || b.SdcppVAEStride != 0 ||
			b.SdcppTAE != "" || b.HighNoiseCFG != 0 || b.HighNoiseSteps != 0 || b.HighNoiseSampler != "" {
			return fmt.Errorf("videogen_families[%q]: sdcpp_* keys are set but engine is %q — set \"engine\": \"sdcpp\" or remove them", name, b.Engine)
		}
		return nil
	case EngineSdcpp:
	default:
		return fmt.Errorf("videogen_families[%q].engine: %q is not \"\", \"comfy\" or \"sdcpp\"", name, b.Engine)
	}
	if !familyNameRe.MatchString(name) {
		return fmt.Errorf("videogen_families[%q]: a family name is lower-case letters, digits, '.', '_' or '-' (at most 64)", name)
	}
	if err := CPUBackendRefusal(b.SdcppBackend); err != nil {
		return fmt.Errorf("videogen_families[%q].sdcpp_backend: %w", name, err)
	}
	if b.Steps < 0 || b.CFG < 0 || b.FlowShift < 0 {
		return fmt.Errorf("videogen_families[%q]: steps, cfg and flow_shift must not be negative", name)
	}
	if b.HighNoiseCFG < 0 || b.HighNoiseSteps < 0 {
		return fmt.Errorf("videogen_families[%q]: high_noise_cfg and high_noise_steps must not be negative", name)
	}
	if (b.HighNoiseCFG != 0 || b.HighNoiseSteps != 0 || b.HighNoiseSampler != "") && b.SdcppHighNoiseModel == "" {
		return fmt.Errorf("videogen_families[%q]: high_noise_cfg / high_noise_steps / high_noise_sampler tune the high-noise expert, but sdcpp_high_noise_model is not set", name)
	}
	if err := ExtraArgsRefusal(fmt.Sprintf("videogen_families[%q].sdcpp_extra_args", name), ExtraArgsSdcpp, b.SdcppExtraArgs); err != nil {
		return err
	}
	return validateTokenCap(fmt.Sprintf("videogen_families[%q]", name), "sdcpp_max_tokens", "sdcpp_vae_stride", b.SdcppMaxTokens, b.SdcppVAEStride)
}

// validateIGPUMedia is the load-time door for the animate and audio engine keys.
func validateIGPUMedia(c Config) error {
	switch c.AnimateGenEngine {
	case "":
	case EngineSdcpp:
		if err := CPUBackendRefusal(c.AnimateGenSdcppBackend); err != nil {
			return fmt.Errorf("animategen_sdcpp_backend: %w", err)
		}
	default:
		return fmt.Errorf("animategen_engine: %q is not \"\" or \"sdcpp\"", c.AnimateGenEngine)
	}
	if c.AnimateGenSteps < 0 || c.AnimateGenCFG < 0 || c.AnimateGenFlowShift < 0 {
		return fmt.Errorf("animategen_steps, animategen_cfg and animategen_flow_shift must not be negative")
	}
	if err := ExtraArgsRefusal("animategen_sdcpp_extra_args", ExtraArgsSdcpp, c.AnimateGenSdcppExtraArgs); err != nil {
		return err
	}
	if err := ExtraArgsRefusal("animategen_depth_extra_args", ExtraArgsDepth, c.AnimateGenDepthExtraArgs); err != nil {
		return err
	}
	if err := validateTokenCap("animate", "animategen_sdcpp_max_tokens", "animategen_sdcpp_vae_stride", c.AnimateGenSdcppMaxTokens, c.AnimateGenSdcppVAEStride); err != nil {
		return err
	}
	if err := ExtraArgsRefusal("audiocpp_extra_args", ExtraArgsAudiocpp, c.AudiocppExtraArgs); err != nil {
		return err
	}
	for _, e := range []struct{ key, v string }{{"voicegen_engine", c.VoiceGenEngine}, {"musicgen_engine", c.MusicGenEngine}} {
		switch e.v {
		case "", EngineAudiocpp:
		default:
			return fmt.Errorf("%s: %q is not \"\" or \"audiocpp\"", e.key, e.v)
		}
	}
	if c.VoiceGenEngine == EngineAudiocpp || c.MusicGenEngine == EngineAudiocpp {
		if err := AudiocppBackendRefusal(c.AudiocppBackend); err != nil {
			return fmt.Errorf("audiocpp_backend: %w", err)
		}
	}
	if err := AudiocppDeviceRefusal(c.AudiocppDevice); err != nil {
		return err
	}
	return nil
}

// SdcppVideoFamily reports whether name is a videogen_families entry bound to the
// sdcpp engine. It is what lets resolveVideoFamily (internal/pipeline) accept a
// family name outside the ComfyUI runner's closed dispatch set: an sdcpp family is
// matched by its own name, exactly.
func (c Config) SdcppVideoFamily(name string) bool {
	b, ok := c.VideoGenFamilies[strings.TrimSpace(name)]
	return ok && b.UsesSdcpp()
}

// AudiocppVoiceFamilyName / AudiocppMusicFamilyName are the audio.cpp --family values,
// with the verified defaults applied.
func (c Config) AudiocppVoiceFamilyName() string {
	if f := strings.TrimSpace(c.AudiocppVoiceFamily); f != "" {
		return f
	}
	return DefaultAudiocppVoiceFamily
}

func (c Config) AudiocppMusicFamilyName() string {
	if f := strings.TrimSpace(c.AudiocppMusicFamily); f != "" {
		return f
	}
	return DefaultAudiocppMusicFamily
}

// expandVideoFamilyPaths tilde-expands the sdcpp paths inside every videogen_families
// entry. They live in a variable-size map, so (like pipelines entries) they cannot join
// the flat pathFields list.
func expandVideoFamilyPaths(c *Config, home string) {
	for k, b := range c.VideoGenFamilies {
		b.SdcppBin = ExpandTilde(b.SdcppBin, home)
		b.SdcppModel = ExpandTilde(b.SdcppModel, home)
		b.SdcppHighNoiseModel = ExpandTilde(b.SdcppHighNoiseModel, home)
		b.SdcppVAE = ExpandTilde(b.SdcppVAE, home)
		b.SdcppT5xxl = ExpandTilde(b.SdcppT5xxl, home)
		b.SdcppTAE = ExpandTilde(b.SdcppTAE, home)
		c.VideoGenFamilies[k] = b
	}
}

// canonicalVideoFamily mirrors internal/pipeline.canonicalVideoFamily: the runner's closed
// dispatch set maps to itself and everything else renders Wan 2.2.
// TestDefaultVideoSdcppFamilyMirrorsThePipeline (internal/pipeline) keeps the two together.
func canonicalVideoFamily(fam string) string {
	switch fam {
	case "ltx25", "h3", "hunyuan", "ace":
		return fam
	}
	return videoFamilyWanSentinel
}

// DefaultVideoSdcppFamily reports the sdcpp family a generate_video request that names no
// model resolves to, exactly as the pipeline resolves it (resolveVideoFamily, then
// ResolveVideoFamilyBinding): an sdcpp family may be named like a ComfyUI family (wan22,
// ltx25, ...), and an unset videogen_family means wan22. mediacap's generate_video verdict
// and the fleet's bound helpers both ask this, so none of them re-derives the default.
func (c Config) DefaultVideoSdcppFamily() (string, bool) {
	fam := strings.TrimSpace(c.VideoGenFamily)
	render := ""
	if fam != "" {
		if c.SdcppVideoFamily(fam) {
			return fam, true
		}
		render = canonicalVideoFamily(fam)
	}
	if !c.ResolveVideoFamilyBinding(render).UsesSdcpp() {
		return "", false
	}
	if render == "" {
		render = c.defaultVideoFamily()
	}
	return render, true
}

// The four *Bound methods say whether a lane has ANY renderer bound on this box: its
// ComfyUI/python script (voice: or the TTS endpoint) OR the CT-49 engine that replaces it.
// They are the seam the fleet advertisement and fleet-measure key on (CT-51), so a box
// whose only video is the sdcpp engine is not told it has no video lane.

// VideoGenBound: videogen_script, or the default video family is bound to sdcpp.
func (c Config) VideoGenBound() bool {
	if c.VideoGenScript != "" {
		return true
	}
	_, ok := c.DefaultVideoSdcppFamily()
	return ok
}

// AnimateGenBound: animategen_script, or animategen_engine sdcpp.
func (c Config) AnimateGenBound() bool {
	return c.AnimateGenScript != "" || c.AnimateGenEngine == EngineSdcpp
}

// VoiceGenBound: voicegen_script, tts_endpoint, or voicegen_engine audiocpp.
func (c Config) VoiceGenBound() bool {
	return c.VoiceGenScript != "" || c.TTSEndpoint != "" || c.VoiceGenEngine == EngineAudiocpp
}

// MusicGenBound: musicgen_script, or musicgen_engine audiocpp.
func (c Config) MusicGenBound() bool {
	return c.MusicGenScript != "" || c.MusicGenEngine == EngineAudiocpp
}
