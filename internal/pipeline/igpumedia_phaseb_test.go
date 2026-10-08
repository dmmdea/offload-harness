package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// CT-49 fix round, phase B: the pipeline half of the runner contract (flags then `--` then the
// positionals, one binary-resolution rule), the tiny-autoencoder opt-in and its notes, the
// high-noise recipe, audio.cpp's own backend values, the out dir, and the lane behaviours no test
// pinned (seed minting, the voice clone fallback, the COMFY_DIR strip, no ComfyUI /free, the
// empty-prompt and missing-input defers, a per-request steps override).

func audioReq(dir, kind, text string, params map[string]any) core.Request {
	if params == nil {
		params = map[string]any{}
	}
	params["kind"] = kind
	if _, ok := params["out"]; !ok {
		params["out"] = filepath.Join(dir, kind+".wav")
	}
	return core.Request{Task: core.TaskGenerateAudio, Input: text, Params: params}
}

// notesOf reads a result's "notes" array ("" joined), and whether the key was present at all.
func notesOf(t *testing.T, res core.Result) (string, bool) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(res.Data, &raw); err != nil {
		t.Fatal(err)
	}
	v, ok := raw["notes"]
	if !ok {
		return "", false
	}
	var parts []string
	for _, e := range v.([]any) {
		parts = append(parts, e.(string))
	}
	return strings.Join(parts, " | "), true
}

// ---------------------------------------------------------------- G11: the -- terminator

func TestEveryIGPULanePutsTheFlagsFirstThenADashDashTerminatorThenThePositionals(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	const dashed = "--- Intro --- a calm sea"

	check := func(name string, args []string, wantPos ...string) {
		t.Helper()
		term := -1
		for i, a := range args {
			if a == "--" {
				term = i
			}
		}
		if term < 0 {
			t.Fatalf("%s: no -- terminator in %v", name, args)
		}
		for _, a := range args[:term] {
			if a == dashed {
				t.Errorf("%s: the prompt appears before the terminator, where it would be read as a flag: %v", name, args)
			}
		}
		if got := args[term+1:]; strings.Join(got, "\x00") != strings.Join(wantPos, "\x00") {
			t.Errorf("%s: positionals = %q, want %q", name, got, wantPos)
		}
		if !strings.HasPrefix(args[0], "--") {
			t.Errorf("%s: the argv must open with a flag, got %q", name, args[0])
		}
	}

	pv := &Pipeline{cfg: sdcppVideoCfg(t, dir)}
	vOut := filepath.Join(dir, "v.mp4")
	out, _, _ := decodeVideo(t, pv.Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: dashed, Params: map[string]any{"out": vOut, "seed": 1}}))
	check("video", readArgs(t, out), vOut, dashed)

	pa := &Pipeline{cfg: animateCfg(t, dir)}
	aOut := filepath.Join(dir, "a.mp4")
	ares := pa.Run(context.Background(), core.Request{Task: core.TaskAnimateCharacter, Input: dashed, Image: "ref.png", Video: "drive.mp4", Params: map[string]any{"out": aOut, "seed": 1}})
	aPath, _, _ := decodeVideo(t, ares)
	check("animate", readArgs(t, aPath), aOut, "ref.png", "drive.mp4", dashed)

	pu := &Pipeline{cfg: audiocppCfg(t, dir)}
	for _, kind := range []string{"voice", "music"} {
		res := pu.Run(context.Background(), audioReq(dir, kind, dashed, map[string]any{"seed": 1, "lyrics": "--- Verse ---"}))
		if !res.OK {
			t.Fatalf("%s: %s", kind, res.Reason)
		}
		var v struct {
			AudioPath string `json:"audio_path"`
		}
		_ = json.Unmarshal(res.Data, &v)
		check(kind, readArgs(t, v.AudioPath), v.AudioPath, dashed)
		if kind == "music" && igpuFlag(readArgs(t, v.AudioPath), "lyrics") != "--- Verse ---" {
			t.Errorf("music lyrics must ride as the --lyrics value: %v", readArgs(t, v.AudioPath))
		}
	}
}

// ---------------------------------------------------------------- G10/G23: one binary-resolution rule

func TestTheRunnerGetsTheAbsolutePathABoundNameResolvesTo(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "igpu-test-engine"
	file := name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, file), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	onPath, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	abs, _ := filepath.Abs(onPath)

	// a bare name on PATH (what doctor reports CONFIGURED) reaches every runner as an absolute path
	cfgV := sdcppVideoCfg(t, dir)
	fb := cfgV.VideoGenFamilies["fastwan"]
	fb.SdcppBin = name
	cfgV.VideoGenFamilies["fastwan"] = fb
	out, _, _ := decodeVideo(t, (&Pipeline{cfg: cfgV}).Run(context.Background(), videoReq(dir, nil)))
	if got := igpuFlag(readArgs(t, out), "sd-bin"); got != abs || !filepath.IsAbs(got) {
		t.Errorf("video --sd-bin = %q, want the absolute %q", got, abs)
	}

	cfgA := animateCfg(t, dir)
	cfgA.AnimateGenSdcppBin, cfgA.AnimateGenDepthBin = name, name
	aOut, _, _ := decodeVideo(t, (&Pipeline{cfg: cfgA}).Run(context.Background(), animateReq(dir, nil)))
	aArgs := readArgs(t, aOut)
	if igpuFlag(aArgs, "sd-bin") != abs || igpuFlag(aArgs, "depth-bin") != abs {
		t.Errorf("animate bins = %q / %q, want %q", igpuFlag(aArgs, "sd-bin"), igpuFlag(aArgs, "depth-bin"), abs)
	}

	cfgU := audiocppCfg(t, dir)
	cfgU.AudiocppBin = name
	res := (&Pipeline{cfg: cfgU}).Run(context.Background(), audioReq(dir, "music", "lofi", nil))
	var v struct {
		AudioPath string `json:"audio_path"`
	}
	_ = json.Unmarshal(res.Data, &v)
	if got := igpuFlag(readArgs(t, v.AudioPath), "bin"); got != abs {
		t.Errorf("audio --bin = %q, want %q", got, abs)
	}

	// a RELATIVE explicit path becomes absolute too
	t.Chdir(binDir)
	cfgR := sdcppVideoCfg(t, dir)
	fbr := cfgR.VideoGenFamilies["fastwan"]
	fbr.SdcppBin = "." + string(os.PathSeparator) + file
	cfgR.VideoGenFamilies["fastwan"] = fbr
	rOut, _, _ := decodeVideo(t, (&Pipeline{cfg: cfgR}).Run(context.Background(), videoReq(dir, map[string]any{"out": filepath.Join(dir, "rel.mp4")})))
	if got := igpuFlag(readArgs(t, rOut), "sd-bin"); !filepath.IsAbs(got) || filepath.Base(got) != file {
		t.Errorf("relative --sd-bin = %q, want an absolute path to %s", got, file)
	}
}

func TestABoundBinaryThatResolvesToNothingIsADeferNamingTheKeyAndNothingRuns(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-engine")

	cfgV := sdcppVideoCfg(t, dir)
	fb := cfgV.VideoGenFamilies["fastwan"]
	fb.SdcppBin = missing
	cfgV.VideoGenFamilies["fastwan"] = fb
	cfgA := animateCfg(t, dir)
	cfgA.AnimateGenDepthBin = missing
	cfgS := animateCfg(t, dir)
	cfgS.AnimateGenSdcppBin = "igpu-no-such-command-xyz"
	cfgU := audiocppCfg(t, dir)
	cfgU.AudiocppBin = missing

	for name, c := range map[string]struct {
		cfg  config.Config
		req  core.Request
		want string
	}{
		"video":       {cfgV, videoReq(dir, nil), "sdcpp_bin=" + missing + " not found"},
		"depth":       {cfgA, animateReq(dir, nil), "animategen_depth_bin=" + missing + " not found"},
		"animate bin": {cfgS, animateReq(dir, nil), "animategen_sdcpp_bin=igpu-no-such-command-xyz not found"},
		"audio":       {cfgU, audioReq(dir, "voice", "hola", nil), "audiocpp_bin=" + missing + " not found"},
	} {
		res := (&Pipeline{cfg: c.cfg}).Run(context.Background(), c.req)
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, c.want) {
			t.Errorf("%s: want a defer containing %q, got ok=%v reason=%q", name, c.want, res.OK, res.Reason)
		}
	}
	for _, f := range []string{"v.mp4", "a.mp4", "voice.wav"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("a runner ran although its binary resolves to nothing (%s exists)", f)
		}
	}
}

// ---------------------------------------------------------------- G13: audio.cpp's own backend values

func TestAudiocppBackendIsValidatedAgainstAudioCppsValuesAtThePipelineToo(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	for _, backend := range []string{"vulkan", "cuda", "hip", "rocm", "metal"} {
		cfg := audiocppCfg(t, dir)
		cfg.AudiocppBackend = backend
		res := (&Pipeline{cfg: cfg}).Run(context.Background(), audioReq(dir, "music", "lofi", map[string]any{"out": filepath.Join(dir, backend+".wav")}))
		if !res.OK {
			t.Errorf("audiocpp_backend %q is one of audio.cpp's own values and must run: %s", backend, res.Reason)
		}
	}
	for _, backend := range []string{"vulkan0", "vulkan1", "cpu", "best", ""} {
		cfg := audiocppCfg(t, dir)
		cfg.AudiocppBackend = backend
		res := (&Pipeline{cfg: cfg}).Run(context.Background(), audioReq(dir, "voice", "hola", map[string]any{"out": filepath.Join(dir, "bad-"+strconv.Quote(backend)+".wav")}))
		mustDeferWith(t, res, "cpu_backend_refused", "refused")
	}
	// the hint for sd.cpp's spelling names the separate device key
	cfg := audiocppCfg(t, dir)
	cfg.AudiocppBackend = "vulkan1"
	mustDeferWith(t, (&Pipeline{cfg: cfg}).Run(context.Background(), audioReq(dir, "voice", "hola", nil)), "cpu_backend_refused", "audiocpp_device")
	cfg = audiocppCfg(t, dir)
	cfg.AudiocppDevice = "gpu0"
	mustDeferWith(t, (&Pipeline{cfg: cfg}).Run(context.Background(), audioReq(dir, "voice", "hola", nil)), "cpu_backend_refused", "not a device index")
}

// ---------------------------------------------------------------- G27: the out dir

func TestTheOutDirIsMadeUpFrontAndAnUncreatableOneIsReported(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "clip.mp4")
	out, _, _ := decodeVideo(t, (&Pipeline{cfg: sdcppVideoCfg(t, dir)}).Run(context.Background(), videoReq(dir, map[string]any{"out": nested})))
	if out != nested {
		t.Fatalf("out = %q, want %q", out, nested)
	}

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("i am a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a caller-supplied out under a regular file, and a media_dir that is one (the default out)
	cfgV := sdcppVideoCfg(t, dir)
	res := (&Pipeline{cfg: cfgV}).Run(context.Background(), videoReq(dir, map[string]any{"out": filepath.Join(blocker, "clip.mp4")}))
	if res.OK || !res.Deferred || !strings.Contains(res.Reason, "cannot create the output directory") || !strings.Contains(res.Reason, blocker) {
		t.Errorf("video: want the MkdirAll error reported, got ok=%v reason=%q", res.OK, res.Reason)
	}
	cfgA := animateCfg(t, dir)
	cfgA.MediaDir = blocker
	req := animateReq(dir, nil)
	delete(req.Params, "out")
	res = (&Pipeline{cfg: cfgA}).Run(context.Background(), req)
	if res.OK || !strings.Contains(res.Reason, "cannot create the output directory") {
		t.Errorf("animate default out under a file media_dir: ok=%v reason=%q", res.OK, res.Reason)
	}
	cfgU := audiocppCfg(t, dir)
	cfgU.MediaDir = blocker
	res = (&Pipeline{cfg: cfgU}).Run(context.Background(), core.Request{Task: core.TaskGenerateAudio, Input: "hola", Params: map[string]any{"kind": "voice"}})
	if res.OK || !strings.Contains(res.Reason, "cannot create the output directory") {
		t.Errorf("audio default out under a file media_dir: ok=%v reason=%q", res.OK, res.Reason)
	}
}

// ---------------------------------------------------------------- G9: the high-noise recipe

func TestTheHighNoiseExpertsRecipeReachesTheRunnerOnlyWhenBound(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := sdcppVideoCfg(t, dir)
	fb := cfg.VideoGenFamilies["fastwan"]
	fb.HighNoiseCFG, fb.HighNoiseSteps, fb.HighNoiseSampler = 1, 2, "euler"
	cfg.VideoGenFamilies["fastwan"] = fb
	out, _, _ := decodeVideo(t, (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, nil)))
	args := readArgs(t, out)
	for k, v := range map[string]string{"high-noise-model": "/models/high.gguf", "high-noise-cfg": "1", "high-noise-steps": "2", "high-noise-sampler": "euler"} {
		if got := igpuFlag(args, k); got != v {
			t.Errorf("--%s = %q, want %q (%v)", k, got, v, args)
		}
	}
	// a pair with no recipe leaves sd-cli's own defaults: no flags
	plain := sdcppVideoCfg(t, dir)
	out2, _, _ := decodeVideo(t, (&Pipeline{cfg: plain}).Run(context.Background(), videoReq(dir, map[string]any{"out": filepath.Join(dir, "p.mp4")})))
	for _, f := range []string{"high-noise-cfg", "high-noise-steps", "high-noise-sampler"} {
		if hasFlag(readArgs(t, out2), f) {
			t.Errorf("--%s must be absent when the family binds no recipe", f)
		}
	}
	// the recipe belongs to the high-noise model: with a single-model family nothing is sent
	single := sdcppVideoCfg(t, dir)
	sfb := single.VideoGenFamilies["fastwan"]
	sfb.SdcppHighNoiseModel, sfb.HighNoiseCFG = "", 1
	single.VideoGenFamilies["fastwan"] = sfb
	out3, _, _ := decodeVideo(t, (&Pipeline{cfg: single}).Run(context.Background(), videoReq(dir, map[string]any{"out": filepath.Join(dir, "s.mp4")})))
	if hasFlag(readArgs(t, out3), "high-noise-cfg") || hasFlag(readArgs(t, out3), "high-noise-model") {
		t.Errorf("no high-noise model: no high-noise flags, got %v", readArgs(t, out3))
	}
}

// ---------------------------------------------------------------- the TAE opt-in and its notes

func TestFastDecodesWithTheTinyAutoencoderOnlyWhenOneIsBoundAndSaysSoOtherwise(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	tae := igpuBin(t, dir, "taew2_2.safetensors")

	video := func(taePath string, params map[string]any) core.Result {
		cfg := sdcppVideoCfg(t, dir)
		fb := cfg.VideoGenFamilies["fastwan"]
		fb.SdcppTAE = taePath
		cfg.VideoGenFamilies["fastwan"] = fb
		if params == nil {
			params = map[string]any{}
		}
		params["out"] = filepath.Join(dir, "v-"+strconv.Itoa(len(taePath))+"-"+strconv.FormatBool(params["fast"] == true)+".mp4")
		return (&Pipeline{cfg: cfg}).Run(context.Background(), core.Request{Task: core.TaskGenerateVideo, Input: "p", Params: params})
	}

	// fast + a bound TAE: --tae and a note
	res := video(tae, map[string]any{"fast": true})
	out, _, _ := decodeVideo(t, res)
	if got := igpuFlag(readArgs(t, out), "tae"); got != tae {
		t.Errorf("--tae = %q, want %q", got, tae)
	}
	if n, ok := notesOf(t, res); !ok || !strings.Contains(n, "tiny autoencoder taew2_2.safetensors") {
		t.Errorf("notes = %q (present=%v), want the TAE decode named", n, ok)
	}
	// fast + no key: a no-op that SAYS so; the full VAE decodes
	res = video("", map[string]any{"fast": true})
	out, _, _ = decodeVideo(t, res)
	if hasFlag(readArgs(t, out), "tae") {
		t.Error("no sdcpp_tae bound: no --tae")
	}
	if n, ok := notesOf(t, res); !ok || !strings.Contains(n, "no-op") || !strings.Contains(n, "full VAE") {
		t.Errorf("notes = %q, want the no-op said", n)
	}
	// fast + a bound but MISSING file: the full VAE, and the note says why
	res = video(filepath.Join(dir, "missing-tae.safetensors"), map[string]any{"fast": true})
	out, _, _ = decodeVideo(t, res)
	if hasFlag(readArgs(t, out), "tae") {
		t.Error("a missing TAE file must not reach sd-cli")
	}
	if n, _ := notesOf(t, res); !strings.Contains(n, "does not exist") {
		t.Errorf("notes = %q, want the missing file named", n)
	}
	// no fast: the full VAE even with a TAE bound, and no notes key at all
	res = video(tae, nil)
	out, _, _ = decodeVideo(t, res)
	if hasFlag(readArgs(t, out), "tae") {
		t.Error("fast was not asked for: the full VAE is the default")
	}
	if _, ok := notesOf(t, res); ok {
		t.Error("a request with nothing to note must carry no notes key (the result stays byte-identical)")
	}

	// animate
	anim := func(taePath string, fast bool) core.Result {
		cfg := animateCfg(t, dir)
		cfg.AnimateGenSdcppTAE = taePath
		params := map[string]any{}
		if fast {
			params["fast"] = true
		}
		req := animateReq(dir, params)
		req.Params["out"] = filepath.Join(dir, "an-"+strconv.Itoa(len(taePath))+strconv.FormatBool(fast)+".mp4")
		return (&Pipeline{cfg: cfg}).Run(context.Background(), req)
	}
	res = anim(tae, true)
	out, _, _ = decodeVideo(t, res)
	if igpuFlag(readArgs(t, out), "tae") != tae {
		t.Errorf("animate --tae missing: %v", readArgs(t, out))
	}
	res = anim("", true)
	out, _, _ = decodeVideo(t, res)
	if n, _ := notesOf(t, res); !strings.Contains(n, "no-op") || hasFlag(readArgs(t, out), "tae") {
		t.Errorf("animate without animategen_sdcpp_tae: notes %q args %v", n, readArgs(t, out))
	}
	res = anim(tae, false)
	if _, ok := notesOf(t, res); ok {
		t.Error("animate without fast carries no notes")
	}
}

// ---------------------------------------------------------------- G2: the ledger family of an sdcpp wan22

func TestADefaultSdcppFamilyNamedWan22IsTheLedgerFamilyNotTheGenericSdcppLabel(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := sdcppVideoCfg(t, dir)
	cfg.VideoGenFamily = "" // unset: the default is wan22, which is bound to sdcpp below
	cfg.VideoGenFamilies = map[string]config.VideoFamilyBinding{"wan22": cfg.VideoGenFamilies["fastwan"]}
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, nil))
	decodeVideo(t, res)
	if res.Meta.Model != "sdcpp-video:wan22" {
		t.Errorf("meta.model = %q, want sdcpp-video:wan22 (the family doctor and the fleet name)", res.Meta.Model)
	}
}

// ---------------------------------------------------------------- G37: lane behaviours with no failing test

func TestEveryIGPULaneMintsASeedWhenNoneIsGivenAndReportsIt(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	type lane struct {
		name string
		p    *Pipeline
		req  core.Request
		path string
	}
	noSeed := func(r core.Request) core.Request {
		delete(r.Params, "seed")
		return r
	}
	for _, l := range []lane{
		{"video", &Pipeline{cfg: sdcppVideoCfg(t, dir)}, noSeed(videoReq(dir, nil)), "video_path"},
		{"animate", &Pipeline{cfg: animateCfg(t, dir)}, noSeed(animateReq(dir, nil)), "video_path"},
		{"voice", &Pipeline{cfg: audiocppCfg(t, dir)}, audioReq(dir, "voice", "hola", nil), "audio_path"},
		{"music", &Pipeline{cfg: audiocppCfg(t, dir)}, audioReq(dir, "music", "lofi", nil), "audio_path"},
	} {
		res := l.p.Run(context.Background(), l.req)
		if !res.OK {
			t.Fatalf("%s: %s", l.name, res.Reason)
		}
		var raw map[string]any
		_ = json.Unmarshal(res.Data, &raw)
		seed, _ := raw["seed"].(float64)
		if seed <= 0 {
			t.Errorf("%s: the result seed = %v, want a minted positive seed", l.name, raw["seed"])
		}
		args := readArgs(t, raw[l.path].(string))
		if got := igpuFlag(args, "seed"); got != strconv.Itoa(int(seed)) || got == "0" {
			t.Errorf("%s: --seed = %q, want the minted %d (never 0)", l.name, got, int(seed))
		}
	}
}

func TestTheVoiceCloneReferenceFallsBackToVoicegenRefAndAParamWins(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := audiocppCfg(t, dir)
	cfg.VoiceGenRef = "/refs/house-voice.wav"
	p := &Pipeline{cfg: cfg}
	clone := func(params map[string]any) string {
		res := p.Run(context.Background(), audioReq(dir, "voice", "hola", params))
		if !res.OK {
			t.Fatalf("voice: %s", res.Reason)
		}
		var v struct {
			AudioPath string `json:"audio_path"`
		}
		_ = json.Unmarshal(res.Data, &v)
		return igpuFlag(readArgs(t, v.AudioPath), "clone")
	}
	if got := clone(map[string]any{"seed": 1, "out": filepath.Join(dir, "a.wav")}); got != "/refs/house-voice.wav" {
		t.Errorf("--clone = %q, want the configured voicegen_ref", got)
	}
	if got := clone(map[string]any{"seed": 1, "clone": "/refs/mine.wav", "out": filepath.Join(dir, "b.wav")}); got != "/refs/mine.wav" {
		t.Errorf("--clone = %q, want the request's own reference", got)
	}
	cfg.VoiceGenRef = ""
	p = &Pipeline{cfg: cfg}
	if got := clone(map[string]any{"seed": 1, "out": filepath.Join(dir, "c.wav")}); got != "" {
		t.Errorf("no reference anywhere: --clone = %q, want none", got)
	}
}

func TestAnIGPURunnerNeverSeesComfyDirAndNeverTriggersAComfyUIFree(t *testing.T) {
	requireNodePipeline(t)
	var frees atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/free") {
			frees.Add(1)
		}
	}))
	defer srv.Close()
	t.Setenv("COMFY_API", srv.URL)
	t.Setenv("COMFY_DIR", "inherited")
	if err := os.Unsetenv("COMFY_DIR"); err != nil { // restored by t.Setenv's cleanup
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgV := sdcppVideoCfg(t, dir)
	cfgV.ComfyDir = "/opt/comfy"
	cfgA := animateCfg(t, dir)
	cfgA.ComfyDir = "/opt/comfy"
	cfgU := audiocppCfg(t, dir)
	cfgU.ComfyDir = "/opt/comfy"
	var paths []string
	for name, c := range map[string]struct {
		cfg config.Config
		req core.Request
	}{
		"video":   {cfgV, videoReq(dir, nil)},
		"animate": {cfgA, animateReq(dir, nil)},
		"voice":   {cfgU, audioReq(dir, "voice", "hola", nil)},
		"music":   {cfgU, audioReq(dir, "music", "lofi", nil)},
	} {
		res := (&Pipeline{cfg: c.cfg}).Run(context.Background(), c.req)
		if !res.OK {
			t.Fatalf("%s: %s", name, res.Reason)
		}
		var raw map[string]any
		_ = json.Unmarshal(res.Data, &raw)
		for _, k := range []string{"video_path", "audio_path"} {
			if p, ok := raw[k].(string); ok {
				paths = append(paths, p)
			}
		}
	}
	for _, out := range paths {
		b, err := os.ReadFile(out + ".env")
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			ComfyDir *string `json:"COMFY_DIR"`
		}
		_ = json.Unmarshal(b, &env)
		if env.ComfyDir != nil {
			t.Errorf("%s: the runner saw COMFY_DIR=%q; an iGPU engine has no ComfyUI", out, *env.ComfyDir)
		}
	}
	if n := frees.Load(); n != 0 {
		t.Errorf("a ComfyUI /free was fired %d time(s) from a ComfyUI-less lane", n)
	}
}

func TestEmptyPromptMissingInputsAndAMissingRunnerScriptAreDefersThatNeverSpawn(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	run := func(c config.Config, req core.Request) core.Result {
		return (&Pipeline{cfg: c}).Run(context.Background(), req)
	}
	for name, res := range map[string]core.Result{
		"video":   run(sdcppVideoCfg(t, dir), core.Request{Task: core.TaskGenerateVideo, Input: "  ", Params: map[string]any{"out": filepath.Join(dir, "v.mp4")}}),
		"animate": run(animateCfg(t, dir), core.Request{Task: core.TaskAnimateCharacter, Input: "", Image: "r.png", Video: "d.mp4", Params: map[string]any{"out": filepath.Join(dir, "a.mp4")}}),
		"voice":   run(audiocppCfg(t, dir), audioReq(dir, "voice", " ", nil)),
	} {
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "empty") {
			t.Errorf("%s: want an empty-prompt defer, got ok=%v reason=%q", name, res.OK, res.Reason)
		}
	}
	// animate needs both the reference image and the driver video
	for name, req := range map[string]core.Request{
		"no ref":    {Task: core.TaskAnimateCharacter, Input: "a knight", Video: "d.mp4", Params: map[string]any{"out": filepath.Join(dir, "a.mp4")}},
		"no driver": {Task: core.TaskAnimateCharacter, Input: "a knight", Image: "r.png", Params: map[string]any{"out": filepath.Join(dir, "a.mp4")}},
	} {
		res := run(animateCfg(t, dir), req)
		if res.OK || !strings.Contains(res.Reason, "needs both a reference image and a driver video") {
			t.Errorf("%s: reason = %q", name, res.Reason)
		}
	}
	// a runner script that is not there
	gone := filepath.Join(dir, "no-such-runner.mjs")
	cfgV := sdcppVideoCfg(t, dir)
	cfgV.VideoGenSdcppScript = gone
	cfgA := animateCfg(t, dir)
	cfgA.AnimateGenSdcppScript = gone
	cfgU := audiocppCfg(t, dir)
	cfgU.AudiocppScript = gone
	for name, res := range map[string]core.Result{
		"video":   run(cfgV, videoReq(dir, nil)),
		"animate": run(cfgA, animateReq(dir, nil)),
		"audio":   run(cfgU, audioReq(dir, "music", "lofi", nil)),
	} {
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "script not found") {
			t.Errorf("%s: want a missing-script defer, got ok=%v reason=%q", name, res.OK, res.Reason)
		}
	}
	for _, f := range []string{"v.mp4", "a.mp4", "voice.wav", "music.wav"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("a runner ran although the request was refused (%s exists)", f)
		}
	}
}

func TestAPerRequestStepsCountOverTheFamilysAndTheAnimateDefault(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := sdcppVideoCfg(t, dir) // the family's Steps is 3
	out, _, _ := decodeVideo(t, (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, map[string]any{"steps": 9})))
	if got := igpuFlag(readArgs(t, out), "steps"); got != "9" {
		t.Errorf("video --steps = %q, want the request's 9", got)
	}
	out2, _, _ := decodeVideo(t, (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, map[string]any{"out": filepath.Join(dir, "s2.mp4")})))
	if got := igpuFlag(readArgs(t, out2), "steps"); got != "3" {
		t.Errorf("video --steps = %q, want the family's 3", got)
	}
	acfg := animateCfg(t, dir)
	acfg.AnimateGenSteps = 20
	out3, _, _ := decodeVideo(t, (&Pipeline{cfg: acfg}).Run(context.Background(), animateReq(dir, map[string]any{"steps": 12})))
	if got := igpuFlag(readArgs(t, out3), "steps"); got != "12" {
		t.Errorf("animate --steps = %q, want the request's 12", got)
	}
	req := animateReq(dir, nil)
	req.Params["out"] = filepath.Join(dir, "a2.mp4")
	out4, _, _ := decodeVideo(t, (&Pipeline{cfg: acfg}).Run(context.Background(), req))
	if got := igpuFlag(readArgs(t, out4), "steps"); got != "20" {
		t.Errorf("animate --steps = %q, want animategen_steps 20", got)
	}
}

// ---------------------------------------------------------------- the config helper mirrors the pipeline

// config.DefaultVideoSdcppFamily restates the pipeline's resolution (resolveVideoFamily, then
// ResolveVideoFamilyBinding) for the callers that cannot import the pipeline (mediacap's verdict,
// the fleet's bound helpers). This pins the two together over every spelling that matters: for a
// request that names no model, the pipeline serves it by sdcpp exactly when the helper says so, and
// under the family the helper names.
func TestDefaultVideoSdcppFamilyMirrorsThePipeline(t *testing.T) {
	sd := config.VideoFamilyBinding{Engine: config.EngineSdcpp, SdcppBin: "/b", SdcppModel: "/m", SdcppBackend: "vulkan0"}
	cases := []struct {
		name string
		fam  string
		fams map[string]config.VideoFamilyBinding
	}{
		{"nothing bound", "", nil},
		{"free name, default", "fastwan", map[string]config.VideoFamilyBinding{"fastwan": sd}},
		{"free name, not the default", "", map[string]config.VideoFamilyBinding{"fastwan": sd}},
		{"wan22, videogen_family unset", "", map[string]config.VideoFamilyBinding{"wan22": sd}},
		{"wan22, videogen_family wan22", "wan22", map[string]config.VideoFamilyBinding{"wan22": sd}},
		{"wan22, videogen_family spelled wan", "wan", map[string]config.VideoFamilyBinding{"wan22": sd}},
		{"ltx25 is the default", "ltx25", map[string]config.VideoFamilyBinding{"ltx25": sd}},
		{"ltx25 sdcpp, comfy default", "", map[string]config.VideoFamilyBinding{"ltx25": sd}},
		{"wan22 sdcpp, comfy ltx25 default", "ltx25", map[string]config.VideoFamilyBinding{"wan22": sd}},
		{"hunyuan is the default", "hunyuan", map[string]config.VideoFamilyBinding{"hunyuan": sd}},
		{"h3 is the default", "h3", map[string]config.VideoFamilyBinding{"h3": sd}},
		{"a spelling the runner does not know", "foo", map[string]config.VideoFamilyBinding{"wan22": sd}},
	}
	for _, tc := range cases {
		cfg := config.Default()
		cfg.VideoGenFamily, cfg.VideoGenFamilies = tc.fam, tc.fams
		render, _, ok := (&Pipeline{cfg: cfg}).sdcppVideoBinding(core.Request{Task: core.TaskGenerateVideo})
		name, helperOK := cfg.DefaultVideoSdcppFamily()
		if ok != helperOK {
			t.Errorf("%s: the pipeline serves sdcpp=%v but DefaultVideoSdcppFamily says %v", tc.name, ok, helperOK)
			continue
		}
		if ok && render != "" && render != name {
			t.Errorf("%s: the pipeline renders family %q but the helper names %q", tc.name, render, name)
		}
		if !ok && name != "" {
			t.Errorf("%s: the helper names %q for a request the pipeline does not serve by sdcpp", tc.name, name)
		}
	}
}
