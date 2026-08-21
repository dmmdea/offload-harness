package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func newUpscalePipeline(cfg config.Config) *Pipeline {
	return New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
}

func TestUpscaleDeferReasons(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.png")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(p *Pipeline, params map[string]any) core.Result {
		t.Helper()
		return p.Run(context.Background(), core.Request{Task: core.TaskUpscaleImage, Params: params})
	}

	// default config ships the script but no model → unconfigured, naming both keys
	cfg := config.Default()
	res := run(newUpscalePipeline(cfg), map[string]any{"image": img})
	if res.OK || !strings.HasPrefix(res.Reason, "no upscale route configured") || !strings.Contains(res.Reason, "videogen_upscale_model") {
		t.Fatalf("want unconfigured defer naming the fallback key, got %+v", res)
	}

	// the video route's upscaler binds stills too (EffectiveUpscaleModel fallback):
	// with it set the gate passes and the next check (missing image) is what defers
	cfg = config.Default()
	cfg.VideoGenUpscaleModel = "4x-UltraSharp.pth"
	p := newUpscalePipeline(cfg)
	res = run(p, nil)
	if res.OK || res.Reason != "upscale requires params.image" {
		t.Fatalf("want missing-image defer past the fallback-bound gate, got %+v", res)
	}
	res = run(p, map[string]any{"image": filepath.Join(dir, "missing.png")})
	if res.OK || !strings.HasPrefix(res.Reason, "upscale input not found") {
		t.Fatalf("want input-not-found defer, got %+v", res)
	}

	// a half-given size defers BEFORE any script resolution or GPU work
	cfg = config.Default()
	cfg.UpscaleModel = "4x-UltraSharp.pth"
	cfg.UpscaleScript = filepath.Join(dir, "definitely-missing-runner.mjs")
	p = newUpscalePipeline(cfg)
	res = run(p, map[string]any{"image": img, "width": 3000})
	if res.OK || res.Reason != "upscale width and height must be given together" {
		t.Fatalf("want half-size defer, got %+v", res)
	}
	res = run(p, map[string]any{"image": img, "scale": -2.0})
	if res.OK || res.Reason != "upscale scale must be > 0" {
		t.Fatalf("want bad-scale defer, got %+v", res)
	}
	// ...and with valid params the missing runner is the (actionable) reason
	res = run(p, map[string]any{"image": img, "scale": 2.0})
	if res.OK || !strings.Contains(res.Reason, "script not found at") {
		t.Fatalf("want script-not-found defer, got %+v", res)
	}

	// a request model overrides an empty binding and passes the gate
	cfg = config.Default()
	cfg.UpscaleScript = filepath.Join(dir, "definitely-missing-runner.mjs")
	res = run(newUpscalePipeline(cfg), map[string]any{"image": img, "model": "RealESRGAN_x4plus.pth"})
	if res.OK || !strings.Contains(res.Reason, "script not found at") {
		t.Fatalf("want gate passed on request model (script-not-found), got %+v", res)
	}
}
