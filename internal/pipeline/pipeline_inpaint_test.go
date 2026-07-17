package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func TestParseVisionBoxes(t *testing.T) {
	// bare JSON array
	b, err := parseVisionBoxes(`[{"x":1,"y":2,"width":3,"height":4}]`)
	if err != nil || len(b) != 1 || b[0].Width != 3 {
		t.Fatalf("bare array: %v %v", b, err)
	}
	// {"boxes":[...]} envelope
	b, err = parseVisionBoxes(`{"boxes":[{"x":0,"y":0,"width":10,"height":10},{"x":5,"y":5,"width":1,"height":1}]}`)
	if err != nil || len(b) != 2 {
		t.Fatalf("envelope: %v %v", b, err)
	}
	// JSON embedded in prose (models pad despite instructions)
	b, err = parseVisionBoxes("The text regions are:\n[{\"x\":7,\"y\":8,\"width\":9,\"height\":10}]\nDone.")
	if err != nil || len(b) != 1 || b[0].X != 7 {
		t.Fatalf("embedded: %v %v", b, err)
	}
	// garbage → error, never a silent empty
	if _, err = parseVisionBoxes("no text found"); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestValidateTextBoxes(t *testing.T) {
	// clip to bounds + round
	mb, err := validateTextBoxes([]visionBox{{X: -5, Y: 10, Width: 30, Height: 2000}}, 100, 100)
	if err != nil || len(mb) != 1 {
		t.Fatalf("clip: %v %v", mb, err)
	}
	if mb[0].X != 0 || mb[0].Y != 10 || mb[0].X+mb[0].Width > 100 || mb[0].Y+mb[0].Height > 100 {
		t.Fatalf("box not clipped to bounds: %+v", mb[0])
	}
	// degenerate boxes dropped; all-degenerate → error
	if _, err = validateTextBoxes([]visionBox{{X: 10, Y: 10, Width: 0, Height: 5}}, 100, 100); err == nil {
		t.Fatal("all-degenerate must error")
	}
	// empty → error
	if _, err = validateTextBoxes(nil, 100, 100); err == nil {
		t.Fatal("empty must error")
	}
	// absurd coverage (>60% of the image) → error (would repaint the whole image)
	if _, err = validateTextBoxes([]visionBox{{X: 0, Y: 0, Width: 90, Height: 90}}, 100, 100); err == nil {
		t.Fatal(">60%% coverage must error")
	}
}

func TestInpaintDeferReasons(t *testing.T) {
	// unconfigured route
	cfg := config.Default()
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean it",
		Params: map[string]any{"image": "a.png", "mask": "m.png"}})
	if res.OK || res.Reason != "no inpaint route configured" {
		t.Fatalf("want unconfigured defer, got %+v", res)
	}
	// configured route, no mask and no auto_text → distinct reason
	cfg.InpaintScript = "render/comfy-inpaint.mjs"
	cfg.InpaintCkpt = "sdxl.safetensors"
	p = New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res = p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean it",
		Params: map[string]any{"image": "a.png"}})
	if res.OK || res.Reason != "inpaint requires params.mask" {
		t.Fatalf("want missing-mask defer, got %+v", res)
	}
	// auto_text runs by default (grounding eval PASSED 3/3, 2026-07-17 — the
	// always-defer gate was removed per its own unlock condition). Here the chain
	// fails on the missing image → the localization-failure defer naming the
	// manual mask_boxes path.
	res = p.Run(context.Background(), core.Request{Task: core.TaskInpaintImage, Input: "clean it",
		Params: map[string]any{"image": "definitely-missing-image.png", "auto_text": true}})
	if res.OK || !strings.Contains(res.Reason, "auto text localization failed") ||
		!strings.Contains(res.Reason, "mask_boxes") {
		t.Fatalf("want auto-text defer naming mask_boxes fallback, got %+v", res)
	}
}
