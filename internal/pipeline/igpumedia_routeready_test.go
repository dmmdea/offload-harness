package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// CT-54: a bound media route whose weight is missing defers TYPED (err_class route_not_ready), before the lease, on the
// local path. A fleet node answers the same condition at its door (routeNotReadyError, 503); the local lanes took the lease,
// spawned the runner, and deferred err_class "other" ("--model not found") 31 ms later. These tests drive the five engine
// lanes (sdcpp video, sdcpp animate, audio.cpp voice and music, the sdcpp image lane) through Pipeline.Run with a config
// whose every file is present, then delete one weight: the "weight went missing under a running node" case.

// weightFile writes a small file under dir/models and returns its absolute path.
func weightFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, "models", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// leaseTraces is what a lease taken in dir leaves behind (the isolated lock path and state dir isolateLease names).
func leaseTraces(dir string) []string {
	var out []string
	if m, _ := filepath.Glob(filepath.Join(dir, "gpu.lock*")); len(m) > 0 {
		out = append(out, m...)
	}
	if fi, err := os.Stat(filepath.Join(dir, "state")); err == nil && fi.IsDir() {
		out = append(out, filepath.Join(dir, "state"))
	}
	return out
}

// routeLane is one engine lane: its config with every file present, the weight to delete and the key that weight is bound under,
// the request, and the label the lane's defers carry.
type routeLane struct {
	name string
	cfg  func(t *testing.T, dir string) (cfg config.Config, weight string)
	key  string // the config key the reason must name
	req  func(dir string) core.Request
	what string // the lane's defer prefix
	// makesOutDir: the lane creates the directory its output lands in before it takes the lease (the iGPU lanes do, ensureOutDir);
	// the sdcpp image lane leaves that to its runner.
	makesOutDir bool
}

func routeLanes() []routeLane {
	return []routeLane{
		{
			name: "generate_video (sdcpp)", key: "sdcpp_model", what: "video generation refused:", makesOutDir: true,
			cfg: func(t *testing.T, dir string) (config.Config, string) {
				cfg := sdcppVideoCfg(t, dir)
				fb := cfg.VideoGenFamilies["fastwan"]
				fb.SdcppModel = weightFile(t, dir, "wan-ti2v-5b-q8_0.gguf")
				fb.SdcppHighNoiseModel = weightFile(t, dir, "high.gguf")
				fb.SdcppVAE = weightFile(t, dir, "wan2.2_vae.safetensors")
				fb.SdcppT5xxl = weightFile(t, dir, "umt5.gguf")
				cfg.VideoGenFamilies["fastwan"] = fb
				cfg.FFmpegPath = ""
				return cfg, fb.SdcppModel
			},
			req: func(dir string) core.Request { return videoReq(dir, nil) },
		},
		{
			name: "animate_character (sdcpp)", key: "animategen_sdcpp_model", what: "character animation refused:", makesOutDir: true,
			cfg: func(t *testing.T, dir string) (config.Config, string) {
				cfg := animateCfg(t, dir)
				cfg.AnimateGenSdcppModel = weightFile(t, dir, "wan2.1_vace_1.3B_fp16.safetensors")
				cfg.AnimateGenSdcppVAE = weightFile(t, dir, "wan_2.1_vae.safetensors")
				cfg.AnimateGenSdcppT5xxl = weightFile(t, dir, "umt5.gguf")
				cfg.AnimateGenDepthModel = weightFile(t, dir, "depth.gguf")
				cfg.FFmpegPath = ""
				return cfg, cfg.AnimateGenSdcppModel
			},
			req: func(dir string) core.Request { return animateReq(dir, nil) },
		},
		{
			name: "generate_audio voice (audiocpp)", key: "audiocpp_voice_model", what: "audio generation refused:", makesOutDir: true,
			cfg: func(t *testing.T, dir string) (config.Config, string) {
				cfg := audiocppCfg(t, dir)
				cfg.AudiocppVoiceModel = weightFile(t, dir, "chatterbox-q8_0.gguf")
				cfg.AudiocppMusicModel = weightFile(t, dir, "ace-step-1.5-turbo-bf16.gguf")
				cfg.FFmpegPath = ""
				return cfg, cfg.AudiocppVoiceModel
			},
			req: func(dir string) core.Request { return audioReq(dir, "voice", "hola mundo", nil) },
		},
		{
			name: "generate_audio music (audiocpp)", key: "audiocpp_music_model", what: "audio generation refused:", makesOutDir: true,
			cfg: func(t *testing.T, dir string) (config.Config, string) {
				cfg := audiocppCfg(t, dir)
				cfg.AudiocppVoiceModel = weightFile(t, dir, "chatterbox-q8_0.gguf")
				cfg.AudiocppMusicModel = weightFile(t, dir, "ace-step-1.5-turbo-bf16.gguf")
				cfg.FFmpegPath = ""
				return cfg, cfg.AudiocppMusicModel
			},
			req: func(dir string) core.Request { return audioReq(dir, "music", "a calm piano", nil) },
		},
		{
			name: "generate_image (sdcpp)", key: "sdcpp_model", what: "image generation refused:",
			cfg: func(t *testing.T, dir string) (config.Config, string) {
				cfg := config.Default()
				cfg.MediaDir = dir
				cfg.ImageGenEngine = "sdcpp"
				cfg.SdcppBin = igpuBin(t, dir, "sd-cli")
				cfg.SdcppModel = weightFile(t, dir, "z_image_turbo-Q8_0.gguf")
				cfg.SdcppScript = writeStub(t, dir) // writes its first argument, the out path
				isolateLease(&cfg, dir)
				return cfg, cfg.SdcppModel
			},
			req: func(dir string) core.Request {
				return core.Request{Task: core.TaskGenerateImage, Input: "a gray sphere", Params: map[string]any{"out": filepath.Join(dir, "i.png"), "seed": 7}}
			},
		},
	}
}

// TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirWeightIsMissing: the weight is deleted, the lane defers route_not_ready,
// the reason names the config key and the path, no lease is taken, no output directory is created and the runner never runs.
// (Today the lane takes the lease, spawns the runner and the failure is classed "other".)
func TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirWeightIsMissing(t *testing.T) {
	requireNodePipeline(t)
	for _, lane := range routeLanes() {
		t.Run(lane.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, weight := lane.cfg(t, dir)
			ledgerPath := filepath.Join(dir, "ledger.jsonl")
			led, err := ledger.Open(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer led.Close()
			p := &Pipeline{cfg: cfg, led: led}
			req := lane.req(dir)
			// a fresh output dir the lane would have to create, so "nothing was created" is observable
			out := filepath.Join(dir, "fresh", "out.bin")
			req.Params["out"] = out
			if !lane.makesOutDir {
				if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(weight); err != nil {
				t.Fatal(err)
			}

			res := p.Run(context.Background(), req)

			mustDeferWith(t, res, "route_not_ready", lane.what, lane.key, weight, "BOUND-BUT-MISSING")
			if tr := leaseTraces(dir); len(tr) > 0 {
				t.Errorf("the lane took the media lease before it refused: %v", tr)
			}
			if lane.makesOutDir {
				if _, err := os.Stat(filepath.Dir(out)); err == nil {
					t.Errorf("the lane created its output directory %s before it refused", filepath.Dir(out))
				}
			}
			if _, err := os.Stat(out); err == nil {
				t.Error("the runner ran: it wrote the output")
			}
			// the ledger row carries the class, and the closed reason set files it as a config fault of the node
			rows, err := ledger.ReadAll(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ErrClass != "route_not_ready" || rows[0].ReasonCode != ledger.ReasonConfig {
				t.Errorf("ledger rows = %+v, want one deferred row with err_class route_not_ready and reason code %q", rows, ledger.ReasonConfig)
			}
		})
	}
}

// TestAReasonNamesTheMissingKeyAndPathAndSaysAnotherNodeMayServeIt: the sentence is the one an operator reads.
func TestAReasonNamesTheMissingKeyAndPathAndSaysAnotherNodeMayServeIt(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	lane := routeLanes()[0]
	cfg, weight := lane.cfg(t, dir)
	if err := os.Remove(weight); err != nil {
		t.Fatal(err)
	}
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), lane.req(dir))
	mustDeferWith(t, res, "route_not_ready", "route generate_video", "sdcpp_model="+weight, "another node")
}

// TestAWeightThatIsThereStillRuns: with every file present the lane reaches its runner exactly as before (the guard).
func TestAWeightThatIsThereStillRuns(t *testing.T) {
	requireNodePipeline(t)
	for _, lane := range routeLanes() {
		t.Run(lane.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, _ := lane.cfg(t, dir)
			req := lane.req(dir)
			out := filepath.Join(dir, "out", "made.bin")
			req.Params["out"] = out
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				t.Fatal(err)
			}
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), req)
			if res.Deferred || !res.OK {
				t.Fatalf("a route with every file present must run, got defer %q (class %q)", res.Reason, res.Meta.ErrClass)
			}
			if len(leaseTraces(dir)) == 0 {
				t.Error("the lease probe is blind: a lane that ran left no trace, so \"no lease\" proves nothing")
			}
		})
	}
}

// TestAnUnboundRouteKeepsItsOldDefer: a lane whose route is NOT CONFIGURED (nothing bound) keeps the lane's own
// "is not configured" defer and its (absent) class; CT-54 speaks only for a bound route with a missing file.
func TestAnUnboundRouteKeepsItsOldDefer(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg, _ := routeLanes()[0].cfg(t, dir)
	fb := cfg.VideoGenFamilies["fastwan"]
	fb.SdcppVAE = "" // a required key left unset: the lane's own message, not route_not_ready
	cfg.VideoGenFamilies["fastwan"] = fb
	res := (&Pipeline{cfg: cfg}).Run(context.Background(), videoReq(dir, nil))
	if res.OK || !res.Deferred {
		t.Fatalf("want a defer, got ok=%v", res.OK)
	}
	if res.Meta.ErrClass != "" {
		t.Errorf("ErrClass = %q, want none (the old defer is untyped)", res.Meta.ErrClass)
	}
	if !strings.Contains(res.Reason, "sdcpp_vae is not configured") {
		t.Errorf("reason %q must keep the lane's own sentence", res.Reason)
	}
}

// TestTheLaneAndTheNodeDoorReadTheSameVerdict: the lane refuses exactly when mediacap derives the route as BOUND-BUT-MISSING,
// the verdict the fleet node's door (routeNotReadyError) and /fleet/health media_routes publish.
func TestTheLaneAndTheNodeDoorReadTheSameVerdict(t *testing.T) {
	requireNodePipeline(t)
	for _, lane := range routeLanes() {
		t.Run(lane.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, weight := lane.cfg(t, dir)
			// ask for the lane's own route by name: a bare box reports unrelated verdicts beside it
			want := map[string]string{
				"generate_video (sdcpp)": "generate_video", "animate_character (sdcpp)": "animate_character",
				"generate_audio voice (audiocpp)": "generate_audio:voice", "generate_audio music (audiocpp)": "generate_audio:music",
				"generate_image (sdcpp)": "generate_image",
			}[lane.name]
			find := func() mediacap.State {
				for _, r := range mediacap.Routes(cfg) {
					if r.Name == want {
						return r.State
					}
				}
				return ""
			}
			if got := find(); got != mediacap.Configured {
				t.Fatalf("the premise is gone: route %s reads %s with every file present", want, got)
			}
			if err := os.Remove(weight); err != nil {
				t.Fatal(err)
			}
			if got := find(); got != mediacap.BoundButMissing {
				t.Fatalf("route %s reads %s with its weight deleted, want BOUND-BUT-MISSING", want, got)
			}
			res := (&Pipeline{cfg: cfg}).Run(context.Background(), lane.req(dir))
			if res.Meta.ErrClass != "route_not_ready" {
				t.Errorf("ErrClass = %q, want route_not_ready for a route mediacap reads BOUND-BUT-MISSING", res.Meta.ErrClass)
			}
		})
	}
}

// TestAVideoFamilyThatIsNotTheDefaultIsJudgedOnItsOwnRoute: mediacap derives the default sdcpp family as "generate_video" and every
// other one as "generate_video:<family>". A request that names the second must be judged on ITS files, not the default's.
func TestAVideoFamilyThatIsNotTheDefaultIsJudgedOnItsOwnRoute(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg, _ := routeLanes()[0].cfg(t, dir)
	alt := cfg.VideoGenFamilies["fastwan"]
	alt.SdcppModel = weightFile(t, dir, "alt-model.gguf")
	cfg.VideoGenFamilies["altwan"] = alt

	req := videoReq(dir, map[string]any{"model": "altwan"})
	p := &Pipeline{cfg: cfg}
	if res := p.Run(context.Background(), req); !res.OK {
		t.Fatalf("a non-default family with every file present must run, got defer %q (class %q)", res.Reason, res.Meta.ErrClass)
	}

	// the ALTERNATIVE family's weight goes missing: the default family still runs, the alternative defers on its own route
	if err := os.Remove(alt.SdcppModel); err != nil {
		t.Fatal(err)
	}
	res := p.Run(context.Background(), videoReq(dir, map[string]any{"model": "altwan"}))
	mustDeferWith(t, res, "route_not_ready", "route generate_video:altwan", "sdcpp_model="+alt.SdcppModel)
	if res := p.Run(context.Background(), videoReq(dir, nil)); !res.OK {
		t.Fatalf("the default family has every file and must still run, got defer %q (class %q)", res.Reason, res.Meta.ErrClass)
	}
}
