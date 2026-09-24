//go:build !windows

package nodeswap

import "testing"

// TestPlatformDepsFindProcessesByExeIsEmptySuccessNotAnError pins the fix for
// a real Linux CI failure (PR fix/model-switch-gaps, gap 6a): stopForSwap
// (nodeswap.go) calls Deps.FindProcessesByExe UNCONDITIONALLY, on every
// platform, before any restart-mechanism check — so an ERROR here used to
// fail EVERY real node-swap run on Linux at the very first step, even a
// standalone binary-only swap with nothing to stop. A live Linux binary can
// be renamed/replaced out from under a running process with no lock at all
// (the documented rename-over-open-inode pattern), so "no holders" is the
// correct, successful answer here — never an error.
func TestPlatformDepsFindProcessesByExeIsEmptySuccessNotAnError(t *testing.T) {
	var d Deps
	platformDeps(&d)
	if d.FindProcessesByExe == nil {
		t.Fatal("platformDeps must set FindProcessesByExe")
	}
	procs, err := d.FindProcessesByExe("/opt/offload/bin/local-offload")
	if err != nil {
		t.Fatalf("FindProcessesByExe on a non-Windows platform must succeed with no holders, got error: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("FindProcessesByExe on a non-Windows platform must report no holders, got %+v", procs)
	}
}
