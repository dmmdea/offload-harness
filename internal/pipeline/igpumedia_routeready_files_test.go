package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// CT-54, the rest of the route: a bound route stops being runnable by more than a deleted weight. The engine binary can move and
// the runner script can go, and mediacap derives both as BOUND-BUT-MISSING exactly as it derives a missing weight (the node door
// answers 503 for either). The lanes resolved the script and the binary BEFORE they asked for the verdict, so those two cases
// still deferred with the lane's own untyped sentence on the local path while the door answered the typed one. The gate now sits
// right after the lane's own unset-key checks and before the script and binary are resolved.

// routeBreak is one more way a bound route stops being runnable: apply(t, dir, &cfg) breaks it in cfg (or on disk) and returns
// the path that is now missing.
type routeBreak struct {
	name  string
	key   string // the config key mediacap, and so the reason, names
	apply func(t *testing.T, dir string, cfg *config.Config) string
}

func removeFile(t *testing.T, p string) string {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// routeBreaks lists, per lane (routeLanes' name), the binaries and the runner script the lane resolves itself.
func routeBreaks() map[string][]routeBreak {
	gone := func(dir string) string { return filepath.Join(dir, "gone-runner.mjs") }
	video := []routeBreak{
		{"the engine binary", "sdcpp_bin", func(t *testing.T, dir string, cfg *config.Config) string {
			return removeFile(t, cfg.VideoGenFamilies["fastwan"].SdcppBin)
		}},
		{"the runner script", "videogen_sdcpp_script", func(t *testing.T, dir string, cfg *config.Config) string {
			cfg.VideoGenSdcppScript = gone(dir)
			return cfg.VideoGenSdcppScript
		}},
	}
	animate := []routeBreak{
		{"the engine binary", "animategen_sdcpp_bin", func(t *testing.T, dir string, cfg *config.Config) string {
			return removeFile(t, cfg.AnimateGenSdcppBin)
		}},
		{"the depth binary", "animategen_depth_bin", func(t *testing.T, dir string, cfg *config.Config) string {
			return removeFile(t, cfg.AnimateGenDepthBin)
		}},
		{"the runner script", "animategen_sdcpp_script", func(t *testing.T, dir string, cfg *config.Config) string {
			cfg.AnimateGenSdcppScript = gone(dir)
			return cfg.AnimateGenSdcppScript
		}},
	}
	audio := []routeBreak{
		{"the engine binary", "audiocpp_bin", func(t *testing.T, dir string, cfg *config.Config) string {
			return removeFile(t, cfg.AudiocppBin)
		}},
		{"the runner script", "audiocpp_script", func(t *testing.T, dir string, cfg *config.Config) string {
			cfg.AudiocppScript = gone(dir)
			return cfg.AudiocppScript
		}},
	}
	image := []routeBreak{
		{"the engine binary", "sdcpp_bin", func(t *testing.T, dir string, cfg *config.Config) string {
			return removeFile(t, cfg.SdcppBin)
		}},
		{"the runner script", "sdcpp_script", func(t *testing.T, dir string, cfg *config.Config) string {
			cfg.SdcppScript = gone(dir)
			return cfg.SdcppScript
		}},
	}
	return map[string][]routeBreak{
		"generate_video (sdcpp)": video, "animate_character (sdcpp)": animate,
		"generate_audio voice (audiocpp)": audio, "generate_audio music (audiocpp)": audio,
		"generate_image (sdcpp)": image,
	}
}

// routeOf is the mediacap route a lane (by routeLanes' name) is derived under.
func routeOf(lane string) string {
	return map[string]string{
		"generate_video (sdcpp)": "generate_video", "animate_character (sdcpp)": "animate_character",
		"generate_audio voice (audiocpp)": "generate_audio:voice", "generate_audio music (audiocpp)": "generate_audio:music",
		"generate_image (sdcpp)": "generate_image",
	}[lane]
}

func routeState(cfg config.Config, name string) mediacap.State {
	for _, r := range mediacap.Routes(cfg) {
		if r.Name == name {
			return r.State
		}
	}
	return ""
}

// TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirBinaryOrScriptIsMissing: the engine binary moved, or the runner script gone: the
// route is BOUND-BUT-MISSING at the node door, and the local lane now says the same, typed, before the lease. (Before, the lane's
// own binary and script checks answered first, untyped.)
func TestEngineLanesDeferTypedBeforeTheLeaseWhenTheirBinaryOrScriptIsMissing(t *testing.T) {
	requireNodePipeline(t)
	breaks := routeBreaks()
	for _, lane := range routeLanes() {
		for _, brk := range breaks[lane.name] {
			t.Run(lane.name+"/"+brk.name, func(t *testing.T) {
				dir := t.TempDir()
				cfg, _ := lane.cfg(t, dir)
				want := routeOf(lane.name)
				if got := routeState(cfg, want); got != mediacap.Configured {
					t.Fatalf("the premise is gone: route %s reads %s with every file present", want, got)
				}
				missing := brk.apply(t, dir, &cfg)
				if got := routeState(cfg, want); got != mediacap.BoundButMissing {
					t.Fatalf("route %s reads %s with %s broken, want BOUND-BUT-MISSING (the node door's verdict)", want, got, brk.name)
				}
				ledgerPath := filepath.Join(dir, "ledger.jsonl")
				led, err := ledger.Open(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				defer led.Close()
				req := lane.req(dir)
				out := filepath.Join(dir, "fresh", "out.bin")
				req.Params["out"] = out
				if !lane.makesOutDir {
					if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
						t.Fatal(err)
					}
				}

				res := (&Pipeline{cfg: cfg, led: led}).Run(context.Background(), req)

				mustDeferWith(t, res, "route_not_ready", lane.what, brk.key, missing, "BOUND-BUT-MISSING")
				if tr := leaseTraces(dir); len(tr) > 0 {
					t.Errorf("the lane took the media lease before it refused: %v", tr)
				}
				if lane.makesOutDir {
					if _, err := os.Stat(filepath.Dir(out)); err == nil {
						t.Errorf("the lane created its output directory %s before it refused", filepath.Dir(out))
					}
				}
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
}
