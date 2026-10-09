package config

import (
	"encoding/json"
	"os"
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
		// an allowlist: anything that is not vulkan / vulkanN is refused, not left to the binary
		"cuda0", "hip", "blas", "opencl", "rpc", "vulcan", "bestest0", "cpufreq0x", "vulkan-0", "vulkan0 vulkan1",
	}
	for _, b := range refused {
		if err := CPUBackendRefusal(b); err == nil {
			t.Errorf("CPUBackendRefusal(%q) = nil, want a refusal", b)
		}
	}
	allowed := []string{
		"vulkan0", "Vulkan1", "diffusion=vulkan0,vae=vulkan0", "diffusion=vulkan0&vulkan1", "vulkan",
		// a module whose NAME contains the letters is fine: the value is what is checked
		"mycpu=vulkan0",
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

// ---- extra args (A5): nothing that changes the backend or the placement gets through

func TestExtraArgsRefusal(t *testing.T) {
	bad := [][]string{
		{"--backend", "cpu"}, {"--backend", "vulkan0"}, {"--backend=vulkan0"}, {"-b", "vulkan0"}, {"-b=cpu"}, {"--BACKEND", "x"},
		{"--params-backend", "cpu"}, {"--params-backend=vulkan0"}, {"--clip-on-cpu"}, {"--vae-on-cpu"},
		{"--control-net-cpu"}, {"--rpc", "192.0.2.1:50052"}, {"--rpc=192.0.2.1:50052"}, {"--cpu-moe"}, {"--n-cpu-moe", "8"},
		{"--offload-params-to-cpu"}, {"--offload-to-cpu=cpu"}, {"--some-flag", "cpu"}, {"--some-flag", "CPU0"}, {"--assign=te=cpu"}, {"--assign", "te=cpu,vae=vulkan0"},
		{"--assign", "diffusion=vulkan0&cpu"},
	}
	for _, a := range bad {
		if err := ExtraArgsRefusal("sdcpp_extra_args", ExtraArgsSdcpp, a); err == nil || !strings.HasPrefix(err.Error(), "EXTRA_ARGS_REFUSED") {
			t.Errorf("%v must be refused with EXTRA_ARGS_REFUSED, got %v", a, err)
		}
	}
	// --device belongs to the audiocpp_device key, and only audio.cpp has it
	for _, a := range [][]string{{"--device", "1"}, {"--device=1"}} {
		if ExtraArgsRefusal("audiocpp_extra_args", ExtraArgsAudiocpp, a) == nil {
			t.Errorf("audiocpp %v must be refused", a)
		}
		if err := ExtraArgsRefusal("sdcpp_extra_args", ExtraArgsSdcpp, a); err != nil {
			t.Errorf("sdcpp %v: --device is not an sd-cli placement flag here: %v", a, err)
		}
	}
	// --offload-to-cpu is sanctioned spill (weights parked in RAM, staged to the device, all compute
	// on the GPU), so it is accepted on every engine and alongside the flags that stay refused.
	for _, eng := range []string{ExtraArgsSdcpp, ExtraArgsDepth, ExtraArgsAudiocpp} {
		if err := ExtraArgsRefusal("x_extra_args", eng, []string{"--vae-tiling", "--offload-to-cpu", "--diffusion-fa"}); err != nil {
			t.Errorf("%s: --offload-to-cpu is sanctioned spill and must pass: %v", eng, err)
		}
		if err := ExtraArgsRefusal("x_extra_args", eng, []string{"--OFFLOAD-TO-CPU"}); err != nil {
			t.Errorf("%s: the flag name is case-insensitive: %v", eng, err)
		}
	}
	if i, a, _, ok := ScreenExtraArgs(ExtraArgsSdcpp, []string{"--offload-to-cpu", "--clip-on-cpu"}); !ok || i != 1 || a != "--clip-on-cpu" {
		t.Errorf("--offload-to-cpu must not mask the placement flag after it: %d %q %v", i, a, ok)
	}
	good := [][]string{{"--vae-tiling"}, {"--vae-tile-overlap", "0.25"}, {"--flag with space"}, {"--diffusion-fa"}, {"--threads", "4"}, nil, {}}
	for _, a := range good {
		if err := ExtraArgsRefusal("sdcpp_extra_args", ExtraArgsSdcpp, a); err != nil {
			t.Errorf("%v must pass: %v", a, err)
		}
	}
	i, arg, _, ok := ScreenExtraArgs(ExtraArgsSdcpp, []string{"--threads", "4", "--clip-on-cpu"})
	if !ok || i != 2 || arg != "--clip-on-cpu" {
		t.Errorf("ScreenExtraArgs = %d %q %v", i, arg, ok)
	}
}

func TestExtraArgsAreRefusedAtConfigLoadInEveryKey(t *testing.T) {
	cases := []struct{ name, body, key string }{
		{"video family backend", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_extra_args":["--backend","cpu"]}}}`, `videogen_families["fastwan"].sdcpp_extra_args[0]`},
		{"video family clip-on-cpu", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_extra_args":["--vae-tiling","--clip-on-cpu"]}}}`, `sdcpp_extra_args[1]`},
		{"animate", `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0","animategen_sdcpp_extra_args":["--vae-on-cpu"]}`, "animategen_sdcpp_extra_args[0]"},
		{"animate depth", `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0","animategen_depth_extra_args":["--backend=cpu"]}`, "animategen_depth_extra_args[0]"},
		{"audio backend", `{"voicegen_engine":"audiocpp","audiocpp_backend":"vulkan","audiocpp_extra_args":["--backend","cpu"]}`, "audiocpp_extra_args[0]"},
		{"audio device", `{"musicgen_engine":"audiocpp","audiocpp_backend":"vulkan","audiocpp_extra_args":["--device","1"]}`, "audiocpp_extra_args[0]"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), "EXTRA_ARGS_REFUSED") || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: err = %v, want EXTRA_ARGS_REFUSED naming %s", tc.name, err, tc.key)
		}
	}
	// a clean list loads
	if _, err := Load(writeCfg(t, `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_extra_args":["--vae-tile-overlap","0.25"]}}}`)); err != nil {
		t.Errorf("a clean extra-args list must load: %v", err)
	}
}

// ---- token cap (A3)

type tokenCapTable struct {
	Defaults struct{ Width, Height, Frames int } `json:"defaults"`
	Rows     []struct {
		Note                                       string
		Width, Height, Frames, Stride, Ref, Tokens int
	} `json:"rows"`
}

func loadTokenCapTable(t *testing.T) tokenCapTable {
	t.Helper()
	raw, err := os.ReadFile("../../render/testdata/token-cap-table.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab tokenCapTable
	if err := json.Unmarshal(raw, &tab); err != nil {
		t.Fatal(err)
	}
	return tab
}

// The Go and Node formulas are pinned to ONE table of inputs: render/igpu-engine.test.mjs reads
// the same file.
func TestLatentTokensAgreesWithTheSharedTable(t *testing.T) {
	tab := loadTokenCapTable(t)
	if len(tab.Rows) < 8 {
		t.Fatalf("the shared table lost rows: %d", len(tab.Rows))
	}
	want := map[int]bool{5070: false, 15600: false, 5760: false}
	for _, r := range tab.Rows {
		if got := LatentTokens(r.Width, r.Height, r.Frames, r.Stride, r.Ref); got != r.Tokens {
			t.Errorf("%s: LatentTokens(%dx%dx%d stride %d ref %d) = %d, want %d", r.Note, r.Width, r.Height, r.Frames, r.Stride, r.Ref, got, r.Tokens)
		}
		if _, ok := want[r.Tokens]; ok {
			want[r.Tokens] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("the table must carry the measured row with %d tokens", k)
		}
	}
}

func TestTokenCapRefusal(t *testing.T) {
	if err := TokenCapRefusal("sdcpp_max_tokens", 4096, 4096, 121, 16, 0, 0); err != nil {
		t.Errorf("no cap configured = no check: %v", err)
	}
	if err := TokenCapRefusal("sdcpp_max_tokens", 832, 480, 49, 16, 0, 5070); err != nil {
		t.Errorf("exactly at the cap passes: %v", err)
	}
	err := TokenCapRefusal("sdcpp_max_tokens", 832, 480, 49, 16, 0, 5000)
	if err == nil {
		t.Fatal("5070 tokens over a cap of 5000 must be refused")
	}
	for _, w := range []string{"TOKEN_CAP_EXCEEDED", "needs 5070 latent tokens", "cap is 5000", "(sdcpp_max_tokens)", "up to 45 frames fit", "Not retried"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("message %q must contain %q", err, w)
		}
	}
	err = TokenCapRefusal("animategen_sdcpp_max_tokens", 480, 832, 33, 8, 1, 3000)
	if err == nil || !strings.Contains(err.Error(), "needs 15600") || !strings.Contains(err.Error(), "+ reference") || !strings.Contains(err.Error(), "even 5 frames do not fit") {
		t.Errorf("animate refusal = %v", err)
	}
}

func TestTokenCapKeysValidate(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"video cap without stride", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_max_tokens":5000}}}`, "sdcpp_vae_stride is not"},
		{"video bad stride", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_max_tokens":5000,"sdcpp_vae_stride":4}}}`, "want 8 or 16"},
		{"video negative cap", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_max_tokens":-1,"sdcpp_vae_stride":16}}}`, "must not be negative"},
		{"video negative stride", `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_vae_stride":-8}}}`, "must not be negative"},
		{"cap keys on a comfy entry", `{"videogen_families":{"wan22":{"sdcpp_max_tokens":5000,"sdcpp_vae_stride":16}}}`, "sdcpp_* keys are set"},
		{"animate cap without stride", `{"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0","animategen_sdcpp_max_tokens":5760}`, "animategen_sdcpp_vae_stride is not"},
		{"animate bad stride", `{"animategen_sdcpp_max_tokens":5760,"animategen_sdcpp_vae_stride":12}`, "want 8 or 16"},
		{"animate negative cap", `{"animategen_sdcpp_max_tokens":-5,"animategen_sdcpp_vae_stride":8}`, "must not be negative"},
	}
	for _, tc := range cases {
		_, err := Load(writeCfg(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	c, err := Load(writeCfg(t, `{"videogen_families":{"fastwan":{"engine":"sdcpp","sdcpp_backend":"vulkan0","sdcpp_max_tokens":5070,"sdcpp_vae_stride":16}},
		"animategen_engine":"sdcpp","animategen_sdcpp_backend":"vulkan0","animategen_sdcpp_max_tokens":5760,"animategen_sdcpp_vae_stride":8}`))
	if err != nil {
		t.Fatalf("valid caps must load: %v", err)
	}
	if fb := c.VideoGenFamilies["fastwan"]; fb.SdcppMaxTokens != 5070 || fb.SdcppVAEStride != 16 || c.AnimateGenSdcppMaxTokens != 5760 || c.AnimateGenSdcppVAEStride != 8 {
		t.Errorf("the cap keys did not round-trip: %+v %d %d", fb, c.AnimateGenSdcppMaxTokens, c.AnimateGenSdcppVAEStride)
	}
}
