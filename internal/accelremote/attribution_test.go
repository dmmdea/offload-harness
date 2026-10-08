package accelremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// D7/D11: a forwarded accelerator call names its asker, and tells the node to card the job only when
// this box will not (its PAIR emitter is not enabled).
func TestForwardedCallCarriesTheAttributionHeaders(t *testing.T) {
	var (
		mu  sync.Mutex
		hdr http.Header
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "node-c", "accelerators": []string{"coral-edgetpu"}})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		mu.Lock()
		hdr = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": env["job_id"], "state": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(`{"ok":true}`)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	img := filepath.Join(t.TempDir(), "x.jpg")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DelegateRemotes = []string{srv.URL}
	call := func() http.Header {
		t.Helper()
		if _, err := Call(context.Background(), cfg, "coral-edgetpu", "classify", map[string]any{"image_path": img}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return hdr
	}

	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	h := call()
	if h.Get(core.PairCardHeader) != core.PairCardNode || h.Get(core.AskerHeader) == "" {
		t.Fatalf("emitter disabled: pair-card %q asker %q, want %q and a name", h.Get(core.PairCardHeader), h.Get(core.AskerHeader), core.PairCardNode)
	}

	app := t.TempDir()
	for name, body := range map[string]string{"node-id.json": `{"node_uuid":"self-uuid"}`, filepath.Join("cluster", "members.json"): `[{"nodeUuid":"self-uuid","name":"Node-A"}]`} {
		p := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OFFLOAD_PAIR_APPDIR", app)
	cfg.PairWorkloadsEnabled = true
	h = call()
	if _, sent := h[http.CanonicalHeaderKey(core.PairCardHeader)]; sent {
		t.Fatalf("emitter enabled: %s sent (%q): the job would be carded twice", core.PairCardHeader, h.Get(core.PairCardHeader))
	}
	if h.Get(core.AskerHeader) != "node-a" {
		t.Fatalf("emitter enabled: asker %q, want the member name node-a", h.Get(core.AskerHeader))
	}
}
