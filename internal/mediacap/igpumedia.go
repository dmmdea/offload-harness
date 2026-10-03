package mediacap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The iGPU media engines (CT-49): sdcpp video + animate, audio.cpp voice + music. Their
// routes are derived the same way every other route is (the three verdicts, the same
// bindings the pipeline routes on), so doctor, offload_status and the fleet's honest
// advertisement see the truth about a box whose only GPU is a Vulkan iGPU:
//
//   - a route is CONFIGURED only when its runner, its engine binaries and every model file
//     it binds exist,
//   - NOT CONFIGURED when the engine key is set and nothing it needs is bound,
//   - BOUND-BUT-MISSING when something is bound and absent, or the backend is a CPU one:
//     no model runs on CPU on these engines, so a cpu (or unset) backend is a route that
//     defers every call, which is the middle verdict's whole meaning.
//
// None of these routes touches ComfyUI, so they never mark comfyUsed (an sdcpp-only node is
// never told it is missing ComfyUI).

// Engine names as the Route.Engine field carries them.
const (
	engineSdcpp    = "sdcpp"
	engineAudiocpp = "audiocpp"
)

// VideoFamilyRoute names a non-default sdcpp video family's route.
func VideoFamilyRoute(name string) string { return "generate_video:" + name }

// anyPathBinding: a file OR a directory (audio.cpp's --model may be a model package dir).
const anyPathBinding bindingKind = 100

// ffprobeBinding is resolved the way the runners do (render/audio-qa.mjs resolveFfprobe):
// beside the configured ffmpeg first, else on PATH. value carries the ffmpeg_path.
const ffprobeBinding bindingKind = 101

// ffmpegBindings is what every iGPU job ends with: the mp4 encode and black/frozen-clip gate
// (video, animate) or the trim / loudness / dead-air gate (audio) need ffmpeg AND ffprobe
// (the VACE frame count, the dead-air measurement). An unset ffmpeg_path adds nothing: the
// `media` route already reports it as NOT CONFIGURED, and the runner then probes PATH on its own.
func ffmpegBindings(cfg config.Config) []binding {
	if strings.TrimSpace(cfg.FFmpegPath) == "" {
		return nil
	}
	return []binding{
		{key: "ffmpeg_path", value: cfg.FFmpegPath, kind: binaryBinding},
		{key: "ffprobe (beside ffmpeg_path, else PATH)", value: cfg.FFmpegPath, kind: ffprobeBinding},
	}
}

// optionalTAEDetail words the opt-in tiny autoencoder: bound and present, bound and missing
// (fast=true then decodes with the full VAE and says so; never a route failure), or unbound.
func optionalTAEDetail(key, path string) string {
	switch {
	case strings.TrimSpace(path) == "":
		return ""
	case fileExists(path):
		return fmt.Sprintf("; optional %s=%s (fast=true decodes with it)", key, path)
	}
	return fmt.Sprintf("; optional %s=%s is MISSING (fast=true falls back to the full VAE and says so)", key, path)
}

// engineRoute derives one iGPU route. required lists the (key, value) pairs the engine
// cannot run without, in a stable order; bindings is the full set to stat once all are set.
func engineRoute(name, engine, backendKey, backend string, refuse func(string) error, required []struct{ key, value string }, bindings []binding, exeDir string) Route {
	set := 0
	var unset []string
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			unset = append(unset, r.key)
		} else {
			set++
		}
	}
	if set == 0 {
		return Route{Name: name, Engine: engine, State: NotConfigured,
			Detail: fmt.Sprintf("%s is selected but nothing is bound (%s unset)", engine, strings.Join(unset, ", "))}
	}
	if len(unset) > 0 {
		return Route{Name: name, Engine: engine, State: BoundButMissing,
			Detail: fmt.Sprintf("%s is selected but %s is unset", engine, strings.Join(unset, ", "))}
	}
	if err := refuse(backend); err != nil {
		return Route{Name: name, Engine: engine, State: BoundButMissing,
			Detail: fmt.Sprintf("%s: %v — every call defers", backendKey, err)}
	}
	r := fileRoute(name, engine, exeDir, bindings...)
	if r.State == Configured {
		r.Detail += "; " + backendKey + "=" + backend
	}
	return r
}

type kv = struct{ key, value string }

// sdcppVideoRoute is generate_video for one sdcpp family binding.
func sdcppVideoRoute(name, family string, fb config.VideoFamilyBinding, scriptCfg string, cfg config.Config, exeDir string) Route {
	if scriptCfg == "" {
		scriptCfg = "render/sdcpp-video.mjs" // the pipeline's own fallback
	}
	bs := []binding{
		{key: "sdcpp_bin", value: fb.SdcppBin, kind: binaryBinding},
		{key: "sdcpp_model", value: fb.SdcppModel, kind: fileBinding},
	}
	if fb.SdcppHighNoiseModel != "" {
		bs = append(bs, binding{key: "sdcpp_high_noise_model", value: fb.SdcppHighNoiseModel, kind: fileBinding})
	}
	bs = append(bs,
		binding{key: "sdcpp_vae", value: fb.SdcppVAE, kind: fileBinding},
		binding{key: "sdcpp_t5xxl", value: fb.SdcppT5xxl, kind: fileBinding},
		binding{key: "videogen_sdcpp_script", value: scriptCfg, kind: scriptBinding})
	bs = append(bs, ffmpegBindings(cfg)...)
	prefix := fmt.Sprintf("videogen_families[%q].", family)
	r := engineRoute(name, engineSdcpp, prefix+"sdcpp_backend", fb.SdcppBackend, config.CPUBackendRefusal,
		[]kv{{prefix + "sdcpp_bin", fb.SdcppBin}, {prefix + "sdcpp_model", fb.SdcppModel}, {prefix + "sdcpp_vae", fb.SdcppVAE}, {prefix + "sdcpp_t5xxl", fb.SdcppT5xxl}},
		bs, exeDir)
	if r.State == Configured {
		r.Detail = "family=" + family + "; " + r.Detail + optionalTAEDetail("sdcpp_tae", fb.SdcppTAE)
	}
	r.Detail = licenseDetail(config.FamilyInfo{License: fb.License}) + r.Detail
	return r
}

// sdcppAnimateRoute is animate_character with animategen_engine sdcpp: sd-cli, the depth
// binary, the VACE model, the VAE, the T5 encoder and the depth model.
func sdcppAnimateRoute(cfg config.Config, exeDir string) Route {
	script := cfg.AnimateGenSdcppScript
	if script == "" {
		script = "render/sdcpp-animate.mjs"
	}
	bs := []binding{
		{key: "animategen_sdcpp_bin", value: cfg.AnimateGenSdcppBin, kind: binaryBinding},
		{key: "animategen_depth_bin", value: cfg.AnimateGenDepthBin, kind: binaryBinding},
		{key: "animategen_sdcpp_model", value: cfg.AnimateGenSdcppModel, kind: fileBinding},
		{key: "animategen_sdcpp_vae", value: cfg.AnimateGenSdcppVAE, kind: fileBinding},
		{key: "animategen_sdcpp_t5xxl", value: cfg.AnimateGenSdcppT5xxl, kind: fileBinding},
		{key: "animategen_depth_model", value: cfg.AnimateGenDepthModel, kind: fileBinding},
		{key: "animategen_sdcpp_script", value: script, kind: scriptBinding},
	}
	bs = append(bs, ffmpegBindings(cfg)...)
	r := engineRoute("animate_character", engineSdcpp, "animategen_sdcpp_backend", cfg.AnimateGenSdcppBackend, config.CPUBackendRefusal,
		[]kv{{"animategen_sdcpp_bin", cfg.AnimateGenSdcppBin}, {"animategen_depth_bin", cfg.AnimateGenDepthBin},
			{"animategen_sdcpp_model", cfg.AnimateGenSdcppModel}, {"animategen_sdcpp_vae", cfg.AnimateGenSdcppVAE},
			{"animategen_sdcpp_t5xxl", cfg.AnimateGenSdcppT5xxl}, {"animategen_depth_model", cfg.AnimateGenDepthModel}},
		bs, exeDir)
	if r.State == Configured {
		// the public Wan2.1 VACE GGUFs lack vace_patch_embedding.weight and sd-cli refuses them
		// ("model metadata validation failed"): a .safetensors VACE model is the one that loads.
		if strings.HasSuffix(strings.ToLower(cfg.AnimateGenSdcppModel), ".gguf") {
			r.Detail += "; NOTE animategen_sdcpp_model is a .gguf: the public VACE GGUFs lack vace_patch_embedding.weight and sd-cli refuses them - use the .safetensors VACE model (wan2.1_vace_1.3B_fp16)"
		}
		r.Detail += optionalTAEDetail("animategen_sdcpp_tae", cfg.AnimateGenSdcppTAE)
	}
	return r
}

// audiocppRoute is generate_audio:voice or :music with the audiocpp engine.
func audiocppRoute(kind string, cfg config.Config, exeDir string) Route {
	name := "generate_audio:" + kind
	modelKey, model, family := "audiocpp_voice_model", cfg.AudiocppVoiceModel, cfg.AudiocppVoiceFamilyName()
	if kind == "music" {
		modelKey, model, family = "audiocpp_music_model", cfg.AudiocppMusicModel, cfg.AudiocppMusicFamilyName()
	}
	script := cfg.AudiocppScript
	if script == "" {
		script = "render/audiocpp-generate.mjs"
	}
	bs := []binding{
		{key: "audiocpp_bin", value: cfg.AudiocppBin, kind: binaryBinding},
		{key: modelKey, value: model, kind: anyPathBinding},
		{key: "audiocpp_script", value: script, kind: scriptBinding},
	}
	// both kinds end in ffmpeg: the trim / fade / loudness chain and the dead-air gate
	bs = append(bs, ffmpegBindings(cfg)...)
	r := engineRoute(name, engineAudiocpp, "audiocpp_backend", cfg.AudiocppBackend, config.AudiocppBackendRefusal,
		[]kv{{"audiocpp_bin", cfg.AudiocppBin}, {modelKey, model}}, bs, exeDir)
	if r.State == Configured {
		r.Detail = "family=" + family + "; " + r.Detail
	}
	return r
}

// sdcppVideoFamilyNames lists the videogen_families bound to sdcpp, sorted.
func sdcppVideoFamilyNames(cfg config.Config) []string {
	var names []string
	for n, b := range cfg.VideoGenFamilies {
		if b.UsesSdcpp() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// igpuRoutes derives the iGPU routes that REPLACE a ComfyUI/python route of the same name
// (the default video family, animate, voice, music) and the extra per-family video routes.
// replaced is keyed by route name; extra are appended after the loop's own routes.
func igpuRoutes(cfg config.Config, exeDir string) (replaced map[string]Route, extra []Route) {
	replaced = map[string]Route{}
	// The default is resolved the way the pipeline resolves a request that names no model
	// (config.DefaultVideoSdcppFamily): an sdcpp family named like a ComfyUI one (wan22,
	// ltx25, ...) is the default when videogen_family is unset or spelled differently.
	defName, hasDef := cfg.DefaultVideoSdcppFamily()
	for _, n := range sdcppVideoFamilyNames(cfg) {
		fb := cfg.VideoGenFamilies[n]
		if hasDef && n == defName {
			replaced["generate_video"] = sdcppVideoRoute("generate_video", n, fb, cfg.VideoGenSdcppScript, cfg, exeDir)
			continue
		}
		extra = append(extra, sdcppVideoRoute(VideoFamilyRoute(n), n, fb, cfg.VideoGenSdcppScript, cfg, exeDir))
	}
	if cfg.AnimateGenEngine == config.EngineSdcpp {
		replaced["animate_character"] = sdcppAnimateRoute(cfg, exeDir)
	}
	if cfg.VoiceGenEngine == config.EngineAudiocpp {
		replaced["generate_audio:voice"] = audiocppRoute("voice", cfg, exeDir)
	}
	if cfg.MusicGenEngine == config.EngineAudiocpp {
		replaced["generate_audio:music"] = audiocppRoute("music", cfg, exeDir)
	}
	return replaced, extra
}

// sdcppVideoFamilyRows are the VideoFamilyBindingRows entries for the sdcpp families:
// what each binds, by config key, for doctor and offload_status.
func sdcppVideoFamilyRows(cfg config.Config) []VideoFamilyBindingRow {
	defaultFamily, _ := cfg.DefaultVideoSdcppFamily()
	var out []VideoFamilyBindingRow
	for _, n := range sdcppVideoFamilyNames(cfg) {
		fb := cfg.VideoGenFamilies[n]
		files := map[string]string{}
		for _, f := range []kv{{"sdcpp_bin", fb.SdcppBin}, {"sdcpp_model", fb.SdcppModel}, {"sdcpp_high_noise_model", fb.SdcppHighNoiseModel},
			{"sdcpp_vae", fb.SdcppVAE}, {"sdcpp_t5xxl", fb.SdcppT5xxl}, {"sdcpp_backend", fb.SdcppBackend}} {
			if f.value != "" {
				files[f.key] = f.value
			}
		}
		row := VideoFamilyBindingRow{Family: n, Default: n == defaultFamily, Files: files, CommercialUse: fb.CommercialUse}
		if fb.License != "" {
			lic := fb.License
			row.License = &lic
		}
		out = append(out, row)
	}
	return out
}
