# offload-dream.ps1 - nightly self-learning loop for the local-offload harness.
# Phased, gated adoption. Runs the INFERENCE-FREE stats jobs
# (calibrate/health/train-router/optimize) directly, uses an optional external
# reasoning CLI only for the rare GEPA prompt-reflection, and STAGES everything
# for human review.
#
# Register nightly (pick a quiet slot; offset it from any other nightly jobs):
#   schtasks /Create /TN "offload-dream" /SC DAILY /ST 03:40 /F ^
#     /TR "powershell -NoProfile -ExecutionPolicy Bypass -File <repo>\scripts\offload-dream.ps1"
[CmdletBinding()]
param(
  [string]$Bin     = (Join-Path $PSScriptRoot '..\local-offload.exe'),
  [string]$DataDir = "$env:USERPROFILE\.local-offload",
  [switch]$Gepa,            # allow the codex prompt-reflection (paid; SELF-THROTTLED below)
  [int]$GepaMinDays = 7,    # min days between codex GEPA runs (cost control)
  [int]$GepaMinHard = 30,   # min hard cases required before a GEPA run is worth it
  [switch]$AutoAdoptStats,  # adopt threshold/router/override JSON (prompts are NEVER auto-adopted)
  [switch]$DryRun,
  [double]$MaxThresholdDelta = 0.1  # trust-region cap: max per-task threshold move per night (anti-collapse)
)
$ErrorActionPreference = 'Stop'

if (-not (Test-Path $Bin)) { $Bin = (Join-Path $PSScriptRoot '..\local-offload.exe') }
$ledger  = Join-Path $DataDir 'ledger.jsonl'
$staging = Join-Path $DataDir 'staging'
$backup  = Join-Path $DataDir ('backup-' + (Get-Date -Format 'yyyyMMdd'))
$marker  = Join-Path $DataDir 'DREAM-PROPOSAL.md'
$logFile = Join-Path $DataDir 'offload-dream.log'
New-Item -ItemType Directory -Force -Path $staging, $backup | Out-Null

# Optional shared plumbing (Invoke-CodexSubagent / Acquire-CodexLock / Write-MemoryLog):
# point $env:MEMORY_COMMON_PS1 at a memory-common.ps1 to enable codex GEPA reflection
# + shared logging/locking. Unset → the codex/GEPA path is gracefully skipped (stats jobs still run).
$common = $env:MEMORY_COMMON_PS1
$haveCommon = $common -and (Test-Path $common)
if ($haveCommon) { . $common }
function Log($m) {
  if ($haveCommon) { try { Write-MemoryLog -Component 'offload-dream' -Message $m } catch {} }
  $line = (Get-Date -Format 'HH:mm:ss') + '  ' + $m
  try { Add-Content -Path $logFile -Value $line } catch {}
  Write-Output $line
}

# Clamp-Thresholds caps each per-task threshold in $New to within $MaxDelta of the
# live value $Live (anti-collapse trust region; survey 3.1.3 / RLHF KL-to-reference).
# Tasks absent from $Live pass through (first observation). Returns a hashtable;
# clamp notices go to the Warning stream so they don't pollute the return value.
function Clamp-Thresholds {
  param([hashtable]$New, [hashtable]$Live, [double]$MaxDelta = 0.1)
  $out = @{}
  foreach ($task in $New.Keys) {
    $proposed = [double]$New[$task]
    if ($Live.ContainsKey($task)) {
      $cur = [double]$Live[$task]
      $clamped = [math]::Max($cur - $MaxDelta, [math]::Min($cur + $MaxDelta, $proposed))
      if ([math]::Abs($clamped - $proposed) -gt 1e-12) {
        Write-Warning ("trust-region: '$task' proposed=$proposed clamped=$clamped (live=$cur +/- $MaxDelta)")
      }
      $out[$task] = $clamped
    } else {
      $out[$task] = $proposed
    }
  }
  return $out
}

if (-not (Test-Path $ledger)) { Log ('no ledger at ' + $ledger + ' - nothing to learn from yet'); exit 0 }

# Coordinate: only the GEPA step needs codex; share the lock so we never collide.
$gotLock = $false
if ($Gepa -and $haveCommon) {
  $gotLock = Acquire-CodexLock -Owner 'offload-dream' -MaxAgeMinutes 30
  if (-not $gotLock) { Log 'skip GEPA: codex lock held by another worker' }
}

$report = New-Object System.Collections.ArrayList
[void]$report.Add('# offload-dream proposal - ' + (Get-Date -Format 'yyyy-MM-dd HH:mm'))
[void]$report.Add('')

try {
  # PHASE 1, stage 0 — shadow-label: replay counterfactual tiers on the captured
  # queue and write confhead labels BEFORE the train steps consume them.
  # Bounded (--n 200) and fail-safe: a shadow-label error does NOT abort the cycle.
  Log 'running shadow-label (drain queue -> confhead labels)'
  try {
    $shadowOut = & $Bin shadow-label --n 200 2>&1
    Log ($shadowOut -join "`n")
  } catch {
    Log ('shadow-label failed (non-fatal): ' + $_)
  }

  # PHASE 1 - inference-free stats jobs -> STAGING (no codex, no GPU)
  $jobs = @(
    @{ name='calibrate';         out=(Join-Path $staging 'thresholds.json');          live=(Join-Path $DataDir 'thresholds.json') },
    @{ name='health';            out=(Join-Path $staging 'tier_overrides.json');       live=(Join-Path $DataDir 'tier_overrides.json') },
    @{ name='train-router';      out=(Join-Path $staging 'router-weights.json');       live=(Join-Path $DataDir 'router-weights.json') },
    @{ name='train-confhead';    out=(Join-Path $staging 'confhead-weights.json');     live=(Join-Path $DataDir 'confhead-weights.json') },
    @{ name='confhead-calibrate';out=(Join-Path $staging 'confhead-thresholds.json'); live=(Join-Path $DataDir 'confhead-thresholds.json') }
  )
  foreach ($j in $jobs) {
    Log ('running ' + $j.name + ' -> staging')
    $out = & $Bin $j.name --out $j.out 2>&1
    [void]$report.Add('## ' + $j.name)
    foreach ($l in ($out | Select-Object -Last 12)) { [void]$report.Add('    ' + $l) }
    [void]$report.Add('')
  }
  Log 'running optimize (exemplar selection)'
  $optOut = & $Bin optimize 2>&1
  [void]$report.Add('## optimize')
  foreach ($l in ($optOut | Select-Object -Last 8)) { [void]$report.Add('    ' + $l) }
  [void]$report.Add('')

  # PHASE 2 - codex prompt reflection (GEPA), SELF-THROTTLED (cost control) + human-gated adoption.
  # Runs at most every GepaMinDays and only when >= GepaMinHard hard cases exist, so a DAILY
  # schedule with -Gepa still only spends codex ~weekly, and only when there's real new signal.
  if ($Gepa -and $haveCommon -and $gotLock) {
    $gepaState = Join-Path $DataDir '.gepa-state.json'
    $lastRun = $null
    if (Test-Path $gepaState) { try { $lastRun = (Get-Content $gepaState -Raw | ConvertFrom-Json).last_run } catch {} }
    $daysSince = if ($lastRun) { ((Get-Date) - [datetime]$lastRun).TotalDays } else { 99999 }
    $hardJson = (& $Bin audit-sample --n 500 --hard --json 2>&1 | Out-String)
    $hardCount = 0; try { $hardCount = @($hardJson | ConvertFrom-Json).Count } catch {}
    if ($daysSince -lt $GepaMinDays) {
      Log ('gepa: not due (' + [math]::Round($daysSince,1) + 'd since last < ' + $GepaMinDays + 'd) - no codex spend')
    } elseif ($hardCount -lt $GepaMinHard) {
      Log ('gepa: skip (' + $hardCount + ' hard cases < ' + $GepaMinHard + ') - not worth a codex call')
    } else {
      Log ('gepa: DUE - ' + $hardCount + ' hard cases, reflecting via codex')
      $prompt = 'You are tuning the prompts of a local Gemma-4 offload harness. Below are its hardest cases (low margin / deferred / ungrounded) as JSON. For each task type, suggest a concise improvement to the system/user prompt that reduces these failures WITHOUT changing the required JSON output schema. Short actionable proposal per task. Hard cases:' + "`n" + $hardJson
      try {
        $resp = Invoke-CodexSubagent -Prompt $prompt -ReasoningEffort 'medium' -TimeoutSeconds 180
        Set-Content -Path (Join-Path $staging 'gepa-proposal.txt') -Value $resp
        [void]$report.Add('## GEPA prompt proposal (codex - REVIEW + apply to tasks.go; prompts are code, so this stays human-gated)')
        foreach ($l in ($resp -split "`n")) { [void]$report.Add('    ' + $l) }
        [void]$report.Add('')
        Set-Content -Path $gepaState -Value ((@{ last_run=(Get-Date).ToString('o'); hard_count=$hardCount } | ConvertTo-Json))
        Log 'gepa: codex proposal staged'
      } catch { Log ('gepa codex failed: ' + $_) }
    }
  }

  # PHASE 3 - stage + surface for review (skillopt-style gating)
  #
  # confhead adoption gate: run confhead-eval (OOF AURC + paired-bootstrap CI) before
  # promoting staged confhead-weights.json or confhead-thresholds.json to live.
  # confhead-eval prints a JSON map of {task -> {verdict, ci_lo, ci_hi, ...}} (or
  # {note:"insufficient labels"} for under-sampled tasks). Gate (regression-aware,
  # since the confhead is a single shared model adopted all-or-nothing):
  #   - improvement: ci_lo > 0  (the paired-bootstrap CI excludes 0 on the good side)
  #   - regression:  ci_hi < 0  (CI excludes 0 on the bad side)
  #   - neutral:     CI spans 0 (e.g. a task already at base_error 0 — nothing to gain,
  #                  nothing lost) -> does NOT block.
  # ADOPT iff >=1 task genuinely improves AND no task regresses. A neutral task (like
  # a perfectly-grounded extract) must not veto a real win on another task.
  $confheadGatePass = $false
  $confheadFiles    = @('confhead-weights.json', 'confhead-thresholds.json')
  $hasConfheadStaged = ($confheadFiles | Where-Object { Test-Path (Join-Path $staging $_) }).Count -gt 0
  if ($hasConfheadStaged) {
    Log 'running confhead-eval (adoption gate: AURC + paired-bootstrap CI)'
    try {
      $evalOut = & $Bin confhead-eval 2>&1
      if ($LASTEXITCODE -ne 0) { throw "confhead-eval exited $LASTEXITCODE" }
      $evalJson = $evalOut -join "`n"
      Log ('confhead-eval: ' + $evalJson)
      $evalObj = $evalJson | ConvertFrom-Json
      $anyRegress = $false
      $adoptCount = 0
      foreach ($prop in $evalObj.PSObject.Properties) {
        $v = $prop.Value.verdict
        if (-not $v -or $v -eq '') { continue }   # insufficient labels (Note set, Verdict empty) → skip
        $ciLo = [double]$prop.Value.ci_lo
        $ciHi = [double]$prop.Value.ci_hi
        if ($ciLo -gt 0) {
          $adoptCount++                            # genuine improvement
        } elseif ($ciHi -lt 0) {
          $anyRegress = $true                      # genuine regression -> block
          Log ('confhead-eval: REGRESSION on task ' + $prop.Name + ' (ci_hi=' + $prop.Value.ci_hi + ') -> block adoption')
        } else {
          Log ('confhead-eval: neutral on task ' + $prop.Name + ' (CI spans 0; no gain, no regression) -> not blocking')
        }
      }
      $confheadGatePass = (-not $anyRegress) -and ($adoptCount -gt 0)
      if ($confheadGatePass) { Log ('confhead-eval: ' + $adoptCount + ' task(s) improve, 0 regress -> gate PASS') }
      else { Log 'confhead-eval: gate FAIL — no improving task, or a regression present; confhead staged for manual review' }
    } catch {
      Log ('confhead-eval failed (non-fatal): ' + $_ + ' — confhead staged but NOT adopted; manual review required')
    }
  }

  foreach ($j in $jobs) {
    if (Test-Path $j.out) {
      # confhead files require passing the eval gate before any auto-adoption.
      $isConfheadFile = ($j.name -eq 'train-confhead' -or $j.name -eq 'confhead-calibrate')
      if ($isConfheadFile -and -not $confheadGatePass) {
        Log ($j.name + ': staged at ' + $j.out + ' — confhead gate not passed; SKIPPING auto-adopt (manual: copy to ' + $j.live + ' after reviewing confhead-eval output)')
        continue
      }
      if (Test-Path $j.live) { Copy-Item $j.live (Join-Path $backup (Split-Path $j.live -Leaf)) -Force }
      if ($j.name -eq 'calibrate' -and (Test-Path $j.out) -and (Test-Path $j.live)) {
        $newT  = Get-Content $j.out  -Raw | ConvertFrom-Json
        $liveT = Get-Content $j.live -Raw | ConvertFrom-Json
        $newH  = @{}; $newT.PSObject.Properties  | ForEach-Object { $newH[$_.Name]  = $_.Value }
        $liveH = @{}; $liveT.PSObject.Properties | ForEach-Object { $liveH[$_.Name] = $_.Value }
        $clamped = Clamp-Thresholds -New $newH -Live $liveH -MaxDelta $MaxThresholdDelta
        if ($AutoAdoptStats -and -not $DryRun) {
          ($clamped | ConvertTo-Json) | Set-Content -Path $j.live
          Log ('auto-adopted calibrate thresholds (trust-region clamped, delta<=' + $MaxThresholdDelta + '; effective on next MCP restart)')
        } else {
          Log ('dry-run: calibrate thresholds trust-region clamped (delta<=' + $MaxThresholdDelta + '); not written')
        }
      } elseif ($AutoAdoptStats -and -not $DryRun) {
        Copy-Item $j.out $j.live -Force
        Log ('auto-adopted ' + $j.name + ' (stats, backed up; effective on next MCP restart)')
      }
    }
  }
  Set-Content -Path $marker -Value ($report -join "`n")
  Log ('proposal written -> ' + $marker + ' (adopt stats: copy staging JSON over ' + $DataDir + ', then restart the MCP). Backups in ' + $backup)
}
finally {
  if ($gotLock) { try { Release-CodexLock -Owner 'offload-dream' } catch {} }
}
