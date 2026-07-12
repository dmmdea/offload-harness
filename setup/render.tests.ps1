# setup/render.tests.ps1 - H2 render self-test. Drives install.ps1 -RenderOnly with
# synthetic profiles (OFFLOAD_BACKEND/OFFLOAD_PROFILE/OFFLOAD_RAM_TIER overrides,
# NO hardware detection, NO downloads/build) and asserts the rendered llama-swap.yaml
# matches the matrix. Prints PASS/FAIL per check; exit 0 = all pass, 1 = any fail.
#
# Usage (both shells):
#   pwsh       -File setup/render.tests.ps1
#   powershell -ExecutionPolicy Bypass -File setup\render.tests.ps1
$ErrorActionPreference = 'Stop'
$here    = Split-Path -Parent $MyInvocation.MyCommand.Path
$install = Join-Path $here 'install.ps1'
$psExe   = (Get-Process -Id $PID).Path         # render with the SAME host running this test
$work    = Join-Path ([System.IO.Path]::GetTempPath()) ("offload-render-test-" + [guid]::NewGuid().ToString('N').Substring(0,8))
New-Item -ItemType Directory -Force -Path $work | Out-Null

$fail = 0
function Ok  { param([string]$m) Write-Host "PASS $m" }
function Bad { param([string]$m) Write-Host "FAIL $m" -ForegroundColor Red; $script:fail++ }

# Render one profile via install.ps1 -RenderOnly. Returns @{ yaml=<text>; verdict=<obj> }.
function Invoke-Render {
  param([string]$Backend, [string]$ProfileId, [string]$RamTier, [bool]$BigRam)
  $out = Join-Path $work ("$ProfileId-$RamTier.yaml")
  if (Test-Path $out) { Remove-Item $out -Force }
  $env:OFFLOAD_BACKEND  = $Backend
  $env:OFFLOAD_PROFILE  = $ProfileId
  $env:OFFLOAD_RAM_TIER = $RamTier
  if ($BigRam) { $env:OFFLOAD_BIG_RAM = '1' } else { Remove-Item Env:OFFLOAD_BIG_RAM -ErrorAction SilentlyContinue }
  $env:OFFLOAD_HOME = $work
  try {
    $stdout = & $psExe -NoProfile -File $install -RenderOnly -RenderOut $out 2>&1
  } finally {
    Remove-Item Env:OFFLOAD_BACKEND, Env:OFFLOAD_PROFILE, Env:OFFLOAD_RAM_TIER, Env:OFFLOAD_HOME -ErrorAction SilentlyContinue
    Remove-Item Env:OFFLOAD_BIG_RAM -ErrorAction SilentlyContinue
  }
  if (-not (Test-Path $out)) {
    Write-Host ($stdout -join "`n") -ForegroundColor Yellow
    throw "render produced no file for profile=$ProfileId ram=$RamTier"
  }
  $jsonLine = ($stdout | Where-Object { $_ -match '^\s*\{.*"render_only".*\}\s*$' } | Select-Object -Last 1)
  $verdict = if ($jsonLine) { $jsonLine | ConvertFrom-Json } else { $null }
  return @{ yaml = (Get-Content -Raw $out); verdict = $verdict; path = $out }
}

# The 'common' macro is a YAML folded scalar (>-) spanning 2 lines; join them so
# a match sees the whole thing (e.g. --threads is on the continuation line).
function Get-CommonMacro {
  param([string]$Yaml)
  $lines = $Yaml -split "`r?`n"
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match '^\s*common:\s*>-') {
      $buf = @()
      for ($j = $i + 1; $j -lt $lines.Count -and $lines[$j] -match '^\s{4,}\S'; $j++) { $buf += $lines[$j].Trim() }
      return ($buf -join ' ')
    }
  }
  return ''
}

# A model's cmd is also a folded scalar (>-) spanning 2 lines; join the whole cmd
# so a match sees the MoE flags (--cpu-moe / -ngl) that sit on the continuation.
function Get-ModelCmd {
  param([string]$Yaml, [string]$ModelKey)
  $lines = $Yaml -split "`r?`n"
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match "^\s{2}$([regex]::Escape($ModelKey)):\s*$") {
      # find the cmd: >- inside this model block, then join its continuation lines
      for ($k = $i + 1; $k -lt $lines.Count -and $lines[$k] -notmatch '^\s{2}\S'; $k++) {
        if ($lines[$k] -match '^\s+cmd:\s*>-') {
          $buf = @()
          for ($j = $k + 1; $j -lt $lines.Count -and $lines[$j] -match '^\s{6,}\S'; $j++) { $buf += $lines[$j].Trim() }
          return ($buf -join ' ')
        }
      }
    }
  }
  return ''
}

Write-Host "== this box (ampere-8, ram_tier=mid) - the primary validation =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'ampere-8' -RamTier 'mid' -BigRam $false
$macro = Get-CommonMacro $r.yaml
Write-Host "   common: $macro"
if ($macro -match '--ctx-size 16384')                          { Ok 'ampere-8/mid ctx=16384' }        else { Bad "ampere-8/mid ctx (got: $macro)" }
if ($macro -match '--cache-type-k q8_0' -and $macro -match '--cache-type-v q8_0') { Ok 'ampere-8/mid KV=q8_0 (symmetric)' } else { Bad 'ampere-8/mid KV q8_0' }
if ($macro -match '--flash-attn on')                           { Ok 'ampere-8/mid flash-attn on' }    else { Bad 'ampere-8/mid flash-attn' }
if ($r.yaml -match '(?m)^\s{2}gemma4-26b-a4b:')                 { Ok 'ampere-8/mid includes 26B tier' } else { Bad 'ampere-8/mid 26B present' }
$b26 = Get-ModelCmd -Yaml $r.yaml -ModelKey 'gemma4-26b-a4b'
if ($b26 -match '--cpu-moe')                                   { Ok 'ampere-8/mid 26B uses --cpu-moe (ram=mid)' } else { Bad "ampere-8/mid 26B --cpu-moe (got: $b26)" }
if ($r.yaml -match 'members:\s*\[[^\]]*gemma4-26b-a4b[^\]]*\]') { Ok 'ampere-8/mid 26B in swap-group members' } else { Bad 'ampere-8/mid 26B group member' }
if ($r.yaml -notmatch '__[A-Z0-9_]+__')                        { Ok 'ampere-8/mid no unsubstituted tokens' } else { Bad 'ampere-8/mid leftover tokens' }
if ($r.verdict -and [int]$r.verdict.agent_ctx_tokens -eq 16384) { Ok 'ampere-8/mid agent_ctx_tokens=16384' } else { Bad 'ampere-8/mid agent_ctx_tokens' }

Write-Host "== ampere-8, ram_tier=low - 26B must DROP (no RAM path) =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'ampere-8' -RamTier 'low' -BigRam $false
if ($r.yaml -notmatch '(?m)^\s{2}gemma4-26b-a4b:')             { Ok 'ampere-8/low drops 26B model block' } else { Bad 'ampere-8/low 26B dropped' }
if ($r.yaml -notmatch 'gemma4-26b-a4b')                        { Ok 'ampere-8/low 26B gone from group members too' } else { Bad 'ampere-8/low 26B removed from members' }
if ($r.verdict -and (-not $r.verdict.include_26b))             { Ok 'ampere-8/low verdict include_26b=false' } else { Bad 'ampere-8/low verdict include_26b' }

Write-Host "== blackwell-16 - ctx 32768 + 26B on GPU (-ngl 99, no --cpu-moe) =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'blackwell-16' -RamTier 'mid' -BigRam $false
$macro = Get-CommonMacro $r.yaml
if ($macro -match '--ctx-size 32768')                          { Ok 'blackwell-16 ctx=32768' } else { Bad "blackwell-16 ctx (got: $macro)" }
$b26 = Get-ModelCmd -Yaml $r.yaml -ModelKey 'gemma4-26b-a4b'
if ($b26 -match '-ngl 99' -and $b26 -notmatch '--cpu-moe')     { Ok 'blackwell-16 26B on GPU (-ngl 99, no cpu-moe)' } else { Bad "blackwell-16 26B gpu (got: $b26)" }
if ($r.verdict -and $r.verdict.moe_mode -eq 'gpu')             { Ok 'blackwell-16 verdict moe_mode=gpu' } else { Bad 'blackwell-16 moe_mode' }

Write-Host "== ampere-6 - E2B, ctx 16384, NO 26B =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'ampere-6' -RamTier 'min' -BigRam $false
$macro = Get-CommonMacro $r.yaml
if ($macro -match '--ctx-size 16384')                          { Ok 'ampere-6 ctx=16384' } else { Bad "ampere-6 ctx (got: $macro)" }
if ($r.yaml -match '(?m)^\s{2}gemma4-e2b:')                    { Ok 'ampere-6 has E2B tier' } else { Bad 'ampere-6 E2B present' }
if ($r.yaml -notmatch 'gemma4-26b-a4b')                        { Ok 'ampere-6 has NO 26B tier' } else { Bad 'ampere-6 26B absent' }
if ($r.verdict -and $r.verdict.resident_tier -ne 'gemma4-26b-a4b') { Ok 'ampere-6 resident is not 26B' } else { }  # informational

Write-Host "== amd-gcn - 8192 / f16 / flash-attn off (vulkan) =="
$r = Invoke-Render -Backend 'vulkan' -ProfileId 'amd-gcn' -RamTier 'low' -BigRam $false
$macro = Get-CommonMacro $r.yaml
if ($macro -match '--ctx-size 8192')                           { Ok 'amd-gcn ctx=8192' } else { Bad "amd-gcn ctx (got: $macro)" }
if ($macro -match '--cache-type-k f16' -and $macro -match '--cache-type-v f16') { Ok 'amd-gcn KV=f16' } else { Bad 'amd-gcn KV f16' }
if ($macro -match '--flash-attn off')                          { Ok 'amd-gcn flash-attn off' } else { Bad "amd-gcn flash-attn off (got: $macro)" }
if ($r.yaml -notmatch 'gemma4-26b-a4b')                        { Ok 'amd-gcn NO 26B' } else { Bad 'amd-gcn 26B absent' }

Write-Host "== dual-gpu - two groups + per-GPU CUDA_VISIBLE_DEVICES, no exclusive swap =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'dual-gpu' -RamTier 'mid' -BigRam $false
if (($r.yaml -split "`r?`n" | Where-Object { $_ -match 'members:\s*\[' }).Count -ge 2) { Ok 'dual-gpu renders TWO groups' } else { Bad 'dual-gpu two groups' }
if ($r.yaml -match 'CUDA_VISIBLE_DEVICES=0' -and $r.yaml -match 'CUDA_VISIBLE_DEVICES=1') { Ok 'dual-gpu pins device 0 AND device 1' } else { Bad 'dual-gpu CUDA_VISIBLE_DEVICES' }
if ($r.yaml -notmatch 'exclusive:\s*true')                     { Ok 'dual-gpu has NO exclusive swap group' } else { Bad 'dual-gpu exclusive:true present' }
# 26B (architect) block must carry the device-0 pin in its env list.
$arch26 = ($r.yaml -split "`r?`n" | Where-Object { $_ -match 'GGML_CUDA_DISABLE_GRAPHS' } | Select-Object -First 1)
if ($arch26 -match 'CUDA_VISIBLE_DEVICES=0')                   { Ok 'dual-gpu architect (26B) env pins device 0' } else { Bad "dual-gpu architect device (got: $arch26)" }
$macro = Get-CommonMacro $r.yaml
if ($macro -match '--ctx-size 32768')                          { Ok 'dual-gpu ctx=32768' } else { Bad "dual-gpu ctx (got: $macro)" }
if ($r.verdict -and $r.verdict.render_backend -eq 'dual-cuda') { Ok 'dual-gpu renders the dual-cuda template' } else { Bad 'dual-gpu render_backend' }
if ($r.yaml -notmatch '__[A-Z0-9_]+__')                        { Ok 'dual-gpu no unsubstituted tokens' } else { Bad 'dual-gpu leftover tokens' }

Write-Host "== cpu - 26B kept on ram=mid OR high (>= ~56GB); dropped on low/min =="
$rHigh = Invoke-Render -Backend 'cpu' -ProfileId 'cpu' -RamTier 'high' -BigRam $false
$rLow  = Invoke-Render -Backend 'cpu' -ProfileId 'cpu' -RamTier 'low'  -BigRam $false
if ($rHigh.yaml -match 'gemma4-26b-a4b')                       { Ok 'cpu/high keeps 26B' } else { Bad 'cpu/high 26B present' }
if ($rLow.yaml -notmatch 'gemma4-26b-a4b')                     { Ok 'cpu/low drops 26B' } else { Bad 'cpu/low 26B dropped' }
$macro = Get-CommonMacro $rHigh.yaml
if ($macro -match '--ctx-size 8192' -and $macro -match '--threads') { Ok 'cpu ctx=8192 + threads substituted' } else { Bad "cpu ctx/threads (got: $macro)" }

Write-Host "== unknown profile - falls back to the backend's baked defaults (valid config) =="
$r = Invoke-Render -Backend 'cuda' -ProfileId 'does-not-exist' -RamTier 'mid' -BigRam $false
if ($r.yaml -notmatch '__[A-Z0-9_]+__')                        { Ok 'unknown profile: NO leftover tokens (fallback substituted)' } else { Bad 'unknown profile leftover tokens' }
$macro = Get-CommonMacro $r.yaml
if ($macro -match '--ctx-size 16384' -and $macro -match '--cache-type-k q8_0') { Ok 'unknown profile: CUDA fallback = 16384/q8_0' } else { Bad "unknown profile CUDA defaults (got: $macro)" }
if ($r.verdict -and $r.verdict.profile -eq 'does-not-exist')   { Ok 'unknown profile: verdict echoes the unknown id' } else { Bad 'unknown profile verdict' }

Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue

Write-Host ""
if ($fail -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $fail" -ForegroundColor Red; exit 1
