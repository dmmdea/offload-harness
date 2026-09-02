# seat-contention-probe.ps1 — the mechanism probe behind ADR 0032.
#
# Fires N concurrent 1-token chat calls at one llama-swap seat and records, per call, the HTTP
# status, Retry-After, the body's code/src and the wall — so the "what does llama-swap actually
# answer under fan-out" question is settled by observation, not by reading its source alone.
# Expected on a default concurrencyLimit (10): calls beyond ten reserved answer
#   429 {"error":{"code":"concurrency_limit","src":"llama-swap"}} with Retry-After: 1
# and the rest queue (200 after a wait). Zero 429s with N > 10 means the limit is not the
# default on this box and seatwait's classes must be re-read against the real config.
#
# Read-only against the seat; the calls are max_tokens=1. Run only when the seat is idle for
# you to use (a peer's batch or a benchmark lease on the same cards makes the numbers theirs).
param(
  [string]$Endpoint = "http://127.0.0.1:11436",
  [string]$Model = "qwen3.8-27b",
  [int]$N = 14,
  [string]$Out = "$PSScriptRoot\..\seat-contention-probe.json"
)
$ErrorActionPreference = "Continue"
$body = @{ model = $Model; messages = @(@{ role = "user"; content = "Reply with one word." }); max_tokens = 1; stream = $false } | ConvertTo-Json -Depth 5
$jobs = 1..$N | ForEach-Object {
  # -ThrottleLimit: Start-ThreadJob's default is FIVE concurrent jobs, which silently turns an
  # N-wide probe into a 5-wide one (found 2026-09-02: 64 calls, 5 at a time, zero 429s).
  Start-ThreadJob -ThrottleLimit $N -ArgumentList $_, $Endpoint, $body -ScriptBlock {
    param($i, $ep, $b)
    $t0 = [DateTime]::UtcNow
    try {
      $r = Invoke-WebRequest -Uri "$ep/v1/chat/completions" -Method POST -ContentType "application/json" -Body $b -TimeoutSec 600 -SkipHttpErrorCheck
      $status = [int]$r.StatusCode; $ra = $r.Headers["Retry-After"]; $txt = [string]$r.Content
    } catch { $status = -1; $ra = ""; $txt = $_.Exception.Message }
    $ms = [int]([DateTime]::UtcNow - $t0).TotalMilliseconds
    $code = ""; $src = ""
    try { $j = $txt | ConvertFrom-Json; $code = $j.error.code; $src = $j.error.src } catch {}
    [pscustomobject]@{ call = $i; status = $status; retry_after = "$ra"; code = "$code"; src = "$src"; wall_ms = $ms; body = ($txt -replace '\s+', ' ').Substring(0, [Math]::Min(120, $txt.Length)) }
  }
}
$rows = $jobs | Wait-Job | Receive-Job | Sort-Object call
$rows | Format-Table call, status, retry_after, code, src, wall_ms -AutoSize | Out-String | Write-Host
$summary = [pscustomobject]@{
  endpoint = $Endpoint; model = $Model; n = $N; ran_at = (Get-Date -Format s)
  status_counts = ($rows | Group-Object status | ForEach-Object { "$($_.Name)=$($_.Count)" }) -join " "
  n_429 = ($rows | Where-Object status -eq 429).Count
  max_wall_ms = ($rows | Measure-Object wall_ms -Maximum).Maximum
  verdict = if (($rows | Where-Object status -eq 429).Count -gt 0) { "429 observed: concurrencyLimit is live on this seat" } elseif ($N -le 10) { "INCONCLUSIVE: N <= 10 cannot exceed the default limit" } else { "NO 429 with N > 10: the limit is not the default here — re-read seatwait's classes against the real config" }
  rows = $rows
}
$summary | ConvertTo-Json -Depth 5 | Set-Content -Encoding UTF8 $Out
Write-Host "verdict: $($summary.verdict)"
Write-Host "wrote $Out"
