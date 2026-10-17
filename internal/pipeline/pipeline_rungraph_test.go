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

func TestRunGraphParamsForATemplate(t *testing.T) {
	p, err := buildRunGraphParams(core.Request{Task: core.TaskRunGraph, Params: map[string]any{
		"template": "tt_open", "manifest_path": "m.json", "overrides_path": "o.json", "seed": "7", "out_dir": "o",
	}})
	if err != nil || p.Template != "tt_open" || p.GraphPath != "" || p.ManifestPath != "m.json" || p.OverridesPath != "o.json" || p.Seed != "7" {
		t.Fatalf("params: %+v err %v", p, err)
	}
	// A graph and a template are two ways to say what to run; both is a contradiction, not a precedence rule.
	if _, err := buildRunGraphParams(core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": "g.json", "template": "tt_open"}}); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("graph_path and template together: %v", err)
	}
	// The template name is a name, never a path or a flag.
	for _, bad := range []string{"../x", "a/b", "-x", "a b", "a\b"} {
		if _, err := buildRunGraphParams(core.Request{Task: core.TaskRunGraph, Params: map[string]any{"template": bad}}); err == nil || !strings.Contains(err.Error(), "template") {
			t.Errorf("template %q: %v", bad, err)
		}
	}
}

func TestRunGraphFootprintFamilyOfATemplateIsTheTemplate(t *testing.T) {
	if got := runGraphFootprintFamily(map[string]any{"template": "tt_open"}); got != "comfy-template-tt_open" {
		t.Errorf("family = %q", got)
	}
	if got := runGraphFootprintFamily(map[string]any{"template": "tt_open", "model_family": "wan22"}); got != "wan22" {
		t.Errorf("a declared family wins: %q", got)
	}
	if got := runGraphFootprintFamily(map[string]any{}); got != "comfy-graph" {
		t.Errorf("a graph run is what it was: %q", got)
	}
}
