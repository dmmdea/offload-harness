# setup/tests/selftest-remediation.test.ps1 - stub-integration test for the R4.2 auto-remediation
# path in setup/selftest.ps1 (Invoke-Remediate26B), whose live debut would otherwise be an end
# user's machine hitting a real 26B allocation failure.
#
# How it works: selftest.ps1 has a test seam (OFFLOAD_SELFTEST_DOT_SOURCE=1 -> defines functions,
# paths, and receipt state, returns before MAIN). This test dot-sources it against a temp
# OFFLOAD_HOME containing a COPY of a rendered llama-swap.yaml, overrides the three transport
# helpers PowerShell-style (a later function definition in the same scope wins; name resolution
# is dynamic at call time), then drives the EXACT wiring MAIN uses for the 26b tier:
#   Invoke-Chat(26b) fails with an alloc-class error -> Test-AllocFailure (REAL) classifies it ->
#   Invoke-Remediate26B (REAL: yaml read, block find, surgical --cpu-moe insert, BOM-less rewrite,
#   restart via stub, readiness via stub, retry via stub) -> remediation recorded.
#
# Asserts: retry succeeded; remediation recorded outcome=pass; yaml rewrite surgical (exactly one
# changed line, --cpu-moe after the 26b .gguf, e4b/e2b untouched, BOM-less); restart+retry ran in
# order; the ORIGINAL source yaml is byte-untouched (only the temp copy is rewritten).
#
# Usage:
#   pwsh -NoProfile -File setup\tests\selftest-remediation.test.ps1
#     (default: renders setup\templates\llama-swap.win-vulkan.yaml with dummy paths - the
#      vulkan template ships WITHOUT --cpu-moe on the 26b tier, the exact remediation target)
#   ... -SourceYaml <path-to-a-real-rendered-llama-swap.yaml>
#     (test against a copy of a real rendered config; it must NOT already have --cpu-moe on 26b)
# Exit: 0 all assertions pass, 1 otherwise. PS 5.1 + pwsh 7 compatible.
param(
  [string]$SourceYaml = ''
)
$ErrorActionPreference = 'Stop'
$here     = Split-Path -Parent $MyInvocation.MyCommand.Path
$setupDir = Split-Path -Parent $here
$selftestPath = Join-Path $setupDir 'selftest.ps1'

$failures = 0
function Assert {
  param([bool]$Cond, [string]$Name)
  if ($Cond) { Write-Host "  PASS $Name" -ForegroundColor Green }
  else       { Write-Host "  FAIL $Name" -ForegroundColor Red; $script:failures++ }
}

$tmpDir = Join-Path $env:TEMP ("remtest-{0}" -f ([guid]::NewGuid().ToString('N').Substring(0, 8)))
New-Item -ItemType Directory -Force -Path $tmpDir | Out-Null
$prevHome = $env:OFFLOAD_HOME
$prevSeam = $env:OFFLOAD_SELFTEST_DOT_SOURCE
try {
  Write-Host "==> R4.2 remediation stub-integration test | engine=$($PSVersionTable.PSVersion) | tmp=$tmpDir"

  # --- arrange: put the yaml-under-test at the temp OFFLOAD_HOME ------------------------------
  $workYaml = Join-Path $tmpDir 'llama-swap.yaml'
  if ($SourceYaml) {
    if (-not (Test-Path $SourceYaml)) { throw "SourceYaml not found: $SourceYaml" }
    $srcPath = (Resolve-Path $SourceYaml).Path
    Copy-Item $srcPath $workYaml
    Write-Host "    yaml under test: copy of $srcPath"
  } else {
    $srcPath = Join-Path $setupDir 'templates\llama-swap.win-vulkan.yaml'
    # -Encoding UTF8: PS 5.1 would decode the BOM-less template as ANSI and mojibake its em-dash.
    $text = (Get-Content -Raw $srcPath -Encoding UTF8).Replace('__LLAMA_BIN__\', 'C:/x/llama/').Replace('__MODELS__\', 'C:/x/models/')
    [System.IO.File]::WriteAllText($workYaml, $text, (New-Object System.Text.UTF8Encoding($false)))
    Write-Host "    yaml under test: vulkan template rendered with dummy paths"
  }
  $srcHashBefore = (Get-FileHash -Path $srcPath -Algorithm SHA256).Hash
  # Guard on NON-COMMENT lines only: the vulkan template's comment header itself mentions
  # --cpu-moe ("...add --cpu-moe to the 26b cmd"), which is not a config occurrence.
  $cpuMoeConfigLines = @(Get-Content $workYaml -Encoding UTF8 | Where-Object { ($_ -notmatch '^\s*#') -and ($_ -match '--cpu-moe') })
  if ($cpuMoeConfigLines.Count -gt 0) {
    throw "yaml under test already has --cpu-moe in config (not comments) - pick a source without it (the vulkan template/render)"
  }
  # Read as UTF8 explicitly (PS 5.1 decodes BOM-less files as ANSI otherwise). Also remember
  # whether the source contains non-ASCII bytes (the templates' comment em-dash, UTF-8 E2 80 94)
  # so we can assert the rewrite preserves them - the exact 5.1 mojibake class this test caught.
  $before = Get-Content $workYaml -Encoding UTF8
  $srcHasNonAscii = [bool]([System.IO.File]::ReadAllBytes($workYaml) | Where-Object { $_ -ge 0x80 } | Select-Object -First 1)

  # --- dot-source the selftest through its test seam (functions + paths, no MAIN) -------------
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = '1'
  $env:OFFLOAD_HOME = $tmpDir
  . $selftestPath
  Assert ($yamlPath -eq $workYaml) 'seam: dot-source defined functions and $yamlPath resolves to the temp copy'

  # --- stub the transport helpers (later definition in the same scope wins) -------------------
  $script:callLog      = New-Object System.Collections.ArrayList
  $script:startSwapArg = $null
  $script:chatCalls    = 0
  function Start-Swap {
    param([string]$ConfigPath)
    $script:startSwapArg = $ConfigPath
    [void]$script:callLog.Add('start-swap')
    return [pscustomobject]@{ Id = 0; HasExited = $true }
  }
  function Wait-SwapReady {
    param([int]$TimeoutSec = 120)
    [void]$script:callLog.Add('wait-ready')
    return $true
  }
  function Invoke-Chat {
    param([string]$Model, [string]$UserContent, [int]$MaxTokens = 64, [int]$TimeoutSec = 300)
    $script:chatCalls++
    [void]$script:callLog.Add("chat#$($script:chatCalls):$Model")
    if ($script:chatCalls -eq 1) {
      return @{ ok = $false; latency_s = 1.2; tokens = 0; tok_s = $null; text = ''
                error = 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory VK_ERROR_OUT_OF_DEVICE_MEMORY' }
    }
    return @{ ok = $true; latency_s = 2.0; tokens = 8; tok_s = 4.0; text = 'ready'; error = $null }
  }

  # --- act: drive the EXACT wiring MAIN's tier loop uses for the 26b tier ---------------------
  $remediations = @()
  $res = Invoke-Chat -Model 'gemma4-26b-a4b' -UserContent 'Reply with exactly the single word: ready' -MaxTokens 48 -TimeoutSec 300
  Assert ((-not $res.ok) -and (Test-AllocFailure $res.error)) 'initial 26b chat fails with an alloc-class error (REAL Test-AllocFailure classifies it)'
  if (-not $res.ok -and (Test-AllocFailure $res.error)) {
    $remOutcome = Invoke-Remediate26B
    $remediations += ,([ordered]@{ tier = 'gemma4-26b-a4b'; action = 'added --cpu-moe'; outcome = $remOutcome.outcome })
    if ($remOutcome.retry) { $res = $remOutcome.retry }
  }

  # --- assert ----------------------------------------------------------------------------------
  Assert ($res.ok -eq $true) 'post-remediation retry succeeded'
  Assert ($remediations.Count -eq 1 -and $remediations[0].outcome -eq 'pass' -and $remediations[0].action -eq 'added --cpu-moe' -and $remediations[0].tier -eq 'gemma4-26b-a4b') 'remediation recorded: tier=gemma4-26b-a4b action=added --cpu-moe outcome=pass'
  $seq = ($script:callLog -join ' | ')
  Write-Host "    call sequence: $seq"
  Assert ($seq -eq 'chat#1:gemma4-26b-a4b | start-swap | wait-ready | chat#2:gemma4-26b-a4b') 'restart+retry ran in order: fail -> rewrite -> start-swap -> wait-ready -> retry(26b)'
  Assert ($script:startSwapArg -eq $workYaml) 'restart pointed at the rewritten temp yaml'

  $after = Get-Content $workYaml -Encoding UTF8
  $changed = @(Compare-Object $before $after | Where-Object { $_.SideIndicator -eq '=>' })
  Assert ($changed.Count -eq 1 -and $changed[0].InputObject -match '26B-A4B.*\.gguf --cpu-moe') 'yaml rewrite surgical: exactly ONE changed line; --cpu-moe sits after the 26b .gguf path'
  if ($srcHasNonAscii) {
    # Regression assert for the 5.1 mojibake bug this test caught: the rewrite must round-trip
    # non-ASCII comment bytes (em-dash U+2014 = E2 80 94) intact, not double-encode them.
    $afterText = [System.Text.Encoding]::UTF8.GetString([System.IO.File]::ReadAllBytes($workYaml))
    Assert ($afterText.Contains([string][char]0x2014)) 'non-ASCII comment chars (em-dash) survived the rewrite un-mojibaked'
  }
  $otherTierHits = @($after | Where-Object { ($_ -match 'E4B|E2B|embeddinggemma') -and ($_ -match '--cpu-moe') })
  Assert ($otherTierHits.Count -eq 0) 'e4b / e2b / embedding lines untouched'
  $bytes = [System.IO.File]::ReadAllBytes($workYaml)
  Assert (-not ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF)) 'rewritten yaml is BOM-less (Go/yaml-parser safe on both engines)'
  $srcHashAfter = (Get-FileHash -Path $srcPath -Algorithm SHA256).Hash
  Assert ($srcHashBefore -eq $srcHashAfter) 'ORIGINAL source yaml untouched (SHA256 identical; only the temp copy was rewritten)'
}
finally {
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = $prevSeam
  $env:OFFLOAD_HOME = $prevHome
  Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
}

if ($failures -gt 0) { Write-Host "RESULT: FAIL ($failures assertion(s) failed)" -ForegroundColor Red; exit 1 }
Write-Host 'RESULT: PASS (all assertions)' -ForegroundColor Green
exit 0
