# seat-cmd.ps1 <seat> — llama-swap `cmd` for a vLLM seat whose engine runs in WSL. RENDERED; do not
# hand-edit (see __SEAT_ENV__).
#
# Why a stub and not the engine itself: llama-swap runs as SYSTEM, and a WSL distro belongs to the
# interactive user. SYSTEM and S4U both measured unable to start the distro, so the engine is started
# by a scheduled task registered against the operator's INTERACTIVE logon; this script triggers that
# task and then stays alive for as long as the seat answers, because llama-swap treats this process
# as the model and health-checks the proxy URL rather than us.
#
# It exits — so llama-swap marks the seat stopped and health-waits on the next request — when the
# task ends before the seat answers, when the seat never answers inside the load budget, or when the
# seat stops answering for 30 s. That last exit ends every UNLOAD (llama-swap's cmdStop stops the engine and this
# process then sees 30 s of silence: about nine exits in ten) and every CRASH. An unload has its stop run already:
# seat-cmdstop.ps1 starts the stop task and leaves a marker. A crash has none, and nothing else runs one: the engine's
# API server dies, but the workers it could not stop and the LMCache MP server outlive it (2026-09-29: three workers
# survived, the MP server kept its HTTP port, and every restart refused for 23 minutes). So with no marker it runs the
# stop task once before it exits (seat_stop.sh reaps only what is this seat's own) and waits for it; see
# Invoke-CrashCleanup.
param([Parameter(Mandatory)][string]$Seat)
$ErrorActionPreference = 'Continue'
$task = "vllm-seat-$Seat"
$log = Join-Path '__SEAT_DIR__' "seat-cmd-$Seat.log"
# seat-cmdstop.ps1 (llama-swap's cmdStop) leaves this file when it is asked to stop the seat: the mark of an unload.
$stopMarker = Join-Path '__SEAT_DIR__' "seat-stop-requested-$Seat"
# The crash cleanup. From the process llama-swap already supervises — no scheduler, no watchdog — and never a relaunch:
# llama-swap starts the seat again on the next request, as it always did, and an idle seat stays unloaded. It runs for a
# CRASH only. After an unload (the marker is there) the stop task has already run through cmdStop, and a second run would
# wake a distro that had powered itself off, only to find nothing, and delay the exit that completes the unload. It WAITS
# for the stop task (bounded: 45 x 2 s; a task never seen running gets 30 s to appear), so the next start does not overlap
# it: a stop that ran late could stop the MP server the next start had just begun. It does nothing while the seat's own
# start task is still running: a live launcher owns the seat then, and nothing in the distro is a leftover. A failure only
# logs; the exit that follows is the same. Once the stop task has been seen to finish, its last result is read and logged:
# seat_stop.sh exits 1 when a process of the seat survived SIGKILL or the engine port is still bound, wscript hands that
# code to the task, and the task's state alone says "finished" whatever it was. (The result of a task never seen running is
# an earlier run's, and is not read.)
function Invoke-CrashCleanup {
  $stopTask = "vllm-seat-stop-$Seat"
  if (Test-Path -LiteralPath $stopMarker) {
    "[$(Get-Date -Format s)] crash cleanup skipped: llama-swap asked for this stop, so this is an unload, not a crash, and its stop task already ran" | Out-File -Append $log
    return
  }
  $st = (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue).State
  if ($st -eq 'Running') {
    "[$(Get-Date -Format s)] crash cleanup skipped: the start task is still running, a live launcher owns the seat" | Out-File -Append $log
    return
  }
  try { Start-ScheduledTask -TaskName $stopTask -ErrorAction Stop } catch {
    "[$(Get-Date -Format s)] crash cleanup: $stopTask failed to start: $($_.Exception.Message)" | Out-File -Append $log
    return
  }
  "[$(Get-Date -Format s)] crash cleanup: $stopTask started (seat_stop.sh reaps the dead generation's workers and MP server)" | Out-File -Append $log
  # A task that has not started yet reads as not-Running: wait to SEE it run before believing it is done, and give a
  # Task Scheduler that is slow to start it 30 s to do so.
  $seen = $false
  for ($i = 1; $i -le 45; $i++) {
    Start-Sleep 2
    $s = (Get-ScheduledTask -TaskName $stopTask -ErrorAction SilentlyContinue).State
    if ($s -eq 'Running') { $seen = $true; continue }
    if ($seen) {
      "[$(Get-Date -Format s)] crash cleanup finished after about $($i * 2) s (task state=$s)" | Out-File -Append $log
      $r = $null
      try { $r = (Get-ScheduledTaskInfo -TaskName $stopTask -ErrorAction Stop).LastTaskResult } catch {
        "[$(Get-Date -Format s)] could not read the stop task's result: $($_.Exception.Message)" | Out-File -Append $log
      }
      if ($null -ne $r -and $r -ne 0) {
        "[$(Get-Date -Format s)] WARN: seat_stop.sh reported a stop that did not finish (task result $r): a process of the seat survived SIGKILL or the engine port is still bound - see __WSL_SEAT_DIR__/seat.log" | Out-File -Append $log
      }
      return
    }
    if ($i -ge 15) {
      "[$(Get-Date -Format s)] WARN: the stop task was not seen running in 30 s (state=$s) - exiting anyway; a stop that starts late may overlap the next start" | Out-File -Append $log
      return
    }
  }
  "[$(Get-Date -Format s)] WARN: the crash cleanup was still running after 90 s - exiting anyway" | Out-File -Append $log
}
"[$(Get-Date -Format s)] start requested" | Out-File -Append $log
try { Start-ScheduledTask -TaskName $task -ErrorAction Stop } catch {
  "[$(Get-Date -Format s)] Start-ScheduledTask $task failed: $($_.Exception.Message)" | Out-File -Append $log
  exit 2
}
# Stay under llama-swap's healthCheckTimeout so IT reports the failure, not us.
$deadline = (Get-Date).AddSeconds([Math]::Max(60, __HEALTH_TIMEOUT__ - 20))
$up = $false
while ((Get-Date) -lt $deadline) {
  Start-Sleep 5
  try {
    $m = Invoke-RestMethod -Uri 'http://__PROXY_HOST__:__PORT__/v1/models' -TimeoutSec 4
    if ($m.data.id -contains '__SEAT_ID__') { $up = $true; break }
  } catch {}
  $st = (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue).State
  if ($st -and $st -ne 'Running') {
    "[$(Get-Date -Format s)] task ended before the seat answered (state=$st) - see __WSL_SEAT_DIR__/seat.log" | Out-File -Append $log
    exit 3
  }
}
if (-not $up) { "[$(Get-Date -Format s)] seat never answered within the load budget" | Out-File -Append $log; exit 4 }
"[$(Get-Date -Format s)] seat up" | Out-File -Append $log
# From here on, a stop that llama-swap asks for is an unload. A marker an earlier generation left says nothing about this one.
Remove-Item -LiteralPath $stopMarker -Force -ErrorAction SilentlyContinue
$miss = 0
while ($true) {
  Start-Sleep 5
  try { $null = Invoke-RestMethod -Uri 'http://__PROXY_HOST__:__PORT__/health' -TimeoutSec 4; $miss = 0 } catch { $miss++ }
  if ($miss -ge 6) {
    "[$(Get-Date -Format s)] seat stopped answering (30 s) - exiting so llama-swap sees it" | Out-File -Append $log
    Invoke-CrashCleanup
    exit 0
  }
}
