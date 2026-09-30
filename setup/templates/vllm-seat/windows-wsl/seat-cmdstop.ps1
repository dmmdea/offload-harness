# seat-cmdstop.ps1 <seat> — llama-swap `cmdStop` for a vLLM seat whose engine runs in WSL.
# RENDERED; do not hand-edit.
#
# A real stop, so an unload through llama-swap (ttl expiry, `gpu reserve --drain --unload-seat`,
# /api/models/unload, a llama-swap restart) truly frees the cards. It triggers the stop task in the
# operator's session — which runs seat_stop.sh with this seat's env file — and then WAITS until the
# port stops answering, because reporting a stop that has not happened lets the next seat start
# against cards that are still held.
#
# It also leaves a marker for seat-cmd.ps1. That stub exits after 30 s of silence on every unload, as it does on a
# crash, and the marker is how it knows this stop already ran and needs no crash cleanup of its own.
param([Parameter(Mandatory)][string]$Seat)
$ErrorActionPreference = 'Continue'
$log = Join-Path '__SEAT_DIR__' "seat-cmd-$Seat.log"
"[$(Get-Date -Format s)] stop requested" | Out-File -Append $log
# A marker that cannot be written costs the stub one redundant stop after this unload, never this stop.
try { Set-Content -LiteralPath (Join-Path '__SEAT_DIR__' "seat-stop-requested-$Seat") -Value (Get-Date -Format s) -ErrorAction Stop } catch {
  "[$(Get-Date -Format s)] could not leave the stop marker: $($_.Exception.Message)" | Out-File -Append $log
}
try { Start-ScheduledTask -TaskName "vllm-seat-stop-$Seat" -ErrorAction Stop } catch {
  "[$(Get-Date -Format s)] stop task failed to start: $($_.Exception.Message)" | Out-File -Append $log
}
# Stay inside the entry's unloadTimeout, which covers the engine plus the MP server.
$deadline = (Get-Date).AddSeconds(55)
while ((Get-Date) -lt $deadline) {
  Start-Sleep 2
  $busy = $false
  try { $null = Invoke-RestMethod -Uri 'http://__PROXY_HOST__:__PORT__/health' -TimeoutSec 2; $busy = $true } catch {}
  if (-not $busy) { "[$(Get-Date -Format s)] seat stopped" | Out-File -Append $log; exit 0 }
}
"[$(Get-Date -Format s)] WARN: seat still answering after 55 s" | Out-File -Append $log
exit 1
