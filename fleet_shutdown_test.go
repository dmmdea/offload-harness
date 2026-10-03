package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// A fleet-serve shutdown waits for the node emitter's background posts: the frame of a card closed
// during the drain reaches PAIR before the process exits, instead of being lost and left for the
// next process's orphan sweep to close as failed.
func TestFleetServeDrainDeliversTheNodeCardsBeforeExit(t *testing.T) {
	var mu sync.Mutex
	got := 0
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // a PAIR that answers slowly: the post is still in flight when the drain ends
		mu.Lock()
		got++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ingress.Close()
	app := t.TempDir()
	for name, body := range map[string]string{
		"node-id.json":                           `{"node_uuid":"node-s-uuid"}`,
		filepath.Join("cluster", "members.json"): `[{"nodeUuid":"node-s-uuid","name":"node-s"}]`,
	} {
		p := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := pairworkloads.New(pairworkloads.Config{Enabled: true, Endpoint: ingress.URL, AppDir: app, OpenDir: t.TempDir()})
	e.Emit(pairworkloads.Event{JobID: "j1", Model: "m", Engine: "llamacpp", State: "failed", Error: "dropped", CreatedAt: 1, CompletedAt: 2})

	fleetServeDrain(fleetnode.NewJobs(time.Hour, 1), e, time.Second)

	mu.Lock()
	defer mu.Unlock()
	if got != 1 {
		t.Fatalf("the ingress had %d frames when the drain returned, want 1: the shutdown did not wait for the node's card posts", got)
	}
}
