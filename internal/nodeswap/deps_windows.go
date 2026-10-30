//go:build windows

package nodeswap

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

func platformDeps(d *Deps) {
	d.FindProcessesByExe = findProcessesByExeWindows
	d.StopProcess = stopProcessWindows
}

// runPlatformCommand runs a --restart-command (and the scheduled-task verbs)
// through PowerShell on Windows, hidden.
func runPlatformCommand(ctx context.Context, timeout time.Duration, command string) (string, error) {
	return runPowerShell(ctx, timeout, command)
}

type cimProcRow struct {
	ProcessId      int    `json:"ProcessId"`
	CommandLine    string `json:"CommandLine"`
	ExecutablePath string `json:"ExecutablePath"`
	Name           string `json:"Name"`
}

// findProcessesByExeWindows enumerates every process whose ExecutablePath
// matches exePath via CIM (Win32_Process) — the same primitive
// fleet-node-restart.ps1 and deploy-node-exe.ps1 use, and the one proven to
// work unelevated on a SYSTEM-owned process where Get-Process's .StartTime
// throws Access Denied (service_process_windows.go's glueProbeViaCIM has the
// same reasoning). Matching by ExecutablePath (not Name) is deliberate: the
// caller passes the exact --target path, and this box can run more than one
// exe with the same base name (local-offload.exe vs
// local-offload-fleet.exe, per the 2026-09-23/24 <node-b> deploy records) — a
// name-only filter would return the wrong process's holders.
//
// The one exception is a row the OS will not describe: CIM returns an EMPTY ExecutablePath and
// CommandLine for a process that runs elevated (or as another account) when this process is not
// elevated. Measured 2026-10-19 on a Windows node: a fleet node restarted over `ssh localhost` ran
// elevated in session 0, and an unelevated swap saw no node to stop and no new node to verify,
// then rolled back against the running node. Such a row is returned when its Name is the target's
// file name, marked Unreadable, so the swap can refuse instead of guessing.
func findProcessesByExeWindows(exePath string) ([]ProcessInfo, error) {
	abs, err := filepath.Abs(exePath)
	if err != nil {
		abs = exePath
	}
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | `+
			`Where-Object { ($_.ExecutablePath -and ($_.ExecutablePath -ieq %s)) -or (-not $_.ExecutablePath -and ($_.Name -ieq %s)) } | `+
			`Select-Object ProcessId,CommandLine,ExecutablePath,Name | ConvertTo-Json -Compress`,
		psQuote(abs), psQuote(filepath.Base(abs)))
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
		procs = append(procs, ProcessInfo{PID: r.ProcessId, CommandLine: r.CommandLine, ExecutablePath: r.ExecutablePath, Unreadable: r.ExecutablePath == ""})
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
	out, err := winexec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// "not found" means it already exited — not a failure to report as one.
		if strings.Contains(strings.ToLower(msg), "not found") {
			return nil
		}
		return fmt.Errorf("taskkill pid %d: %w (%s)", pid, err, msg)
	}
	return nil
}
