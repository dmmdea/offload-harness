package vllmseat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seatCmdDriver stands in for the cmdlets seat-cmd.ps1 calls, so the RENDERED stub runs for real in a PowerShell with no
// scheduled task, no network and no waiting. Every call is written to a call log the test reads back in order.
//
// The scenario decides the world. The seat answers /v1/models at once and /health twice, then stops answering (the
// crash). The stub's own start task is Running until then and Ready after the crash, unless the scenario says a live
// launcher still holds it. The stop task is Ready until it is started, then Running for three reads, then Ready — or
// Running forever, or refusing to start, or never seen Running at all (a Task Scheduler slow to start it).
const seatCmdDriver = `param([string]$Stub, [string]$Scenario, [string]$Calls)
$ErrorActionPreference = 'Continue'
$global:Scn = $Scenario
$global:CallLog = $Calls
$global:Probes = 0
$global:StopReads = 0
function Note([string]$m) { Add-Content -Path $global:CallLog -Value $m }
function Start-Sleep { param($Seconds) }
function Start-ScheduledTask {
  [CmdletBinding()] param([string]$TaskName)
  Note "start-task $TaskName"
  if ($TaskName -like 'vllm-seat-stop-*') {
    if ($global:Scn -eq 'stop-task-missing') { throw 'The system cannot find the file specified.' }
    $global:StopReads = 0
  }
}
function Get-ScheduledTask {
  [CmdletBinding()] param([string]$TaskName)
  if ($TaskName -like 'vllm-seat-stop-*') {
    $global:StopReads++
    Note "read-stop-task $($global:StopReads)"
    if ($global:Scn -eq 'stop-task-never-runs') { return [pscustomobject]@{ State = 'Ready' } }
    if ($global:Scn -eq 'stop-task-hangs' -or $global:StopReads -le 3) { return [pscustomobject]@{ State = 'Running' } }
    return [pscustomobject]@{ State = 'Ready' }
  }
  if ($global:Scn -eq 'live-launcher') { return [pscustomobject]@{ State = 'Running' } }
  if ($global:Probes -ge 3) { return [pscustomobject]@{ State = 'Ready' } }
  return [pscustomobject]@{ State = 'Running' }
}
function Invoke-RestMethod {
  [CmdletBinding()] param([string]$Uri, [int]$TimeoutSec)
  if ($Uri -like '*/v1/models') { return [pscustomobject]@{ data = @([pscustomobject]@{ id = 'qwen3.8-27b-vllm' }) } }
  $global:Probes++
  Note "health $($global:Probes)"
  if ($global:Probes -le 2) { return $null }
  throw 'Unable to connect to the remote server'
}
& $Stub -Seat pp3
Note "exit=$LASTEXITCODE"
`

// powerShellHosts lists the PowerShells on this box, the one llama-swap's stub runs under first: Windows PowerShell 5.1 on
// Windows, PowerShell 7 everywhere (it is what a Linux runner has).
func powerShellHosts() []string {
	var hosts []string
	for _, n := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(n); err == nil {
			hosts = append(hosts, p)
		}
	}
	return hosts
}

// runSeatCmd renders seat-cmd.ps1 from the shipped template with a scratch seat directory, runs it in the given
// PowerShell under the driver for one scenario, and returns the call log and the stub's own log.
func runSeatCmd(t *testing.T, host, scenario string) (calls, stubLog string) {
	t.Helper()
	dir := t.TempDir()
	rt := wslRT()
	rt.SeatDir = filepath.ToSlash(dir)
	rt.ProxyHost = "192.0.2.10"
	files, err := pinned().Artifacts(wslTemplatesDir(), rt)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "seat-cmd.ps1")
	driver := filepath.Join(dir, "driver.ps1")
	callLog := filepath.Join(dir, "calls.txt")
	if err := os.WriteFile(stub, []byte(files["seat-cmd.ps1"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, []byte(seatCmdDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, host, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", driver, "-Stub", stub, "-Scenario", scenario, "-Calls", callLog).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: the driver failed (%v):\n%s", scenario, err, out)
	}
	raw, _ := os.ReadFile(callLog)
	logRaw, _ := os.ReadFile(filepath.Join(dir, "seat-cmd-pp3.log"))
	// Out-File writes UTF-16 in Windows PowerShell 5.1: drop the NULs so the log reads as text.
	calls = strings.ReplaceAll(string(raw), "\r", "")
	stubLog = strings.ReplaceAll(strings.ReplaceAll(string(logRaw), "\x00", ""), "\r", "")
	t.Logf("%s via %s\ncalls:\n%s\nstub log:\n%s", scenario, filepath.Base(host), calls, stubLog)
	// In every scenario: the stub starts the seat's own task once, at its start, and never again. A crash is cleaned up,
	// not relaunched: an idle seat stays unloaded (ttl 300), and llama-swap starts it again on the next request.
	if n := countLines(calls, "start-task vllm-seat-pp3"); n != 1 {
		t.Errorf("%s: the stub started the seat's own task %d times; the crash cleanup must never relaunch the seat:\n%s", scenario, n, calls)
	}
	return calls, stubLog
}

func countLines(s, line string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == line {
			n++
		}
	}
	return n
}

// TestSeatCmdCrashExitReapsTheDeadGenerationThroughTheStopTask (2026-09-29 crash refusal loop). The stub exits when the
// seat stops answering for 30 s — a crash — and nothing else runs a stop for one, so the workers and the MP server the
// dead API server left behind outlived it and every restart refused. The exit now runs the operator-session stop task
// once (the task seat-cmdstop.ps1 uses), waits for it (bounded), and only then exits 0. It is a cleanup, never a
// relaunch; it does nothing while a live launcher still owns the seat; and a stop task that will not start or never
// finishes cannot keep the stub from exiting. Runs in every PowerShell the box has (5.1 is what llama-swap runs).
func TestSeatCmdCrashExitReapsTheDeadGenerationThroughTheStopTask(t *testing.T) {
	hosts := powerShellHosts()
	if len(hosts) == 0 {
		t.Skip("no PowerShell on this box: seat-cmd.ps1 runs under it")
	}
	for _, host := range hosts {
		host := host
		t.Run(strings.TrimSuffix(filepath.Base(host), ".exe"), func(t *testing.T) { seatCmdCrashScenarios(t, host) })
	}
}

func seatCmdCrashScenarios(t *testing.T, host string) {
	t.Run("crash: stop task run once, waited for, then exit 0, and no relaunch", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "crash")
		lines := strings.Split(strings.TrimSpace(calls), "\n")
		stopAt, exitAt, lastRead := -1, -1, -1
		for i, l := range lines {
			switch {
			case l == "start-task vllm-seat-stop-pp3":
				stopAt = i
			case strings.HasPrefix(l, "read-stop-task "):
				lastRead = i
			case strings.HasPrefix(l, "exit="):
				exitAt = i
			}
		}
		if countLines(calls, "start-task vllm-seat-stop-pp3") != 1 {
			t.Fatalf("the crash exit must start the stop task exactly once:\n%s", calls)
		}
		if !(stopAt >= 0 && lastRead > stopAt && exitAt > lastRead) {
			t.Fatalf("the stub must start the stop task, wait for it to finish, and only then exit (stop %d, last read %d, exit %d):\n%s", stopAt, lastRead, exitAt, calls)
		}
		if !strings.Contains(calls, "read-stop-task 4") || strings.Contains(calls, "read-stop-task 5") {
			t.Fatalf("the wait must end when the task stops running (3 Running reads, then Ready):\n%s", calls)
		}
		if !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Fatalf("the crash exit stays exit 0 (llama-swap reads it as a stopped seat):\n%s", calls)
		}
		for _, want := range []string{"seat stopped answering (30 s) - exiting so llama-swap sees it", "crash cleanup: vllm-seat-stop-pp3 started", "crash cleanup finished"} {
			if !strings.Contains(log, want) {
				t.Errorf("the stub's log lost %q:\n%s", want, log)
			}
		}
	})

	t.Run("a live launcher owns the seat: nothing is reaped", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "live-launcher")
		if strings.Contains(calls, "start-task vllm-seat-stop-pp3") {
			t.Fatalf("the stop task ran while the seat's start task was still running — that kills a live engine:\n%s", calls)
		}
		if !strings.Contains(log, "crash cleanup skipped") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Fatalf("a skipped cleanup must say so and the stub must still exit 0:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
	})

	t.Run("a stop task that will not start only logs; the exit is unchanged", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "stop-task-missing")
		if !strings.Contains(log, "failed to start") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Fatalf("a missing stop task must be logged and must not change the exit:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
		if strings.Contains(calls, "read-stop-task") {
			t.Fatalf("nothing to wait for when the stop task never started:\n%s", calls)
		}
	})

	t.Run("a stop task never seen running is given 30 s to appear, then the stub says so and exits", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "stop-task-never-runs")
		if n := strings.Count(calls, "read-stop-task "); n != 15 {
			t.Fatalf("a task that never shows Running must be given 15 reads (30 s) to appear, got %d:\n%s", n, calls)
		}
		if !strings.Contains(log, "not seen running in 30 s") || strings.Contains(log, "crash cleanup finished") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Fatalf("a stop task never seen running is not a finished cleanup: it must say so, and the stub must still exit 0:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
	})

	t.Run("a stop task that never finishes is waited for a bounded time", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "stop-task-hangs")
		if n := strings.Count(calls, "read-stop-task "); n != 45 {
			t.Fatalf("the wait must be bounded at 45 reads of the stop task, got %d:\n%s", n, calls)
		}
		if !strings.Contains(log, "still running after 90 s") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Fatalf("a cleanup that outlasts its bound must say so and the stub must still exit 0:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
	})
}
