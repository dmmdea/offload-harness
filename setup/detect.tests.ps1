# setup/detect.tests.ps1 — unit-style assertions for detect.ps1's classifier.
# Runs the pure classification helpers (Get-GpuArch / Get-RamTier / Get-Profile)
# against synthetic tuples and asserts the expected profile ids. This is a thin
# wrapper: it invokes detect.ps1 -SelfTest, which owns the assertion table so the
# rules and their tests stay in one file. It then runs a REAL detection (child
# process) to assert the emitted JSON verdict's accelerators wiring — that part
# cannot live in -SelfTest, which never reaches the verdict emission.
#   PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail.
# Usage (both shells): powershell -ExecutionPolicy Bypass -File setup\detect.tests.ps1
#                      pwsh       -File setup/detect.tests.ps1
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $here 'detect.ps1') -SelfTest
$fail = $LASTEXITCODE

# -- accelerators in the emitted verdict (ADR 0024) -------------------------
# Same-host PowerShell as this run (5.1 and 7 both), exactly as install.ps1 does.
$psHost = (Get-Process -Id $PID).Path
function Assert {
  param([bool]$Cond, [string]$Label)
  if ($Cond) { Write-Host "PASS accel $Label" }
  else { Write-Host "FAIL accel $Label"; $script:fail++ }
}

Write-Host '== accelerators in the emitted verdict =='
$json = (& $psHost -NoProfile -ExecutionPolicy Bypass -File (Join-Path $here 'detect.ps1') |
  Where-Object { $_ -match '^\s*\{.*\}\s*$' } | Select-Object -Last 1) | ConvertFrom-Json
Assert ($null -ne $json -and $null -ne $json.PSObject.Properties['accelerators']) 'verdict carries an accelerators array (may be empty)'

$env:OFFLOAD_ACCELERATORS = 'hailo-8l'
try {
  $json2 = (& $psHost -NoProfile -ExecutionPolicy Bypass -File (Join-Path $here 'detect.ps1') |
    Where-Object { $_ -match '^\s*\{.*\}\s*$' } | Select-Object -Last 1) | ConvertFrom-Json
} finally {
  Remove-Item Env:OFFLOAD_ACCELERATORS -ErrorAction SilentlyContinue
}
Assert ($null -ne $json2 -and (@($json2.accelerators) -contains 'hailo-8l')) 'OFFLOAD_ACCELERATORS override lands in the verdict'

if ($fail -eq 0) { exit 0 }
Write-Host "FAILURES: $fail"
exit 1
