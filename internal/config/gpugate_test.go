package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// Load is the ONE place the model-affinity load gate is armed, because it is the
// one place every entry point that can make a text call passes through. If that
// wiring is ever dropped, the gate goes quiet rather than loud — text resumes
// loading models on top of running renders and nothing reports it. So the wiring
// itself is pinned here, on the honest observation point (modelaffinity.GPULeaseDir)
// rather than on a re-derivation that would pass even with the call removed.
func TestLoadArmsTheGPULoadGate(t *testing.T) {
	root := t.TempDir()
	body, err := json.Marshal(map[string]string{"state_dir": root})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The gate must resolve through gpulease.LeaseDir — THE one resolver — so it
	// reads the SAME directory the media path writes. Comparing against LeaseDir's
	// own answer is what makes a second resolution order impossible to introduce
	// here without this test noticing.
	want, err := gpulease.LeaseDir("", root)
	if err != nil {
		t.Fatalf("LeaseDir: %v", err)
	}
	if got := modelaffinity.GPULeaseDir(); got != want {
		t.Fatalf("gate armed at %q after loading state_dir=%q, want %q", got, root, want)
	}
}

// The empty path is the fresh-install case: Load returns built-in defaults. It
// takes a DIFFERENT exit out of the function, and the gate must still be armed on
// it — a box with no config file still renders and still serves text.
func TestLoadArmsTheGPULoadGateOnDefaults(t *testing.T) {
	if _, err := Load(""); err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	want, err := gpulease.LeaseDir("", "")
	if err != nil {
		t.Skipf("platform lease dir unresolvable here: %v", err)
	}
	if got := modelaffinity.GPULeaseDir(); got != want {
		t.Fatalf("gate armed at %q on default config, want the platform default %q", got, want)
	}
}
