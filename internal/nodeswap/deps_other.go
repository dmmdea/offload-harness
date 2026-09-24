//go:build !windows

package nodeswap

import (
	"errors"
	"os"
	"os/exec"
)

// hideWindow is a no-op off Windows: there is no console window to hide
// (same convention as gpu_hide_other.go / internal/accelclient/sidecar_other.go).
func hideWindow(*exec.Cmd) {}

// platformDeps leaves FindProcessesByExe/StopProcess unset with a clear
// error rather than faking a Linux/macOS process-holder story this package
// does not implement — node-swap targets Windows fleet nodes (the OS with
// the live-exe rename trap this tool exists to fix); a Linux binary swap
// uses the existing rename-over-open-inode pattern documented in the
// 2026-09-24 deploy record, which needs none of this.
func platformDeps(d *Deps) {
	d.FindProcessesByExe = func(string) ([]ProcessInfo, error) {
		return nil, errors.New("process enumeration by executable path is implemented for windows only")
	}
	d.StopProcess = func(pid int) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Kill()
	}
}
