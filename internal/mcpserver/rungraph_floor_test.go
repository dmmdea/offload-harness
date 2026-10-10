package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// offload_run_graph takes min_vram_gb (the least size, in GB, of the largest card of the node that takes the graph) and
// model_family (the family the graph belongs to, which the default floor keys on). Both reach the request the placement
// reads; absent, neither key is there, so a call that never passes them is byte for byte what it was.
func TestRunGraphDoorCarriesMinVRAMAndModelFamily(t *testing.T) {
	s := New(nil)
	var got []core.Request
	s.runHook = func(_ context.Context, r core.Request) core.Result {
		got = append(got, r)
		return core.Result{OK: true, Data: json.RawMessage(`{}`)}
	}
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":"{}","min_vram_gb":16,"model_family":"wan22"}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Task != core.TaskRunGraph {
		t.Fatalf("requests = %+v", got)
	}
	if v, ok := got[0].Params["min_vram_gb"].(float64); !ok || v != 16 {
		t.Errorf("min_vram_gb = %#v, want 16", got[0].Params["min_vram_gb"])
	}
	if got[0].Params["model_family"] != "wan22" {
		t.Errorf("model_family = %#v", got[0].Params["model_family"])
	}

	// 0 is a number the caller gave (it lifts the default floor), not an absent one.
	got = nil
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":"{}","min_vram_gb":0}`)); err != nil {
		t.Fatal(err)
	}
	if v, ok := got[0].Params["min_vram_gb"]; !ok || v != float64(0) {
		t.Errorf("an explicit 0 must reach the request: %#v", got[0].Params)
	}

	got = nil
	if _, err := s.handleRunGraph(context.Background(), callReq(`{"graph_json":"{}"}`)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"min_vram_gb", "model_family"} {
		if _, has := got[0].Params[k]; has {
			t.Errorf("%s must be absent when the caller did not pass it: %#v", k, got[0].Params)
		}
	}
}

func TestRunGraphToolAdvertisesMinVRAMAndModelFamily(t *testing.T) {
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_run_graph" {
			continue
		}
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		props, _ := m["properties"].(map[string]any)
		if mv, _ := props["min_vram_gb"].(map[string]any); mv["type"] != "number" {
			t.Errorf("min_vram_gb = %v, want a number", props["min_vram_gb"])
		}
		if mf, _ := props["model_family"].(map[string]any); mf["type"] != "string" {
			t.Errorf("model_family = %v, want a string", props["model_family"])
		}
		// The description no longer says the graph runs only on the local ComfyUI: with a fleet it can run on a node.
		if strings.Contains(tool.Description, "on the LOCAL ComfyUI") {
			t.Errorf("the description still says the graph is local-only: %s", tool.Description)
		}
		for _, want := range []string{"one card", "min_vram_gb", "wan22"} {
			if !strings.Contains(tool.Description+string(b), want) {
				t.Errorf("the tool must explain %q", want)
			}
		}
		return
	}
	t.Fatal("offload_run_graph is not listed")
}
