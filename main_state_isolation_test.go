package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// stateIsolationPrefix names the root package's throwaway state root, so
// TestMainIsolatesTheStateRoot can tell the TestMain's directory from one a
// caller exported.
const stateIsolationPrefix = "lo-root-state-"

// TestMain keeps the root package's tests off the machine-wide state root
// (%ProgramData%\local-offload on Windows), the root seat-inflight, the GPU
// lease, the delegate intent ledger and the PAIR open-card register all share.
//
// WHY. TestLeaseCardLifecycle built an enabled PAIR emitter with no StateDir;
// the register resolved to the machine-wide pair-open directory, and the
// emitter's first sweep closed the operator's REAL orphan markers into the
// test's httptest ingress and deleted them, so the real cards stayed "Running"
// (a 31.9 h ghost card, and the recorded `frames = 4` flake). A test that
// forgets its own isolation must land in a throwaway root, not the real one.
//
// LOCAL_OFFLOAD_STATE_DIR is the override gpulease.ResolveStateRoot honours in
// production, and an explicit cfg.StateDir still wins over it, so the tests that
// set their own root (leaseFixture and the gpu_* tests) are unchanged; none of
// this package's tests depends on the default resolution. Same shape as
// internal/pipeline/leaseisolation_test.go and internal/pairworkloads/main_test.go.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", stateIsolationPrefix)
	if err != nil {
		os.Stderr.WriteString("root tests: could not isolate the state root: " + err.Error() + "\n")
		os.Exit(m.Run())
	}
	if err := os.Setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("root tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestMainIsolatesTheStateRoot pins the safety net: the default state-root
// resolution is the TestMain's throwaway directory, not the machine's.
func TestMainIsolatesTheStateRoot(t *testing.T) {
	env := os.Getenv("LOCAL_OFFLOAD_STATE_DIR")
	if !strings.HasPrefix(filepath.Base(env), stateIsolationPrefix) {
		t.Fatalf("LOCAL_OFFLOAD_STATE_DIR = %q: the root package's TestMain must point it at its own throwaway directory", env)
	}
	got, err := gpulease.ResolveStateRoot("")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(env)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("default state root = %q, want the isolated %q", got, want)
	}
}
