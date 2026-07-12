# setup/detect.tests.ps1 — unit-style assertions for detect.ps1's classifier.
# Runs the pure classification helpers (Get-GpuArch / Get-RamTier / Get-Profile)
# against synthetic tuples and asserts the expected profile ids. This is a thin
# wrapper: it invokes detect.ps1 -SelfTest, which owns the assertion table so the
# rules and their tests stay in one file.
#   PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail.
# Usage (both shells): powershell -ExecutionPolicy Bypass -File setup\detect.tests.ps1
#                      pwsh       -File setup/detect.tests.ps1
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $here 'detect.ps1') -SelfTest
exit $LASTEXITCODE
