package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The "pair" status block says which way this box's PAIR emitter reports. It is absent while the
// box never opted in, so the default answer stays what it was.
func TestStatusPairBlockNamesTheEmitterMode(t *testing.T) {
	cfg := config.Default()
	if got := statusPair(cfg); got != nil {
		t.Fatalf("pair block on a box with pair_workloads_enabled off = %v, want absent", got)
	}

	cfg.PairWorkloadsEnabled = true
	app := t.TempDir()
	t.Setenv("OFFLOAD_PAIR_APPDIR", app)

	// No PAIR here and nothing to relay to: off, with the reason.
	cfg.PairWorkloadsRelay = []string{"off"}
	got, _ := statusPair(cfg).(map[string]any)
	if got["mode"] != "off" || got["reason"] == "" || got["reason"] == nil {
		t.Fatalf("no PAIR, relay off = %v", got)
	}

	// No PAIR identity but an explicit relay: relay mode, naming the route.
	cfg.PairWorkloadsRelay = []string{"http://192.0.2.9:18811"}
	got, _ = statusPair(cfg).(map[string]any)
	if got["mode"] != "relay" || got["relay"] != "http://192.0.2.9:18811/fleet/pair-relay" {
		t.Fatalf("explicit relay = %v", got)
	}

	// A readable node-id.json: the local ingress, the relay never consulted. (A box's emitter is
	// cached per app dir, so this is another box.)
	member := t.TempDir()
	t.Setenv("OFFLOAD_PAIR_APPDIR", member)
	if err := os.WriteFile(filepath.Join(member, "node-id.json"), []byte(`{"node_uuid":"node-a-uuid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = statusPair(cfg).(map[string]any)
	if got["mode"] != "local ingress" || got["relay"] != nil {
		t.Fatalf("with node-id.json = %v", got)
	}
}

// The block reads the cached per-config emitter: on a non-member in auto mode the relay members'
// health is probed once, not on every offload_status call.
func TestStatusPairBlockProbesTheRelayMembersOnce(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"node_id":"node-a","pair_relay":true}`))
	}))
	defer member.Close()
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir()) // no PAIR here
	cfg := config.Default()
	cfg.PairWorkloadsEnabled = true
	cfg.DelegateRemotes = []string{member.URL}
	cfg.FleetAuthToken = "tok"
	for i := 0; i < 3; i++ {
		got, _ := statusPair(cfg).(map[string]any)
		if got["mode"] != "relay" || got["relay"] != member.URL+"/fleet/pair-relay" {
			t.Fatalf("call %d: %v", i, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("the member's health was read %d times by 3 status calls, want 1", hits)
	}
}
