package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The load itself refuses a mistyped fleet_ingress_guard, naming the key (review: the mode
// test called validate directly, so a Load that skipped validation stayed green).
func TestLoadRefusesAMistypedIngressGuard(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"fleet_ingress_guard": "enforced"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "fleet_ingress_guard") {
		t.Fatalf("Load accepted a mistyped mode or did not name the key: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"fleet_ingress_guard": "enforce"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(p); err != nil || c.FleetIngressGuardMode() != "enforce" {
		t.Fatalf("a valid mode did not load: %v / %q", err, c.FleetIngressGuardMode())
	}
}
