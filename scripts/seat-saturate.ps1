# seat-saturate.ps1 — hold a llama-swap seat SATURATED for a measurement window.
#
# scripts/seat-contention-probe.ps1 sends max_tokens=1 requests: they finish in
# milliseconds and can never keep a seat's in-flight count up. The composite-tier
# live gates (docs/superpowers/plans/2026-09-09-composite-tier.md, Task 13) need the
# pair seat to sit at its max_num_seqs (32) for minutes, so this driver opens N
# long-generation streams at once and keeps them open until each completes.
#
# 32 streams x 1,500 tokens at the seat's c32 aggregate (~220 tok/s on the reference
# pair) is ~3-4 minutes of saturation. Per-stream status and wall land in -Out as
# JSON, so the gate can prove the seat WAS saturated (status 200, wall in minutes)
# rather than assume it.
#
# Usage:
#   pwsh -NoProfile -File scripts/seat-saturate.ps1 -Model agent-pool -N 32 -MaxTokens 1500
param(
  [string]$Endpoint = "http://127.0.0.1:11436",
  [string]$Model = "agent-pool",
  [int]$N = 32,
  [int]$MaxTokens = 1500,
  [string]$Prompt = "Write a long, detailed essay about the history of graphics processing units, from the first fixed-function rasterizers to modern tensor cores. Do not stop early.",
  [string]$Out = "$PSScriptRoot\..\seat-saturate.json"
)

$body = @{
  model       = $Model
  messages    = @(@{ role = "user"; content = $Prompt })
  max_tokens  = $MaxTokens
  temperature = 0.7
  stream      = $false
} | ConvertTo-Json -Depth 5

$t0 = [DateTime]::UtcNow
$jobs = 1..$N | ForEach-Object {
  # ThrottleLimit MUST equal N: the default (5) would run the streams five at a
  # time and never saturate anything (memory: start-threadjob-throttles-to-five).
  Start-ThreadJob -ThrottleLimit $N -ArgumentList $_, $Endpoint, $body -ScriptBlock {
    param($i, $ep, $b)
    $s = [DateTime]::UtcNow
    try {
      $r = Invoke-WebRequest -Uri "$ep/v1/chat/completions" -Method Post -ContentType "application/json" -Body $b -TimeoutSec 1800 -UseBasicParsing
      $status = [int]$r.StatusCode
      $len = ([string]$r.Content).Length
      $err = ""
    } catch {
      $status = -1; $len = 0; $err = [string]$_.Exception.Message
    }
    $ms = [int]([DateTime]::UtcNow - $s).TotalMilliseconds
    [pscustomobject]@{ i = $i; status = $status; wall_ms = $ms; bytes = $len; error = $err }
  }
}
$rows = $jobs | Wait-Job | Receive-Job
$jobs | Remove-Job -Force
$wall = [int]([DateTime]::UtcNow - $t0).TotalMilliseconds
$ok = @($rows | Where-Object { $_.status -eq 200 }).Count
$summary = [pscustomobject]@{
  endpoint = $Endpoint; model = $Model; n = $N; max_tokens = $MaxTokens
  ran_at = $t0.ToString("s"); wall_ms = $wall; ok = $ok
  rows = @($rows | Sort-Object i)
}
$summary | ConvertTo-Json -Depth 6 | Set-Content -Path $Out -Encoding UTF8
Write-Host ("seat-saturate: {0}/{1} streams returned 200 on {2} in {3} s -> {4}" -f $ok, $N, $Model, [int]($wall / 1000), $Out)
