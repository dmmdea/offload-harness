package pipeline

import (
	"os"
	"path/filepath"
	"testing"
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
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
