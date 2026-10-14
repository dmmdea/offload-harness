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
#   - Test-AllocFailure (the classifier that sends the ctx probe down the downshift path, load-bearing on nine tiers
#     since the probe loads the card-resident 26B) classifies the error strings llama.cpp, Vulkan and a dying server
#     print, and a plain timeout or a model the yaml does not define as NOT an allocation failure;
#   - Select-ServedTiers / Test-YamlServesModel: a chat tier whose gguf is on disk is exercised only when the active
#     yaml still defines it (a box re-rendered without the 26B keeps the 14 GB file, and asking llama-swap for a model
#     it does not know gets blamed on the card);
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

  Write-Host "== Test-AllocFailure: the class that sends the ctx probe down the downshift path =="
  # Nine tiers load a card-resident 26B in the ctx probe since 0186 w4, so this regex is the gate between an OOM that
  # gets one halved-ctx retry and a load failure that is recorded as 'not-attempted'. It had no test of its own after
  # the remediation test (its only caller of it) was retired, so a regression here would have been silent.
  $allocCases = @(
    @{ n = 'Vulkan out of device memory, code form';  t = 'ggml_vulkan: vk::Device::allocateMemory: ErrorOutOfDeviceMemory VK_ERROR_OUT_OF_DEVICE_MEMORY'; want = $true }
    @{ n = 'Vulkan out of device memory, prose form'; t = 'ggml_vulkan: out of device memory'; want = $true }
    @{ n = 'CUDA runtime form with a colon';          t = 'CUDA error: out of memory'; want = $true }
    @{ n = 'CUDA form without a colon';               t = 'CUDA out of memory'; want = $true }
    @{ n = 'llama.cpp cudaMalloc buffer failure';     t = 'ggml_backend_cuda_buffer_type_alloc_buffer: allocating 14000 MiB on device 0: cudaMalloc failed: out of memory'; want = $true }
    @{ n = 'CUDA error code';                         t = 'cudaErrorMemoryAllocation'; want = $true }
    @{ n = 'device lost, prose';                      t = 'Device lost'; want = $true }
    @{ n = 'device lost, Vulkan code';                t = 'VK_ERROR_DEVICE_LOST'; want = $true }
    @{ n = 'failed to allocate';                      t = 'failed to allocate'; want = $true }
    @{ n = 'unable to allocate';                      t = 'unable to allocate memory'; want = $true }
    @{ n = 'server died mid-load, connection closed'; t = 'The connection was forcibly closed by the remote host'; want = $true }
    @{ n = 'server died mid-load, refused';           t = 'No connection could be made because the target machine actively refused it'; want = $true }
    @{ n = 'server exited during load';               t = 'the server process exited unexpectedly'; want = $true }
    @{ n = 'a plain timeout is not an allocation failure'; t = 'The operation has timed out'; want = $false }
    @{ n = 'a model the yaml does not define is not an allocation failure'; t = 'HTTP 400 could not find real modelID for gemma4-26b-a4b'; want = $false }
    @{ n = 'empty error text';                        t = ''; want = $false }
  )
  foreach ($c in $allocCases) {
    $got = [bool](Test-AllocFailure $c.t)
    Assert ($got -eq $c.want) "Test-AllocFailure [$($c.n)] -> $($c.want)"
  }

  Write-Host "== Select-ServedTiers: only a chat tier the ACTIVE yaml defines is exercised =="
  # A gguf outlives the render that wanted it. The CUDA fallback served the 26B until 0186 w4, so a box that downloaded
  # it keeps the 14 GB file after a re-render that no longer defines the model. File existence says what can load; the
  # yaml says what can be asked for; the selftest needs both, or it asks llama-swap for a model it does not know and
  # blames the card for the answer.
  $yamlNo26 = "models:`n  offload-e4b:`n    aliases: [gemma4-e4b]`n    cmd: >-`n      llama-server -m e4b.gguf`n  gemma4-e2b:`n    cmd: >-`n      llama-server -m e2b.gguf`n"
  $yamlAll  = $yamlNo26 + "  gemma4-26b-a4b:`n    cmd: >-`n      llama-server -m 26b.gguf`n"
  Assert ([bool](Test-YamlServesModel -YamlText $yamlNo26 -Id 'offload-e4b'))     'a model key is served'
  Assert ([bool](Test-YamlServesModel -YamlText $yamlNo26 -Id 'gemma4-e4b'))      'an alias is served'
  Assert (-not (Test-YamlServesModel -YamlText $yamlNo26 -Id 'gemma4-26b-a4b'))   'a model the yaml does not define is not served'
  Assert ([bool](Test-YamlServesModel -YamlText $yamlAll -Id 'gemma4-26b-a4b'))   'the 26B is served by a yaml that defines it'
  Assert ([bool](Test-YamlServesModel -YamlText "models:`n  `"gemma4-26b-a4b`":`n    cmd: x`n" -Id 'gemma4-26b-a4b')) 'a quoted model key is served'
  Assert (-not (Test-YamlServesModel -YamlText "models:`n  offload-e4b:`n    cmd: x`n  # gemma4-26b-a4b: dropped by this tier`n# gemma4-26b-a4b:`n" -Id 'gemma4-26b-a4b')) 'a model named only in a comment is not served'
  Assert (-not (Test-YamlServesModel -YamlText "models:`n  gemma4-26b-a4b-q8:`n    cmd: x`n" -Id 'gemma4-26b-a4b')) 'a longer key sharing the prefix is not the model'
  Assert (-not (Test-YamlServesModel -YamlText "models:`n  offload-e4b:`n    cmd: llama-server -m gemma4-26b-a4b.gguf`n" -Id 'gemma4-26b-a4b')) 'a gguf file name inside a command is not a model key'
  Assert (-not (Test-YamlServesModel -YamlText '' -Id 'offload-e4b'))             'no yaml text serves nothing'
  Assert (-not (Test-YamlServesModel -YamlText $yamlAll -Id ''))                  'an empty id is not served'
  $realTpl = [System.IO.File]::ReadAllText((Join-Path (Join-Path $setupDir 'templates') 'llama-swap.win-cuda.yaml'))
  foreach ($tid in @('offload-e4b', 'gemma4-e2b', 'gemma4-26b-a4b')) {
    Assert ([bool](Test-YamlServesModel -YamlText $realTpl -Id $tid)) "the shipped win-cuda template serves $tid (the key layout the check reads)"
  }

  $yNo26File = Join-Path $tmpDir 'no26.yaml'
  $yAllFile  = Join-Path $tmpDir 'all.yaml'
  $yNoneFile = Join-Path $tmpDir 'none.yaml'
  [System.IO.File]::WriteAllText($yNo26File, $yamlNo26, (New-Object System.Text.UTF8Encoding($false)))
  [System.IO.File]::WriteAllText($yAllFile,  $yamlAll,  (New-Object System.Text.UTF8Encoding($false)))
  [System.IO.File]::WriteAllText($yNoneFile, "models:`n  some-other-model:`n    cmd: x`n", (New-Object System.Text.UTF8Encoding($false)))
  $ids = { param($sel) (@($sel) | ForEach-Object { $_.id }) -join ',' }
  Assert ((& $ids (Select-ServedTiers -Tiers $TIER_SPEC -ConfigPath $yNo26File)) -eq 'offload-e4b,gemma4-e2b') 'the 26B gguf on disk is not exercised when the yaml no longer defines it (order kept)'
  Assert ((& $ids (Select-ServedTiers -Tiers $TIER_SPEC -ConfigPath $yAllFile))  -eq 'offload-e4b,gemma4-e2b,gemma4-26b-a4b') 'a yaml that defines all three exercises all three'
  Assert ((& $ids (Select-ServedTiers -Tiers $TIER_SPEC -ConfigPath (Join-Path $tmpDir 'absent.yaml'))) -eq 'offload-e4b,gemma4-e2b,gemma4-26b-a4b') 'an unreadable yaml is not a verdict: every installed tier stays'
  Assert ((& $ids (Select-ServedTiers -Tiers $TIER_SPEC -ConfigPath $yNoneFile)) -eq 'offload-e4b,gemma4-e2b,gemma4-26b-a4b') 'a yaml that defines none of the chat tiers is not one this rule can judge: every installed tier stays'
  Assert (@(Select-ServedTiers -Tiers @($TIER_SPEC[0]) -ConfigPath $yNo26File).Count -eq 1) 'one served tier comes back as a one-element list'
  Assert (@(Select-ServedTiers -Tiers @() -ConfigPath $yNo26File).Count -eq 0) 'no tier on disk selects nothing'

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
  Assert ($main -match '\$installedTiers = @\(Select-ServedTiers -Tiers \$tiersOnDisk -ConfigPath \$yamlPath\)') 'MAIN filters the chat tiers whose gguf is on disk through the ACTIVE yaml'
  Assert ($main -match 'not defined by the active yaml') 'MAIN says which tier it skipped because the yaml no longer defines it'
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
