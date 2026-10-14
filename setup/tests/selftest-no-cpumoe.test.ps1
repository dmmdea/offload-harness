# setup/tests/selftest-no-cpumoe.test.ps1 - the selftest never moves a 26B's experts into host RAM
# (0186 w4; INV-1: the cards do the inference and host RAM is overflow only).
#
# History. On a 26B allocation failure the selftest used to run Invoke-Remediate26B: insert --cpu-moe into the
# LIVE llama-swap.yaml, restart, retry, and record "added --cpu-moe" in the receipt. The audit (0.184.0) refuses
# that flag in a render, so the selftest was writing what the next audit-yaml fails; and its ctx probe launched a
# standalone 26B with --cpu-moe and recorded "ctx MEASURED ok" for a placement the yaml does not serve. Now a 26B
# tier that fails is a tier-local FAIL carrying operator advice (tiers[].detail, from Get-MoEFailureAdvice), the
# yaml is never edited, and no probe passes an expert-placement flag.
#
# How it works: selftest.ps1 has a test seam (OFFLOAD_SELFTEST_DOT_SOURCE=1 -> defines functions, paths and receipt
# state, returns before MAIN). This test dot-sources it against a temp OFFLOAD_HOME and asserts:
#   - the remediator is gone and Invoke-ProbeLoad has no -CpuMoe switch;
#   - Get-MoEFailureAdvice (pure: text in, text out) names the real options and never the flag;
#   - MAIN is below the seam and cannot run here, so it is pinned by source: no remediator, no CpuMoe switch or
#     variable on a non-comment line, the tier loop calls the advice and records it on the tier row, and the only
#     receipt.remediations entry left is the ctx downshift.
# CI does not run this file (only render.tests.ps1 and install-config-seed.test.ps1 are wired in); the same source
# pins run in `go test` (TestSelftestHasNoCpuMoeSwitchAndNoRemediator, TestInstallerSourcesWriteNoEveryExpertFlag).
#
# Usage:
#   pwsh -NoProfile -File setup\tests\selftest-no-cpumoe.test.ps1
# Exit: 0 all assertions pass, 1 otherwise. PS 5.1 + pwsh 7 compatible.
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

$tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("nocpumoe-{0}" -f ([guid]::NewGuid().ToString('N').Substring(0, 8)))
[void][System.IO.Directory]::CreateDirectory($tmpDir)
$prevHome = $env:OFFLOAD_HOME
$prevSeam = $env:OFFLOAD_SELFTEST_DOT_SOURCE
try {
  Write-Host "==> selftest never places experts in RAM | engine=$($PSVersionTable.PSVersion) | tmp=$tmpDir"

  # --- dot-source the selftest through its test seam (functions + paths, no MAIN) -------------
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = '1'
  $env:OFFLOAD_HOME = $tmpDir
  . $selftestPath
  Assert ($yamlPath -eq (Join-Path $tmpDir 'llama-swap.yaml')) 'seam: dot-source defined the paths and $yamlPath resolves under the temp OFFLOAD_HOME'

  Write-Host "== the remediator and the -CpuMoe switch are gone =="
  Assert (-not (Get-Command Invoke-Remediate26B -ErrorAction SilentlyContinue)) 'Invoke-Remediate26B is retired (it inserted --cpu-moe into the live yaml)'
  Assert ((Get-Command Invoke-ProbeLoad).Parameters.Keys -notcontains 'CpuMoe') 'Invoke-ProbeLoad has no -CpuMoe switch: no probe launches a standalone 26B with the experts in RAM'
  Assert (@($receipt.remediations).Count -eq 0) 'the receipt starts with no remediation'

  Write-Host "== Get-MoEFailureAdvice: what the operator is told when the 26B does not load =="
  Assert ([bool](Get-Command Get-MoEFailureAdvice -ErrorAction SilentlyContinue)) 'the advice helper exists'
  $cases = @(
    @{ name = 'Vulkan out of device memory'; err = 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory VK_ERROR_OUT_OF_DEVICE_MEMORY' },
    @{ name = 'CUDA out of memory';          err = 'CUDA error: out of memory' },
    @{ name = 'no error text';               err = '' }
  )
  foreach ($c in $cases) {
    $advice = [string](Get-MoEFailureAdvice -ErrorText $c.err)
    $n = $c.name
    Assert (-not [string]::IsNullOrWhiteSpace($advice)) "[$n] the advice is not empty"
    Assert (($c.err -eq '') -or $advice.Contains($c.err)) "[$n] the advice carries the error it answers"
    Assert ($advice -match 'RAM is overflow only') "[$n] the advice says why selftest will not move experts to the CPU"
    Assert (($advice -match 'moe_26b') -and ($advice -match '\bdrop\b')) "[$n] the advice names the table fix: moe_26b drop"
    Assert ($advice -match 'n_cpu_moe_max') "[$n] the advice names the one sanctioned spill: a measured n_cpu_moe with n_cpu_moe_max"
    Assert ($advice -match 'resident_tier') "[$n] the advice says a 26B-resident tier needs its resident_tier changed too"
    Assert ($advice -notmatch '(?i)--cpu-moe|cmoe') "[$n] the advice never suggests the every-expert flag"
    Assert ($advice -notmatch '[^\x00-\x7F]') "[$n] the advice is ASCII (selftest.ps1 is ASCII only)"
  }

  Write-Host "== MAIN (below the seam) is pinned by source =="
  $text = [System.IO.File]::ReadAllText($selftestPath, [System.Text.Encoding]::UTF8)
  $seamLine = 'if ($env:OFFLOAD_SELFTEST_DOT_SOURCE -eq ''1'') { return }'
  $seamAt = $text.IndexOf($seamLine)
  Assert ($seamAt -gt 0) 'the dot-source seam line is still there'
  $main = if ($seamAt -gt 0) { $text.Substring($seamAt) } else { '' }
  $code = @($text -split "`r?`n" | Where-Object { $_.TrimStart() -notmatch '^#' })
  Assert (-not $text.Contains('Invoke-Remediate26B')) 'no mention of Invoke-Remediate26B, in code or in prose'
  Assert (@($code | Where-Object { $_ -match '(?i)cpumoe' }).Count -eq 0) 'no non-comment line has a CpuMoe switch or variable'
  Assert (@($code | Where-Object { $_ -match '(?i)--cpu-moe' }).Count -eq 0) 'no non-comment line spells --cpu-moe'
  Assert ($main -match 'Get-MoEFailureAdvice -ErrorText') 'the tier loop calls Get-MoEFailureAdvice for a failed 26B tier'
  Assert ($main -match 'status = ''fail''; detail = \$detail') 'the failed tier row records the advice in tiers[].detail'
  Assert ([regex]::Matches($text, '\$receipt\.remediations \+=').Count -eq 1) 'receipt.remediations is appended to in exactly one place'
  Assert ($text -match 'remediations \+= ,\(\[ordered\]@\{ tier = \$residentId; action = \("downshift ctx') 'and that place is the ctx downshift, not a yaml edit'
  Assert ($text -notmatch 'Write-Utf8NoBom -Path \$yamlPath') 'selftest never rewrites llama-swap.yaml'
}
finally {
  $env:OFFLOAD_SELFTEST_DOT_SOURCE = $prevSeam
  $env:OFFLOAD_HOME = $prevHome
  if ([System.IO.Directory]::Exists($tmpDir)) { [System.IO.Directory]::Delete($tmpDir, $true) }
}

if ($failures -gt 0) { Write-Host "RESULT: FAIL ($failures assertion(s) failed)" -ForegroundColor Red; exit 1 }
Write-Host 'RESULT: PASS (all assertions)' -ForegroundColor Green
exit 0
