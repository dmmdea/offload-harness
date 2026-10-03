package mcpserver

import (
	"os"
	"path/filepath"
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

	// A readable node-id.json: the local ingress, the relay never consulted.
	if err := os.WriteFile(filepath.Join(app, "node-id.json"), []byte(`{"node_uuid":"node-a-uuid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = statusPair(cfg).(map[string]any)
	if got["mode"] != "local ingress" || got["relay"] != nil {
		t.Fatalf("with node-id.json = %v", got)
	}
}
