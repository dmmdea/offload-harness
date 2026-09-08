# register-seat-tasks.ps1 — register the two interactive-session tasks this vLLM seat needs.
# RENDERED; run ELEVATED, once per seat. Re-runnable (-Force).
#
# Principal: the operator's account with an INTERACTIVE logon. WSL distros are per user and need
# that user's session — SYSTEM and S4U both measured unable to start the distro — which is the whole
# reason llama-swap (running as SYSTEM) triggers a task instead of launching the engine itself.
# Action: wscript + hidden.vbs, so no console window ever appears.
$ErrorActionPreference = 'Stop'

# The account is read from the live identity rather than composed as DOMAIN\user: on a workgroup
# machine $env:USERDOMAIN is not the principal the task service accepts, and the mismatch registers
# a task that never runs.
$user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$expected = '__USER__'
if ($expected -and $user -ne $expected) {
  Write-Warning "running as '$user' but the seat was rendered for '$expected'; registering for '$user'"
}

$vbs = Join-Path '__SEAT_DIR__' 'hidden.vbs'
$wsl = Join-Path $env:SystemRoot 'System32\wsl.exe'
$env_ = '__WSL_SEAT_DIR__/__SEAT_ENV__'
$defs = @(
  @{ name = 'vllm-seat-__SEAT_ID__';      cmd = "$wsl -d __DISTRO__ -u root -e bash __WSL_SEAT_DIR__/seat_fg.sh $env_" },
  @{ name = 'vllm-seat-stop-__SEAT_ID__'; cmd = "$wsl -d __DISTRO__ -u root -e bash __WSL_SEAT_DIR__/seat_stop.sh $env_" }
)

$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -ExecutionTimeLimit (New-TimeSpan -Days 30) -MultipleInstances IgnoreNew -Hidden
foreach ($d in $defs) {
  $action = New-ScheduledTaskAction -Execute (Join-Path $env:SystemRoot 'System32\wscript.exe') `
    -Argument ('"' + $vbs + '" "' + $d.cmd + '"')
  Register-ScheduledTask -TaskName $d.name -Action $action -Principal $principal -Settings $settings -Force | Out-Null
  "registered $($d.name)"
}
