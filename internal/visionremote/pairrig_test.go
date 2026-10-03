package visionremote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// pairRig is the asking box's PAIR side for an attribution test: a stand-in ingress that records
// every frame, a PAIR app dir (this box is node-a; the serving node is a member named after its
// dispatch host, and node-b is another member the fleet node id can resolve to), a real ledger and a
// real Pipeline, which is the core.RemoteAttributor the remote lanes receive in production.
type pairRig struct {
	t      *testing.T
	mu     sync.Mutex
	frames []map[string]any
	e      *pairworkloads.Emitter
	led    *ledger.Ledger
	path   string
	appDir string
	p      *pipeline.Pipeline
}

// newPairRig builds the rig; members are extra PAIR member names (each gets the uuid "<name>-uuid").
func newPairRig(t *testing.T, enabled bool, members ...string) *pairRig {
	t.Helper()
	r := &pairRig{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var f map[string]any
		_ = json.NewDecoder(req.Body).Decode(&f)
		r.mu.Lock()
		r.frames = append(r.frames, f)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r.appDir = t.TempDir()
	list := `[{"nodeUuid":"self-uuid","name":"Node-A","ipAddress":"127.0.0.1"}`
	for _, m := range members {
		list += `,{"nodeUuid":"` + m + `-uuid","name":"` + m + `"}`
	}
	list += `]`
	for name, body := range map[string]string{"node-id.json": `{"node_uuid":"self-uuid"}`, filepath.Join("cluster", "members.json"): list} {
		path := filepath.Join(r.appDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.e = pairworkloads.New(pairworkloads.Config{Enabled: enabled, Endpoint: srv.URL, OpenDir: t.TempDir(), AppDir: r.appDir})
	r.path = filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(r.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	r.led = led
	r.e.AttachLedger(led) // the observer must leave the remote rows alone
	r.p = pipeline.New(config.Default(), nil, nil, led)
	r.p.SetPairEmitter(r.e)
	return r
}

// cards waits for the emitter and returns the frames grouped by state, failing unless every frame
// belongs to one card.
func (r *pairRig) cards() map[string]map[string]any {
	r.t.Helper()
	r.e.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]map[string]any{}
	ids := map[any]bool{}
	for _, f := range r.frames {
		wi := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
		ids[wi["id"]] = true
		out[wi["state"].(string)] = wi
	}
	if len(ids) > 1 {
		r.t.Fatalf("one call opened %d cards: %v", len(ids), ids)
	}
	return out
}

func (r *pairRig) frameCount() int {
	r.e.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *pairRig) rows() []ledger.Entry {
	r.t.Helper()
	rows, err := ledger.ReadAll(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

var _ core.RemoteAttributor = (*pipeline.Pipeline)(nil)
