package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// A media call that waited its window with no card answers with a place in line (err class
// gpu_queued, data carrying waiter_token). The caller can only resume that place if the door lets
// it send the token back: every media door advertises the field and threads it to the request.

// mediaDoors maps each media tool to its handler and a minimal valid argument object.
func mediaDoors(s *Server) map[string]struct {
	h    func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)
	args string
} {
	type door = struct {
		h    func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args string
	}
	return map[string]door{
		"offload_generate_image":        {s.handleGenerateImage, `{"prompt":"a calm ocean"`},
		"offload_edit_image_generative": {s.handleEditImageGenerative, `{"image":"a.png","prompt":"make it snow"`},
		"offload_inpaint_image":         {s.handleInpaintImage, `{"image":"a.png","mask":"m.png","prompt":"clean it"`},
		"offload_upscale_image":         {s.handleUpscaleImage, `{"image":"a.png"`},
		"offload_generate_video":        {s.handleGenerateVideo, `{"prompt":"waves","still":"s.png"`},
		"offload_animate_character":     {s.handleAnimateCharacter, `{"prompt":"a toy","ref":"r.png","driver":"d.mp4"`},
		"offload_generate_audio":        {s.handleGenerateAudio, `{"text":"hello","kind":"voice"`},
		"offload_run_graph":             {s.handleRunGraph, `{"graph_json":"{}"`},
	}
}

func TestEveryMediaDoorAdvertisesWaiterToken(t *testing.T) {
	schemas := map[string]map[string]any{}
	for _, tool := range listTools(t, config.Default()) {
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err == nil {
			schemas[tool.Name] = m
		}
	}
	for name := range mediaDoors(New(nil)) {
		props, _ := schemas[name]["properties"].(map[string]any)
		tok, _ := props["waiter_token"].(map[string]any)
		if tok == nil || tok["type"] != "string" {
			t.Errorf("%s does not advertise waiter_token (a string): %v", name, props["waiter_token"])
			continue
		}
		if d, _ := tok["description"].(string); len(d) < 40 {
			t.Errorf("%s: waiter_token needs a description that says where the value comes from: %q", name, d)
		}
		for _, r := range asStrings(schemas[name]["required"]) {
			if r == "waiter_token" {
				t.Errorf("%s: waiter_token must be optional", name)
			}
		}
	}
	props, _ := schemas["offload_run_graph"]["properties"].(map[string]any)
	dev, _ := props["devices"].(map[string]any)
	if dev == nil || dev["type"] != "array" {
		t.Errorf("offload_run_graph does not advertise devices (an array): %v", props["devices"])
	}
}

func asStrings(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// The token reaches the pipeline as params["waiter_token"] on every door, and only when given.
func TestMediaDoorsThreadTheWaiterTokenToTheRequest(t *testing.T) {
	s := New(nil)
	var got []core.Request
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		got = append(got, r)
		return core.Result{OK: true, Data: json.RawMessage(`{}`)}
	}
	for name, d := range mediaDoors(s) {
		got = nil
		if _, err := d.h(context.Background(), callReq(d.args+`,"waiter_token":"tk-zzzzzzzzzzzz"}`)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0].Params["waiter_token"] != "tk-zzzzzzzzzzzz" {
			t.Errorf("%s: params = %v, want waiter_token threaded to the request", name, paramsOf(got))
		}
		got = nil
		if _, err := d.h(context.Background(), callReq(d.args+`}`)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: %d request(s)", name, len(got))
		}
		if _, ok := got[0].Params["waiter_token"]; ok {
			t.Errorf("%s: a call with no token must carry none: %v", name, got[0].Params)
		}
		got = nil
		if _, err := d.h(context.Background(), callReq(d.args+`,"waiter_token":"  "}`)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: %d request(s)", name, len(got))
		}
		if _, ok := got[0].Params["waiter_token"]; ok {
			t.Errorf("%s: a blank token is no token: %v", name, got[0].Params)
		}
	}
}

func paramsOf(rs []core.Request) any {
	if len(rs) == 0 {
		return nil
	}
	return rs[0].Params
}

func TestRunGraphThreadsTheOperatorsDevices(t *testing.T) {
	s := New(nil)
	var got core.Request
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		got = r
		return core.Result{OK: true, Data: json.RawMessage(`{}`)}
	}
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":"{}","devices":["0","GPU-aaaa"]}`)); err != nil {
		t.Fatal(err)
	}
	devs, _ := got.Params["devices"].([]string)
	if len(devs) != 2 || devs[0] != "0" || devs[1] != "GPU-aaaa" {
		t.Fatalf("devices = %v, want both threaded", got.Params["devices"])
	}
	got = core.Request{}
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":"{}"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Params["devices"]; ok {
		t.Errorf("no devices declared: the key must be absent so the call holds the whole node, got %v", got.Params["devices"])
	}
}
