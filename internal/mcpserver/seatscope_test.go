package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// Plan P5: the agent_run door registers its run with the pins of its seat, so a drain waits only
// for runs on the cards a lease holds and a single-card seat's run-cap line is its card's.
func TestAgentRunDoorRecordsItsSeatPins(t *testing.T) {
	const seat = "agent-pool"
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.Model = seat
	cfg.AgentModel = seat
	cfg.AgentAdmissionWaitSec = 30
	var pins atomic.Value // []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			fmt.Fprintf(w, `{"running":[{"model":%q,"state":"ready","cmd":"y"}]}`, seat)
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case "/upstream/" + seat + "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case "/v1/chat/completions":
			if reg, err := gpuactivity.Open(cfg.GPULockPath, cfg.StateDir); err == nil {
				for _, run := range reg.List(time.Now()) {
					if run.Seat == seat {
						pins.Store(append([]string(nil), run.Devices...))
					}
				}
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg.Endpoint = srv.URL
	modelaffinity.SetSeatPins(func(m string) ([]string, bool) {
		if m == seat {
			return []string{"0"}, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })

	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	if m := decodeResult(t, res); m["deferred"] == true {
		t.Fatalf("the run deferred: %v", m)
	}
	got, _ := pins.Load().([]string)
	if len(got) != 1 || got[0] != "0" {
		t.Fatalf("the registered run carries the pins of its seat, got %v", got)
	}
}

// offload_status hands the activity snapshot the evidence rule's scope: a legacy lease whose
// cards were inferred shows them on the holder.
func TestLeaseViewCarriesAnInferredScope(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = root
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	}))
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	// The record an older binary wrote, on a host that turned the inference on.
	m.EmulateLegacyWriter()
	modelaffinity.SetLegacyInference(true)
	t.Cleanup(func() { modelaffinity.SetLegacyInference(false) })
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "legacy film render"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	sidecar := `{"epoch":` + strconv.FormatUint(l.Epoch(), 10) + `,"devices":["gpu-cccc0000"],"source":"command line"}`
	if err := os.WriteFile(filepath.Join(m.Dir(), "seen."+strconv.FormatUint(l.Epoch(), 10)), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	_, act := localLeaseViewWithActivity(context.Background(), cfg)
	if act.Holder == nil || len(act.Holder.Devices) == 0 {
		t.Fatalf("the holder of a scoped legacy lease carries its cards, got %+v", act.Holder)
	}
	found := false
	for _, d := range act.Holder.Devices {
		if d == "gpu-cccc0000" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the inferred card is among the holder's devices, got %v", act.Holder.Devices)
	}
}
