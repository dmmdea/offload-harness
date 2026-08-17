package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// agentHealthJSON is a /fleet/health body from an agent-enabled node, plus
// unknown fields (vram_*, model_footprints, and a deliberately future key) —
// FetchNodeView must decode LOOSELY per FLEET-NODE.md so staggered node
// deploys never turn an additive field into a flag-day upgrade.
const agentHealthJSON = `{
	"node_id": "lenovo-node", "schema_version": 1,
	"gpu_vendor": "nvidia", "gpu_arch": "ampere",
	"vram_total_gb": 6, "vram_free_gb": 5.5,
	"supported_task_types": ["agent"],
	"loadable_model_families": [],
	"model_footprints": [],
	"queue_depth": 2,
	"agent_seat": "offload-e4b",
	"agent_ctx_tokens": 8192,
	"agent_seat_resident": true,
	"agent_enabled": true,
	"some_future_field": {"nested": true}
}`

// mediaHealthJSON is a pre-delegation node's health: no agent fields at all.
const mediaHealthJSON = `{
	"node_id": "media-only", "schema_version": 1,
	"gpu_vendor": "nvidia", "gpu_arch": "blackwell",
	"vram_total_gb": 16, "vram_free_gb": 12,
	"supported_task_types": ["image-gen"],
	"loadable_model_families": ["sdxl"],
	"model_footprints": [],
	"queue_depth": 1
}`

// healthServer serves body on GET /fleet/health and records the Authorization
// header it saw.
func healthServer(t *testing.T, body string, sawAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/health" {
			http.NotFound(w, r)
			return
		}
		if sawAuth != nil {
			*sawAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write health body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchNodeViewMapsAgentFields(t *testing.T) {
	srv := healthServer(t, agentHealthJSON, nil)
	got, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchNodeView: %v", err)
	}
	want := NodeView{
		NodeID:         "lenovo-node",
		AgentEnabled:   true,
		AgentSeat:      "offload-e4b",
		AgentResident:  true,
		AgentCtxTokens: 8192,
		QueueDepth:     2,
		Local:          false, // a FETCHED view is by definition a remote node
	}
	if got != want {
		t.Fatalf("NodeView = %+v, want %+v", got, want)
	}
}

// TestFetchNodeViewWithoutAgentFields: a media-only node's health (agent keys
// absent) maps to zero agent fields — which the gate reads as ineligible, so
// a pre-delegation node can never be placed on. queue_depth/node_id still map.
func TestFetchNodeViewWithoutAgentFields(t *testing.T) {
	srv := healthServer(t, mediaHealthJSON, nil)
	got, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchNodeView: %v", err)
	}
	want := NodeView{NodeID: "media-only", QueueDepth: 1}
	if got != want {
		t.Fatalf("NodeView = %+v, want %+v", got, want)
	}
}

// A trailing slash on base must not produce ".../fleet/health" double-slash 404s.
func TestFetchNodeViewToleratesTrailingSlashBase(t *testing.T) {
	srv := healthServer(t, mediaHealthJSON, nil)
	if _, err := FetchNodeView(context.Background(), srv.URL+"/", ""); err != nil {
		t.Fatalf("FetchNodeView with trailing slash: %v", err)
	}
}

func TestFetchNodeViewBearerHeader(t *testing.T) {
	var saw string
	srv := healthServer(t, agentHealthJSON, &saw)
	if _, err := FetchNodeView(context.Background(), srv.URL, "s3cret"); err != nil {
		t.Fatalf("FetchNodeView: %v", err)
	}
	if saw != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want \"Bearer s3cret\"", saw)
	}
	// And no token means no header — health is open today; the parameter is
	// reserved for a future health-auth posture, not a mandatory credential.
	saw = "unset-sentinel"
	if _, err := FetchNodeView(context.Background(), srv.URL, ""); err != nil {
		t.Fatalf("FetchNodeView without token: %v", err)
	}
	if saw != "" {
		t.Fatalf("Authorization = %q, want absent when no token is given", saw)
	}
}

func TestFetchNodeViewNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write([]byte(`{"status":"error","error":"vram snapshot unavailable"}`)); err != nil {
			t.Errorf("write error body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	_, err := FetchNodeView(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatal("FetchNodeView succeeded on a 503 health")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error %q does not name the status", err)
	}
}

// TestFetchNodeViewRefusesOffTailnetBase: the fetch rides
// netguard.SafeTransport, so a base outside loopback/CGNAT dies at the dial
// gate with ZERO network activity (192.0.2.x is TEST-NET-1 — a public
// literal that is guaranteed unroutable, so a regression here would still
// fail, just slowly).
func TestFetchNodeViewRefusesOffTailnetBase(t *testing.T) {
	_, err := FetchNodeView(context.Background(), "http://192.0.2.10:9", "")
	if err == nil {
		t.Fatal("FetchNodeView dialed a public IP literal")
	}
	if !strings.Contains(err.Error(), "tailnet guard") {
		t.Fatalf("error %q does not come from the tailnet dial gate", err)
	}
}
