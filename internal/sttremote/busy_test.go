package sttremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The production trigger of the auto route is the question the local whisper request itself will
// meet: would the upstream fence hold it right now. A lease on the cards is not enough to spill (a
// whisper that is already resident is served at once, fence or no fence), and the model asked about
// is the one the call will use (hq picks stt_model_hq).
func TestDefaultLocalBusyIsTheWhisperFenceNotTheLease(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", t.TempDir()) }) // an armed gate over an empty dir is inert

	running := `{"running":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/running":
			_, _ = w.Write([]byte(running))
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{{"id": "whisper-stt"}, {"id": "whisper-stt-hq"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.STTModel, cfg.STTModelHQ = "whisper-stt", "whisper-stt-hq"
	ctx := context.Background()

	if localBusy(ctx, cfg, false) {
		t.Fatal("busy with no lease held")
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	if !localBusy(ctx, cfg, false) {
		t.Fatal("a render holds the cards and whisper is cold: the request would be held, so the call must spill")
	}
	running = `{"running":[{"model":"whisper-stt","state":"ready"}]}`
	if localBusy(ctx, cfg, false) {
		t.Fatal("a resident whisper is served at once under a render: spilling would waste a fleet node")
	}
	if !localBusy(ctx, cfg, true) {
		t.Fatal("an hq call asks about stt_model_hq, which is cold: it must read as blocked")
	}
	running = `{"running":[{"model":"whisper-stt-hq","state":"ready"}]}`
	if localBusy(ctx, cfg, true) {
		t.Fatal("hq resident: served at once")
	}
	if !localBusy(ctx, cfg, false) {
		t.Fatal("the standard model is cold now: blocked")
	}
	noHQ := cfg
	noHQ.STTModelHQ = ""
	running = `{"running":[{"model":"whisper-stt","state":"ready"}]}`
	if localBusy(ctx, noHQ, true) {
		t.Fatal("with no stt_model_hq an hq call runs on stt_model (the pipeline's own fallback): resident, so served")
	}
	none := cfg
	none.STTModel = ""
	if localBusy(ctx, none, false) {
		t.Fatal("no stt model at all: nothing to block, the local run defers by itself")
	}
}
