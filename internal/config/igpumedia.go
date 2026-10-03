package config

import (
	"fmt"
	"strings"
)

// iGPU media engines (CT-49): the video, animate, voice and music lanes on a box whose
// only GPU is a Vulkan iGPU. All four are spawn-per-job native CLIs (stable-diffusion.cpp
// `sd-cli -M vid_gen`, depth-anything.cpp, audio.cpp `audiocpp_cli`), zero-warm under the
// media lease. The operator rule behind this file: NO model runs on CPU on these engines.
// A CPU backend is refused at three layers: here (config load, so `doctor` fails), in
// mediacap (the route verdict is BOUND-BUT-MISSING), and in the pipeline (a typed defer
// for an in-process config that never went through Load). The render scripts add a
// fourth, reading their engine's own log for CPU placement (CPU_PLACEMENT).

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

// CPUBackendRefusal returns an error when backend names (or could place a module on)
// the CPU, and nil for a backend that names only GPU devices. The empty string is
// refused too: an engine with no backend would let the binary pick its own, and the
// binary's own choice is CPU on a box whose GPU it cannot open.
//
// sd-cli's --backend takes either one value ("vulkan0") or per-module assignments
// ("diffusion=vulkan0,vae=cpu", with "&" joining devices of one module), so every
// assignment is checked, not just the bare word. "cpu" followed by digits ("cpu0")
// is the same refusal, and so are "best" and "auto" (the binary's own choice).
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
		if isCPUBackendName(v) {
			return fmt.Errorf("backend %q places a model on the CPU (refused: no model runs on CPU on this engine; name a GPU device such as \"vulkan0\")", backend)
		}
	}
	return nil
}

func isCPUBackendName(v string) bool {
	// "best" and "auto" let the binary pick, and its pick on a box whose GPU it cannot open
	// is the CPU (audio.cpp does not log the resolution): refused like the CPU itself.
	if v == "best" || v == "auto" {
		return true
	}
	if !strings.HasPrefix(v, "cpu") {
		return false
	}
	for _, r := range v[len("cpu"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validateVideoFamilyEngine checks one videogen_families entry's engine contract.
func validateVideoFamilyEngine(name string, b VideoFamilyBinding) error {
	switch b.Engine {
	case "", EngineComfy:
		if b.SdcppBin != "" || b.SdcppModel != "" || b.SdcppHighNoiseModel != "" || b.SdcppVAE != "" ||
			b.SdcppT5xxl != "" || b.SdcppBackend != "" || len(b.SdcppExtraArgs) > 0 {
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
	return nil
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
	for _, e := range []struct{ key, v string }{{"voicegen_engine", c.VoiceGenEngine}, {"musicgen_engine", c.MusicGenEngine}} {
		switch e.v {
		case "", EngineAudiocpp:
		default:
			return fmt.Errorf("%s: %q is not \"\" or \"audiocpp\"", e.key, e.v)
		}
	}
	if c.VoiceGenEngine == EngineAudiocpp || c.MusicGenEngine == EngineAudiocpp {
		if err := CPUBackendRefusal(c.AudiocppBackend); err != nil {
			return fmt.Errorf("audiocpp_backend: %w", err)
		}
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
		c.VideoGenFamilies[k] = b
	}
}
