package pipeline

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// A copy of this binary that a browse test re-launched as the fake sidecar (TestBrowseHelperProcess) speaks
	// stdio and ends with os.Exit from inside the test, so it never reaches the RemoveAll below: a root of its
	// own was left in the OS temp dir by every launch (13 per run, counted with the temp dir pointed at an
	// empty folder). It touches no lease, home or ledger, so it runs without one.
	if isBrowseSidecarCopy() {
		os.Exit(m.Run())
	}
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
	os.Exit(finishIsolation(code, dir, os.RemoveAll))
}

// isolationStderr is where finishIsolation reports a throwaway root it could not remove; os.Stderr, except in the test of that report.
var isolationStderr io.Writer = os.Stderr

// removeRetryDelay is how long finishIsolation waits before it tries a removal that failed once more. On Windows a process that has just
// ended (or a scanner) can still hold a file of the folder, or the folder itself as its working directory, for a moment.
const removeRetryDelay = 250 * time.Millisecond

// removeStateRoot removes the suite's throwaway state root. A removal that fails is tried once more after removeRetryDelay, and a root
// that is still there after that is named on isolationStderr with the error, so that a leak is seen where it is made and not found later as
// a folder in the OS temp dir, and the error is answered so that finishIsolation can fail the package: `go test ./...` shows the stderr of a
// package that passed only with -v, so the line alone reaches no one.
func removeStateRoot(dir string, removeAll func(string) error) error {
	err := removeAll(dir)
	if err == nil {
		return nil
	}
	time.Sleep(removeRetryDelay)
	if err = removeAll(dir); err != nil {
		fmt.Fprintf(isolationStderr, "pipeline tests: could not remove the throwaway state root %s: %v (a process may still hold it open; remove it by hand); the package fails so that this is not missed\n", filepath.Clean(dir), err)
	}
	return err
}

// finishIsolation ends the run: it removes the throwaway state root and answers the exit code. A root that stays behind fails a passing
// suite (exit 1) and leaves a failing suite's own code alone: the tests ran, but the isolation net did not clean up after them.
func finishIsolation(code int, dir string, removeAll func(string) error) int {
	if removeStateRoot(dir, removeAll) != nil && code == 0 {
		return 1
	}
	return code
}

// isBrowseSidecarCopy reports whether this process is the test binary re-launched as the fake browse sidecar.
// The sidecar's environment is an allowlist (browseRunnerEnv), so no variable the suite set can reach it:
// its argument is what identifies it, the same check TestBrowseHelperProcess makes to know it is the child.
func isBrowseSidecarCopy() bool {
	return len(os.Args) >= 2 && os.Args[1] == browseHelperArg
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
