<#
.SYNOPSIS
  Opt-in installer for the browse-lane sidecar.
.DESCRIPTION
  Copies runner.py, pyproject.toml and uv.lock into <OffloadHome>/browse, builds the pinned
  environment with `uv sync --frozen`, runs the unit tests with the venv python, disables
  browser-harness telemetry, and prints one final JSON line:
    {"browse_python": "...", "browse_script": "...", "ok": true}
  or {"ok": false, "reason": "..."}.
#>
param(
    [string]$OffloadHome = "$env:USERPROFILE\.local-offload"
)

$ErrorActionPreference = 'Stop'

function Fail([string]$reason) {
    [Console]::Error.WriteLine("browse install failed: $reason")
    (@{ ok = $false; reason = $reason } | ConvertTo-Json -Compress)
    exit 1
}

try {
    if (-not (Get-Command uv -ErrorAction SilentlyContinue)) { Fail 'uv is not on PATH (https://docs.astral.sh/uv/)' }

    $src = $PSScriptRoot
    $dir = Join-Path $OffloadHome 'browse'
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    foreach ($f in 'runner.py', 'pyproject.toml', 'uv.lock', 'test_runner.py') {
        Copy-Item -Force -Path (Join-Path $src $f) -Destination (Join-Path $dir $f)
    }

    & uv sync --frozen --project $dir 1>&2
    if ($LASTEXITCODE -ne 0) { Fail "uv sync --frozen exited $LASTEXITCODE" }

    $venvPython = if ($IsLinux -or $IsMacOS) { Join-Path $dir '.venv/bin/python' } else { Join-Path $dir '.venv\Scripts\python.exe' }
    if (-not (Test-Path $venvPython)) { Fail "venv python not found at $venvPython" }

    Push-Location $dir
    try {
        & $venvPython -m unittest test_runner 1>&2
        if ($LASTEXITCODE -ne 0) { Fail 'unit tests failed in the installed environment' }
    } finally { Pop-Location }

    $bh = if ($IsLinux -or $IsMacOS) { Join-Path $dir '.venv/bin/browser-harness' } else { Join-Path $dir '.venv\Scripts\browser-harness.exe' }
    if (Test-Path $bh) {
        & $bh telemetry disable 1>&2
        if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine("warning: 'browser-harness telemetry disable' exited $LASTEXITCODE (the sidecar also sets the disable env vars)") }
    } else {
        [Console]::Error.WriteLine('warning: browser-harness executable not found; the sidecar still sets the telemetry-disable env vars')
    }

    (@{ browse_python = $venvPython; browse_script = (Join-Path $dir 'runner.py'); ok = $true } | ConvertTo-Json -Compress)
} catch {
    Fail $_.Exception.Message
}
