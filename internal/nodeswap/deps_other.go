//go:build !windows

package nodeswap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
//
// VERIFICATION is a different question (0.143.0): after a --restart-command
// the swap must find the restarted node running the new image, and an empty
// finder made every Linux swap with a restart mechanism fail its proof and
// roll back — the 2026-09-29 parity deploy had to restart both Linux nodes
// by hand and leave final_pid 0 in their results. FindRunningByExe
// reads /proc for the processes whose executable is the target.
func platformDeps(d *Deps) {
	d.FindProcessesByExe = func(string) ([]ProcessInfo, error) {
		return nil, nil
	}
	d.FindRunningByExe = func(exePath string) ([]ProcessInfo, error) {
		return findRunningByExeProc("/proc", exePath)
	}
	d.StopProcess = func(pid int) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return p.Kill()
	}
}

// runPlatformCommand runs a --restart-command through the POSIX shell (a
// `systemctl restart <unit>` in every Linux deploy record). Until 0.143.0 it
// ran through `powershell`, which Linux nodes do not have: "restart command
// failed: exec: "powershell": executable file not found in $PATH".
func runPlatformCommand(ctx context.Context, timeout time.Duration, command string) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "/bin/sh", "-c", command).CombinedOutput()
	return string(out), err
}

// findRunningByExeProc lists the processes under procRoot whose executable
// (/proc/<pid>/exe) is exePath — resolved through symlinks on both sides, so a
// node launched through a symlinked path (a second copy under /usr/local/bin vs
// /opt/offload/bin) still matches its real file. A replaced image reads
// "<path> (deleted)": that process runs the OLD binary and is not a match.
// Unreadable entries (another user's process, a PID that exited mid-scan) are
// skipped, never an error.
func findRunningByExeProc(procRoot, exePath string) ([]ProcessInfo, error) {
	want := exePath
	if abs, err := filepath.Abs(exePath); err == nil {
		want = abs
	}
	if r, err := filepath.EvalSymlinks(want); err == nil {
		want = r
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []ProcessInfo
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || !e.IsDir() {
			continue
		}
		exe, lerr := os.Readlink(filepath.Join(procRoot, e.Name(), "exe"))
		if lerr != nil || strings.HasSuffix(exe, " (deleted)") {
			continue
		}
		if r, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = r
		}
		if exe != want {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		cmdline := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
		out = append(out, ProcessInfo{PID: pid, CommandLine: cmdline, ExecutablePath: exe})
	}
	return out, nil
}
