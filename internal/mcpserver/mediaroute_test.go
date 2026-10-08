package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// tools/list changed on every box (ADR 0077): the five media doors gained `route` and `remotes`. This pins
// the change deliberately, and pins that the other media tools did NOT gain it: an edit, an SVG or an
// ffmpeg op is never placed on a fleet node by this door.
func TestMediaToolsAdvertiseRouteAndRemotes(t *testing.T) {
	want := map[string]bool{
		"offload_generate_image": true, "offload_generate_video": true, "offload_animate_character": true,
		"offload_generate_audio": true, "offload_run_graph": true,
		"offload_generate_svg": false, "offload_edit_image": false, "offload_media": false, "offload_upscale_image": false,
	}
	seen := 0
	for _, tool := range listTools(t, config.Default()) {
		expect, care := want[tool.Name]
		if !care {
			continue
		}
		seen++
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Type  string `json:"type"`
				Enum  []string
				Items struct {
					Type string `json:"type"`
				} `json:"items"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema not JSON: %v", tool.Name, err)
		}
		route, hasRoute := schema.Properties["route"]
		remotes, hasRemotes := schema.Properties["remotes"]
		if hasRoute != expect || hasRemotes != expect {
			t.Fatalf("%s: route present=%v remotes present=%v, want %v", tool.Name, hasRoute, hasRemotes, expect)
		}
		if expect {
			if strings.Join(route.Enum, ",") != "local,auto,remote" {
				t.Errorf("%s route enum = %v", tool.Name, route.Enum)
			}
			if remotes.Type != "array" || remotes.Items.Type != "string" {
				t.Errorf("%s remotes must be an array of strings: %+v", tool.Name, remotes)
			}
		}
		for _, r := range schema.Required {
			if r == "route" || r == "remotes" {
				t.Errorf("%s: %s must stay optional, every existing caller omits it", tool.Name, r)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d media tools", seen, len(want))
	}
}

// An unknown route, remotes outside delegate_remotes and route remote with no fleet are all defers produced
// BEFORE the pipeline runs, on every one of the five doors.
func TestMediaRouteRefusalsNeverRunThePipeline(t *testing.T) {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.10:18811"}
	s := New(pipeline.New(cfg, nil, nil, nil))
	doors := map[string]struct {
		call func(map[string]any) string
		args map[string]any
	}{
		"image": {func(a map[string]any) string { return callVision(t, s.handleGenerateImage, a) }, map[string]any{"prompt": "a door"}},
		"video": {func(a map[string]any) string { return callVision(t, s.handleGenerateVideo, a) }, map[string]any{"prompt": "pan"}},
		"animate": {func(a map[string]any) string { return callVision(t, s.handleAnimateCharacter, a) },
			map[string]any{"prompt": "a fox", "ref": "r.png", "driver": "d.mp4"}},
		"audio": {func(a map[string]any) string { return callVision(t, s.handleGenerateAudio, a) }, map[string]any{"text": "hola"}},
		"graph": {func(a map[string]any) string { return callVision(t, s.handleRunGraph, a) }, map[string]any{"graph_json": `{"1":{"class_type":"X"}}`}},
	}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, d := range doors {
		txt := d.call(with(d.args, map[string]any{"route": "cloud"}))
		if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, `"defer_class":"contract"`) || !strings.Contains(txt, "unrecognized route") {
			t.Errorf("%s: unknown route: %s", name, txt)
		}
		txt = d.call(with(d.args, map[string]any{"route": "remote", "remotes": []string{"http://192.0.2.99:18811"}}))
		if !strings.Contains(txt, `"defer_class":"contract"`) || !strings.Contains(txt, "not in delegate_remotes") {
			t.Errorf("%s: a remote outside delegate_remotes: %s", name, txt)
		}
	}
	// Remote with no fleet configured at all is a config defer, naming delegate_remotes.
	bare := New(pipeline.New(config.Default(), nil, nil, nil))
	txt := callVision(t, bare.handleGenerateImage, map[string]any{"prompt": "a door", "route": "remote"})
	if !strings.Contains(txt, `"defer_class":"config"`) || !strings.Contains(txt, "delegate_remotes") {
		t.Fatalf("remote with no fleet: %s", txt)
	}
}

// route omitted, "" and local run the pipeline here and come back unstamped: on a box with no fleet the
// result is what the door returned before the route existed (the lane's own deferral, no placement, no node).
func TestMediaDefaultRouteRunsLocalUnstamped(t *testing.T) {
	cfg := config.Default()
	cfg.ImageGenScript, cfg.ImageGenEngine = "", "" // no image lane here: the pipeline defers by itself
	cfg.LedgerPath = ""
	s := New(pipeline.New(cfg, nil, nil, nil))
	var want string
	for i, route := range []any{nil, "", "local", "auto"} {
		args := map[string]any{"prompt": "a red door"}
		if route != nil {
			args["route"] = route
		}
		txt := callVision(t, s.handleGenerateImage, args)
		var res struct {
			Deferred bool   `json:"deferred"`
			Reason   string `json:"reason"`
			Meta     struct {
				Node      string `json:"node"`
				Placement string `json:"placement"`
			} `json:"meta"`
		}
		if err := json.Unmarshal([]byte(txt), &res); err != nil {
			t.Fatalf("route %v: %v: %s", route, err, txt)
		}
		if !res.Deferred || res.Reason == "" {
			t.Fatalf("route %v: want the pipeline's own deferral, got %s", route, txt)
		}
		if res.Meta.Node != "" || res.Meta.Placement != "" || strings.Contains(txt, "delegate_remotes") {
			t.Fatalf("route %v: a local run must carry no node, placement or fleet text: %s", route, txt)
		}
		if i == 0 {
			want = res.Reason
		} else if res.Reason != want {
			t.Fatalf("route %v changed the local result: %q vs %q", route, res.Reason, want)
		}
	}
}
