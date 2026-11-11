package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// A shared tool whose owner cannot take the call runs on the next device that serves it (ADR 0102).
// Seen for real on 2026-11-11: four tools were handed to the Axera card on another node, and for the
// hours that node refused them they deferred while the Coral and the RK3588 NPU sat idle. Here the
// owner is the fleet Axera with no node to reach, and the next device is a Coral on this box.
//
// An embedding is the exception that must hold: another device's vector is in another space, so it
// moves only when the caller says any_space, and the caller's word never reaches a sidecar.
func TestASharedToolRunsOnTheNextDeviceWhenItsOwnerCannot(t *testing.T) {
	var embedCalls int
	var detectArgs, embedArgs map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true,"models_missing":[]}`)) })
	mux.HandleFunc("/v1/object_detect", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&detectArgs)
		w.Write([]byte(`{"objects":[{"label":"cat","class_id":15,"x":1,"y":2,"w":3,"h":4,"score":0.9}],"count":1}`))
	})
	mux.HandleFunc("/v1/embed", func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		_ = json.NewDecoder(r.Body).Decode(&embedArgs)
		w.Write([]byte(`{"embedding":[0.1,0.2],"dim":2,"space":"efficientnet-edgetpu-s"}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = fake.URL
	cfg.FleetAccelerators = []string{"axera-ax650"}
	cfg.AcceleratorToolOwners = map[string]string{"offload_object_detect": "axera-ax650", "offload_image_embed": "axera-ax650"}
	cs := doorSession(t, New(pipeline.New(cfg, nil, nil, nil)))

	call := func(name, args string) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal([]byte(callTool(t, cs, name, args).Content[0].(*mcp.TextContent).Text), &out); err != nil {
			t.Fatalf("%s: not a JSON object: %v", name, err)
		}
		return out
	}

	got := call("offload_object_detect", `{"image_path":"/tmp/cat.jpg","score_threshold":0.4,"top_k":3}`)
	fb, _ := got["fallback"].(map[string]any)
	if got["count"] != float64(1) || fb["from"] != "axera-ax650" || fb["served_by"] != "coral-edgetpu" || fb["reason"] == "" {
		t.Fatalf("object_detect did not run on the Coral with the owner named: %v", got)
	}
	if detectArgs["score_threshold"] != 0.4 || detectArgs["image_path"] != "/tmp/cat.jpg" {
		t.Fatalf("the Coral did not get the arguments it takes: %v", detectArgs)
	}
	if _, sent := detectArgs["top_k"]; sent {
		t.Fatalf("an argument the Coral's detector does not take travelled to it: %v", detectArgs)
	}

	got = call("offload_image_embed", `{"image_path":"/tmp/cat.jpg"}`)
	if got["deferred"] != true || got["fallback_withheld"] == nil || embedCalls != 0 {
		t.Fatalf("an embedding moved to another device's space without the caller asking (%d calls): %v", embedCalls, got)
	}
	got = call("offload_image_embed", `{"image_path":"/tmp/cat.jpg","any_space":true}`)
	fb, _ = got["fallback"].(map[string]any)
	if got["space"] != "efficientnet-edgetpu-s" || fb["served_by"] != "coral-edgetpu" {
		t.Fatalf("any_space:true did not take the Coral's vector: %v", got)
	}
	if _, sent := embedArgs["any_space"]; sent {
		t.Fatalf("any_space reached the sidecar: %v", embedArgs)
	}
}
