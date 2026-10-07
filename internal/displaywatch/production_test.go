package displaywatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// swap fakes the llama-swap surface the watcher goes through: /running, the roster, the per-model
// unload route, and the TOTAL route (/unload) it must never reach.
type swap struct {
	mu         sync.Mutex
	running    string
	runStatus  int
	unloadCode int
	posted     []string
	totalHits  int
}

func (s *swap) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gemma-4-e4b-display","meta":{"llamaswap":{"aliases":[]}}},
			{"id":"gemma-4-e2b-display","meta":{"llamaswap":{"aliases":[]}}},
			{"id":"agent-pool","meta":{"llamaswap":{"aliases":[]}}}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.runStatus != 0 {
			w.WriteHeader(s.runStatus)
		}
		_, _ = w.Write([]byte(s.running))
	})
	mux.HandleFunc("/api/models/unload/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method == http.MethodPost {
			s.posted = append(s.posted, strings.TrimPrefix(r.URL.Path, "/api/models/unload/"))
		}
		if s.unloadCode != 0 {
			w.WriteHeader(s.unloadCode)
		}
	})
	mux.HandleFunc("/unload", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.totalHits++
		s.mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// isolate points the llama-swap client's keep-set loader at nothing, so a test never reads the
// serving config of the machine it runs on.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("LLAMASWAP_YAML", filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv("LLAMASWAP_KEEP_SET", "")
	t.Setenv("LLAMASWAP_CONFIG", "")
}

func healthyDevices(at time.Time) func() ([]gpuprobe.Device, time.Time, bool) {
	return func() ([]gpuprobe.Device, time.Time, bool) {
		return []gpuprobe.Device{
			{Index: 0, UUID: "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", FreeGiB: 1},
			{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", FreeGiB: 12, DisplayAttached: true},
			{Index: 2, UUID: "GPU-cccc3333-dddd-eeee-ffff-000000000000", FreeGiB: 1},
		}, at, true
	}
}

// The real wiring, against a fake llama-swap: the operator's presence is the default (present), a
// twin is loaded, and the check takes it down through the per-model route, and takes down nothing else.
func TestProductionUnloadsTheLoadedTwinThroughThePerModelRoute(t *testing.T) {
	isolate(t)
	s := &swap{running: `{"running":[{"model":"gemma-4-e4b-display","state":"ready"},{"model":"agent-pool","state":"ready"}]}`}
	srv := s.serve(t)
	cfg := watchedCfg()
	cfg.Endpoint = srv.URL
	cfg.OperatorPresence = "present" // the shipped default: the display card is closed
	cfg.StateDir = t.TempDir()

	w := New(cfg, Production(cfg, healthyDevices(time.Now())))
	if w == nil {
		t.Fatal("an awake display layer is watched")
	}
	out := w.Check(context.Background())
	if len(out.Unloaded) != 1 || out.Unloaded[0] != "gemma-4-e4b-display" {
		t.Fatalf("the twin comes down: %+v", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posted) != 1 || s.posted[0] != "gemma-4-e4b-display" {
		t.Fatalf("llama-swap saw exactly one per-model unload, for the twin, got %v", s.posted)
	}
	if s.totalHits != 0 {
		t.Fatalf("the total unload route must never be reached, got %d hits", s.totalHits)
	}
}

// A llama-swap build without the per-model route answers 404. That is an error the watcher reports and
// retries; it is not a licence to unload everything.
func TestProductionNeverFallsBackToTheTotalUnloadRoute(t *testing.T) {
	isolate(t)
	s := &swap{running: `{"running":[{"model":"gemma-4-e4b-display","state":"ready"}]}`, unloadCode: http.StatusNotFound}
	srv := s.serve(t)
	cfg := watchedCfg()
	cfg.Endpoint = srv.URL
	cfg.OperatorPresence = "present"
	cfg.StateDir = t.TempDir()

	out := New(cfg, Production(cfg, healthyDevices(time.Now()))).Check(context.Background())
	if len(out.Unloaded) != 0 || len(out.Failed) != 1 || !strings.Contains(out.Failed[0], "route absent") {
		t.Fatalf("an absent route is a reported failure: %+v", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.totalHits != 0 {
		t.Fatalf("never the total route, got %d hits", s.totalHits)
	}
}

// A llama-swap that is failing can send a body that decodes into "nothing running". The reader refuses
// any non-2xx answer, so a seat on the display card is not read as gone.
func TestProductionDoesNotReadAFailingRunningAnswerAsNothingLoaded(t *testing.T) {
	isolate(t)
	s := &swap{running: `{"running":null}`, runStatus: http.StatusServiceUnavailable}
	srv := s.serve(t)
	cfg := watchedCfg()
	cfg.Endpoint = srv.URL
	cfg.StateDir = t.TempDir()
	out := New(cfg, Production(cfg, healthyDevices(time.Now()))).Check(context.Background())
	if out.ReadErr == nil {
		t.Fatalf("a 503 is an unreadable /running, not an empty one: %+v", out)
	}
}

// The device sample is the sampler's last good one: an old one is no reading, and the floor then fails
// closed instead of being read from a number the driver stopped updating.
func TestProductionRefusesAStaleOrEmptyDeviceSample(t *testing.T) {
	cfg := watchedCfg()
	cfg.StateDir = t.TempDir()
	if devs, ok := Production(cfg, healthyDevices(time.Now())).Devices(); !ok || len(devs) != 3 {
		t.Fatalf("a fresh sample is read: %v %v", devs, ok)
	}
	if _, ok := Production(cfg, healthyDevices(time.Now().Add(-2*maxDeviceAge))).Devices(); ok {
		t.Fatal("a sample older than the health payload's bound is no reading")
	}
	empty := func() ([]gpuprobe.Device, time.Time, bool) { return nil, time.Now(), true }
	if _, ok := Production(cfg, empty).Devices(); ok {
		t.Fatal("a sample with no devices is no reading")
	}
	none := func() ([]gpuprobe.Device, time.Time, bool) { return nil, time.Time{}, false }
	if _, ok := Production(cfg, none).Devices(); ok {
		t.Fatal("no sample yet is no reading")
	}
}

// The status path rides the machine-wide state root, never a synced folder.
func TestStatePathUsesTheStateRoot(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	p, err := StatePath(cfg)
	if err != nil || filepath.Base(p) != "display-watch.json" || filepath.Dir(p) != cfg.StateDir {
		t.Fatalf("state path = %q err=%v, want display-watch.json directly under the state root %q", p, err, cfg.StateDir)
	}
	cfg.StateDir = filepath.Join(t.TempDir(), "My Drive", "state")
	if _, err := StatePath(cfg); err == nil {
		t.Fatal("a state root inside a cloud-sync folder must be refused, as the lease's is")
	}
}
