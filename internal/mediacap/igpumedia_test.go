package mediacap

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The iGPU engine routes (CT-49): CONFIGURED / NOT CONFIGURED / BOUND-BUT-MISSING for sdcpp
// video + animate and audio.cpp voice + music, derived from the same bindings the pipeline
// routes on.

// igpuBox is a bare config plus a node runtime and every runner script on disk.
func igpuBox(t *testing.T) (config.Config, string) {
	t.Helper()
	exeDir := t.TempDir()
	cfg := bare()
	cfg.NodePath = "node"
	for _, s := range []string{"sdcpp-video", "sdcpp-animate", "audiocpp-generate"} {
		touch(t, exeDir, "render/"+s+".mjs")
	}
	return cfg, exeDir
}

func bindVideo(t *testing.T, cfg *config.Config, exeDir string) {
	t.Helper()
	cfg.VideoGenFamily = "fastwan"
	cfg.VideoGenSdcppScript = "render/sdcpp-video.mjs"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": {
		Engine: config.EngineSdcpp, SdcppBackend: "vulkan0",
		SdcppBin: touch(t, exeDir, "sd-cli"), SdcppModel: touch(t, exeDir, "m/wan.gguf"),
		SdcppVAE: touch(t, exeDir, "m/vae.safetensors"), SdcppT5xxl: touch(t, exeDir, "m/t5.gguf"),
		License: "Apache-2.0",
	}}
}

func TestSdcppVideoRouteIsConfiguredOnlyWhenEveryBoundFileExists(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	bindVideo(t, &cfg, exeDir)
	got := byName(routesIn(cfg, exeDir))
	r := got["generate_video"]
	if r.State != Configured || r.Engine != "sdcpp" {
		t.Fatalf("all present: %+v", r)
	}
	if !strings.Contains(r.Detail, "family=fastwan") || !strings.Contains(r.Detail, "sdcpp_backend=vulkan0") || !strings.Contains(r.Detail, "license Apache-2.0") {
		t.Errorf("detail should name the family, backend and license: %s", r.Detail)
	}
	if _, ok := got["comfyui"]; ok {
		t.Error("an sdcpp video box must not be told it is missing ComfyUI")
	}
	if _, ok := got["node"]; !ok {
		t.Error("the runner is a node script: the node prereq must be reported")
	}

	// every bound file, one at a time: a missing one is BOUND-BUT-MISSING and names itself
	for name, mutate := range map[string]func(*config.VideoFamilyBinding){
		"sdcpp_bin":   func(b *config.VideoFamilyBinding) { b.SdcppBin = exeDir + "/gone-sd-cli" },
		"sdcpp_model": func(b *config.VideoFamilyBinding) { b.SdcppModel = exeDir + "/m/gone.gguf" },
		"sdcpp_vae":   func(b *config.VideoFamilyBinding) { b.SdcppVAE = exeDir + "/m/gone.safetensors" },
		"sdcpp_t5xxl": func(b *config.VideoFamilyBinding) { b.SdcppT5xxl = exeDir + "/m/gone-t5.gguf" },
		"sdcpp_high_noise_model": func(b *config.VideoFamilyBinding) {
			b.SdcppHighNoiseModel = exeDir + "/m/gone-high.gguf"
		},
	} {
		c := cfg
		fams := map[string]config.VideoFamilyBinding{"fastwan": cfg.VideoGenFamilies["fastwan"]}
		b := fams["fastwan"]
		mutate(&b)
		fams["fastwan"] = b
		c.VideoGenFamilies = fams
		r := byName(routesIn(c, exeDir))["generate_video"]
		if r.State != BoundButMissing || !strings.Contains(r.Detail, name) {
			t.Errorf("%s missing: %+v", name, r)
		}
	}
}

func TestSdcppVideoRouteStatesNotConfiguredAndPartial(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	cfg.VideoGenFamily = "fastwan"
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": {Engine: config.EngineSdcpp, SdcppBackend: "vulkan0"}}
	if r := byName(routesIn(cfg, exeDir))["generate_video"]; r.State != NotConfigured {
		t.Errorf("an engine with nothing bound: %+v", r)
	}
	b := cfg.VideoGenFamilies["fastwan"]
	b.SdcppBin = touch(t, exeDir, "sd-cli")
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": b}
	r := byName(routesIn(cfg, exeDir))["generate_video"]
	if r.State != BoundButMissing || !strings.Contains(r.Detail, "sdcpp_model") {
		t.Errorf("a half-bound engine: %+v", r)
	}
}

func TestACPUOrUnsetBackendMakesEveryIGPURouteBoundButMissing(t *testing.T) {
	for _, backend := range []string{"cpu", "", "diffusion=vulkan0,vae=cpu"} {
		cfg, exeDir := igpuBox(t)
		bindVideo(t, &cfg, exeDir)
		b := cfg.VideoGenFamilies["fastwan"]
		b.SdcppBackend = backend
		cfg.VideoGenFamilies["fastwan"] = b

		cfg.AnimateGenEngine = config.EngineSdcpp
		cfg.AnimateGenSdcppBackend = backend
		cfg.AnimateGenSdcppBin, cfg.AnimateGenDepthBin = touch(t, exeDir, "sd-cli2"), touch(t, exeDir, "da3-cli")
		cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE = touch(t, exeDir, "m/vace.gguf"), touch(t, exeDir, "m/v2.safetensors")
		cfg.AnimateGenSdcppT5xxl, cfg.AnimateGenDepthModel = touch(t, exeDir, "m/t52.gguf"), touch(t, exeDir, "m/depth.gguf")

		cfg.VoiceGenEngine, cfg.MusicGenEngine = config.EngineAudiocpp, config.EngineAudiocpp
		cfg.AudiocppBackend = backend
		cfg.AudiocppBin = touch(t, exeDir, "audiocpp_cli")
		cfg.AudiocppVoiceModel, cfg.AudiocppMusicModel = touch(t, exeDir, "m/cb.gguf"), touch(t, exeDir, "m/ace.gguf")

		got := byName(routesIn(cfg, exeDir))
		for _, name := range []string{"generate_video", "animate_character", "generate_audio:voice", "generate_audio:music"} {
			r := got[name]
			if r.State != BoundButMissing || !strings.Contains(r.Detail, "CPU") && !strings.Contains(r.Detail, "unset") {
				t.Errorf("%s with backend %q: %+v, want BOUND-BUT-MISSING naming the backend", name, backend, r)
			}
		}
	}
}

func TestSdcppAnimateRoute(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	cfg.AnimateGenEngine = config.EngineSdcpp
	cfg.AnimateGenSdcppBackend = "vulkan0"
	cfg.AnimateGenSdcppBin, cfg.AnimateGenDepthBin = touch(t, exeDir, "sd-cli"), touch(t, exeDir, "da3-cli")
	cfg.AnimateGenSdcppModel, cfg.AnimateGenSdcppVAE = touch(t, exeDir, "m/vace.gguf"), touch(t, exeDir, "m/vae.safetensors")
	cfg.AnimateGenSdcppT5xxl, cfg.AnimateGenDepthModel = touch(t, exeDir, "m/t5.gguf"), touch(t, exeDir, "m/depth.gguf")
	r := byName(routesIn(cfg, exeDir))["animate_character"]
	if r.State != Configured || r.Engine != "sdcpp" {
		t.Fatalf("all present: %+v", r)
	}
	for _, c := range []struct {
		name string
		set  func(*config.Config)
	}{
		{"animategen_depth_bin", func(c *config.Config) { c.AnimateGenDepthBin = exeDir + "/gone-da3" }},
		{"animategen_sdcpp_model", func(c *config.Config) { c.AnimateGenSdcppModel = exeDir + "/m/gone.gguf" }},
		{"animategen_sdcpp_vae", func(c *config.Config) { c.AnimateGenSdcppVAE = exeDir + "/m/gone-vae" }},
		{"animategen_sdcpp_t5xxl", func(c *config.Config) { c.AnimateGenSdcppT5xxl = exeDir + "/m/gone-t5" }},
		{"animategen_depth_model", func(c *config.Config) { c.AnimateGenDepthModel = exeDir + "/m/gone-depth" }},
		{"animategen_sdcpp_bin", func(c *config.Config) { c.AnimateGenSdcppBin = exeDir + "/gone-sd" }},
	} {
		cc := cfg
		c.set(&cc)
		r := byName(routesIn(cc, exeDir))["animate_character"]
		if r.State != BoundButMissing || !strings.Contains(r.Detail, c.name) {
			t.Errorf("%s missing: %+v", c.name, r)
		}
	}
	// the ComfyUI animate route is not consulted: no animategen_script needed
	if cfg.AnimateGenScript != "" {
		t.Fatal("fixture should have no comfy animate script")
	}
}

func TestAudiocppRoutesAcceptAModelFileOrDirectory(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	cfg.VoiceGenEngine, cfg.MusicGenEngine = config.EngineAudiocpp, config.EngineAudiocpp
	cfg.AudiocppBackend = "vulkan"
	cfg.AudiocppBin = touch(t, exeDir, "audiocpp_cli")
	cfg.AudiocppVoiceModel = touch(t, exeDir, "m/chatterbox-q8_0.gguf")
	touch(t, exeDir, "m/ace/ace-step-1.5-turbo-bf16.gguf")
	cfg.AudiocppMusicModel = exeDir + "/m/ace" // a package directory
	got := byName(routesIn(cfg, exeDir))
	for _, n := range []string{"generate_audio:voice", "generate_audio:music"} {
		if r := got[n]; r.State != Configured || r.Engine != "audiocpp" {
			t.Errorf("%s: %+v", n, r)
		}
	}
	if !strings.Contains(got["generate_audio:voice"].Detail, "family=chatterbox") || !strings.Contains(got["generate_audio:music"].Detail, "family=ace_step") {
		t.Errorf("family defaults must show: %s | %s", got["generate_audio:voice"].Detail, got["generate_audio:music"].Detail)
	}
	cfg.AudiocppVoiceModel = exeDir + "/m/gone.gguf"
	if r := byName(routesIn(cfg, exeDir))["generate_audio:voice"]; r.State != BoundButMissing || !strings.Contains(r.Detail, "audiocpp_voice_model") {
		t.Errorf("a missing voice model: %+v", r)
	}
	cfg.AudiocppBin = ""
	cfg.AudiocppVoiceModel = ""
	if r := byName(routesIn(cfg, exeDir))["generate_audio:voice"]; r.State != NotConfigured {
		t.Errorf("an audiocpp voice engine with nothing bound: %+v", r)
	}
}

func TestANonDefaultSdcppFamilyGetsItsOwnRouteAndRow(t *testing.T) {
	cfg, exeDir := igpuBox(t)
	bindVideo(t, &cfg, exeDir)
	cfg.VideoGenFamily = "" // the default is ComfyUI Wan (script unset in the bare fixture)
	got := byName(routesIn(cfg, exeDir))
	if r := got["generate_video"]; r.State != NotConfigured || r.Engine != "comfyui" {
		t.Errorf("the default route stays the ComfyUI one: %+v", r)
	}
	r, ok := got["generate_video:fastwan"]
	if !ok || r.State != Configured || r.Engine != "sdcpp" {
		t.Fatalf("generate_video:fastwan = %+v", r)
	}
	var found bool
	for _, row := range VideoFamilyBindingRows(cfg) {
		if row.Family == "fastwan" {
			found = true
			if row.Default || row.Files["sdcpp_model"] == "" || row.License == nil || *row.License != "Apache-2.0" {
				t.Errorf("row = %+v", row)
			}
		}
	}
	if !found {
		t.Error("VideoFamilyBindingRows must list the bound sdcpp family")
	}
	cfg.VideoGenFamily = "fastwan"
	for _, row := range VideoFamilyBindingRows(cfg) {
		if row.Family == "fastwan" && !row.Default {
			t.Error("the default sdcpp family must be marked default")
		}
	}
}

// A box with no engine key is byte-for-byte what it was: the comfy routes, in the same order.
func TestNoEngineKeyLeavesTheRoutesUntouched(t *testing.T) {
	exeDir := t.TempDir()
	cfg := config.Default()
	for _, r := range routesIn(cfg, exeDir) {
		if r.Engine == "sdcpp" || r.Engine == "audiocpp" || strings.HasPrefix(r.Name, "generate_video:") {
			t.Errorf("route %+v must not appear without an engine key", r)
		}
	}
	got := byName(routesIn(cfg, exeDir))
	if got["generate_video"].Engine != "comfyui" || got["animate_character"].Engine != "comfyui" ||
		got["generate_audio:voice"].Engine != "chatterbox-tts" || got["generate_audio:music"].Engine != "acestep" {
		t.Errorf("the comfy/python engines must stay: %+v", got)
	}
}
