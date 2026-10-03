package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The iGPU media engines (CT-49) routed through a node stub that records its argv, so the
// whole Go path (family resolution, lease, gpugen, result shape) runs without a GPU, an
// engine binary or ffmpeg.

func sdcppVideoCfg(t *testing.T, dir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.VideoGenScript = "" // proves the sdcpp lane needs no ComfyUI runner
	cfg.VideoGenSdcppScript = writeArgStub(t, dir)
	cfg.VideoGenFamily = "fastwan"
	yes := true
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": {
		Engine: config.EngineSdcpp, SdcppBin: "/opt/sdcpp/sd-cli", SdcppModel: "/models/wan-ti2v-5b-q8_0.gguf",
		SdcppHighNoiseModel: "/models/high.gguf", SdcppVAE: "/models/wan2.2_vae.safetensors", SdcppT5xxl: "/models/umt5.gguf",
		SdcppBackend: "vulkan0", SdcppExtraArgs: []string{"--flag with space"},
		Steps: 3, CFG: 1, FlowShift: 3, Sampler: "euler", FPS: 24, Width: 832, Height: 480, Frames: 49,
		License: "Apache-2.0", CommercialUse: &yes,
	}}
	return cfg
}

func decodeVideo(t *testing.T, res core.Result) (path string, seed int, raw map[string]any) {
	t.Helper()
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	if err := json.Unmarshal(res.Data, &raw); err != nil {
		t.Fatal(err)
	}
	path, _ = raw["video_path"].(string)
	s, _ := raw["seed"].(float64)
	return path, int(s), raw
}

func igpuFlag(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--"+flag {
			return args[i+1]
		}
	}
	return ""
}

func TestRunGenerateVideo_SdcppFamilyRunsTheSdcppScriptWithBoundPathsAndNormalizedFrames(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	p := &Pipeline{cfg: sdcppVideoCfg(t, dir)}
	still := filepath.Join(dir, "still.png")
	res := p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateVideo, Input: "a calm sea", Image: still,
		// 50 frames and 854x485 are not what the model takes: the Wan family needs 4k+1 frames and /32 sizes
		Params: map[string]any{"seed": 7, "frames": 50, "width": 854, "height": 485, "negative": "blurry"},
	})
	out, seed, raw := decodeVideo(t, res)
	if seed != 7 {
		t.Errorf("seed = %d, want the caller's 7", seed)
	}
	args := readArgs(t, out)
	if args[0] != out || args[1] != still || args[2] != "a calm sea" {
		t.Errorf("positionals = %v, want <out> <still> <prompt>", args[:3])
	}
	want := map[string]string{
		"sd-bin": "/opt/sdcpp/sd-cli", "model": "/models/wan-ti2v-5b-q8_0.gguf", "high-noise-model": "/models/high.gguf",
		"vae": "/models/wan2.2_vae.safetensors", "t5xxl": "/models/umt5.gguf", "backend": "vulkan0",
		"frames": "49", "width": "832", "height": "480", "fps": "24", "steps": "3", "cfg": "1", "flow-shift": "3",
		"sampler": "euler", "seed": "7", "negative": "blurry", "extra-args": `["--flag with space"]`,
	}
	for k, v := range want {
		if got := igpuFlag(args, k); got != v {
			t.Errorf("--%s = %q, want %q (args %v)", k, got, v, args)
		}
	}
	if raw["license"] != "Apache-2.0" || raw["commercial_use"] != true {
		t.Errorf("the family's license must ride the result: %v", raw)
	}
	if res.Meta.Model != "sdcpp-video:fastwan" {
		t.Errorf("meta.model = %q, want the sdcpp tier label", res.Meta.Model)
	}
}

func TestRunGenerateVideo_APerRequestFamilyNamesTheSdcppBindingOnAComfyBox(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := sdcppVideoCfg(t, dir)
	cfg.VideoGenFamily = "" // the box's own default is ComfyUI Wan
	cfg.VideoGenScript = writeArgStub(t, dir)
	p := &Pipeline{cfg: cfg}
	out, _, _ := decodeVideo(t, p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateVideo, Input: "a calm sea", Params: map[string]any{"model": "fastwan", "seed": 3},
	}))
	args := readArgs(t, out)
	if igpuFlag(args, "sd-bin") == "" {
		t.Fatalf("model:fastwan must reach the sdcpp runner, got %v", args)
	}
	// T2V: no still positional
	if args[1] != "a calm sea" {
		t.Errorf("T2V positionals = %v", args[:2])
	}
	// and a request for a comfy family on the same box still goes to the comfy script
	out2, _, _ := decodeVideo(t, p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateVideo, Input: "a calm sea", Params: map[string]any{"seed": 3},
	}))
	if hasFlag(readArgs(t, out2), "sd-bin") {
		t.Errorf("a request with no model on a comfy-default box must not reach sdcpp")
	}
}

func TestRunAnimateCharacter_SdcppEngineRunsTheAnimateScript(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.AnimateGenScript = "" // no ComfyUI animate runner needed
	cfg.AnimateGenEngine = config.EngineSdcpp
	cfg.AnimateGenSdcppScript = writeArgStub(t, dir)
	cfg.AnimateGenSdcppBin, cfg.AnimateGenSdcppModel = "/opt/sdcpp/sd-cli", "/models/wan2.1-vace-1.3b-q8_0.gguf"
	cfg.AnimateGenSdcppVAE, cfg.AnimateGenSdcppT5xxl = "/models/wan_2.1_vae.safetensors", "/models/umt5.gguf"
	cfg.AnimateGenSdcppBackend = "vulkan0"
	cfg.AnimateGenSdcppExtraArgs = []string{"--x"}
	cfg.AnimateGenDepthBin, cfg.AnimateGenDepthModel = "/opt/depth/da3-cli", "/models/depth.gguf"
	cfg.AnimateGenDepthExtraArgs = []string{"--y"}
	cfg.AnimateGenSteps, cfg.AnimateGenCFG, cfg.AnimateGenFlowShift = 20, 6, 5
	cfg.AnimateGenWidth, cfg.AnimateGenHeight = 854, 480
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task: core.TaskAnimateCharacter, Input: "a knight", Image: "ref.png", Video: "drive.mp4",
		Params: map[string]any{"seed": 9, "frames": 82},
	})
	outPath, _, _ := decodeVideo(t, res)
	args := readArgs(t, outPath)
	if args[1] != "ref.png" || args[2] != "drive.mp4" || args[3] != "a knight" {
		t.Errorf("positionals = %v", args[:4])
	}
	want := map[string]string{
		"sd-bin": "/opt/sdcpp/sd-cli", "model": "/models/wan2.1-vace-1.3b-q8_0.gguf", "vae": "/models/wan_2.1_vae.safetensors",
		"t5xxl": "/models/umt5.gguf", "backend": "vulkan0", "depth-bin": "/opt/depth/da3-cli", "depth-model": "/models/depth.gguf",
		"frames": "81", "width": "832", "height": "480", "steps": "20", "cfg": "6", "flow-shift": "5", "seed": "9",
		"extra-args": `["--x"]`, "depth-extra-args": `["--y"]`,
	}
	for k, v := range want {
		if got := igpuFlag(args, k); got != v {
			t.Errorf("--%s = %q, want %q (args %v)", k, got, v, args)
		}
	}
	if res.Meta.Model != "sdcpp-animate:wan2.1-vace" {
		t.Errorf("meta.model = %q", res.Meta.Model)
	}
}

func audiocppCfg(t *testing.T, dir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = dir
	cfg.VoiceGenScript, cfg.MusicGenScript = "", ""
	cfg.VoiceGenEngine, cfg.MusicGenEngine = config.EngineAudiocpp, config.EngineAudiocpp
	cfg.AudiocppScript = writeArgStub(t, dir)
	cfg.AudiocppBin, cfg.AudiocppBackend, cfg.AudiocppDevice = "/opt/audiocpp/audiocpp_cli", "vulkan", "0"
	cfg.AudiocppVoiceModel, cfg.AudiocppMusicModel = "/models/chatterbox-q8_0.gguf", "/models/ace-step-1.5-turbo-bf16.gguf"
	cfg.AudiocppExtraArgs = []string{"--threads", "4"}
	return cfg
}

func TestRunGenerateAudio_AudiocppVoiceAndMusicRouteToTheirScript(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	p := &Pipeline{cfg: audiocppCfg(t, dir)}

	res := p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateAudio, Input: "hola mundo",
		Params: map[string]any{"kind": "voice", "seed": 5, "lang": "es", "clone": "/refs/me.wav"},
	})
	if !res.OK {
		t.Fatalf("voice defer: %s", res.Reason)
	}
	var v struct {
		AudioPath string `json:"audio_path"`
		Kind      string `json:"kind"`
		Seed      int    `json:"seed"`
	}
	_ = json.Unmarshal(res.Data, &v)
	if v.Kind != "voice" || v.Seed != 5 || !strings.HasSuffix(v.AudioPath, ".wav") {
		t.Errorf("voice result = %+v", v)
	}
	args := readArgs(t, v.AudioPath)
	if args[0] != v.AudioPath || args[1] != "hola mundo" || igpuFlag(args, "kind") != "voice" {
		t.Errorf("voice positionals = %v", args[:4])
	}
	for k, w := range map[string]string{"bin": "/opt/audiocpp/audiocpp_cli", "family": "chatterbox", "model": "/models/chatterbox-q8_0.gguf",
		"backend": "vulkan", "device": "0", "clone": "/refs/me.wav", "lang": "es", "seed": "5", "extra-args": `["--threads","4"]`} {
		if got := igpuFlag(args, k); got != w {
			t.Errorf("voice --%s = %q, want %q", k, got, w)
		}
	}
	if res.Meta.Model != "audiocpp:chatterbox" {
		t.Errorf("voice meta.model = %q", res.Meta.Model)
	}

	res = p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateAudio, Input: "warm lo-fi bed",
		Params: map[string]any{"kind": "music", "seed": 6, "seconds": 30, "lyrics": "la la"},
	})
	if !res.OK {
		t.Fatalf("music defer: %s", res.Reason)
	}
	_ = json.Unmarshal(res.Data, &v)
	if v.Kind != "music" || !strings.HasSuffix(v.AudioPath, ".wav") {
		t.Errorf("music result = %+v", v)
	}
	args = readArgs(t, v.AudioPath)
	for k, w := range map[string]string{"kind": "music", "family": "ace_step", "model": "/models/ace-step-1.5-turbo-bf16.gguf",
		"seconds": "30", "lyrics": "la la", "seed": "6"} {
		if got := igpuFlag(args, k); got != w {
			t.Errorf("music --%s = %q, want %q", k, got, w)
		}
	}
	if hasFlag(args, "clone") || hasFlag(args, "lang") {
		t.Errorf("music must carry no voice flags: %v", args)
	}
}

func TestRunGenerateAudio_AudiocppLeavesFinetunedAndEndpointOnTheirOwnLanes(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := audiocppCfg(t, dir)
	stub := writeArgStub(t, dir)
	cfg.VoiceGenScript = stub
	cfg.VoiceGenFTModel, cfg.VoiceGenFTBaseDir = "/ft/model.safetensors", "/ft/base"
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{"kind": "voice", "voice": "finetuned", "seed": 2},
	})
	if !res.OK {
		t.Fatalf("finetuned defer: %s", res.Reason)
	}
	var v struct {
		AudioPath string `json:"audio_path"`
	}
	_ = json.Unmarshal(res.Data, &v)
	if !hasFlagVal(readArgs(t, v.AudioPath), "engine", "finetuned") {
		t.Errorf("voice=finetuned must still reach the python worker")
	}
	// the endpoint is not the default on a box whose local voice engine is audiocpp
	cfg.VoiceGenScript, cfg.TTSEndpoint = "", "http://192.0.2.10:8000"
	if useTTSEndpoint(cfg, "") {
		t.Error("an audiocpp voice engine is a local voice: the tts endpoint must not become the default")
	}
	if !useTTSEndpoint(cfg, "endpoint") {
		t.Error("voice=endpoint must still select the endpoint")
	}
	cfg.VoiceGenEngine = ""
	if !useTTSEndpoint(cfg, "") {
		t.Error("with no local voice at all the endpoint stays the default, exactly as before")
	}
}

// The no-CPU rule at the pipeline's own door: a cpu (or unset) backend is a typed defer,
// the runner never spawns and nothing is written.
func TestACPUBackendIsATypedDeferOnEveryIGPULane(t *testing.T) {
	requireNodePipeline(t)
	for _, backend := range []string{"cpu", "", "diffusion=vulkan0,vae=cpu"} {
		dir := t.TempDir()
		cfgV := sdcppVideoCfg(t, dir)
		fb := cfgV.VideoGenFamilies["fastwan"]
		fb.SdcppBackend = backend
		cfgV.VideoGenFamilies["fastwan"] = fb

		cfgA := config.Default()
		cfgA.MediaDir = dir
		cfgA.AnimateGenEngine = config.EngineSdcpp
		cfgA.AnimateGenSdcppScript = writeArgStub(t, dir)
		cfgA.AnimateGenSdcppBin, cfgA.AnimateGenSdcppModel, cfgA.AnimateGenSdcppVAE, cfgA.AnimateGenSdcppT5xxl = "b", "m", "v", "t"
		cfgA.AnimateGenDepthBin, cfgA.AnimateGenDepthModel = "d", "dm"
		cfgA.AnimateGenSdcppBackend = backend

		cfgU := audiocppCfg(t, dir)
		cfgU.AudiocppBackend = backend

		for name, c := range map[string]struct {
			cfg config.Config
			req core.Request
		}{
			"video":   {cfgV, core.Request{Task: core.TaskGenerateVideo, Input: "p", Params: map[string]any{"out": filepath.Join(dir, "v.mp4")}}},
			"animate": {cfgA, core.Request{Task: core.TaskAnimateCharacter, Input: "p", Image: "r.png", Video: "d.mp4", Params: map[string]any{"out": filepath.Join(dir, "a.mp4")}}},
			"voice":   {cfgU, core.Request{Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{"kind": "voice", "out": filepath.Join(dir, "s.wav")}}},
			"music":   {cfgU, core.Request{Task: core.TaskGenerateAudio, Input: "lofi", Params: map[string]any{"kind": "music", "out": filepath.Join(dir, "m.wav")}}},
		} {
			p := &Pipeline{cfg: c.cfg}
			res := p.Run(context.Background(), c.req)
			if res.OK || !res.Deferred {
				t.Errorf("%s backend %q: want a typed defer, got ok=%v", name, backend, res.OK)
				continue
			}
			if res.Meta.ErrClass != "cpu_backend_refused" || !strings.Contains(res.Reason, "refused") {
				t.Errorf("%s backend %q: ErrClass=%q reason=%q, want cpu_backend_refused", name, backend, res.Meta.ErrClass, res.Reason)
			}
			for _, f := range []string{"v.mp4", "a.mp4", "s.wav", "m.wav"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					t.Errorf("%s backend %q: the runner ran anyway (%s exists)", name, backend, f)
				}
			}
		}
	}
}

func TestIGPULanesDeferByNameWhenAFileIsNotBound(t *testing.T) {
	dir := t.TempDir()
	cfg := sdcppVideoCfg(t, dir)
	fb := cfg.VideoGenFamilies["fastwan"]
	fb.SdcppT5xxl = ""
	cfg.VideoGenFamilies["fastwan"] = fb
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "p"})
	if res.OK || !strings.Contains(res.Reason, "sdcpp_t5xxl is not configured") {
		t.Errorf("reason = %q", res.Reason)
	}
	cfgU := audiocppCfg(t, dir)
	cfgU.AudiocppBin = ""
	res = (&Pipeline{cfg: cfgU}).Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "music"}})
	if res.OK || !strings.Contains(res.Reason, "audiocpp_bin is not configured") {
		t.Errorf("reason = %q", res.Reason)
	}
	cfgM := audiocppCfg(t, dir)
	cfgM.AudiocppMusicModel = ""
	res = (&Pipeline{cfg: cfgM}).Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "x", Params: map[string]any{"kind": "music"}})
	if res.OK || !strings.Contains(res.Reason, "audiocpp_music_model is not configured") {
		t.Errorf("reason = %q", res.Reason)
	}
}

func TestResolveVideoFamilyKnowsAnSdcppFamilyByItsOwnName(t *testing.T) {
	cfg := config.Default()
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"fastwan": {Engine: config.EngineSdcpp, SdcppBackend: "vulkan0"}}
	if arg, fam := resolveVideoFamily(cfg, "fastwan"); arg != "fastwan" || fam != "fastwan" {
		t.Errorf("explicit request: %q %q", arg, fam)
	}
	cfg.VideoGenFamily = "fastwan"
	if arg, fam := resolveVideoFamily(cfg, ""); arg != "fastwan" || fam != "fastwan" {
		t.Errorf("the box default: %q %q", arg, fam)
	}
	// an unbound name is still the closed set's Wan fallback, exactly as before
	if arg, fam := resolveVideoFamily(config.Default(), "fastwan"); arg != "fastwan" || fam != videoFamilyWanSentinel {
		t.Errorf("unbound name: %q %q", arg, fam)
	}
}

func TestNormalizeVideoFramesAndSizes(t *testing.T) {
	for in, want := range map[int]int{0: 0, -3: 0, 1: 5, 5: 5, 6: 5, 7: 9, 49: 49, 50: 49, 51: 53, 81: 81, 82: 81} {
		if got := normalizeVideoFrames(in); got != want {
			t.Errorf("normalizeVideoFrames(%d) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[int]int{0: 0, 10: 32, 32: 32, 480: 480, 481: 480, 854: 832} {
		if got := floorTo32(in); got != want {
			t.Errorf("floorTo32(%d) = %d, want %d", in, got, want)
		}
	}
	if quantFromModelFile("/m/Wan-TI2V-5B-Q8_0.gguf") != "q8_0" || quantFromModelFile("/models-bf16/x.gguf") != "" || quantFromModelFile("a-BF16.gguf") != "bf16" {
		t.Error("quantFromModelFile")
	}
}
