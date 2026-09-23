package fleetnode

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func composeFleetCfg() config.Config {
	return config.Config{
		ComposeScript:          "render/compose-hyperframes.mjs",
		HyperframesDir:         "/opt/offload/hyperframes",
		HyperframesBrowserPath: "/opt/offload/hyperframes/chrome/chrome-headless-shell",
	}
}

// TestComposeVideoAdvertisedOnlyWhenFullyBound: health advertises compose-video exactly
// when the pipeline would run it — script, install and pinned browser all bound.
func TestComposeVideoAdvertisedOnlyWhenFullyBound(t *testing.T) {
	if got := SupportedTasks(composeFleetCfg()); !reflect.DeepEqual(got, []string{ComposeTask}) {
		t.Fatalf("SupportedTasks = %v, want [compose-video]", got)
	}
	for name, mut := range map[string]func(*config.Config){
		"no script":  func(c *config.Config) { c.ComposeScript = "" },
		"no install": func(c *config.Config) { c.HyperframesDir = "" },
		"no browser": func(c *config.Config) { c.HyperframesBrowserPath = "" },
	} {
		cfg := composeFleetCfg()
		mut(&cfg)
		if slices.Contains(SupportedTasks(cfg), ComposeTask) {
			t.Errorf("%s: compose-video advertised on a half-bound route", name)
		}
		if _, _, err := BuildRequest(context.Background(), cfg, true, ComposeTask, json.RawMessage(`{"template":"title-card"}`)); err == nil ||
			!strings.Contains(err.Error(), "unsupported task_type") {
			t.Errorf("%s: dispatch must 400 like any unbound task, got %v", name, err)
		}
	}
	if f := Families(composeFleetCfg()); f != nil {
		t.Errorf("a composition loads no model family, Families = %v", f)
	}
}

// TestBuildRequestComposeVideo: the template form translates to the same params the MCP
// door sends; free-form html / project_dir and directory outputs are refused at ack time.
func TestBuildRequestComposeVideo(t *testing.T) {
	req, cleanup, err := BuildRequest(context.Background(), composeFleetCfg(), true, ComposeTask, json.RawMessage(
		`{"template":"lower-third","variables":{"name":"A","duration":5},"format":"webm","fps":30,"quality":"high","workers":2,"strict":false,"snapshots":[1,2.5],"out":"/x/lt.webm"}`))
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	defer cleanup()
	if req.Task != core.TaskComposeVideo {
		t.Fatalf("task = %q", req.Task)
	}
	want := map[string]any{"template": "lower-third", "variables": map[string]any{"name": "A", "duration": 5.0}, "format": "webm",
		"fps": 30.0, "quality": "high", "workers": 2.0, "strict": false, "snapshots": []float64{1, 2.5}}
	if !reflect.DeepEqual(req.Params, want) {
		t.Fatalf("params = %#v\nwant %#v", req.Params, want)
	}
	for name, payload := range map[string]string{
		"html":          `{"html":"<html></html>"}`,
		"html+template": `{"template":"title-card","html":"<html></html>"}`,
		"project_dir":   `{"template":"title-card","project_dir":"/srv/p"}`,
		"no template":   `{}`,
		"traversal":     `{"template":"../../etc"}`,
		"png-sequence":  `{"template":"title-card","format":"png-sequence"}`,
		"malformed":     `{not json`,
	} {
		if _, _, err := BuildRequest(context.Background(), composeFleetCfg(), true, ComposeTask, json.RawMessage(payload)); err == nil {
			t.Errorf("%s: must be refused at ack time", name)
		}
	}
}

// TestComposeVideoIsNotConcurrencyCapped: the composition lane is CPU-class and has its
// own slot; it must not hold a fleet execution slot the agent lane needs.
func TestComposeVideoIsNotConcurrencyCapped(t *testing.T) {
	s, _ := newTestServer(t, composeFleetCfg(), nil, nil)
	if s.concurrencyCapped(ComposeTask) {
		t.Fatal("compose-video counted against fleet_max_concurrent_jobs")
	}
	if !s.concurrencyCapped("agent") {
		t.Fatal("control: agent must stay capped")
	}
}
