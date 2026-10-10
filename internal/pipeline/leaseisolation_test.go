package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/gpuprobe/smitest"
)

// TestMain makes this package's tests HERMETIC with respect to the GPU lease.
//
// The generation paths now take a real machine-wide lease before spawning a render.
// Without this redirect the test suite acquires the ACTUAL lease under
// %ProgramData%\local-offload — verified by watching gpu/epoch and gpu/lease change
// timestamps during a test run. That is shared mutable state, and it cuts both ways:
// a suite running while a real render holds the card gets ErrHeld and the gen tests
// fail for reasons that have nothing to do with the code, and a suite running while
// nothing else is active can make a real render defer. A crashed test would leave a
// lease behind on the operator's machine.
//
// LOCAL_OFFLOAD_STATE_DIR is the same override gpulease.ResolveStateRoot honours in
// production, so this exercises the real resolution path rather than bypassing it.
func TestMain(m *testing.M) {
	// First: when this process is the stand-in nvidia-smi a test installed (internal/gpuprobe/smitest) it
	// plays that part and exits, touching neither the state root nor a test.
	smitest.MaybeRun()
	dir, err := os.MkdirTemp("", "lo-pipeline-lease-")
	if err != nil {
		// Better to run non-hermetically than not at all, but say so loudly.
		os.Stderr.WriteString("pipeline tests: could not isolate the GPU lease state root: " + err.Error() + "\n")
		os.Exit(m.Run())
	}
	// Set before any test constructs a Pipeline.
	if err := os.Setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pipeline tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
	}
	// The offload home holds the ledger and, beside it, the footprint store every sampled render records its
	// VRAM and host peaks into; config.Default() fixes those paths when it is called. A suite that builds its
	// config with it must land in a throwaway home, not the operator's (TestMainIsolatesTheHomeAndTheFootprintStore).
	if err := os.Setenv("LOCAL_OFFLOAD_HOME", filepath.Join(filepath.Clean(dir), "home")); err != nil {
		os.Stderr.WriteString("pipeline tests: could not set LOCAL_OFFLOAD_HOME: " + err.Error() + "\n")
	}
	// A media admission reads the host's memory before it grants a lease (internal/gpulease/hostram.go):
	// the suite runs against a known host, not against whatever the machine running it is doing.
	restoreHost := gpuprobe.UseHostMemoryReader(func() (gpuprobe.HostMemory, bool) { return roomyHostMem, true })
	code := m.Run()
	restoreHost()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestMainIsolatesTheHomeAndTheFootprintStore pins the half of the safety net the host-RAM measurement needs
// (G3 of the P0 plan): every sampled render now records its host peaks beside its VRAM peak, and the store those
// land in is derived from the ledger path config.Default() names. Without an isolated offload home a suite that
// builds its config with config.Default() would write the operator's REAL footprints.json on every run, and the
// guard would then calibrate production declarations from fixtures.
func TestMainIsolatesTheHomeAndTheFootprintStore(t *testing.T) {
	state := filepath.Clean(os.Getenv("LOCAL_OFFLOAD_STATE_DIR"))
	home := filepath.Clean(os.Getenv("LOCAL_OFFLOAD_HOME"))
	if os.Getenv("LOCAL_OFFLOAD_HOME") == "" || !strings.HasPrefix(home, state+string(filepath.Separator)) {
		t.Fatalf("LOCAL_OFFLOAD_HOME = %q: TestMain must point it inside its throwaway state root %q", home, state)
	}
	p := &Pipeline{cfg: config.Default()}
	if got := filepath.Clean(p.footprintsPath()); !strings.HasPrefix(got, home+string(filepath.Separator)) {
		t.Fatalf("the default footprint store = %q, want it under the isolated home %q", got, home)
	}
}
