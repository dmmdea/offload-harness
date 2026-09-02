# parallel-sessions-gate.ps1 — the live proof behind ADR 0032 (masterplan #1, 2026-09-01).
#
# Runs K concurrent `delegate --contract` runs (each an 8-contract fan-out, route local) against
# the live llama-swap with the binary you point it at, while one more process streams a long
# chat on the same seat — the shape of "five sessions at once". It then reads every run's JSON
# summary and every result's contention_wait_sec / admission_wait_sec and prints one verdict:
#
#   PASS          no "seat contended:" defers, no failed_verification, and the wait FIRED on
#                 at least one result (contention_wait_sec > 0) — the fix did work, not luck
#   INCONCLUSIVE  no contention was observed at all (no wait fired, no 429): the run proves
#                 nothing about the fix; rerun with a higher K or while a peer is busy
#   FAIL          a "seat contended:" defer or a failed_verification surfaced
#
# Compare `-Binary` = the live exe vs the scratch exe on the same K for the before/after the
# operator asked for. Read-only against production config; runs under the caller's lease.
param(
  [string]$Binary = "$PSScriptRoot\..\bin\local-offload-scratch.exe",
  [string]$Contract = "$PSScriptRoot\..\contracts\digest-8.json",
  [int]$K = 3,
  [string]$Endpoint = "http://127.0.0.1:11436",
  [string]$Model = "qwen3.8-27b",
  [string]$OutDir = "$PSScriptRoot\..\parallel-gate-out"
)
$ErrorActionPreference = "Continue"
New-Item -ItemType Directory -Force $OutDir | Out-Null
if (-not (Test-Path $Binary)) { throw "binary not found: $Binary" }
if (-not (Test-Path $Contract)) { throw "contract file not found: $Contract (an 8-subtask delegation contract)" }

# the "other session": one long streamed chat holding a slot on the same seat
$chatBody = @{ model = $Model; messages = @(@{ role = "user"; content = "Write 600 words about pipeline parallelism." }); max_tokens = 700; stream = $false } | ConvertTo-Json -Depth 5
$peer = Start-ThreadJob -ThrottleLimit ($K + 2) -ArgumentList $Endpoint, $chatBody -ScriptBlock { param($ep, $b); try { Invoke-WebRequest -Uri "$ep/v1/chat/completions" -Method POST -ContentType "application/json" -Body $b -TimeoutSec 900 -SkipHttpErrorCheck | Out-Null } catch {} }

$t0 = Get-Date
$runs = 1..$K | ForEach-Object {
  $i = $_
  $log = Join-Path $OutDir "run-$i.json"
  Start-ThreadJob -ThrottleLimit ($K + 2) -ArgumentList $Binary, $Contract, $log, $i -ScriptBlock {
    param($bin, $c, $log, $i)
    $t = [DateTime]::UtcNow
    # stdout and stderr to SEPARATE files: merged, the CLI's trailing error line interleaves into
    # the JSON and the run becomes unparseable (seen 2026-09-02 on every run that deferred)
    $errLog = [System.IO.Path]::ChangeExtension($log, '.stderr.txt')
    $out = & $bin delegate --contract $c --route local 2>$errLog
    $ms = [int]([DateTime]::UtcNow - $t).TotalMilliseconds
    $out | Out-File -Encoding UTF8 $log
    [pscustomobject]@{ run = $i; wall_ms = $ms; exit = $LASTEXITCODE; log = $log }
  }
}
$runRows = $runs | Wait-Job | Receive-Job | Sort-Object run
$peer | Wait-Job | Out-Null

$contended = 0; $failedVerification = 0; $waited = 0; $admitted = 0; $succeeded = 0; $total = 0; $wallTimeouts = 0; $otherDefers = 0
foreach ($r in $runRows) {
  $raw = Get-Content $r.log -Raw
  # the CLI prints config notes before the JSON and an error line after it: take the JSON object only
  $start = $raw.IndexOf("{"); $end = $raw.LastIndexOf("}")
  if ($start -lt 0 -or $end -le $start) { Write-Host "run $($r.run): no JSON in output (exit $($r.exit))"; continue }
  try { $j = $raw.Substring($start, $end - $start + 1) | ConvertFrom-Json } catch { Write-Host "run $($r.run): output is not JSON (exit $($r.exit)): $($_.Exception.Message)"; continue }
  $sum = $j.summary
  if ($sum) { $failedVerification += [int]$sum.failed_verification; $succeeded += [int]$sum.succeeded }
  foreach ($res in @($j.results)) {
    $total++
    $reason = "$($res.reason)"
    if ($reason -like "seat contended:*") { $contended++ }
    elseif ($reason -like "wall timeout*") { $wallTimeouts++ }
    elseif ($res.deferred) { $otherDefers++ }
    if ([double]$res.contention_wait_sec -gt 0) { $waited++ }
    if ([double]$res.admission_wait_sec -gt 0) { $admitted++ }
  }
}
# PASS needs the wait to have FIRED (a result waited and still succeeded) with no contended defers and no
# verification failures. Contended defers after a full budget, or wall timeouts, are CAPACITY: the seat cannot
# serve this many concurrent loops — report it as such, never as a pass.
$verdict = if ($failedVerification -gt 0) { "FAIL (verification failures)" }
  elseif ($contended -gt 0 -or $wallTimeouts -gt 0) { "CAPACITY-LIMITED: $contended contended after the full budget, $wallTimeouts wall timeouts, $succeeded/$total succeeded (raise concurrencyLimit/--parallel or lower K)" }
  elseif ($waited -eq 0) { "INCONCLUSIVE (no contention observed: no result waited; rerun with a higher -K or a busier peer)" }
  else { "PASS ($waited results waited and succeeded)" }
$report = [pscustomobject]@{
  binary = $Binary; k = $K; contract = $Contract; ran_at = (Get-Date -Format s)
  runs = $runRows; results_total = $total; succeeded = $succeeded; seat_contended_defers = $contended
  failed_verification = $failedVerification; wall_timeouts = $wallTimeouts; other_defers = $otherDefers; results_that_waited = $waited; results_that_waited_for_admission = $admitted
  median_wall_ms = ($runRows.wall_ms | Sort-Object)[[int]([Math]::Floor(($runRows.Count - 1) / 2))]
  wall_total_s = [int]((Get-Date) - $t0).TotalSeconds
  verdict = $verdict
}
$report | ConvertTo-Json -Depth 6 | Set-Content -Encoding UTF8 (Join-Path $OutDir "report.json")
$report | Format-List | Out-String | Write-Host
Write-Host "VERDICT: $verdict"
