package mcpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Both are opt-in bindings (a tier earns them by declaring a media seat), so a
	// node that serves them says so — exactly as a real config does.
	cfg.VisionModel = "qwen3vl"
	cfg.STTModel = "whisper-stt"
	// The agent seat is opt-in the same way (tier-seeded from resident_tier);
	// the roster must report the RESOLVED planner, i.e. cfg.AgentPlannerModel("").
	cfg.AgentModel = "gemma4-26b-a4b"
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
		"agent":      cfg.AgentPlannerModel(""), // the resolved planner seat, not the raw field
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

// TestStatusFleetLocalSeatColdNeverLoads is the regression guard on the fleet
// section's one hard requirement: a status question must NEVER cost a model
// load. The fake llama-swap records every path it serves; the seat is reported
// not-running, so the probe must stop at /running + /v1/models — any request
// that could auto-start the seat (/upstream/..., /v1/chat/completions) fails
// the test by name. The payload must report the seat honestly cold.
func TestStatusFleetLocalSeatColdNeverLoads(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": "qwen38-27b"}},
			})
		case "/running":
			// The seat is COLD: nothing is loaded.
			_ = json.NewEncoder(w).Encode(map[string]any{"running": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Endpoint = upstream.URL
	cfg.AgentModel = "qwen38-27b"
	t.Setenv("NVIDIA_API_KEY", "")
	t.Setenv("NGC_API_KEY", "")

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleStatus(context.Background(), callReq(`{}`))
	if err != nil {
		t.Fatalf("handleStatus error: %v", err)
	}
	m := decodeResult(t, res)
	fleet, ok := m["fleet"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no fleet section: %v", m)
	}
	seat, ok := fleet["local_agent_seat"].(map[string]any)
	if !ok {
		t.Fatalf("fleet has no local_agent_seat: %v", fleet)
	}
	if seat["model"] != "qwen38-27b" {
		t.Fatalf("local seat model = %v, want qwen38-27b", seat["model"])
	}
	if seat["loaded"] != false {
		t.Fatalf("cold seat must report loaded=false, got %v", seat)
	}
	if _, present := seat["ctx_tokens"]; present {
		t.Fatalf("cold seat must not invent a ctx_tokens value: %v", seat)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if strings.HasPrefix(p, "/upstream/") || strings.Contains(p, "/chat/completions") {
			t.Fatalf("status probe hit %q — a path that can auto-start the seat; the probe must never load", p)
		}
	}
}

// TestNCtxFromProps: the window extractor must read both llama.cpp schema
// generations (default_generation_settings.n_ctx on current builds, root n_ctx
// on older ones) and answer "unknown" — never zero-as-a-value — otherwise.
func TestNCtxFromProps(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]any
		want  int
		ok    bool
	}{
		{"current schema", map[string]any{"default_generation_settings": map[string]any{"n_ctx": float64(131072)}}, 131072, true},
		{"legacy root schema", map[string]any{"n_ctx": float64(32768)}, 32768, true},
		{"nested wins over root", map[string]any{"default_generation_settings": map[string]any{"n_ctx": float64(131072)}, "n_ctx": float64(8192)}, 131072, true},
		{"zero is not a window", map[string]any{"n_ctx": float64(0)}, 0, false},
		{"absent", map[string]any{"model": "x"}, 0, false},
		{"wrong type", map[string]any{"n_ctx": "big"}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := nCtxFromProps(c.props)
			if got != c.want || ok != c.ok {
				t.Fatalf("nCtxFromProps(%v) = (%d, %v), want (%d, %v)", c.props, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestStatusReportsKVCacheServer: the optional cache-server tier is reported in BOTH
// states — absent/disabled as a described "no tier" (never an error, never a missing
// key), enabled as its wiring plus a live reachability fact. tools/list is unaffected.
func TestStatusReportsKVCacheServer(t *testing.T) {
	off := kvCacheServerView(context.Background(), config.Default())
	if off["enabled"] != false || off["note"] == nil {
		t.Fatalf("disabled tier must report enabled:false with a note, got %v", off)
	}
	cfg := config.Default()
	// A closed local port: declared, valid, and provably unreachable within the 1 s dial.
	cfg.KVCacheServer = &config.KVCacheServer{Enabled: true, Address: "127.0.0.1:1", Seat: "qwen3.8-27b-vllm", KeyPrefix: "qube-seat-v7"}
	on := kvCacheServerView(context.Background(), cfg)
	if on["enabled"] != true || on["store"] != "valkey" || on["chunk_size"] != 784 || on["l1_staging_gb"] != 8 || on["key_prefix"] != "qube-seat-v7" {
		t.Fatalf("enabled tier view wrong: %v", on)
	}
	if on["reachable"] != false || on["reachable_error"] == nil {
		t.Fatalf("a closed port must report reachable:false with the dial error, got %v", on)
	}
	// A live listener flips the fact.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg.KVCacheServer.Address = ln.Addr().String()
	if v := kvCacheServerView(context.Background(), cfg); v["reachable"] != true {
		t.Fatalf("live listener must report reachable:true, got %v", v)
	}
}

// TestStatusDoesNotDialAnInvalidOrNamedStore: a refused block is reported as invalid and
// never dialed; a hostname store is reported as unprobed; fs_native says "no port".
func TestStatusDoesNotDialAnInvalidOrNamedStore(t *testing.T) {
	cfg := config.Default()
	cfg.KVCacheServer = &config.KVCacheServer{Enabled: true, Address: "8.8.8.8:1", Seat: "s"}
	v := kvCacheServerView(context.Background(), cfg)
	if v["invalid"] == nil || v["enabled"] != true {
		t.Fatalf("refused block must report invalid, got %v", v)
	}
	if _, dialed := v["reachable"]; dialed {
		t.Fatalf("refused block must not be dialed, got %v", v)
	}
	cfg.KVCacheServer = &config.KVCacheServer{Enabled: true, Address: "store-box:18799", Seat: "s"}
	v = kvCacheServerView(context.Background(), cfg)
	if r, ok := v["reachable"]; !ok || r != nil || v["reachable_note"] == nil {
		t.Fatalf("hostname store must report reachable:null with a note, got %v", v)
	}
	cfg.KVCacheServer = &config.KVCacheServer{Enabled: true, Store: "fs_native", Address: "/mnt/kv", Seat: "s"}
	v = kvCacheServerView(context.Background(), cfg)
	if r, ok := v["reachable"]; !ok || r != nil || v["reachable_note"] == nil {
		t.Fatalf("fs_native must report reachable:null with a note, got %v", v)
	}
	off := kvCacheServerView(context.Background(), config.Default())
	if off["declared"] != false {
		t.Fatalf("absent block must report declared:false, got %v", off)
	}
}

// TestFleetViewPublishesServedModelsAndUtilization: offload_status is the
// LIVE surface for the two facts Task 7 taught the gate to consume — a caller
// must be able to see them without reading gate.go. served_models is
// published always (nil/absent decodes to a JSON null, same as any other
// unset field here); gpu_util_pct is published ONLY when the node said
// gpu_util_known, so an unknown never masquerades as a real number.
func TestFleetViewPublishesServedModelsAndUtilization(t *testing.T) {
	known := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "known-node", "agent_enabled": true, "agent_seat": "offload-e4b",
			"agent_seat_resident": true, "agent_ctx_tokens": 8192, "queue_depth": 0,
			"served_models":   []string{"offload-e4b"},
			"gpu_util_pct":    42,
			"gpu_util_known":  true,
		})
	}))
	defer known.Close()
	unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "unknown-node", "agent_enabled": true, "agent_seat": "offload-e4b",
			"agent_seat_resident": true, "agent_ctx_tokens": 8192, "queue_depth": 0,
		})
	}))
	defer unknown.Close()

	cfg := config.Default()
	cfg.AgentDelegationEnabled = true
	cfg.DelegateRemotes = []string{known.URL, unknown.URL}

	s := New(pipeline.New(cfg, nil, nil, nil))
	out := s.fleetView(context.Background(), cfg)
	nodes, ok := out["nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("fleet nodes = %v, want 2 entries", out["nodes"])
	}
	var knownNode, unknownNode map[string]any
	for _, n := range nodes {
		nm := n.(map[string]any)
		switch nm["node_id"] {
		case "known-node":
			knownNode = nm
		case "unknown-node":
			unknownNode = nm
		}
	}
	if knownNode == nil || unknownNode == nil {
		t.Fatalf("missing expected nodes in %v", nodes)
	}
	served, ok := knownNode["served_models"].([]string)
	if !ok || len(served) != 1 || served[0] != "offload-e4b" {
		t.Errorf("known node served_models = %v, want [\"offload-e4b\"]", knownNode["served_models"])
	}
	if knownNode["gpu_util_pct"] != 42 {
		t.Errorf("known node gpu_util_pct = %v, want 42", knownNode["gpu_util_pct"])
	}
	if _, present := unknownNode["gpu_util_pct"]; present {
		t.Errorf("unknown node must NOT publish gpu_util_pct, got %v", unknownNode["gpu_util_pct"])
	}
	if _, present := unknownNode["served_models"]; !present {
		t.Errorf("served_models key must be present (nil) even when the node publishes none, got %v", unknownNode)
	}
}
