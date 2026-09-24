# windows-node-swap-launch.ps1 - detached launcher for `local-offload node-swap`.
#
# Registers the swap through WMI (Win32_Process.Create), which survives the calling SSH
# session ending - Start-Process does NOT (windows-ssh-remote-ops-patterns house memory:
# "Windows OpenSSH kills the whole remote process tree on disconnect"). This is the direct
# fix for the 2026-09-24 Aorus outage: that deploy's restart-and-verify phase ran INSIDE the
# ssh session and died with it mid-swap, with the binary already replaced but no restart, no
# verify, and no rollback ever reached. This launcher starts the swap, prints the log/result
# paths, and returns immediately - the swap process itself keeps running (and can still roll
# itself back) no matter what happens to the connection that launched it.
#
# Plain ASCII only in this file on purpose: PowerShell 5.1 misparses a BOM-less UTF-8 script
# that contains non-ASCII characters (house lesson ps51-bomless-utf8-breaks-strings), and
# this script is meant to run unmodified via `powershell -File` however it is staged.
#
# Usage (local, or piped over ssh - the launch call itself returns almost instantly, so it
# is not vulnerable to the disconnect trap that hit the Aorus swap):
#
#   .\windows-node-swap-launch.ps1 -Staged D:\offload-stack\bin\local-offload-NEW.exe `
#     -Target D:\offload-stack\bin\local-offload.exe -Sha256 <hex> `
#     -RestartTask offload-fleet-node -HealthUrl http://192.0.2.10:18811/fleet/health
#
# Then, over a FRESH connection (the whole point - the old one may be gone):
#
#   Get-Content <LogPath> -Tail 20
#   if (Test-Path <ResultPath>) { Get-Content <ResultPath> -Raw | ConvertFrom-Json }
#
# The result JSON is nodeswap.Outcome (internal/nodeswap/nodeswap.go): ok, steps[], error,
# rolled_back, rollback_ok, old_sha256, new_sha256, final_pid, final_image_sha256,
# health_version. ok:false with rolled_back:true and rollback_ok:true means the swap failed
# safely and the box is back on the old binary, running.
[CmdletBinding()]
param(
  [Parameter(Mandatory)] [string]$Staged,
  [Parameter(Mandatory)] [string]$Target,
  [Parameter(Mandatory)] [string]$Sha256,
  [string]$BackupSuffix = '',
  [string]$HealthUrl = '',
  [string]$RestartTask = '',
  [string]$RestartCommand = '',
  [string]$RenderTarball = '',
  [string]$RenderDir = '',
  [string]$WaitIdleTimeout = '10m',
  [string]$VerifyTimeout = '90s',
  [switch]$DryRun,
  [switch]$SkipHashCheck,
  # The exe used to RUN `node-swap` itself. Defaults to Target: the currently
  # installed binary is what performs its own replacement (matching every
  # prior deploy record's pattern of renaming a live exe out from under
  # itself). Override only for a first-ever rollout of this tool onto a node
  # that does not have `node-swap` in its current build yet.
  [string]$RunnerExe = $Target,
  [string]$LogDir = ''
)
$ErrorActionPreference = 'Stop'

if (-not (Test-Path $RunnerExe)) { throw "RunnerExe not found: $RunnerExe" }
if (-not $LogDir) { $LogDir = Split-Path -Parent $Target }
if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Force -Path $LogDir | Out-Null }

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$LogPath = Join-Path $LogDir "node-swap-$stamp.log"
$ResultPath = Join-Path $LogDir "node-swap-$stamp.result.json"

$argList = @('node-swap', '-staged', $Staged, '-target', $Target, '-sha256', $Sha256, '-result', $ResultPath, '-log', $LogPath, '-json')
if ($BackupSuffix)    { $argList += @('-backup-suffix', $BackupSuffix) }
if ($HealthUrl)       { $argList += @('-health-url', $HealthUrl) }
if ($RestartTask)     { $argList += @('-restart-task', $RestartTask) }
if ($RestartCommand)  { $argList += @('-restart-command', $RestartCommand) }
if ($RenderTarball)   { $argList += @('-render-tarball', $RenderTarball, '-render-dir', $RenderDir) }
if ($WaitIdleTimeout) { $argList += @('-wait-idle-timeout', $WaitIdleTimeout) }
if ($VerifyTimeout)   { $argList += @('-verify-timeout', $VerifyTimeout) }
if ($DryRun)          { $argList += '-dry-run' }
if ($SkipHashCheck)   { $argList += '-skip-hash-check' }

# Win32_Process.Create takes ONE command-line string, not an argv array - quote every
# value (paths and restart commands routinely contain spaces).
function Quote-Arg([string]$s) { return '"' + ($s -replace '"', '""') + '"' }
$cmdLine = (Quote-Arg $RunnerExe) + ' ' + (($argList | ForEach-Object { Quote-Arg $_ }) -join ' ')

Write-Host "[node-swap-launch] command: $cmdLine"
Write-Host "[node-swap-launch] log:    $LogPath"
Write-Host "[node-swap-launch] result: $ResultPath"

# WMI/CIM Win32_Process.Create, NOT Start-Process: proven in this repo's own
# fleet-node-restart.ps1 and the windows-ssh-remote-ops-patterns house memory to survive the
# launching ssh session ending, because the child is created by the WMI provider host
# (WmiPrvSE.exe), never as a descendant of this session's own process tree / job object.
$startup = New-CimInstance -ClassName Win32_ProcessStartup -ClientOnly -Property @{
  CreateFlags = [uint32](0x200 -bor 0x08000000)  # CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW
  ShowWindow  = [uint16]0
}
$created = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
  CommandLine = $cmdLine
  ProcessStartupInformation = $startup
}
if ($created.ReturnValue -ne 0) {
  throw "Win32_Process.Create failed, ReturnValue=$($created.ReturnValue) (see https://learn.microsoft.com/windows/win32/cimwin32prov/create-method-in-class-win32-process for the code)"
}

Write-Host "[node-swap-launch] started, pid $($created.ProcessId)"
Write-Host "[node-swap-launch] this session may disconnect now - the swap is detached. Poll from a fresh connection:"
Write-Host "  Get-Content '$LogPath' -Tail 20"
Write-Host "  if (Test-Path '$ResultPath') { Get-Content '$ResultPath' -Raw | ConvertFrom-Json }"

[pscustomobject]@{ pid = $created.ProcessId; log_path = $LogPath; result_path = $ResultPath } | ConvertTo-Json -Compress
