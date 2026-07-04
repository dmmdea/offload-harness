package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func TestRunGenerateSVG_WritesFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.SVGDir = dir
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateSVG,
		Params: map[string]any{"kind": "gauge", "spec": map[string]any{"value": 60, "label": "Yield", "unit": "%"}},
	})
	if !res.OK {
		t.Fatalf("expected ok, got defer: %s", res.Reason)
	}
	var out struct {
		SVGPath string `json:"svg_path"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out.SVGPath)
	if err != nil || !strings.HasPrefix(string(b), "<svg") {
		t.Fatalf("svg file not written/invalid: %v", err)
	}
	if out.Width != 240 {
		t.Fatalf("gauge width 240 expected, got %d", out.Width)
	}
}

func TestRunGenerateSVG_BadKindDefers(t *testing.T) {
	cfg := config.Default()
	cfg.SVGDir = t.TempDir()
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateSVG,
		Params: map[string]any{"kind": "nope", "spec": map[string]any{}},
	})
	if res.OK || !res.Deferred {
		t.Fatal("unknown kind must defer")
	}
}
