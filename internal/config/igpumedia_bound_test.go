package config

import (
	"strings"
	"testing"
)

// CT-49 phase B: the per-engine backend allowlists (G13), the negative-recipe guards (G40),
// the TAE / high-noise keys, the default sdcpp family and the four *Bound helpers (B4).

func TestAudiocppBackendRefusal(t *testing.T) {
	for _, b := range []string{"vulkan", "Vulkan", " cuda ", "hip", "rocm", "metal"} {
		if err := AudiocppBackendRefusal(b); err != nil {
			t.Errorf("AudiocppBackendRefusal(%q) = %v, want nil", b, err)
		}
	}
	for _, b := range []string{"", " ", "cpu", "CPU", "best", "auto", "vulkan0", "vulkan1", "cuda0", "blas", "opencl", "vulcan", "vulkan,cpu"} {
		if err := AudiocppBackendRefusal(b); err == nil {
			t.Errorf("AudiocppBackendRefusal(%q) = nil, want a refusal", b)
		}
	}
	// the sd.cpp spelling gets a hint that names the right keys
	err := AudiocppBackendRefusal("vulkan1")
	if err == nil || !strings.Contains(err.Error(), `audiocpp_backend "vulkan" and audiocpp_device "1"`) {
		t.Errorf("vulkan1 must say how to spell it for audio.cpp, got %v", err)
	}
}

func TestAudiocppDeviceRefusal(t *testing.T) {
	for _, d := range []string{"", " ", "0", "1", " 2 "} {
		if err := AudiocppDeviceRefusal(d); err != nil {
			t.Errorf("AudiocppDeviceRefusal(%q) = %v, want nil", d, err)
		}
	}
	for _, d := range []string{"-1", "vulkan0", "0,1", "1.5", "cpu"} {
		if err := AudiocppDeviceRefusal(d); err == nil {
			t.Errorf("AudiocppDeviceRefusal(%q) = nil, want a refusal", d)
		}
	}
}

// audiocpp_backend "vulkan0" is sd.cpp's spelling and audio.cpp rejects it at run time: the
// load must refuse it instead of letting doctor say CONFIGURED (G13).
func TestAudiocppBackendValidatesAgainstAudioCppsOwnValues(t *testing.T) {
	for _, body := range []string{
		`{"voicegen_engine":"audiocpp","audiocpp_backend":"vulkan0"}`,
		`{"musicgen_engine":"audiocpp","audiocpp_backend":"best"}`,
	} {
		if _, err := Load(writeCfg(t, body)); err == nil || !strings.Contains(err.Error(), "audiocpp_backend") {
			t.Errorf("%s: want a refusal naming audiocpp_backend, got %v", body, err)
		}
	}
	if _, err := Load(writeCfg(t, `{"voicegen_engine":"audiocpp","audiocpp_backend":"vulkan","audiocpp_device":"vulkan0"}`)); err == nil || !strings.Contains(err.Error(), "audiocpp_device") {
		t.Errorf("a non-numeric audiocpp_device must be refused, got %v", err)
	}
	// and sd.cpp keeps its own spelling: vulkan0 is right THERE
	if _, err := Load(writeCfg(t, `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0"}`)); err != nil {
		t.Errorf("sd.cpp's vulkan0 must load: %v", err)
	}
}

// G40: each negative recipe value is refused by name; removing any one check turns a case red.
func TestNegativeRecipeValuesAreRefused(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"family negative steps", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","steps":-1}}}`, "steps, cfg and flow_shift must not be negative"},
		{"family negative cfg", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","cfg":-1}}}`, "steps, cfg and flow_shift must not be negative"},
		{"family negative flow_shift", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","flow_shift":-3}}}`, "steps, cfg and flow_shift must not be negative"},
		{"animate negative steps", `{"animategen_steps":-1}`, "animategen_steps, animategen_cfg and animategen_flow_shift must not be negative"},
		{"animate negative cfg", `{"animategen_cfg":-6}`, "animategen_steps, animategen_cfg and animategen_flow_shift must not be negative"},
		{"animate negative flow_shift", `{"animategen_flow_shift":-1}`, "animategen_steps, animategen_cfg and animategen_flow_shift must not be negative"},
		{"animate negative frames", `{"animategen_frames":-33}`, "animategen_frames must not be negative"},
		{"high-noise negative cfg", `{"videogen_families":{"a14b":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_high_noise_model":"/m/h.gguf","high_noise_cfg":-1}}}`, "high_noise_cfg and high_noise_steps must not be negative"},
		{"high-noise negative steps", `{"videogen_families":{"a14b":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_high_noise_model":"/m/h.gguf","high_noise_steps":-2}}}`, "high_noise_cfg and high_noise_steps must not be negative"},
		{"high-noise recipe without the expert", `{"videogen_families":{"a14b":{"engine":"sdcpp","sdcpp_backend":"vulkan0","high_noise_cfg":1}}}`, "sdcpp_high_noise_model is not set"},
		{"tae on a comfy entry", `{"videogen_families":{"wan22":{"sdcpp_tae":"/m/taew2_2.safetensors"}}}`, "sdcpp_* keys are set"},
		{"high-noise keys on a comfy entry", `{"videogen_families":{"wan22":{"high_noise_cfg":1}}}`, "sdcpp_* keys are set"},
	}
	for _, tc := range cases {
		if _, err := Load(writeCfg(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestTAEAndHighNoiseKeysRoundTripAndExpandTilde(t *testing.T) {
	c, err := Load(writeCfg(t, `{"videogen_families":{"a14b":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_bin":"/b",
		"sdcpp_model":"/m/l.gguf","sdcpp_high_noise_model":"/m/h.gguf","sdcpp_tae":"~/m/taew2_2.safetensors",
		"high_noise_cfg":1,"high_noise_steps":2,"high_noise_sampler":"euler"}},
		"animategen_sdcpp_tae":"~/m/taew2_1.safetensors"}`))
	if err != nil {
		t.Fatal(err)
	}
	fb := c.VideoGenFamilies["a14b"]
	if fb.HighNoiseCFG != 1 || fb.HighNoiseSteps != 2 || fb.HighNoiseSampler != "euler" {
		t.Errorf("high-noise recipe did not round-trip: %+v", fb)
	}
	if strings.HasPrefix(fb.SdcppTAE, "~") || !strings.HasSuffix(fb.SdcppTAE, "taew2_2.safetensors") {
		t.Errorf("sdcpp_tae = %q, want the tilde expanded", fb.SdcppTAE)
	}
	if strings.HasPrefix(c.AnimateGenSdcppTAE, "~") || !strings.HasSuffix(c.AnimateGenSdcppTAE, "taew2_1.safetensors") {
		t.Errorf("animategen_sdcpp_tae = %q, want the tilde expanded", c.AnimateGenSdcppTAE)
	}
}

func sdcppFam() VideoFamilyBinding {
	return VideoFamilyBinding{Engine: EngineSdcpp, SdcppBin: "/b", SdcppModel: "/m", SdcppBackend: "vulkan0"}
}

// DefaultVideoSdcppFamily answers the way the pipeline's resolution does (the pipeline
// package pins the two together): an sdcpp family named like a ComfyUI one is the default
// when videogen_family is unset or names it, and never otherwise.
func TestDefaultVideoSdcppFamily(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
		ok   bool
	}{
		{"nothing bound", Config{}, "", false},
		{"free name, default", Config{VideoGenFamily: "fastwan", VideoGenFamilies: map[string]VideoFamilyBinding{"fastwan": sdcppFam()}}, "fastwan", true},
		{"free name, not the default", Config{VideoGenFamilies: map[string]VideoFamilyBinding{"fastwan": sdcppFam()}}, "", false},
		{"wan22 sdcpp, videogen_family unset", Config{VideoGenFamilies: map[string]VideoFamilyBinding{"wan22": sdcppFam()}}, "wan22", true},
		{"wan22 sdcpp, videogen_family wan22", Config{VideoGenFamily: "wan22", VideoGenFamilies: map[string]VideoFamilyBinding{"wan22": sdcppFam()}}, "wan22", true},
		{"wan22 sdcpp, videogen_family spelled wan", Config{VideoGenFamily: "wan", VideoGenFamilies: map[string]VideoFamilyBinding{"wan22": sdcppFam()}}, "wan22", true},
		{"ltx25 sdcpp is the default", Config{VideoGenFamily: "ltx25", VideoGenFamilies: map[string]VideoFamilyBinding{"ltx25": sdcppFam()}}, "ltx25", true},
		{"ltx25 sdcpp, but the default is comfy wan22", Config{VideoGenFamilies: map[string]VideoFamilyBinding{"ltx25": sdcppFam()}}, "", false},
		{"wan22 sdcpp, but the default is comfy ltx25", Config{VideoGenFamily: "ltx25", VideoGenFamilies: map[string]VideoFamilyBinding{"wan22": sdcppFam()}}, "", false},
		{"hunyuan sdcpp is the default", Config{VideoGenFamily: "hunyuan", VideoGenFamilies: map[string]VideoFamilyBinding{"hunyuan": sdcppFam()}}, "hunyuan", true},
		{"h3 sdcpp is the default", Config{VideoGenFamily: "h3", VideoGenFamilies: map[string]VideoFamilyBinding{"h3": sdcppFam()}}, "h3", true},
	}
	for _, tc := range cases {
		got, ok := tc.cfg.DefaultVideoSdcppFamily()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: DefaultVideoSdcppFamily() = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// B4: every combination of (script/endpoint bound, engine bound) for each lane.
func TestBoundHelpers(t *testing.T) {
	sd := map[string]VideoFamilyBinding{"fastwan": sdcppFam()}
	type row struct {
		name string
		cfg  Config
		want [4]bool // video, animate, voice, music
	}
	rows := []row{
		{"empty", Config{}, [4]bool{}},
		{"video script", Config{VideoGenScript: "render/comfy-video.mjs"}, [4]bool{true, false, false, false}},
		{"video sdcpp default family", Config{VideoGenFamily: "fastwan", VideoGenFamilies: sd}, [4]bool{true, false, false, false}},
		{"video sdcpp family, not the default", Config{VideoGenFamilies: sd}, [4]bool{false, false, false, false}},
		{"video sdcpp wan22 with videogen_family unset", Config{VideoGenFamilies: map[string]VideoFamilyBinding{"wan22": sdcppFam()}}, [4]bool{true, false, false, false}},
		{"video script AND sdcpp", Config{VideoGenScript: "s", VideoGenFamily: "fastwan", VideoGenFamilies: sd}, [4]bool{true, false, false, false}},
		{"animate script", Config{AnimateGenScript: "render/comfy-animate.mjs"}, [4]bool{false, true, false, false}},
		{"animate sdcpp", Config{AnimateGenEngine: EngineSdcpp}, [4]bool{false, true, false, false}},
		{"animate script AND sdcpp", Config{AnimateGenScript: "s", AnimateGenEngine: EngineSdcpp}, [4]bool{false, true, false, false}},
		{"voice script", Config{VoiceGenScript: "render/tts.mjs"}, [4]bool{false, false, true, false}},
		{"voice tts endpoint", Config{TTSEndpoint: "http://192.0.2.10:8880"}, [4]bool{false, false, true, false}},
		{"voice audiocpp", Config{VoiceGenEngine: EngineAudiocpp}, [4]bool{false, false, true, false}},
		{"voice script AND audiocpp", Config{VoiceGenScript: "s", VoiceGenEngine: EngineAudiocpp}, [4]bool{false, false, true, false}},
		{"music script", Config{MusicGenScript: "render/comfy-music.mjs"}, [4]bool{false, false, false, true}},
		{"music audiocpp", Config{MusicGenEngine: EngineAudiocpp}, [4]bool{false, false, false, true}},
		{"music script AND audiocpp", Config{MusicGenScript: "s", MusicGenEngine: EngineAudiocpp}, [4]bool{false, false, false, true}},
		// the lanes are independent: a voice engine does not bind music, and the TTS endpoint is voice only
		{"everything", Config{VideoGenScript: "v", AnimateGenScript: "a", VoiceGenScript: "t", MusicGenScript: "m"}, [4]bool{true, true, true, true}},
	}
	for _, r := range rows {
		got := [4]bool{r.cfg.VideoGenBound(), r.cfg.AnimateGenBound(), r.cfg.VoiceGenBound(), r.cfg.MusicGenBound()}
		if got != r.want {
			t.Errorf("%s: (video, animate, voice, music) bound = %v, want %v", r.name, got, r.want)
		}
	}
}
