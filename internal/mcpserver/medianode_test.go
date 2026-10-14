package mcpserver

// The three lane doors that can name a fleet node (ADR 0082 Amendment 3): offload_generate_video, offload_animate_character
// and offload_generate_audio take `node`; the first two take `min_vram_gb` as the graph door does. The image and graph doors do
// not advertise `node`, so no tool offers a parameter it would drop.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func toolProps(t *testing.T) map[string]map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]map[string]any{}
	for _, tool := range listTools(t, config.Default()) {
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema not JSON: %v", tool.Name, err)
		}
		out[tool.Name] = schema.Properties
	}
	return out
}

func TestOnlyTheVideoAnimationAndAudioDoorsAdvertiseNode(t *testing.T) {
	props := toolProps(t)
	want := map[string]bool{
		"offload_generate_video": true, "offload_animate_character": true, "offload_generate_audio": true,
		"offload_generate_image": false, "offload_run_graph": false,
	}
	for name, expect := range want {
		p, ok := props[name]
		if !ok {
			t.Fatalf("%s is not listed", name)
		}
		node, has := p["node"]
		if has != expect {
			t.Errorf("%s: node advertised=%v, want %v", name, has, expect)
		}
		if expect {
			if node["type"] != "string" {
				t.Errorf("%s: node must be a string: %v", name, node)
			}
			desc, _ := node["description"].(string)
			for _, w := range []string{"node_id", "delegate_remotes", "only"} {
				if !strings.Contains(desc, w) {
					t.Errorf("%s: the node description must say %q: %s", name, w, desc)
				}
			}
		}
	}
}

func TestVideoAndAnimationAdvertiseMinVRAMGBAndAudioDoesNot(t *testing.T) {
	props := toolProps(t)
	for name, expect := range map[string]bool{"offload_generate_video": true, "offload_animate_character": true, "offload_generate_audio": false, "offload_generate_image": false} {
		mv, has := props[name]["min_vram_gb"]
		if has != expect {
			t.Errorf("%s: min_vram_gb advertised=%v, want %v", name, has, expect)
		}
		if expect && mv["type"] != "number" {
			t.Errorf("%s: min_vram_gb must be a number: %v", name, mv)
		}
	}
}

type laneDoor struct {
	name string
	args string // the call's arguments without the closing brace
	task core.TaskType
	call func(s *Server, args string) string
}

func laneDoors() []laneDoor {
	return []laneDoor{
		{"video", `{"prompt":"pan"`, core.TaskGenerateVideo, func(s *Server, a string) string {
			res, _ := s.handleGenerateVideo(context.Background(), callReq(a))
			return textOf(res)
		}},
		{"animate", `{"prompt":"a fox","ref":"r.png","driver":"d.mp4"`, core.TaskAnimateCharacter, func(s *Server, a string) string {
			res, _ := s.handleAnimateCharacter(context.Background(), callReq(a))
			return textOf(res)
		}},
		{"audio", `{"text":"hola"`, core.TaskGenerateAudio, func(s *Server, a string) string {
			res, _ := s.handleGenerateAudio(context.Background(), callReq(a))
			return textOf(res)
		}},
	}
}

func countingServer() (*Server, *[]core.Request) {
	s := New(nil)
	var got []core.Request
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		got = append(got, r)
		return core.Result{OK: true, Data: json.RawMessage(`{}`)}
	}
	return s, &got
}

// node reaches the request: a named call on route local is the contract refusal that only a request carrying node can produce,
// and the pipeline does not run. Without node the same call runs.
func TestTheLaneDoorsCarryNodeIntoTheRequest(t *testing.T) {
	for _, d := range laneDoors() {
		s, got := countingServer()
		out := d.call(s, d.args+`,"node":"render-a","route":"local"}`)
		if len(*got) != 0 || !strings.Contains(out, `"defer_class":"contract"`) || !strings.Contains(out, "route local") {
			t.Errorf("%s: a named call on route local is refused before the pipeline (runs %d): %.300s", d.name, len(*got), out)
		}
		out = d.call(s, d.args+`,"route":"local"}`)
		if len(*got) != 1 || strings.Contains(out, `"deferred":true`) {
			t.Errorf("%s: without node the same call runs (runs %d): %.300s", d.name, len(*got), out)
		}
	}
}

// min_vram_gb reaches the request only when the caller passed it, 0 included; a malformed one is refused by name on every route,
// as the graph door does.
func TestVideoAndAnimationDoorsCarryMinVRAMGB(t *testing.T) {
	for _, d := range laneDoors()[:2] {
		s, got := countingServer()
		d.call(s, d.args+`,"min_vram_gb":16}`)
		d.call(s, d.args+`,"min_vram_gb":0}`)
		d.call(s, d.args+`}`)
		if len(*got) != 3 || (*got)[0].Task != d.task {
			t.Fatalf("%s: requests = %+v", d.name, *got)
		}
		if v, ok := (*got)[0].Params["min_vram_gb"].(float64); !ok || v != 16 {
			t.Errorf("%s: min_vram_gb = %#v, want 16", d.name, (*got)[0].Params["min_vram_gb"])
		}
		if v, ok := (*got)[1].Params["min_vram_gb"]; !ok || v != float64(0) {
			t.Errorf("%s: an explicit 0 must reach the request: %#v", d.name, (*got)[1].Params)
		}
		if _, has := (*got)[2].Params["min_vram_gb"]; has {
			t.Errorf("%s: absent when the caller did not pass it: %#v", d.name, (*got)[2].Params)
		}
		for _, route := range []string{"", "local", "auto", "remote"} {
			for _, bad := range []string{`"sixteen"`, `-1`, `true`} {
				s, got := countingServer()
				out := d.call(s, d.args+`,"min_vram_gb":`+bad+`,"route":"`+route+`"}`)
				if len(*got) != 0 || !strings.Contains(out, "min_vram_gb") || !strings.Contains(out, `"deferred":true`) {
					t.Errorf("%s route %q min_vram_gb %s: want a refusal naming min_vram_gb, nothing run (runs %d): %.300s", d.name, route, bad, len(*got), out)
				}
			}
		}
	}
}
