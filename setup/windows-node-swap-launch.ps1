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
#   .\windows-node-swap-launch.ps1 -SelfTest   # argv-quoting unit checks, no swap launched
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
  [string]$Staged = '',
  [string]$Target = '',
  [string]$Sha256 = '',
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
  [string]$RunnerExe = '',
  [string]$LogDir = '',
  [switch]$SelfTest
)
$ErrorActionPreference = 'Stop'

# Quote-ArgForNativeArgv encodes ONE argument for a Win32_Process.Create /
# CreateProcess command-line STRING (never an argv array) so the receiving
# process's C-runtime-style argv parser (CommandLineToArgvW; a Go binary's
# os.Args uses the equivalent convention) decodes it back to the exact
# original bytes. This is Go's own `syscall.EscapeArg` algorithm, ported
# verbatim, because the receiving process here is always a Go binary
# (local-offload / local-offload-fleet).
#
# It is NOT PowerShell/cmd escaping (doubling `"` as `""`, what this file
# used before) - that is a DIFFERENT, incompatible convention. A restart
# command or path containing a literal `"` silently corrupted argv under the
# old scheme; this is what setup/windows-node-swap-launch.tests.ps1's
# round-trip check (decoding via the real CommandLineToArgvW Win32 API)
# exists to catch.
#
# Algorithm (mirrors src/syscall/exec_windows.go EscapeArg exactly):
#   - "" for an empty argument.
#   - A run of backslashes is only "significant" when immediately followed
#     by a `"`: doubled there (and at the very end, if the whole argument is
#     being wrapped in quotes) so the receiver's parser doesn't consume them
#     as an escape it shouldn't.
#   - Every literal `"` is escaped as `\"`.
#   - The argument is wrapped in an outer pair of `"` ONLY when it contains
#     a space or a tab - an argument with a `"` or `\` but no whitespace is
#     escaped in place and left unwrapped, exactly like the Go original.
function Quote-ArgForNativeArgv([string]$s) {
  if ($s.Length -eq 0) { return '""' }
  $hasSpace = $false
  $needsEscape = $false
  foreach ($ch in $s.ToCharArray()) {
    if ($ch -eq '"' -or $ch -eq '\') { $needsEscape = $true }
    elseif ($ch -eq ' ' -or $ch -eq "`t") { $hasSpace = $true }
  }
  if (-not $needsEscape -and -not $hasSpace) { return $s }

  $sb = New-Object System.Text.StringBuilder
  if ($hasSpace) { [void]$sb.Append('"') }
  $slashes = 0
  foreach ($ch in $s.ToCharArray()) {
    if ($ch -eq '\') {
      $slashes++
      [void]$sb.Append($ch)
    } elseif ($ch -eq '"') {
      for ($i = 0; $i -lt $slashes; $i++) { [void]$sb.Append('\') }
      [void]$sb.Append('\')
      [void]$sb.Append($ch)
      $slashes = 0
    } else {
      $slashes = 0
      [void]$sb.Append($ch)
    }
  }
  if ($hasSpace) {
    for ($i = 0; $i -lt $slashes; $i++) { [void]$sb.Append('\') }
    [void]$sb.Append('"')
  }
  return $sb.ToString()
}

if ($SelfTest) {
  $fail = 0
  function Assert-Eq($actual, $expected, [string]$label) {
    if ($actual -ceq $expected) { Write-Host "PASS $label" }
    else { Write-Host "FAIL $label`: got [$actual] want [$expected]"; $script:fail++ }
  }

  Write-Host '== Quote-ArgForNativeArgv against Go syscall.EscapeArg reference outputs =='
  Assert-Eq (Quote-ArgForNativeArgv '') '""' 'empty string'
  Assert-Eq (Quote-ArgForNativeArgv 'plain') 'plain' 'no special characters: unwrapped'
  Assert-Eq (Quote-ArgForNativeArgv 'C:\offload-stack\bin') 'C:\offload-stack\bin' 'bare backslashes, no space: unwrapped, unescaped'
  Assert-Eq (Quote-ArgForNativeArgv 'has space') '"has space"' 'space: wrapped, no internal characters to escape'
  Assert-Eq (Quote-ArgForNativeArgv 'say"hi"') 'say\"hi\"' 'embedded quote, no space: escaped in place, NOT wrapped'
  Assert-Eq (Quote-ArgForNativeArgv 'say "hi" now') '"say \"hi\" now"' 'embedded quote AND space: escaped and wrapped'
  Assert-Eq (Quote-ArgForNativeArgv 'a\\b') 'a\\b' 'double backslash, no space: unwrapped, unescaped (no quote follows)'
  Assert-Eq (Quote-ArgForNativeArgv 'a\"b') 'a\\\"b' 'backslash immediately before a quote: doubled, then the quote escaped'
  Assert-Eq (Quote-ArgForNativeArgv 'trailing\') 'trailing\' 'trailing backslash, no space: left single (nothing follows to force doubling)'
  Assert-Eq (Quote-ArgForNativeArgv 'trailing \') '"trailing \\"' 'trailing backslash before the closing wrap quote (a space is present): doubled so it cannot escape the wrap quote'
  Assert-Eq (Quote-ArgForNativeArgv 'powershell -NoProfile -Command "Write-Host hi"') '"powershell -NoProfile -Command \"Write-Host hi\""' 'a nested quoted PowerShell command (the real --restart-command shape)'

  Write-Host '== round-trip through the REAL Win32 CommandLineToArgvW decoder =='
  $sig = @'
using System;
using System.Runtime.InteropServices;
public static class NodeSwapArgvTest {
  [DllImport("shell32.dll", SetLastError = true)]
  static extern IntPtr CommandLineToArgvW([MarshalAs(UnmanagedType.LPWStr)] string cmdLine, out int argc);
  [DllImport("kernel32.dll")]
  static extern IntPtr LocalFree(IntPtr hMem);
  public static string[] Decode(string cmdLine) {
    int argc;
    IntPtr argv = CommandLineToArgvW(cmdLine, out argc);
    if (argv == IntPtr.Zero) { throw new InvalidOperationException("CommandLineToArgvW failed, err=" + Marshal.GetLastWin32Error()); }
    try {
      var result = new string[argc];
      for (int i = 0; i < argc; i++) {
        IntPtr p = Marshal.ReadIntPtr(argv, i * IntPtr.Size);
        result[i] = Marshal.PtrToStringUni(p);
      }
      return result;
    } finally {
      LocalFree(argv);
    }
  }
}
'@
  Add-Type -TypeDefinition $sig -ErrorAction Stop

  $cases = @(
    'plain',
    'C:\offload-stack\bin\local-offload.exe',
    'has space',
    'say "hi"',
    'say "hi" now',
    'a\\b',
    'a\"b',
    'trailing\',
    'trailing \',
    'powershell -NoProfile -ExecutionPolicy Bypass -Command "Write-Host ''hi'' and exit 0"',
    'a value with a " quote, a \ backslash, and	a tab'
  )
  foreach ($case in $cases) {
    # A real command line is argv[0] (the exe path) followed by the argument
    # under test - argv[0] itself always needs the SAME encoding, and using
    # it here catches a bug that only shows up with 2+ arguments.
    $encoded = (Quote-ArgForNativeArgv 'C:\fake\runner.exe') + ' ' + (Quote-ArgForNativeArgv $case)
    $decoded = [NodeSwapArgvTest]::Decode($encoded)
    if ($decoded.Length -ne 2) {
      Write-Host "FAIL round-trip (arg count) for [$case]: decoded to $($decoded.Length) args from [$encoded]"
      $fail++
      continue
    }
    Assert-Eq $decoded[1] $case "round-trip: $case"
  }

  if ($fail -eq 0) { Write-Host 'ALL PASS'; exit 0 }
  Write-Host "FAILURES: $fail"
  exit 1
}

if (-not $Staged -or -not $Target -or -not $Sha256) {
  throw "-Staged, -Target and -Sha256 are required (or pass -SelfTest to run the argv-quoting unit checks only)"
}
if (-not $RunnerExe) { $RunnerExe = $Target }

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

# Win32_Process.Create takes ONE command-line string, not an argv array - encode every
# value (paths and restart commands routinely contain spaces AND, for a restart command
# that itself wraps a quoted inner command, literal double quotes) for the receiving Go
# binary's native argv parser (see Quote-ArgForNativeArgv above; -SelfTest checks it
# against the real Win32 decoder).
$cmdLine = (Quote-ArgForNativeArgv $RunnerExe) + ' ' + (($argList | ForEach-Object { Quote-ArgForNativeArgv $_ }) -join ' ')

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

# The child's own first action (node_swap_cmd.go's runNodeSwap) is to open
# --log; if it dies before reaching that (a bad flag combination the CIM
# layer can't see, or the process failing to start at all despite
# ReturnValue=0) a poller would otherwise find no log and no result file and
# have no way to tell that apart from "still running". A quick liveness
# check here closes that gap for the common case: still running after a
# beat is the expected shape, an exit already recorded is a loud failure
# right here instead of a silent one discovered minutes later.
Start-Sleep -Milliseconds 500
$stillRunning = $null -ne (Get-CimInstance Win32_Process -Filter "ProcessId=$($created.ProcessId)" -ErrorAction SilentlyContinue)
if (-not $stillRunning -and -not (Test-Path $LogPath) -and -not (Test-Path $ResultPath)) {
  throw "node-swap (pid $($created.ProcessId)) exited within 500ms and wrote neither --log nor --result - it likely failed before parsing its flags; re-run attached (without this launcher) to see the error directly"
}

Write-Host "[node-swap-launch] started, pid $($created.ProcessId)"
Write-Host "[node-swap-launch] this session may disconnect now - the swap is detached. Poll from a fresh connection:"
Write-Host "  Get-Content '$LogPath' -Tail 20"
Write-Host "  if (Test-Path '$ResultPath') { Get-Content '$ResultPath' -Raw | ConvertFrom-Json }"

[pscustomobject]@{ pid = $created.ProcessId; log_path = $LogPath; result_path = $ResultPath } | ConvertTo-Json -Compress
