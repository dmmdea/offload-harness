package pipeline

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func TestPlanVideoWatchWindowsCoversWholeDuration(t *testing.T) {
	ws, err := planVideoWatchWindows(30.0, 0, 0, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 4 || ws[0].Start != 0 || ws[3].End != 30 {
		t.Fatalf("want 4 windows 0..30, got %+v", ws)
	}
	for i := 1; i < len(ws); i++ {
		if ws[i].Start != ws[i-1].End {
			t.Fatalf("gap between windows: %+v", ws)
		}
	}
}

func TestPlanVideoWatchWindowsFoldsSliverAndHonoursRange(t *testing.T) {
	ws, err := planVideoWatchWindows(16.2, 4, 0, 6, 0) // 4-10, 10-16, 16-16.2 (sliver folds)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[1].End != 16.2 {
		t.Fatalf("sliver not folded: %+v", ws)
	}
	ws, err = planVideoWatchWindows(100, 20, 36, 8, 0)
	if err != nil || len(ws) != 2 || ws[0].Start != 20 || ws[1].End != 36 {
		t.Fatalf("range not honoured: %+v err=%v", ws, err)
	}
}

func TestPlanVideoWatchWindowsRejectsBadInput(t *testing.T) {
	if _, err := planVideoWatchWindows(0, 0, 0, 8, 0); err == nil {
		t.Fatal("zero duration must error")
	}
	if _, err := planVideoWatchWindows(30, 20, 10, 8, 0); err == nil {
		t.Fatal("start after end must error")
	}
	if _, err := planVideoWatchWindows(3600, 0, 0, 1, 10); err == nil {
		t.Fatal("window cap must error loudly, not truncate silently")
	}
}

func TestVideoWatchNoVisionModelDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = ""
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoWatch, Video: "x.mp4", Params: map[string]any{"question": "q"}})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred, got OK=%v Deferred=%v reason=%q", res.OK, res.Deferred, res.Reason)
	}
}

func TestVideoWatchMissingQuestionDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = "vlm"
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoWatch, Video: "x.mp4"})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred on missing question, got OK=%v", res.OK)
	}
}

func TestVideoWatchBadVideoDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = "vlm"
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoWatch, Video: "no-such-file.mp4", Params: map[string]any{"question": "q"}})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred on bad video, got OK=%v Deferred=%v", res.OK, res.Deferred)
	}
}
