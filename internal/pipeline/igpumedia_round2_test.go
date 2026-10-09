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
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// lanesWithOut builds the four iGPU lanes (video, animate, voice, music) against the argv-recording
// stub, with no extra args bound anywhere, and the request each one takes.
type igpuLane struct {
	name string
	cfg  config.Config
	req  core.Request
	// what the lane's footprint key must be
	family, quant, task string
	ledgerTask          core.TaskType
}

func igpuLanes(t *testing.T, dir string) []igpuLane {
	t.Helper()
	cfgV := sdcppVideoCfg(t, dir)
	fb := cfgV.VideoGenFamilies["fastwan"]
	fb.SdcppExtraArgs = nil
	cfgV.VideoGenFamilies["fastwan"] = fb
	cfgA := animateCfg(t, dir)
	cfgU := audiocppCfg(t, dir)
	cfgU.AudiocppExtraArgs = nil
	return []igpuLane{
		{"video", cfgV, videoReq(dir, nil), "fastwan", "q8_0", "video-gen", core.TaskGenerateVideo},
		{"animate", cfgA, animateReq(dir, nil), "wan-vace", quantFromModelFile(cfgA.AnimateGenSdcppModel), "animate", core.TaskAnimateCharacter},
		{"voice", cfgU, audioReq(dir, "voice", "hola", nil), "chatterbox", "q8_0", "audio-gen", core.TaskGenerateAudio},
		{"music", cfgU, audioReq(dir, "music", "lofi", nil), "ace_step", "bf16", "audio-gen", core.TaskGenerateAudio},
	}
}

// TST13: a lane with nothing bound sends NO --extra-args (the empty-list guard in extraArgsFlag);
// `--extra-args null` is rejected by every runner after the lease is taken.
func TestIGPULanesSendNoExtraArgsFlagWhenNothingIsBound(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	for _, l := range igpuLanes(t, dir) {
		res := (&Pipeline{cfg: l.cfg}).Run(context.Background(), l.req)
		out, _, raw := decodeAny(t, res)
		args := readArgs(t, out)
		for _, a := range args {
			if a == "--extra-args" || a == "--depth-extra-args" {
				t.Errorf("%s: sent %s with nothing bound (argv %v)", l.name, a, args)
			}
		}
		_ = raw
	}
}

// decodeAny returns the produced file path of any lane's success result.
func decodeAny(t *testing.T, res core.Result) (path string, seed int, raw map[string]any) {
	t.Helper()
	if !res.OK {
		t.Fatalf("expected ok via stub, got defer: %s", res.Reason)
	}
	raw = map[string]any{}
	if err := json.Unmarshal(res.Data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"video_path", "audio_path"} {
		if p, ok := raw[k].(string); ok {
			path = p
		}
	}
	s, _ := raw["seed"].(float64)
	return path, int(s), raw
}

// TST14: sizes the request and the binding leave unset are checked against the cap at the RUNNER's
// defaults (832x480x49), before the lease; without orDefault the lane would compute 0 tokens, pass,
// and the refusal would move into the runner after the lease and the node start-up.
func TestTheTokenCapIsCheckedAtTheRunnersDefaultsWhenSizesAreUnset(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()

	cfgV := sdcppVideoCfg(t, dir)
	fb := cfgV.VideoGenFamilies["fastwan"]
	fb.Width, fb.Height, fb.Frames = 0, 0, 0
	fb.SdcppMaxTokens, fb.SdcppVAEStride = 3000, 16 // 832x480x49 on a 16x VAE is 5070 tokens
	cfgV.VideoGenFamilies["fastwan"] = fb
	res := (&Pipeline{cfg: cfgV}).Run(context.Background(), videoReq(dir, nil))
	mustDeferWith(t, res, "token_cap_exceeded", "832x480x49", "5070")

	cfgA := animateCfg(t, dir)
	cfgA.AnimateGenWidth, cfgA.AnimateGenHeight = 0, 0
	cfgA.AnimateGenSdcppMaxTokens, cfgA.AnimateGenSdcppVAEStride = 3000, 16 // + the reference latent frame: 5460
	res = (&Pipeline{cfg: cfgA}).Run(context.Background(), animateReq(dir, nil))
	mustDeferWith(t, res, "token_cap_exceeded", "832x480x49", "5460", "reference")
	for _, f := range []string{"v.mp4", "a.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("the runner ran although the cap was exceeded (%s exists)", f)
		}
	}
}

// CODE3: an in-process Config (never through config.Load) with a cap and no usable stride is
// refused before the lease with the typed class, not after it as a plain exit 1.
func TestACapWithoutAUsableStrideIsRefusedBeforeTheLease(t *testing.T) {
	requireNodePipeline(t)
	for _, stride := range []int{0, 4, 12} {
		dir := t.TempDir()
		cfgV := sdcppVideoCfg(t, dir)
		fb := cfgV.VideoGenFamilies["fastwan"]
		fb.SdcppMaxTokens, fb.SdcppVAEStride = 3000, stride
		cfgV.VideoGenFamilies["fastwan"] = fb
		mustDeferWith(t, (&Pipeline{cfg: cfgV}).Run(context.Background(), videoReq(dir, nil)), "token_cap_exceeded", "stride")

		cfgA := animateCfg(t, dir)
		cfgA.AnimateGenSdcppMaxTokens, cfgA.AnimateGenSdcppVAEStride = 3000, stride
		mustDeferWith(t, (&Pipeline{cfg: cfgA}).Run(context.Background(), animateReq(dir, nil)), "token_cap_exceeded", "stride")
		for _, f := range []string{"v.mp4", "a.mp4"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				t.Errorf("stride %d: the runner ran (%s exists)", stride, f)
			}
		}
	}
}

// TST16: every key a lane cannot run without is pinned on its own, so the defer comes before the lease.
func TestEachUnboundKeyDefersOnItsOwnBeforeTheLease(t *testing.T) {
	requireNodePipeline(t)
	type mut func(c *config.Config)
	videoKeys := map[string]func(fb *config.VideoFamilyBinding){
		"sdcpp_bin":   func(fb *config.VideoFamilyBinding) { fb.SdcppBin = "" },
		"sdcpp_model": func(fb *config.VideoFamilyBinding) { fb.SdcppModel = "" },
		"sdcpp_vae":   func(fb *config.VideoFamilyBinding) { fb.SdcppVAE = "" },
		"sdcpp_t5xxl": func(fb *config.VideoFamilyBinding) { fb.SdcppT5xxl = "" },
	}
	for key, clear := range videoKeys {
		dir := t.TempDir()
		cfg := sdcppVideoCfg(t, dir)
		fb := cfg.VideoGenFamilies["fastwan"]
		clear(&fb)
		cfg.VideoGenFamilies["fastwan"] = fb
		res := (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, nil))
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, key) || !strings.Contains(res.Reason, "not configured") {
			t.Errorf("video, %s unbound: want a defer naming the key, got ok=%v %q", key, res.OK, res.Reason)
		}
		if _, err := os.Stat(filepath.Join(dir, "v.mp4")); err == nil {
			t.Errorf("video, %s unbound: the runner ran", key)
		}
	}
	animateKeys := map[string]mut{
		"animategen_sdcpp_bin":   func(c *config.Config) { c.AnimateGenSdcppBin = "" },
		"animategen_sdcpp_model": func(c *config.Config) { c.AnimateGenSdcppModel = "" },
		"animategen_sdcpp_vae":   func(c *config.Config) { c.AnimateGenSdcppVAE = "" },
		"animategen_sdcpp_t5xxl": func(c *config.Config) { c.AnimateGenSdcppT5xxl = "" },
		"animategen_depth_bin":   func(c *config.Config) { c.AnimateGenDepthBin = "" },
		"animategen_depth_model": func(c *config.Config) { c.AnimateGenDepthModel = "" },
	}
	for key, clear := range animateKeys {
		dir := t.TempDir()
		cfg := animateCfg(t, dir)
		clear(&cfg)
		res := (&Pipeline{cfg: cfg}).Run(context.Background(), animateReq(dir, nil))
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, key) || !strings.Contains(res.Reason, "not configured") {
			t.Errorf("animate, %s unbound: want a defer naming the key, got ok=%v %q", key, res.OK, res.Reason)
		}
	}
	audioKeys := map[string]mut{
		"audiocpp_bin":         func(c *config.Config) { c.AudiocppBin = "" },
		"audiocpp_music_model": func(c *config.Config) { c.AudiocppMusicModel = "" },
	}
	for key, clear := range audioKeys {
		dir := t.TempDir()
		cfg := audiocppCfg(t, dir)
		clear(&cfg)
		res := (&Pipeline{cfg: cfg}).Run(context.Background(), audioReq(dir, "music", "lofi", nil))
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, key) || !strings.Contains(res.Reason, "not configured") {
			t.Errorf("music, %s unbound: want a defer naming the key, got ok=%v %q", key, res.OK, res.Reason)
		}
	}
}

// TST16: the longest quant token wins (BF16 is not F16, Q4_K_M is not Q4_K, Q5_K_M is not Q5_K).
func TestQuantFromModelFileReadsTheLongestTokenFirst(t *testing.T) {
	for file, want := range map[string]string{
		"/m/model-BF16.gguf": "bf16", "/m/model-f16.gguf": "f16", "/m/m-Q4_K_M.gguf": "q4_k_m", "/m/m-Q4_K_S.gguf": "q4_k_s", "/m/m-Q4_K.gguf": "q4_k",
		"/m/m-Q5_K_M.gguf": "q5_k_m", "/m/m-Q5_K_S.gguf": "q5_k_s", "/m/m-Q5_K.gguf": "q5_k", "/m/m-Q8_0.gguf": "q8_0", "/m/m-Q4_1.gguf": "q4_1",
		"/m/m-Q4_0.gguf": "q4_0", "/m/wan2.1_vace_1.3B_fp16.safetensors": "", "/m/plain.gguf": "",
	} {
		if got := quantFromModelFile(file); got != want {
			t.Errorf("quantFromModelFile(%q) = %q, want %q", file, got, want)
		}
	}
}

// SIL6 + TST15: the Spec every iGPU lane hands gpugen. OwnProcessGroup is what makes a cancel or a
// timeout reach the engine on POSIX (without it only node is killed and the engine keeps the iGPU);
// the footprint key feeds the fleet store; a success is one ledger row. Captured through the
// generateIGPU seam: no runner needs to run.
func TestEveryIGPULaneHandsGPUGenAnOwnProcessGroupSpecAndRecordsItsRun(t *testing.T) {
	requireNodePipeline(t)
	var got []gpugen.Spec
	old := generateIGPU
	generateIGPU = func(ctx context.Context, spec gpugen.Spec) (string, error) {
		got = append(got, spec)
		if err := os.WriteFile(spec.Out, []byte("out"), 0o644); err != nil {
			return "", err
		}
		if spec.OnFootprint != nil {
			spec.OnFootprint(4.3) // gpugen fires it on a successful sampled run
		}
		return spec.Out, nil
	}
	t.Cleanup(func() { generateIGPU = old })

	dir := t.TempDir()
	for _, l := range igpuLanes(t, dir) {
		got = nil
		l.cfg.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
		led, err := ledger.Open(l.cfg.LedgerPath)
		if err != nil {
			t.Fatal(err)
		}
		p := &Pipeline{cfg: l.cfg, led: led}
		p.fleetSample = func(int) (float64, error) { return 4.3, nil }
		res := p.Run(context.Background(), l.req)
		_ = led.Close()
		if !res.OK {
			t.Fatalf("%s: %s", l.name, res.Reason)
		}
		if len(got) != 1 {
			t.Fatalf("%s: gpugen.Generate called %d times, want 1", l.name, len(got))
		}
		spec := got[0]
		if !spec.OwnProcessGroup {
			t.Errorf("%s: Spec.OwnProcessGroup is false: a cancel would kill node alone and the engine would keep the iGPU", l.name)
		}
		if !spec.SkipFreeComfy {
			t.Errorf("%s: an iGPU lane has no ComfyUI to free", l.name)
		}
		if spec.Exe != l.cfg.NodePath {
			t.Errorf("%s: Exe = %q, want the configured node %q", l.name, spec.Exe, l.cfg.NodePath)
		}
		if spec.Footprint == nil || spec.Footprint.Family != l.family || spec.Footprint.Quant != l.quant || spec.Footprint.Task != l.task {
			t.Errorf("%s: footprint key = %+v, want {%s %s %s}", l.name, spec.Footprint, l.family, l.quant, l.task)
		}
		if e := findEntry(t, p.FootprintStore().Entries(), l.family, l.task); e.VramPeakGiB != 4.3 || e.Quant != l.quant {
			t.Errorf("%s: footprint entry %+v, want peak 4.3 quant %q", l.name, e, l.quant)
		}
		rows, err := ledger.ReadAll(l.cfg.LedgerPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Task != string(l.ledgerTask) || rows[0].Deferred {
			t.Errorf("%s: want one non-deferred %s ledger row, got %+v", l.name, l.ledgerTask, rows)
		}
	}
}
