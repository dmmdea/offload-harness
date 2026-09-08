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
# seat stops answering for 30 s.
param([Parameter(Mandatory)][string]$Seat)
$ErrorActionPreference = 'Continue'
$task = "vllm-seat-$Seat"
$log = Join-Path '__SEAT_DIR__' "seat-cmd-$Seat.log"
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
$miss = 0
while ($true) {
  Start-Sleep 5
  try { $null = Invoke-RestMethod -Uri 'http://__PROXY_HOST__:__PORT__/health' -TimeoutSec 4; $miss = 0 } catch { $miss++ }
  if ($miss -ge 6) {
    "[$(Get-Date -Format s)] seat stopped answering (30 s) - exiting so llama-swap sees it" | Out-File -Append $log
    exit 0
  }
}
