//go:build !windows

package nodeswap

import (
	"os"
	"os/exec"
)

// hideWindow is a no-op off Windows: there is no console window to hide
// (same convention as gpu_hide_other.go / internal/accelclient/sidecar_other.go).
func hideWindow(*exec.Cmd) {}

// platformDeps reports NO holders on non-Windows — never an error — rather
// than faking a Linux/macOS process-holder story this package does not
// implement. This is not a missing feature: a live Linux binary can be
// renamed/replaced out from under a running process with no lock at all
// (the existing rename-over-open-inode pattern documented in the 2026-09-24
// deploy record), so there is nothing to enumerate OR wait for here — the
// swap simply does not need it. It matters that this is empty-success, not
// an error: stopForSwap (nodeswap.go) calls FindProcessesByExe
// UNCONDITIONALLY, on every platform, before any restart-mechanism check —
// an error here used to fail EVERY real (non-faked-Deps) node-swap run on
// Linux, at the very first step, even a standalone binary-only swap with
// nothing to stop (caught by TestRunNodeSwap_ConfigAutoResolveIsHermetic-
// WithAnExplicitConfigFlag failing on Linux CI while passing on Windows).
// StopProcess is still real (kill by PID): it is simply never called with
// zero holders to stop.
func platformDeps(d *Deps) {
	d.FindProcessesByExe = func(string) ([]ProcessInfo, error) {
		return nil, nil
	}
	d.StopProcess = func(pid int) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Kill()
	}
}
