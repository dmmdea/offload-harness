package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// D8: `install client` seeds pair_workloads_enabled, so a client on a PAIR member cards the work it
// sends to the fleet. Enabled() still demands PAIR's node-id.json, so the key is inert on a box with
// no PAIR: seeding it true costs nothing there.
func TestInstallClientSeedsPairWorkloadsEnabled(t *testing.T) {
	home := t.TempDir()
	if _, err := installClientInto(t, home); err != nil {
		t.Fatalf("install client: %v", err)
	}
	path := filepath.Join(home, "etc", "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["pair_workloads_enabled"].(bool); !ok || !v {
		t.Fatalf("the rendered config seeds pair_workloads_enabled = %v (present %v), want true", m["pair_workloads_enabled"], ok)
	}
	cfg, err := config.Load(path)
	if err != nil || !cfg.PairWorkloadsEnabled {
		t.Fatalf("the loaded client config has PairWorkloadsEnabled = %v (%v), want true", cfg.PairWorkloadsEnabled, err)
	}

	// Inert where PAIR is not installed: no node-id.json, no emitter.
	t.Setenv("OFFLOAD_PAIR_APPDIR", t.TempDir())
	if pairworkloads.New(pairworkloads.FromConfig(cfg)).Enabled() {
		t.Fatal("a client config on a box with no PAIR must not enable the emitter")
	}
}
