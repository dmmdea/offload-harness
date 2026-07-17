package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestRunGraphParamsThreaded(t *testing.T) {
	// buildRunGraphParams maps the request params to rungraph.Params; a missing graph
	// path is a hard error (defer), not a silent empty run.
	_, err := buildRunGraphParams(core.Request{Task: core.TaskRunGraph, Params: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "graph") {
		t.Fatalf("expected a missing-graph error, got %v", err)
	}
	p, err := buildRunGraphParams(core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"graph_path": "g.json", "manifest_path": "m.json", "out_dir": "o",
	}})
	if err != nil || p.GraphPath != "g.json" || p.ManifestPath != "m.json" {
		t.Fatalf("params: %+v err %v", p, err)
	}
}
