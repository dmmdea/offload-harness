// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// glueProbeProcess locates the llama-swap process without elevation.
//
// CIM (Win32_Process) is queried FIRST and Get-Process second, and the order is
// the whole point. Get-Process returns the object but throws
// "Access is denied" on .StartTime for a process owned by another principal —
// and llama-swap runs here as a SYSTEM-principal scheduled task. CIM reads
// ProcessId and CreationDate from the object manager instead of opening a
// process handle, so it answers unelevated where Get-Process cannot.
//
// When neither can produce a start time, the process is reported as found with
// details_readable=false and an explicit note. That is a different fact from
// "not running", and conflating the two is what turns a healthy service into a
// phantom outage report.
func glueProbeProcess(ctx context.Context) glueProcessInfo {
	if info, ok := glueProbeViaCIM(ctx); ok {
		return info
	}
	return glueProbeViaGetProcess(ctx)
}

// gluePowerShell runs a PowerShell snippet with no visible window. A console
// flashing over the operator's work gets closed by reflex, which on this box
// has killed processes under observation.
func gluePowerShell(ctx context.Context, script string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	hideSpawnedWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func glueProbeViaCIM(ctx context.Context) (glueProcessInfo, bool) {
	const script = `$p = Get-CimInstance Win32_Process -Filter "Name='llama-swap.exe'" -ErrorAction SilentlyContinue | Select-Object -First 1
if ($null -eq $p) { '{"found":false}' } else {
  [pscustomobject]@{ found=$true; pid=$p.ProcessId; start=$p.CreationDate.ToString("o") } | ConvertTo-Json -Compress
}`
	raw, err := gluePowerShell(ctx, script)
	if err != nil {
		return glueProcessInfo{}, false
	}
	var parsed struct {
		Found bool   `json:"found"`
		PID   int    `json:"pid"`
		Start string `json:"start"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &parsed) != nil {
		return glueProcessInfo{}, false
	}
	if !parsed.Found {
		return glueProcessInfo{Found: false, Note: "no llama-swap.exe process (Win32_Process)"}, true
	}
	info := glueProcessInfo{Found: true, PID: parsed.PID}
	if t, terr := time.Parse(time.RFC3339, parsed.Start); terr == nil {
		info.StartTime = t.Format(time.RFC3339)
		info.Uptime = time.Since(t).Round(time.Second).String()
		info.DetailsReadable = true
		return info, true
	}
	info.Note = "start time unparseable from Win32_Process"
	return info, true
}

func glueProbeViaGetProcess(ctx context.Context) glueProcessInfo {
	const script = `$p = Get-Process -Name llama-swap -ErrorAction SilentlyContinue | Select-Object -First 1
if ($null -eq $p) { '{"found":false}' } else {
  $start = ''
  try { $start = $p.StartTime.ToString("o") } catch { $start = '' }
  [pscustomobject]@{ found=$true; pid=$p.Id; start=$start } | ConvertTo-Json -Compress
}`
	raw, err := gluePowerShell(ctx, script)
	if err != nil {
		return glueProcessInfo{Note: "process probe failed: " + err.Error()}
	}
	var parsed struct {
		Found bool   `json:"found"`
		PID   int    `json:"pid"`
		Start string `json:"start"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &parsed) != nil {
		return glueProcessInfo{Note: "process probe returned unparseable output: " + truncate(strings.TrimSpace(string(raw)), 120)}
	}
	if !parsed.Found {
		return glueProcessInfo{Found: false, Note: "no llama-swap process"}
	}
	info := glueProcessInfo{Found: true, PID: parsed.PID}
	if t, terr := time.Parse(time.RFC3339, parsed.Start); terr == nil {
		info.StartTime = t.Format(time.RFC3339)
		info.Uptime = time.Since(t).Round(time.Second).String()
		info.DetailsReadable = true
		return info
	}
	info.Note = "SYSTEM-owned, details need elevation"
	return info
}
