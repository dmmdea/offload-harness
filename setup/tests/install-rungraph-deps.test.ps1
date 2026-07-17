# setup/tests/install-rungraph-deps.test.ps1 - unit tests for install.ps1's
# Ensure-RunGraphDeps (the run-graph satisfier tooling step). Uses the
# OFFLOAD_INSTALL_DOT_SOURCE=1 seam: install.ps1 defines its pure helpers and
# returns before ANY main-flow work (no dirs, no transcript, no detection).
#
# Adjustment (2026-07-17 env finding): comfy-cli FAILS to build in the ComfyUI venv
# on this box (pydantic-core has no prebuilt wheel + no Rust toolchain). comfy-cli is
# therefore OPTIONAL/best-effort — its install failure logs WARN and continues; only
# ComfyUI-Manager (cm-cli), the tool the satisfier actually gates on, is REQUIRED and
# a clone failure IS surfaced.
#
# Usage (both shells):
#   pwsh       -File setup/tests/install-rungraph-deps.test.ps1
#   powershell -ExecutionPolicy Bypass -File setup\tests\install-rungraph-deps.test.ps1
# Exit: 0 all assertions pass, 1 otherwise.
$ErrorActionPreference = 'Stop'
$here     = Split-Path -Parent $MyInvocation.MyCommand.Path
$setupDir = Split-Path -Parent $here

$failures = 0
function Assert {
  param([bool]$Cond, [string]$Name)
  if ($Cond) { Write-Host "  PASS $Name" -ForegroundColor Green }
  else       { Write-Host "  FAIL $Name" -ForegroundColor Red; $script:failures++ }
}

$prevSeam = $env:OFFLOAD_INSTALL_DOT_SOURCE
try {
  $env:OFFLOAD_INSTALL_DOT_SOURCE = '1'
  . (Join-Path $setupDir 'install.ps1')
} finally {
  if ($null -ne $prevSeam) { $env:OFFLOAD_INSTALL_DOT_SOURCE = $prevSeam }
  else { Remove-Item Env:OFFLOAD_INSTALL_DOT_SOURCE -ErrorAction SilentlyContinue }
}
Assert ([bool](Get-Command Ensure-RunGraphDeps -ErrorAction SilentlyContinue)) 'dot-source seam defines Ensure-RunGraphDeps'

Write-Host "== Ensure-RunGraphDeps: SKIP when comfy-cli present, Manager cloned, GitPython + uv present =="
$log = Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
  -HasComfyCli { $true } -HasManager { $true } -HasGitPython { $true } -HasUv { $true } `
  -Pip { throw 'should not install' } -Clone { throw 'should not clone' } -PipGitPython { throw 'should not install' } -PipUv { throw 'should not install' }
Assert ($log -match 'SKIP')            'all satisfied -> log reports SKIP'
Assert ($log -match 'comfy-cli')       'log names comfy-cli'
Assert ($log -match 'ComfyUI-Manager') 'log names ComfyUI-Manager'
Assert ($log -match 'GitPython')       'log names GitPython'
Assert ($log -match 'uv')              'log names uv'

Write-Host "== Ensure-RunGraphDeps: installs comfy-cli, clones Manager, installs GitPython + uv when absent =="
$did = @{}
$log = Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
  -HasComfyCli { $false } -HasManager { $false } -HasGitPython { $false } -HasUv { $false } `
  -Pip { $did.pip = $true } -Clone { $did.clone = $true } -PipGitPython { $did.gitpy = $true } -PipUv { $did.uv = $true }
Assert ($did.pip -eq $true)   'comfy-cli install ran'
Assert ($did.clone -eq $true) 'ComfyUI-Manager clone ran'
Assert ($did.gitpy -eq $true) 'GitPython install ran'
Assert ($did.uv -eq $true)    'uv install ran'
Assert ($log -match 'comfy-cli install') 'log records the comfy-cli install'
Assert ($log -match 'clone ComfyUI-Manager') 'log records the Manager clone'
Assert ($log -match 'GitPython') 'log records the GitPython install'
Assert ($log -match 'pip install uv') 'log records the uv install'

Write-Host "== Ensure-RunGraphDeps: a required uv install failure IS surfaced (throws) =="
$threw = $false
try {
  Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
    -HasComfyCli { $true } -HasManager { $true } -HasGitPython { $true } -HasUv { $false } `
    -Pip { } -Clone { } -PipGitPython { } -PipUv { throw 'pip unreachable' } | Out-Null
} catch {
  $threw = $true
}
Assert ($threw) 'a uv install failure propagates (the pack satisfier hard-requires uv)'

Write-Host "== Ensure-RunGraphDeps: comfy-cli install failure is best-effort (WARN + continue), Manager still cloned =="
$did = @{}
$threw = $false
try {
  $log = Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
    -HasComfyCli { $false } -HasManager { $false } -HasGitPython { $true } -HasUv { $true } `
    -Pip { throw 'pydantic-core wheel build failed (no Rust)' } -Clone { $did.clone = $true }
} catch {
  $threw = $true
}
Assert (-not $threw)                'a failing comfy-cli install does NOT throw out of Ensure-RunGraphDeps'
Assert ($log -match 'WARN')         'log carries a WARN for the failed comfy-cli install'
Assert ($log -match 'comfy-cli')    'the WARN names comfy-cli'
Assert ($did.clone -eq $true)       'ComfyUI-Manager is STILL cloned after the comfy-cli WARN'

Write-Host "== Ensure-RunGraphDeps: a required ComfyUI-Manager clone failure IS surfaced (throws) =="
$threw = $false
try {
  Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
    -HasComfyCli { $true } -HasManager { $false } -HasGitPython { $true } -HasUv { $true } `
    -Pip { } -Clone { throw 'network unreachable' } | Out-Null
} catch {
  $threw = $true
}
Assert ($threw) 'a Manager clone failure propagates (Manager is required)'

Write-Host "== Ensure-RunGraphDeps: a required GitPython install failure IS surfaced (throws) =="
$threw = $false
try {
  Ensure-RunGraphDeps -ComfyPy 'PY' -ComfyDir 'C:/ComfyUI' `
    -HasComfyCli { $true } -HasManager { $true } -HasGitPython { $false } -HasUv { $true } `
    -Pip { } -Clone { } -PipGitPython { throw 'pip unreachable' } | Out-Null
} catch {
  $threw = $true
}
Assert ($threw) 'a GitPython install failure propagates (cm-cli hard-requires import git)'

Write-Host ""
if ($failures -eq 0) { Write-Host 'ALL PASS' -ForegroundColor Green; exit 0 }
Write-Host "FAILURES: $failures" -ForegroundColor Red; exit 1
