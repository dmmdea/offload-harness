package config

import (
	"strings"
	"testing"
)

// The iGPU media engines (CT-49). The rule under test: no model runs on CPU on
// these engines, so a cpu (or unset) backend is a config error the load refuses by
// name, never a silent fallback.

func TestCPUBackendRefusal(t *testing.T) {
	refused := []string{
		"", "  ", "cpu", "CPU", " cpu ", "cpu0", "best", "auto", "diffusion=best",
		"diffusion=vulkan0,vae=cpu", "clip=cpu,diffusion=vulkan0", "vulkan0,cpu", "diffusion=cuda0&cpu",
	}
	for _, b := range refused {
		if err := CPUBackendRefusal(b); err == nil {
			t.Errorf("CPUBackendRefusal(%q) = nil, want a refusal", b)
		}
	}
	allowed := []string{
		"vulkan0", "Vulkan1", "cuda0", "diffusion=vulkan0,vae=vulkan0", "diffusion=cuda0&cuda1", "vulkan", "hip", "bestest0",
		// a device or module that merely contains the letters is not the CPU
		"cpufreq0x", "mycpu=vulkan0",
	}
	for _, b := range allowed {
		if err := CPUBackendRefusal(b); err != nil {
			t.Errorf("CPUBackendRefusal(%q) = %v, want nil", b, err)
		}
	}
}

const sdcppVideoFamilyJSON = `{
  "videogen_family": "fastwan",
  "videogen_families": {
    "fastwan": {
      "engine": "sdcpp",
      "sdcpp_bin": "/opt/sdcpp/sd-cli",
      "sdcpp_model": "/models/wan-ti2v-5b-q8_0.gguf",
      "sdcpp_vae": "/models/wan2.2_vae.safetensors",
      "sdcpp_t5xxl": "/models/umt5-xxl-q8_0.gguf",
      "sdcpp_backend": "vulkan0",
      "sdcpp_extra_args": ["--vae-tiling"],
      "steps": 3, "cfg": 1, "flow_shift": 3, "sampler": "euler",
      "fps": 24, "width": 832, "height": 480, "frames": 49,
      "license": "Apache-2.0", "commercial_use": true
    }
  }
}`

func TestSdcppVideoFamilyParsesAndResolvesByItsOwnName(t *testing.T) {
	c, err := Load(writeCfg(t, sdcppVideoFamilyJSON))
	if err != nil {
		t.Fatalf("an sdcpp video family under a free name must load: %v", err)
	}
	if !c.SdcppVideoFamily("fastwan") || c.SdcppVideoFamily("wan22") || c.SdcppVideoFamily("") {
		t.Errorf("SdcppVideoFamily: fastwan=%v wan22=%v empty=%v", c.SdcppVideoFamily("fastwan"), c.SdcppVideoFamily("wan22"), c.SdcppVideoFamily(""))
	}
	// The default family is the sdcpp one, and its entry wins wholesale (the flat
	// videogen_* keys name ComfyUI filenames and must not reach sd-cli).
	for _, ask := range []string{"fastwan", ""} {
		fb := c.ResolveVideoFamilyBinding(ask)
		if !fb.UsesSdcpp() || fb.SdcppModel != "/models/wan-ti2v-5b-q8_0.gguf" || fb.Steps != 3 || fb.Sampler != "euler" ||
			fb.Width != 832 || fb.Frames != 49 || len(fb.SdcppExtraArgs) != 1 || fb.License != "Apache-2.0" {
			t.Errorf("ResolveVideoFamilyBinding(%q) = %+v", ask, fb)
		}
	}
}

func TestSdcppVideoFamilyPathsExpandTilde(t *testing.T) {
	c, err := Load(writeCfg(t, `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_bin":"~/bin/sd-cli",
		"sdcpp_model":"~/m/a.gguf","sdcpp_high_noise_model":"~/m/b.gguf","sdcpp_vae":"~/m/v.safetensors","sdcpp_t5xxl":"~/m/t.gguf",
		"sdcpp_backend":"vulkan0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	fb := c.VideoGenFamilies["fastwan"]
	for _, p := range []string{fb.SdcppBin, fb.SdcppModel, fb.SdcppHighNoiseModel, fb.SdcppVAE, fb.SdcppT5xxl} {
		if strings.HasPrefix(p, "~") {
			t.Errorf("path %q was not tilde-expanded", p)
		}
	}
}

func TestSdcppVideoFamilyRefusesACPUBackend(t *testing.T) {
	for _, backend := range []string{`"cpu"`, `""`, `"diffusion=vulkan0,vae=cpu"`} {
		body := `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_bin":"/b","sdcpp_model":"/m","sdcpp_backend":` + backend + `}}}`
		_, err := Load(writeCfg(t, body))
		if err == nil {
			t.Fatalf("backend %s must refuse the load", backend)
		}
		if !strings.Contains(err.Error(), "videogen_families[\"fastwan\"].sdcpp_backend") {
			t.Errorf("backend %s: the error must name the key, got %v", backend, err)
		}
	}
	// The key omitted entirely is the same refusal (an engine with no backend picks its own).
	if _, err := Load(writeCfg(t, `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_bin":"/b","sdcpp_model":"/m"}}}`)); err == nil {
		t.Error("an sdcpp family with no backend must refuse the load")
	}
}

func TestVideoFamilyEngineContract(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown engine", `{"videogen_families":{"wan22":{"engine":"onnx"}}}`, `engine: "onnx"`},
		{"sdcpp keys on a comfy entry", `{"videogen_families":{"wan22":{"sdcpp_model":"/m"}}}`, "sdcpp_* keys are set"},
		{"unknown name without sdcpp stays refused", `{"videogen_families":{"fastwan":{"fps":24}}}`, "unknown video family"},
		{"bad sdcpp family name", `{"videogen_families":{"Fast Wan":{"engine":"sdcpp","sdcpp_backend":"vulkan0"}}}`, "lower-case"},
		{"negative steps", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","steps":-1}}}`, "must not be negative"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	// engine "comfy" is today's path and stays valid on a known family.
	if _, err := Load(writeCfg(t, `{"videogen_families":{"ltx25":{"engine":"comfy","fps":24}}}`)); err != nil {
		t.Errorf("engine comfy on a known family must load: %v", err)
	}
}

func TestAnimateAndAudioEnginesRefuseACPUBackend(t *testing.T) {
	cases := []struct{ name, body, key string }{
		{"animate cpu", `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"cpu"}`, "animategen_sdcpp_backend"},
		{"animate unset", `{"animategen_engine":"sdcpp"}`, "animategen_sdcpp_backend"},
		{"animate unknown engine", `{"animategen_engine":"comfy"}`, "animategen_engine"},
		{"voice cpu", `{"voicegen_engine":"audiocpp","audiocpp_backend":"cpu"}`, "audiocpp_backend"},
		{"voice unset", `{"voicegen_engine":"audiocpp"}`, "audiocpp_backend"},
		{"music cpu", `{"musicgen_engine":"audiocpp","audiocpp_backend":"CPU"}`, "audiocpp_backend"},
		{"voice unknown engine", `{"voicegen_engine":"sdcpp"}`, "voicegen_engine"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: err = %v, want a refusal naming %s", tc.name, err, tc.key)
		}
	}
	ok := `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0","voicegen_engine":"audiocpp","musicgen_engine":"audiocpp",
		"audiocpp_backend":"vulkan","audiocpp_device":"0"}`
	c, err := Load(writeCfg(t, ok))
	if err != nil {
		t.Fatalf("a GPU backend on every engine must load: %v", err)
	}
	if c.AudiocppVoiceFamilyName() != "chatterbox" || c.AudiocppMusicFamilyName() != "ace_step" {
		t.Errorf("family defaults: %q %q", c.AudiocppVoiceFamilyName(), c.AudiocppMusicFamilyName())
	}
}

// A config that sets none of the engine keys is exactly what it was: no engine set
// anywhere, so every route stays on its ComfyUI / python path.
func TestDefaultConfigSelectsNoIGPUEngine(t *testing.T) {
	c := Default()
	if c.AnimateGenEngine != "" || c.VoiceGenEngine != "" || c.MusicGenEngine != "" || len(c.VideoGenFamilies) != 0 {
		t.Fatalf("Default() must select no iGPU engine: %+v", c)
	}
	if c.SdcppVideoFamily("wan22") || c.SdcppVideoFamily("") {
		t.Error("no family is an sdcpp family by default")
	}
	if fb := c.ResolveVideoFamilyBinding(""); fb.UsesSdcpp() {
		t.Errorf("the default video binding must not use sdcpp: %+v", fb)
	}
}
