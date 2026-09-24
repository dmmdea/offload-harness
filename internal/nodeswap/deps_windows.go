//go:build windows

package nodeswap

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// hideWindow makes a spawned child window-less — same convention as
// gpu_hide_windows.go and internal/accelclient/sidecar_windows.go: a visible
// console on this operator's desktop gets closed by hand, which kills
// whatever ran inside it (house rule, windows-shell-traps memory,
// "invisible-shell-invocations").
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

func platformDeps(d *Deps) {
	d.FindProcessesByExe = findProcessesByExeWindows
	d.StopProcess = stopProcessWindows
}

type cimProcRow struct {
	ProcessId      int    `json:"ProcessId"`
	CommandLine    string `json:"CommandLine"`
	ExecutablePath string `json:"ExecutablePath"`
}

// findProcessesByExeWindows enumerates every process whose ExecutablePath
// matches exePath via CIM (Win32_Process) — the same primitive
// fleet-node-restart.ps1 and deploy-node-exe.ps1 use, and the one proven to
// work unelevated on a SYSTEM-owned process where Get-Process's .StartTime
// throws Access Denied (service_process_windows.go's glueProbeViaCIM has the
// same reasoning). Matching by ExecutablePath (not Name) is deliberate: the
// caller passes the exact --target path, and this box can run more than one
// exe with the same base name (local-offload.exe vs
// local-offload-fleet.exe, per the 2026-09-23/24 Qube deploy records) — a
// name-only filter would return the wrong process's holders.
func findProcessesByExeWindows(exePath string) ([]ProcessInfo, error) {
	abs, err := filepath.Abs(exePath)
	if err != nil {
		abs = exePath
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | `+
			`Where-Object { $_.ExecutablePath -and ($_.ExecutablePath -ieq %s) } | `+
			`Select-Object ProcessId,CommandLine,ExecutablePath | ConvertTo-Json -Compress`,
		psQuote(abs))
	raw, err := runPowerShell(context.Background(), 20*time.Second, script)
	if err != nil {
		return nil, fmt.Errorf("CIM process query failed: %w (output: %s)", err, truncate(raw, 300))
	}
	out := []byte(strings.TrimSpace(raw))
	if len(out) == 0 {
		return nil, nil
	}
	rows, perr := parseCimRows(out)
	if perr != nil {
		return nil, fmt.Errorf("parsing CIM process query output: %w (raw: %s)", perr, truncate(string(out), 300))
	}
	procs := make([]ProcessInfo, 0, len(rows))
	for _, r := range rows {
		procs = append(procs, ProcessInfo{PID: r.ProcessId, CommandLine: r.CommandLine, ExecutablePath: r.ExecutablePath})
	}
	return procs, nil
}

// parseCimRows handles ConvertTo-Json's shape quirk: a single match returns
// a bare object, not a one-element array.
func parseCimRows(out []byte) ([]cimProcRow, error) {
	var rows []cimProcRow
	if err := json.Unmarshal(out, &rows); err == nil {
		return rows, nil
	}
	var one cimProcRow
	if err := json.Unmarshal(out, &one); err != nil {
		return nil, err
	}
	return []cimProcRow{one}, nil
}

// stopProcessWindows kills a process by PID via taskkill /T /F, the same
// primitive internal/gpugen and internal/mediaops already use on this OS for
// the identical reason: Stop-Process needs an owned handle and can be
// blocked by the same ACL edge cases taskkill routes around at the OS level.
func stopProcessWindows(pid int) error {
	out, err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// "not found" means it already exited — not a failure to report as one.
		if strings.Contains(strings.ToLower(msg), "not found") {
			return nil
		}
		return fmt.Errorf("taskkill pid %d: %v (%s)", pid, err, msg)
	}
	return nil
}
