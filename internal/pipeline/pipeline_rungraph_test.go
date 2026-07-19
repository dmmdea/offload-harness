package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestResolveOutDirCreatesCallerDir locks the fix for the ENOENT footgun: a
// caller-supplied out_dir that does not yet exist must be created before the
// render writes into it (previously only the defaulted media dir was created,
// so a caller out_dir failed at first output write with a RUN_ERROR).
func TestResolveOutDirCreatesCallerDir(t *testing.T) {
	tmp := t.TempDir()
	media := filepath.Join(tmp, "media")

	// Empty caller -> the media dir, created.
	got, err := resolveOutDir(media, "")
	if err != nil || got != media {
		t.Fatalf("resolveOutDir(media, \"\") = %q, %v; want %q, nil", got, err, media)
	}
	if _, e := os.Stat(media); e != nil {
		t.Errorf("media dir not created: %v", e)
	}

	// Caller-supplied, not-yet-existing, nested dir -> created (the bug).
	caller := filepath.Join(tmp, "caller", "nested", "out")
	got2, err := resolveOutDir(media, caller)
	if err != nil || got2 != caller {
		t.Fatalf("resolveOutDir(media, caller) = %q, %v; want %q, nil", got2, err, caller)
	}
	if _, e := os.Stat(caller); e != nil {
		t.Errorf("caller out_dir not created (the ENOENT footgun): %v", e)
	}
}

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
