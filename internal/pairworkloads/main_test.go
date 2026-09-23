package pairworkloads

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the open-card register (orphans.go) off the machine: every
// enabled emitter in this package writes markers under the machine-wide state
// root, and a sweep there would close the operator's own orphaned cards into a
// test's httptest ingress. LOCAL_OFFLOAD_STATE_DIR is the override
// gpulease.ResolveStateRoot honours in production.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lo-pairworkloads-state-")
	if err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not isolate the state root: " + err.Error() + "\n")
		os.Exit(m.Run())
	}
	if err := os.Setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("pairworkloads tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
