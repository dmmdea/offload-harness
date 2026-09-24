# setup/windows-node-swap-launch.tests.ps1 - runs windows-node-swap-launch.ps1 -SelfTest,
# which owns the Quote-ArgForNativeArgv assertions (hand-verified reference outputs plus a
# round-trip decode through the real Win32 CommandLineToArgvW API — the same convention a Go
# binary's os.Args uses). Thin wrapper, same shape as detect.tests.ps1.
#   PASS/FAIL lines to stdout; exit 0 = all pass, exit 1 = any fail.
# Usage (both shells): powershell -ExecutionPolicy Bypass -File setup\windows-node-swap-launch.tests.ps1
#                      pwsh       -File setup/windows-node-swap-launch.tests.ps1
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
& (Join-Path $here 'windows-node-swap-launch.ps1') -SelfTest
exit $LASTEXITCODE
