package pipeline

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

func TestVideoDescribeNoVisionModelDefers(t *testing.T) {
	cfg := config.Default()
	cfg.VisionModel = ""
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoDescribe, Video: "x.mp4", Params: map[string]any{"question": "q"}})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred, got OK=%v Deferred=%v reason=%q", res.OK, res.Deferred, res.Reason)
	}
}

func TestVideoDescribeBadVideoDefers(t *testing.T) {
	cfg := config.Default()
	p := New(cfg, llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 0), nil, nil)
	res := p.Run(context.Background(), core.Request{Task: core.TaskVideoDescribe, Video: "no-such-file.mp4", Params: map[string]any{"question": "q"}})
	if res.OK || !res.Deferred {
		t.Fatalf("want deferred on bad video, got OK=%v Deferred=%v", res.OK, res.Deferred)
	}
}
