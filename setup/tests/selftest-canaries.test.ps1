# setup/tests/selftest-canaries.test.ps1 - unit tests for the J1 canary PURE helpers in
# selftest.ps1 (Get-WordOverlap / Get-FaStateFromLog / Get-CosineSim). Uses the standing
# OFFLOAD_SELFTEST_DOT_SOURCE seam: selftest.ps1 defines its functions and returns before
# MAIN, so no hardware, model, or network is touched.
#   PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail.
# Usage (both shells): powershell -ExecutionPolicy Bypass -File setup\tests\selftest-canaries.test.ps1
#                      pwsh       -File setup/tests/selftest-canaries.test.ps1
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$env:OFFLOAD_SELFTEST_DOT_SOURCE = '1'
try {
  . (Join-Path (Split-Path -Parent $here) 'selftest.ps1')
} finally {
  Remove-Item Env:OFFLOAD_SELFTEST_DOT_SOURCE -ErrorAction SilentlyContinue
}

$script:fail = 0
function Assert-Eq {
  param([string]$Label, $Got, $Expected)
  if ("$Got" -eq "$Expected") { Write-Host "PASS $Label -> $Got" }
  else { Write-Host "FAIL $Label -> '$Got' (expected '$Expected')"; $script:fail++ }
}
function Assert-Range {
  param([string]$Label, [double]$Got, [double]$Min, [double]$Max)
  if ($Got -ge $Min -and $Got -le $Max) { Write-Host "PASS $Label -> $Got" }
  else { Write-Host "FAIL $Label -> $Got (expected [$Min, $Max])"; $script:fail++ }
}

Write-Host '== Get-WordOverlap (Jaccard word-set overlap) =='
Assert-Eq 'identical text'        (Get-WordOverlap -A 'two, three, five, seven' -B 'two, three, five, seven') 1
Assert-Eq 'disjoint text'         (Get-WordOverlap -A 'alpha beta' -B 'gamma delta') 0
Assert-Eq 'empty A'               (Get-WordOverlap -A '' -B 'anything') 0
Assert-Eq 'empty both'            (Get-WordOverlap -A '' -B '') 0
Assert-Range 'half overlap'       (Get-WordOverlap -A 'one two three four' -B 'three four five six') 0.30 0.40   # 2/6
Assert-Range 'case+punct folded'  (Get-WordOverlap -A 'Two, Three; FIVE!' -B 'two three five') 1 1
Assert-Range 'near-identical gen' (Get-WordOverlap -A '2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37' -B '2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 41') 0.80 0.95

Write-Host '== Get-FaStateFromLog (flash-attn state scan) =='
Assert-Eq 'classic = 1'          (Get-FaStateFromLog -LogText 'llama_context: flash_attn = 1') 'on'
Assert-Eq 'enabled form'         (Get-FaStateFromLog -LogText 'flash_attn : enabled') 'on'
# The REAL Jul-2026 llama-server line at -lv 10 (captured live on Qube, 2026-07-24):
Assert-Eq 'live -lv10 enabled'   (Get-FaStateFromLog -LogText '0.01.341.213 I llama_context: flash_attn    = enabled') 'on'
Assert-Eq 'live -lv10 disabled'  (Get-FaStateFromLog -LogText '0.01.341.213 I llama_context: flash_attn    = disabled') 'off'
Assert-Eq 'classic = 0'          (Get-FaStateFromLog -LogText 'llama_context: flash_attn = 0') 'off'
Assert-Eq 'disabled notice'      (Get-FaStateFromLog -LogText 'ggml_vulkan: disabling flash attention (head size unsupported)') 'off'
Assert-Eq 'not-supported notice' (Get-FaStateFromLog -LogText 'warning: flash attention is not supported on this backend') 'off'
Assert-Eq 'no marker'            (Get-FaStateFromLog -LogText 'main: server is listening on 127.0.0.1') ''
Assert-Eq 'empty log'            (Get-FaStateFromLog -LogText '') ''
Assert-Eq 'auto alone = unknown' (Get-FaStateFromLog -LogText 'flash_attn = auto') ''
Assert-Eq 'off wins over on'     (Get-FaStateFromLog -LogText "flash_attn = 1`ndisabling flash attention") 'off'

Write-Host '== Get-CosineSim =='
Assert-Eq 'identical vectors'    (Get-CosineSim -A @(1.0, 2.0, 3.0) -B @(1.0, 2.0, 3.0)) 1
Assert-Eq 'orthogonal vectors'   (Get-CosineSim -A @(1.0, 0.0) -B @(0.0, 1.0)) 0
Assert-Eq 'opposite vectors'     (Get-CosineSim -A @(1.0, 0.0) -B @(-1.0, 0.0)) -1
Assert-Eq 'length mismatch null' (Get-CosineSim -A @(1.0, 2.0) -B @(1.0)) ''
Assert-Eq 'zero vector null'     (Get-CosineSim -A @(0.0, 0.0) -B @(1.0, 1.0)) ''

if ($script:fail -eq 0) { Write-Host 'ALL PASS'; exit 0 }
Write-Host "FAILURES: $script:fail"; exit 1
