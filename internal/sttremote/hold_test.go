package sttremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/lanehold"
)

// The 2026-11-25 incident: this box's card is leased, the one fleet node answers the upload 503 queue
// full with a Retry-After, and the call used to leave the fleet's line at once. It must now keep its
// place, upload again when the node's own refusal window has passed, and come back transcribed by the
// node, with the wait said in its placement.
func TestAutoHoldsItsPlaceWhenTheNodeRefusesAndTheLocalCardIsLeased(t *testing.T) {
	setBusy(t, true) // the local card stays leased for the whole call
	pollWas, coolWas := lanehold.PollInterval, lanehold.RefusalCooldown
	lanehold.PollInterval, lanehold.RefusalCooldown = 40*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { lanehold.PollInterval, lanehold.RefusalCooldown = pollWas, coolWas })

	var posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "node-c", "supported_task_types": sttTasks, "queue_depth": 0})
	})
	mux.HandleFunc("POST /fleet/stt", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if posts.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("queue full (4 jobs accepted+running, limit 4): retry later"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": p["job_id"], "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := json.Marshal(okResult(sampleSegments, false))
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(data)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.DelegateRemotes = []string{srv.URL}
	cfg.FleetAuthToken = "tok"
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = fakeFFmpeg(t, false)
	cfg.STTModel = "whisper-stt"

	local := &localRunner{}
	res := Run(context.Background(), cfg, local, sttReq(srcAudio(t), nil), "auto")
	if !res.OK || res.Deferred || res.Meta.Node != "node-c" {
		t.Fatalf("result = %+v, want the call transcribed by node-c", res)
	}
	if local.count() != 0 {
		t.Fatalf("the local card is leased, but the call ran here %d time(s)", local.count())
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("uploads = %d, want the refused one and the accepted one", got)
	}
	if p := res.Meta.Placement; !strings.HasPrefix(p, "remote: local gpu busy, waited ") || !strings.HasSuffix(p, " in line") {
		t.Fatalf("placement = %q, want it to say how long the call waited in line", p)
	}
}
