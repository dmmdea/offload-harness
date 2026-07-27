package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// TestStatusDiscoversLocalCapability: offload_status is the discovery tool an
// inspecting agent calls FIRST. It must surface the LOCAL model roster (the
// asymmetry fix: before this tool, offload_nim was the only tool that named or
// listed models, so inspections concluded the harness's text/LLM capability was
// the cloud NIM catalog and never discovered the local cascade). It reports the
// configured roster, live-probes the local endpoint's /v1/models, and states
// that NIM is the only remote surface.
func TestStatusDiscoversLocalCapability(t *testing.T) {
	// Fake llama-swap /v1/models on a live local listener.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "offload-e4b"}, {"id": "gemma4-e2b"}, {"id": "gemma4-26b-a4b"}, {"id": "embeddinggemma"},
			},
		})
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Endpoint = upstream.URL
	cfg.VisionModel = "qwen3vl"
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("status must not defer on a healthy config: %v", m)
	}

	local, _ := m["local"].(map[string]any)
	if local == nil {
		t.Fatalf("missing local section: %v", m)
	}
	if local["endpoint"] != cfg.Endpoint {
		t.Errorf("local.endpoint = %v, want %v", local["endpoint"], cfg.Endpoint)
	}
	roster, _ := local["roster"].(map[string]any)
	if roster == nil {
		t.Fatalf("missing local.roster: %v", local)
	}
	for key, want := range map[string]string{
		"workhorse":  "offload-e4b",
		"triage":     "gemma4-e2b",
		"escalation": "gemma4-26b-a4b",
		"reasoning":  "gemma4-26b-a4b",
		"vision":     "qwen3vl",
		"stt":        "whisper-stt",
		"embed":      "embeddinggemma",
	} {
		if roster[key] != want {
			t.Errorf("roster.%s = %v, want %q", key, roster[key], want)
		}
	}
	served, _ := local["served_now"].([]any)
	if len(served) != 4 {
		t.Errorf("served_now should list the 4 live model ids, got %v", local["served_now"])
	}

	media, _ := m["media"].(map[string]any)
	if media == nil {
		t.Fatalf("missing media section: %v", m)
	}
	routes, _ := media["routes"].(map[string]any)
	if routes == nil {
		t.Fatalf("missing media.routes: %v", media)
	}
	for _, k := range []string{"generate_image", "generate_video", "edit_image", "media"} {
		e, _ := routes[k].(map[string]any)
		if e == nil || e["state"] == "" || e["engine"] == "" {
			t.Errorf("media.routes.%s must carry a derived state + engine, got %v", k, routes[k])
		}
	}

	remote, _ := m["remote"].(map[string]any)
	if remote == nil {
		t.Fatalf("missing remote section: %v", m)
	}
	if remote["nim_key_present"] != false {
		t.Errorf("nim_key_present = %v, want false with the key env cleared", remote["nim_key_present"])
	}
	if remote["nim_endpoint"] != cfg.NIMEndpoint {
		t.Errorf("nim_endpoint = %v, want %v", remote["nim_endpoint"], cfg.NIMEndpoint)
	}
}

// TestStatusMediaIsDerivedNotDeclared: this block used to state
// "image_engine": "ComfyUI (local)" as a constant and hand it to an autonomous
// planner on a node running stable-diffusion.cpp with no ComfyUI installed. The
// engine must follow the machine's binding, and the declared strings must be
// gone — a capability map the planner acts on is worse wrong than absent.
func TestStatusMediaIsDerivedNotDeclared(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sd-cli")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1"
	cfg.ImageGenEngine = "sdcpp"
	cfg.SdcppBin = bin
	cfg.SdcppModel = bin // any existing file: this asserts the ENGINE, not the model

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	m := decodeResult(t, res)
	media, _ := m["media"].(map[string]any)
	routes, _ := media["routes"].(map[string]any)
	img, _ := routes["generate_image"].(map[string]any)
	if img == nil || img["engine"] != "sdcpp" {
		t.Fatalf("generate_image engine must come from imagegen_engine, got %v", routes["generate_image"])
	}
	for _, declared := range []string{"image_engine", "video_engine", "audio_voice_engine", "audio_music_engine", "edit_pil", "edit_gimp", "media_ffmpeg"} {
		if _, ok := media[declared]; ok {
			t.Errorf("media.%s is a DECLARED capability and must not be reported", declared)
		}
	}
}

// TestStatusEndpointDownStillReportsRoster: a dead local endpoint must NOT turn
// status into a defer — the configured roster is still the answer; the probe
// failure is reported alongside it.
func TestStatusEndpointDownStillReportsRoster(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1" // nothing listens on port 1
	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("status must not defer when only the live probe fails: %v", m)
	}
	local, _ := m["local"].(map[string]any)
	if local == nil {
		t.Fatalf("missing local section: %v", m)
	}
	roster, _ := local["roster"].(map[string]any)
	if roster == nil || roster["workhorse"] != "offload-e4b" {
		t.Errorf("roster must survive a dead endpoint, got %v", local)
	}
	probeErr, _ := local["served_probe_error"].(string)
	if probeErr == "" {
		t.Errorf("served_probe_error must explain the failed probe, got %v", local)
	}
	if _, ok := local["served_now"]; ok {
		t.Errorf("served_now must be absent when the probe failed, got %v", local["served_now"])
	}
}
