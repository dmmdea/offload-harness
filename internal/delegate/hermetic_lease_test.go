// Suite hygiene: the delegate tests must not read the MACHINE's GPU lease.
//
// run.go consults LocalLease(cfg.GPULockPath, cfg.StateDir) on the placement
// path, and gpulease.LeaseDir falls back to the REAL state root when both are
// empty. testCfg left both empty, so TestRunSpread*/TestRunRetries* read
// whatever lease the operator's box happened to hold and failed whenever
// another session had reserved the cards (issue #249, seen 2026-09-07 during
// the seat gate; green again once the lease was released). A suite whose
// verdict depends on what else is running on the desk is not a suite.

package delegate

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestTestCfgIsHermeticAboutTheGPULease pins the isolation itself, not a
// symptom: the lease dir every testCfg-based test resolves must live under the
// test's own temp home and must NOT be the machine's real lease dir.
func TestTestCfgIsHermeticAboutTheGPULease(t *testing.T) {
	cfg := testCfg(t)

	got, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		t.Fatalf("LeaseDir(testCfg) = error %v", err)
	}
	// The machine's real dir, resolved exactly as an unconfigured caller would.
	real, err := gpulease.LeaseDir("", "")
	if err != nil {
		t.Skipf("machine lease dir unresolvable (%v) — nothing to be isolated from", err)
	}
	if got == real {
		t.Fatalf("testCfg resolves the MACHINE lease dir %q: the suite reads whatever "+
			"lease this box holds (issue #249)", got)
	}
	home, err := filepath.Abs(cfg.Home)
	if err != nil {
		t.Fatalf("abs(home): %v", err)
	}
	if !strings.HasPrefix(got, home) {
		t.Fatalf("lease dir %q is outside the test home %q", got, home)
	}
}

// TestTestCfgLeaseIsUnaffectedByGPULockEnv pins the STRONGER guarantee the fix
// relies on: GPU_LOCK in the environment outranks StateDir inside
// gpulease.LeaseDir, so isolating on StateDir alone would still leak on a box
// where that variable is set. testCfg pins GPULockPath, which outranks both.
func TestTestCfgLeaseIsUnaffectedByGPULockEnv(t *testing.T) {
	t.Setenv("GPU_LOCK", t.TempDir())

	cfg := testCfg(t)
	got, err := gpulease.LeaseDir(cfg.GPULockPath, cfg.StateDir)
	if err != nil {
		t.Fatalf("LeaseDir(testCfg) = error %v", err)
	}
	home, err := filepath.Abs(cfg.Home)
	if err != nil {
		t.Fatalf("abs(home): %v", err)
	}
	if !strings.HasPrefix(got, home) {
		t.Fatalf("lease dir %q followed GPU_LOCK instead of the test's own home %q", got, home)
	}
}
