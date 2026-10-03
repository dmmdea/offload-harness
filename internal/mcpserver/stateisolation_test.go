package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// stateIsolationPrefix names this package's throwaway state root, so
// TestMainIsolatesTheStateRoot can tell the TestMain's directory from one a
// caller exported.
const stateIsolationPrefix = "lo-mcpserver-state-"

// TestMain keeps this package's tests off the machine-wide state root
// (%ProgramData%\local-offload on Windows), which the GPU lease, the delegate
// intent ledger (delegate-intent.jsonl), seat-inflight and the PAIR open-card
// register share.
//
// WHY. About fifty tests here drive a handler or a status read through a config.Default() with no StateDir: they appended to the operator's REAL delegate-intent.jsonl and created the real gpu/ lease and activity directories (found by a per-test sweep with %ProgramData% redirected, 2026-10-03). A test that forgets its own isolation must land in a
// throwaway root, not the real one.
//
// LOCAL_OFFLOAD_STATE_DIR is the override gpulease.ResolveStateRoot honours in
// production, and an explicit cfg.StateDir still wins over it, so the tests that
// set their own root are unchanged, and a test that needs the variable itself
// sets it with t.Setenv. Same shape as internal/pipeline/leaseisolation_test.go
// and internal/pairworkloads/main_test.go.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", stateIsolationPrefix)
	if err != nil {
		os.Stderr.WriteString("mcpserver tests: could not isolate the state root: " + err.Error() + "\n")
		os.Exit(m.Run())
	}
	if err := os.Setenv("LOCAL_OFFLOAD_STATE_DIR", filepath.Clean(dir)); err != nil {
		os.Stderr.WriteString("mcpserver tests: could not set LOCAL_OFFLOAD_STATE_DIR: " + err.Error() + "\n")
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
		t.Fatalf("LOCAL_OFFLOAD_STATE_DIR = %q: this package's TestMain must point it at its own throwaway directory", env)
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
