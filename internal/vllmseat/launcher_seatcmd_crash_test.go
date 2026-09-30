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

// stopMarker is the file seat-cmdstop.ps1 leaves in the seat directory when llama-swap asks the seat to stop, and the one
// seat-cmd.ps1 looks for when the seat stops answering: it is how the stub tells an unload from a crash. Both stubs are
// tested against this ONE name, so a drift in either fails a test.
const stopMarker = "seat-stop-requested-pp3"

// seatCmdDriver stands in for the cmdlets seat-cmd.ps1 and seat-cmdstop.ps1 call, so the RENDERED stubs run for real in a
// PowerShell with no scheduled task, no network and no waiting. Every call is written to a call log the test reads back
// in order. The file cmdlets (Set-Content, Remove-Item, Test-Path) are the real ones: the stop marker is a real file in
// the scratch seat directory.
//
// The scenario decides the world. The seat answers /v1/models at once and /health twice, then stops answering. The stub's
// own start task is Running until then and Ready after, unless the scenario says a live launcher still holds it. The stop
// task is Ready until it is started, then Running for three reads, then Ready — or Running forever, or refusing to start,
// or never seen Running at all (a Task Scheduler slow to start it). Its last result (what seat_stop.sh exited with, which
// wscript hands to the task) is 0, or 1 in "stop-task-incomplete", or unreadable in "stop-result-unreadable". Scenario
// "after-cmdstop" is a stop that llama-swap asked for: seat-cmdstop.ps1 is another process, so its marker appears in the
// middle of the stub's polling, at the first /health the seat fails to answer. "cmdstop" and "cmdstop-marker-unwritable"
// run seat-cmdstop.ps1 itself against a seat that stops answering at once.
const seatCmdDriver = `param([string]$Stub, [string]$Scenario, [string]$Calls, [string]$Marker)
$ErrorActionPreference = 'Continue'
$global:Scn = $Scenario
$global:CallLog = $Calls
$global:MarkerPath = $Marker
$global:Probes = 0
$global:StopReads = 0
function Note([string]$m) { Add-Content -Path $global:CallLog -Value $m }
function Start-Sleep { param($Seconds) Note "sleep $Seconds" }
function Start-ScheduledTask {
  [CmdletBinding()] param([string]$TaskName)
  Note "start-task $TaskName"
  if ($TaskName -like 'vllm-seat-stop-*') {
    # The real cmdlet raises a NON-terminating error for a task that does not exist, so a caller that dropped
    # -ErrorAction Stop sails on as if the task had started.
    if ($global:Scn -eq 'stop-task-missing') { Write-Error 'The system cannot find the file specified.'; return }
    $global:StopReads = 0
  }
}
function Get-ScheduledTaskInfo {
  [CmdletBinding()] param([string]$TaskName)
  Note "read-task-result $TaskName"
  if ($global:Scn -eq 'stop-result-unreadable') { throw 'The task result is not available.' }
  $r = 0
  if ($global:Scn -eq 'stop-task-incomplete') { $r = 1 }
  return [pscustomobject]@{ LastTaskResult = [uint32]$r }
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
  if ($global:Scn -eq 'task-ended-early') { return [pscustomobject]@{ State = 'Ready' } }
  if ($global:Scn -eq 'live-launcher') { return [pscustomobject]@{ State = 'Running' } }
  if ($global:Probes -ge 3) { return [pscustomobject]@{ State = 'Ready' } }
  return [pscustomobject]@{ State = 'Running' }
}
function Invoke-RestMethod {
  [CmdletBinding()] param([string]$Uri, [int]$TimeoutSec)
  if ($Uri -like '*/v1/models') {
    if ($global:Scn -eq 'task-ended-early') { throw 'Unable to connect to the remote server' }
    return [pscustomobject]@{ data = @([pscustomobject]@{ id = 'qwen3.8-27b-vllm' }) }
  }
  if ($global:Scn -like 'cmdstop*') { throw 'Unable to connect to the remote server' }
  $global:Probes++
  Note "health $($global:Probes)"
  if ($global:Probes -le 2) { return $null }
  if ($global:Scn -eq 'after-cmdstop' -and $global:Probes -eq 3) { Set-Content -Path $global:MarkerPath -Value 'stop requested' }
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

// stubRun is what one run of a rendered stub under the driver left behind.
type stubRun struct {
	Calls string // the call log, in order
	Log   string // the stub's own log file
	Dir   string // the scratch seat directory
}

func (r stubRun) markerExists() bool {
	_, err := os.Stat(filepath.Join(r.Dir, stopMarker))
	return err == nil
}

// runStub renders the shipped templates with a scratch seat directory, runs one rendered artifact (seat-cmd.ps1 or
// seat-cmdstop.ps1) in the given PowerShell under the driver for one scenario, and returns what it left behind. pre, when
// set, runs against the scratch seat directory first (a marker already there, say).
func runStub(t *testing.T, host, artifact, scenario string, pre func(dir string)) stubRun {
	t.Helper()
	dir := t.TempDir()
	rt := wslRT()
	rt.SeatDir = filepath.ToSlash(dir)
	rt.ProxyHost = "192.0.2.10"
	files, err := pinned().Artifacts(wslTemplatesDir(), rt)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, artifact)
	driver := filepath.Join(dir, "driver.ps1")
	callLog := filepath.Join(dir, "calls.txt")
	if err := os.WriteFile(stub, []byte(files[artifact]), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver, []byte(seatCmdDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, host, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", driver, "-Stub", stub, "-Scenario", scenario, "-Calls", callLog, "-Marker", filepath.Join(dir, stopMarker)).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: the driver failed (%v):\n%s", scenario, err, out)
	}
	raw, _ := os.ReadFile(callLog)
	logRaw, _ := os.ReadFile(filepath.Join(dir, "seat-cmd-pp3.log"))
	// Out-File writes UTF-16 in Windows PowerShell 5.1: drop the NULs so the log reads as text.
	r := stubRun{
		Calls: strings.ReplaceAll(string(raw), "\r", ""),
		Log:   strings.ReplaceAll(strings.ReplaceAll(string(logRaw), "\x00", ""), "\r", ""),
		Dir:   dir,
	}
	t.Logf("%s %s via %s\ncalls:\n%s\nstub log:\n%s", artifact, scenario, filepath.Base(host), r.Calls, r.Log)
	return r
}

// runSeatCmd runs seat-cmd.ps1 under the driver for one scenario and returns the call log and the stub's own log.
func runSeatCmd(t *testing.T, host, scenario string) (calls, stubLog string) {
	t.Helper()
	r := runSeatCmdWith(t, host, scenario, nil)
	return r.Calls, r.Log
}

func runSeatCmdWith(t *testing.T, host, scenario string, pre func(dir string)) stubRun {
	t.Helper()
	r := runStub(t, host, "seat-cmd.ps1", scenario, pre)
	// In every scenario: the stub starts the seat's own task once, at its start, and never again. A crash is cleaned up,
	// not relaunched: an idle seat stays unloaded (ttl 300), and llama-swap starts it again on the next request.
	if n := countLines(r.Calls, "start-task vllm-seat-pp3"); n != 1 {
		t.Errorf("%s: the stub started the seat's own task %d times; the crash cleanup must never relaunch the seat:\n%s", scenario, n, r.Calls)
	}
	return r
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
// seat stops answering for 30 s. That is a crash when nobody asked the seat to stop, and nothing else runs a stop for a
// crash, so the workers and the MP server the dead API server left behind outlived it and every restart refused. A crash
// exit now runs the operator-session stop task once (the task seat-cmdstop.ps1 uses), waits for it (bounded), and only
// then exits 0. It is a cleanup, never a relaunch; it does nothing while a live launcher still owns the seat; it does
// nothing after an unload (llama-swap's cmdStop has run that stop already, and about nine exits in ten are unloads); and a
// stop task that will not start or never finishes cannot keep the stub from exiting. Runs in every PowerShell the box has
// (5.1 is what llama-swap runs).
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
		// The stop's result is read once, after the wait and before the exit; a clean stop (result 0) is not a warning.
		resultAt := -1
		for i, l := range lines {
			if l == "read-task-result vllm-seat-stop-pp3" {
				resultAt = i
			}
		}
		if countLines(calls, "read-task-result vllm-seat-stop-pp3") != 1 || !(resultAt > lastRead && resultAt < exitAt) {
			t.Errorf("the stub must read the stop task's result exactly once, after the wait and before it exits (read at %d, last poll %d, exit %d):\n%s", resultAt, lastRead, exitAt, calls)
		}
		if strings.Contains(log, "did not finish") || strings.Contains(log, "could not read the stop task's result") {
			t.Errorf("a clean stop (result 0) must not be reported as a problem:\n%s", log)
		}
	})

	t.Run("a stop that did not finish is said in the stub's log, and the exit is unchanged", func(t *testing.T) {
		// seat_stop.sh exits 1 when a process of the seat survived SIGKILL or the port is still bound; wscript hands the
		// code to the task. The stub used to log "finished" whatever happened, so nothing on the Windows side showed it.
		calls, log := runSeatCmd(t, host, "stop-task-incomplete")
		if !strings.Contains(log, "crash cleanup finished") || !strings.Contains(log, "seat_stop.sh reported a stop that did not finish (task result 1)") || !strings.Contains(log, "seat.log") {
			t.Errorf("an incomplete stop must be said, with the result and where to read why:\n%s", log)
		}
		if !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Errorf("the exit stays exit 0 whatever the stop reported:\n%s", calls)
		}
	})

	t.Run("a stop result that cannot be read is said, and the exit is unchanged", func(t *testing.T) {
		calls, log := runSeatCmd(t, host, "stop-result-unreadable")
		if !strings.Contains(log, "could not read the stop task's result") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=0") {
			t.Errorf("a result that cannot be read must be said and must not change the exit:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
	})

	t.Run("an unload is not a crash: cmdStop asked for the stop, so no stop task is started and the exit is unchanged", func(t *testing.T) {
		// seat-cmdstop.ps1 runs the stop task itself, in another process, while this stub is still polling; the stub's
		// only sign of it is the marker. Every normal unload (a ttl expiry, a swap-out, gpu reserve --unload-seat) ends
		// with this stub seeing 30 s of silence, exactly like a crash.
		r := runSeatCmdWith(t, host, "after-cmdstop", nil)
		if strings.Contains(r.Calls, "start-task vllm-seat-stop-pp3") || strings.Contains(r.Calls, "read-stop-task") {
			t.Fatalf("the stub ran a second stop after llama-swap's own stop: it wakes the distro for nothing and delays the exit that completes the unload:\n%s", r.Calls)
		}
		if !strings.HasSuffix(strings.TrimSpace(r.Calls), "exit=0") {
			t.Fatalf("the exit stays exit 0 (llama-swap reads it as a stopped seat):\n%s", r.Calls)
		}
		for _, want := range []string{"seat stopped answering (30 s) - exiting so llama-swap sees it", "crash cleanup skipped: llama-swap asked for this stop"} {
			if !strings.Contains(r.Log, want) {
				t.Errorf("the stub's log lost %q:\n%s", want, r.Log)
			}
		}
	})

	t.Run("a marker an earlier generation's unload left does not hide this generation's crash", func(t *testing.T) {
		// The marker is removed when the seat comes up, so only a stop requested while THIS generation was up counts.
		r := runSeatCmdWith(t, host, "crash", func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, stopMarker), []byte("an earlier unload"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if countLines(r.Calls, "start-task vllm-seat-stop-pp3") != 1 || !strings.Contains(r.Log, "crash cleanup finished") {
			t.Fatalf("a stale marker suppressed the cleanup of a real crash:\ncalls:\n%s\nlog:\n%s", r.Calls, r.Log)
		}
		if r.markerExists() {
			t.Fatalf("the stub left an earlier generation's marker in place when the seat came up")
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
		// A task that never ran since it was started has only the result of an EARLIER run: reading it would blame this
		// cleanup for that one (or clear it).
		if strings.Contains(calls, "read-task-result") {
			t.Fatalf("the result of a stop task that was never seen running belongs to an earlier run and must not be read:\n%s", calls)
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
		if strings.Contains(calls, "read-task-result") {
			t.Fatalf("a stop task still running has no result to read yet:\n%s", calls)
		}
	})

	t.Run("the cleanup polls every 2 s, so its 30 s and 90 s bounds are real seconds", func(t *testing.T) {
		// The driver discards Start-Sleep's argument as a wait but records it, so a read count only means seconds when
		// every wait between two reads is the 2 s the log strings and ADR 0035 promise.
		for _, tc := range []struct {
			scn   string
			reads int
		}{{"crash", 4}, {"stop-task-never-runs", 15}, {"stop-task-hangs", 45}} {
			calls, _ := runSeatCmd(t, host, tc.scn)
			lines := strings.Split(strings.TrimSpace(calls), "\n")
			from := -1
			for i, l := range lines {
				if l == "start-task vllm-seat-stop-pp3" {
					from = i
				}
			}
			if from < 0 {
				t.Fatalf("%s: the stop task was never started:\n%s", tc.scn, calls)
			}
			sleeps, reads := 0, 0
			for _, l := range lines[from:] {
				if strings.HasPrefix(l, "sleep ") {
					sleeps++
					if l != "sleep 2" {
						t.Errorf("%s: the cleanup polled with %q, want a 2 s interval:\n%s", tc.scn, l, calls)
					}
				}
				if strings.HasPrefix(l, "read-stop-task ") {
					reads++
				}
			}
			if sleeps != tc.reads || reads != tc.reads {
				t.Errorf("%s: %d sleeps and %d reads after the stop task started, want %d of each:\n%s", tc.scn, sleeps, reads, tc.reads, calls)
			}
		}
	})

	t.Run("a start that fails before the seat answers is not a crash: no stop task, exit 3", func(t *testing.T) {
		// ADR 0035: the load-phase exits are unchanged and leave their leftovers to the next start's cleanup.
		calls, log := runSeatCmd(t, host, "task-ended-early")
		if strings.Contains(calls, "start-task vllm-seat-stop-pp3") || !strings.HasSuffix(strings.TrimSpace(calls), "exit=3") || !strings.Contains(log, "task ended before the seat answered") {
			t.Fatalf("the load-phase exits are unchanged: no cleanup, exit 3:\ncalls:\n%s\nlog:\n%s", calls, log)
		}
	})
}

// TestSeatCmdStopMarksAnUnloadForTheStub runs the RENDERED seat-cmdstop.ps1 (llama-swap's cmdStop): besides starting the
// stop task and waiting for the port to stop answering, it leaves the marker seat-cmd.ps1 reads. If it cannot, the stop
// goes on and the stub falls back to cleaning up after the unload, which is only redundant.
func TestSeatCmdStopMarksAnUnloadForTheStub(t *testing.T) {
	hosts := powerShellHosts()
	if len(hosts) == 0 {
		t.Skip("no PowerShell on this box: seat-cmdstop.ps1 runs under it")
	}
	for _, host := range hosts {
		host := host
		t.Run(strings.TrimSuffix(filepath.Base(host), ".exe"), func(t *testing.T) {
			t.Run("the stop task is started, the unload is marked, and the exit is 0", func(t *testing.T) {
				r := runStub(t, host, "seat-cmdstop.ps1", "cmdstop", nil)
				if countLines(r.Calls, "start-task vllm-seat-stop-pp3") != 1 || !strings.HasSuffix(strings.TrimSpace(r.Calls), "exit=0") {
					t.Fatalf("cmdStop must start the stop task once and exit 0:\n%s", r.Calls)
				}
				if !r.markerExists() {
					t.Fatalf("cmdStop left no %s in the seat directory: seat-cmd.ps1 would take the unload for a crash:\n%s", stopMarker, r.Log)
				}
				if !strings.Contains(r.Log, "stop requested") || !strings.Contains(r.Log, "seat stopped") {
					t.Errorf("cmdStop's log lost its lines:\n%s", r.Log)
				}
			})
			t.Run("a marker that cannot be written does not stop the stop", func(t *testing.T) {
				r := runStub(t, host, "seat-cmdstop.ps1", "cmdstop-marker-unwritable", func(dir string) {
					if err := os.Mkdir(filepath.Join(dir, stopMarker), 0o755); err != nil {
						t.Fatal(err)
					}
				})
				if countLines(r.Calls, "start-task vllm-seat-stop-pp3") != 1 || !strings.HasSuffix(strings.TrimSpace(r.Calls), "exit=0") || !strings.Contains(r.Log, "seat stopped") {
					t.Fatalf("a marker that cannot be written must not change the stop or its exit:\ncalls:\n%s\nlog:\n%s", r.Calls, r.Log)
				}
				if !strings.Contains(r.Log, "could not leave the stop marker") {
					t.Fatalf("a marker that cannot be written must be logged:\n%s", r.Log)
				}
			})
		})
	}
}

// TestSeatStubsAgreeOnTheStopMarker keeps the two stubs' marker expression identical where no PowerShell is around to run
// the driver tests: the start stub reads the file the stop stub writes.
func TestSeatStubsAgreeOnTheStopMarker(t *testing.T) {
	const expr = `Join-Path '__SEAT_DIR__' "seat-stop-requested-$Seat"`
	for _, f := range []string{"seat-cmd.ps1", "seat-cmdstop.ps1"} {
		raw, err := os.ReadFile(filepath.Join(wslTemplatesDir(), f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), expr) {
			t.Errorf("%s no longer builds the stop marker path as %s: the two stubs would disagree about it", f, expr)
		}
	}
}
