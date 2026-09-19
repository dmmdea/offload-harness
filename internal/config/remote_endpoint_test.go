package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// A config whose endpoint is another box's engine disarms this box's
// text-load gate (register C-58): no local load happens, so the machine-wide
// lease has nothing to protect and must not cordon the runs. A loopback
// endpoint arms it as before.
func TestLoadDisarmsTheGPULeaseGateForARemoteEndpoint(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	write := func(endpoint string) string {
		p := filepath.Join(dir, "config-"+filepath.Base(endpoint)+".json")
		body := `{"endpoint":"` + endpoint + `","state_dir":"` + filepath.ToSlash(state) + `"}`
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := Load(write("http://127.0.0.1:11434")); err != nil {
		t.Fatal(err)
	}
	if modelaffinity.GPULeaseDir() == "" {
		t.Fatal("a loopback endpoint must arm the gate")
	}
	if _, err := Load(write("http://node-b:18797")); err != nil {
		t.Fatal(err)
	}
	if got := modelaffinity.GPULeaseDir(); got != "" {
		t.Fatalf("a remote endpoint must disarm the gate, still armed at %q", got)
	}
	if _, err := Load(write("http://127.0.0.1:11434")); err != nil {
		t.Fatal(err)
	}
	if modelaffinity.GPULeaseDir() == "" {
		t.Fatal("loading a loopback config again must re-arm the gate")
	}
}
