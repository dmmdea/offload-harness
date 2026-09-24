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
  # The exe used to RUN `node-swap` itself. Defaults to STAGED — the NEW
  # engine, verified by hash before it ever runs (Resolve-RunnerExe below) —
  # not Target, the currently-installed binary. This inverts the tool's
  # original default (Target) after the 2026-09-24 rollout hit it directly:
  # a fix to the swap engine itself (e.g. a node-swap bug fixed in the very
  # build being staged) can never take effect while the launcher keeps
  # running the OLD installed code to perform the swap — see
  # docs/systems/node-swap.md and internal/nodeswap's deps_other.go history.
  # Falls back to Target only when Staged demonstrably does not support the
  # node-swap subcommand at all (a very old staged build — an intentional
  # downgrade, or the first-ever rollout of this tool in the OTHER
  # direction). Pass -RunnerExe explicitly to bypass all of this and trust a
  # specific exe outright.
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

# Resolve-RunnerExe implements the runner-selection half of the 2026-09-24
# fix (docs/systems/node-swap.md): default the exe used to EXECUTE
# `node-swap` to the STAGED binary, verified by hash first (node-swap's own
# --sha256 check happens INSIDE the process this chooses to start — trusting
# an unverified staged exe to self-check would mean running arbitrary staged
# bytes first), falling back to the currently-installed Target — with a
# clear log line — only when Staged does not support node-swap at all (a
# very old staged build). An explicit -RunnerExe always wins outright and
# skips every check here, exactly like the tool's original override
# contract. $HashFile/$SupportsNodeSwap/$Log are injected (scriptblocks)
# purely so this is unit-testable below (-SelfTest) with fakes — no real
# binary, no real process spawn needed to exercise the branch logic; the
# production caller wires real ones (Get-FileHash, Test-NodeSwapSupport,
# Write-Host).
function Resolve-RunnerExe {
  param(
    [string]$RunnerExe,
    [string]$Staged,
    [string]$Target,
    [string]$Sha256,
    [switch]$SkipHashCheck,
    [scriptblock]$HashFile,
    [scriptblock]$SupportsNodeSwap,
    [scriptblock]$Log
  )
  if ($RunnerExe) { return $RunnerExe }
  if (-not $SkipHashCheck) {
    if (-not $Sha256) {
      throw "-Sha256 is required to verify the staged binary before it can run as the default node-swap runner (pass -RunnerExe to override, or -SkipHashCheck for testing only)"
    }
    $gotHash = & $HashFile $Staged
    if ($gotHash -ne $Sha256) {
      throw "staged binary $Staged sha256 $gotHash does not match -Sha256 $Sha256 - refusing to use it as the node-swap runner"
    }
  }
  if (& $SupportsNodeSwap $Staged) {
    return $Staged
  }
  & $Log "staged binary $Staged does not support node-swap (very old build) - falling back to the currently-installed $Target as the runner"
  return $Target
}

# Test-NodeSwapSupport is SupportsNodeSwap's real implementation: `node-swap`
# called with no flags is always a RECOGNIZED subcommand (it just fails
# Plan validation — --staged/--target are required — which main.go routes
# through the ordinary `error` path, exit code 1); an entirely UNRECOGNIZED
# subcommand falls to main.go's own `default:` case, which prints usage and
# calls os.Exit(2) directly. So "supported" is a NARROW allowlist — exit
# code 0 or 1, the only two codes a real local-offload.exe build can
# produce for this exact call — never a broad "anything but 2", because a
# staged exe that is corrupt, wrong-architecture, or crashes on launch
# (.NET reports this as a large/negative exit code, never a clean 1 or 2)
# is a DIFFERENT failure than "doesn't support node-swap" and must not be
# misread as safe to run as the launcher's own runner (code review finding,
# 2026-09-24 rollout PR): falling back to the currently-installed target is
# the safe response to ANY of those, exactly as it is to a
# confirmed-unsupported (exit 2) staged build.
#
# The try/catch matters: a staged file that is not a valid Win32 executable
# at all (corrupt, wrong architecture, a truncated download) never reaches
# the point of setting $LASTEXITCODE — CreateProcess itself fails, and
# PowerShell's `&` call operator surfaces that as a TERMINATING
# ApplicationFailedException (measured: "Program '...' failed to run: ..."),
# which the script-wide `$ErrorActionPreference = 'Stop'` would otherwise let
# propagate straight out of this function and abort the whole launcher —
# instead of falling back to the installed target like every other
# unsupported-staged-build case (code review finding). Never touches
# --staged/--target either way (Plan validation is the FIRST thing
# node-swap's own Run() does, before any file is read), so this is safe to
# run against any exe.
function Test-NodeSwapSupport([string]$path) {
  try {
    & $path node-swap *> $null
  } catch {
    return $false
  }
  return ($LASTEXITCODE -eq 0) -or ($LASTEXITCODE -eq 1)
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

  Write-Host '== Resolve-RunnerExe (runner-selection: staged-by-default, hash-verified, fallback) =='
  function Assert-Throws([scriptblock]$block, [string]$label) {
    try { & $block; Write-Host "FAIL $label`: expected a throw, got none"; $script:fail++ }
    catch { Write-Host "PASS $label (threw: $($_.Exception.Message))" }
  }
  $hashOk    = { param($p) 'ABC123' }         # matches -Sha256 ABC123 case-insensitively
  $hashBad   = { param($p) 'DEADBEEF' }       # never matches
  $supportsYes = { param($p) $true }
  $supportsNo  = { param($p) $false }
  $noopLog = { param($m) }

  # 1. An explicit -RunnerExe always wins; HashFile/SupportsNodeSwap must
  #    never even be called (both wired to throw here to prove it).
  $throwIfCalled = { param($p) throw "must not be called: explicit -RunnerExe should short-circuit" }
  $got = Resolve-RunnerExe -RunnerExe 'explicit.exe' -Staged 'staged.exe' -Target 'target.exe' -Sha256 '' `
    -HashFile $throwIfCalled -SupportsNodeSwap $throwIfCalled -Log $noopLog
  Assert-Eq $got 'explicit.exe' 'explicit -RunnerExe wins outright'

  # 2. No override, hash matches, staged supports node-swap -> staged.
  $got = Resolve-RunnerExe -RunnerExe '' -Staged 'staged.exe' -Target 'target.exe' -Sha256 'ABC123' `
    -HashFile $hashOk -SupportsNodeSwap $supportsYes -Log $noopLog
  Assert-Eq $got 'staged.exe' 'defaults to the staged binary'

  # 3. Hash mismatch -> refuses outright (never silently falls back to
  #    either binary with an unverified staged exe).
  Assert-Throws { Resolve-RunnerExe -RunnerExe '' -Staged 'staged.exe' -Target 'target.exe' -Sha256 'ABC123' `
    -HashFile $hashBad -SupportsNodeSwap $supportsYes -Log $noopLog } 'hash mismatch refuses to run the staged binary'

  # 4. Staged does not support node-swap (a very old staged build) -> falls
  #    back to Target, and logs why.
  $logged = ''
  $captureLog = { param($m) $script:logged = $m }
  $got = Resolve-RunnerExe -RunnerExe '' -Staged 'staged.exe' -Target 'target.exe' -Sha256 'ABC123' `
    -HashFile $hashOk -SupportsNodeSwap $supportsNo -Log $captureLog
  Assert-Eq $got 'target.exe' 'falls back to the installed target when staged lacks node-swap'
  if ($logged -like '*does not support node-swap*') { Write-Host 'PASS fallback logs why' }
  else { Write-Host "FAIL fallback logs why: got [$logged]"; $fail++ }

  # 5. -SkipHashCheck bypasses hash verification but still probes support.
  $got = Resolve-RunnerExe -RunnerExe '' -Staged 'staged.exe' -Target 'target.exe' -Sha256 '' -SkipHashCheck `
    -HashFile $throwIfCalled -SupportsNodeSwap $supportsYes -Log $noopLog
  Assert-Eq $got 'staged.exe' '-SkipHashCheck bypasses hash verification'

  # 6. Missing -Sha256 with no override and no -SkipHashCheck -> refuses.
  Assert-Throws { Resolve-RunnerExe -RunnerExe '' -Staged 'staged.exe' -Target 'target.exe' -Sha256 '' `
    -HashFile $hashOk -SupportsNodeSwap $supportsYes -Log $noopLog } 'missing -Sha256 refuses'

  Write-Host '== Test-NodeSwapSupport (real exit-code classification, no fakes) =='
  # "supported" must be a narrow allowlist (exit 0 or 1 only), never a broad
  # "anything but 2" (code review finding, 2026-09-24 rollout PR): a staged
  # exe that is corrupt, wrong-architecture, or crashes on launch reports a
  # large/negative exit code (never a clean 1 or 2) and must fall back to
  # the installed target, exactly like a confirmed-unsupported build. These
  # drive Test-NodeSwapSupport against tiny throwaway .cmd files standing in
  # for a local-offload.exe build — real process spawns, real exit codes.
  $fakeBinDir = Join-Path ([System.IO.Path]::GetTempPath()) ("node-swap-selftest-" + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $fakeBinDir -Force | Out-Null
  try {
    function New-FakeBin([string]$name, [long]$code) {
      $p = Join-Path $fakeBinDir $name
      Set-Content -Path $p -Value "@echo off`r`nexit /b $code" -Encoding ASCII
      return $p
    }
    $exit0 = New-FakeBin 'exit0.cmd' 0
    $exit1 = New-FakeBin 'exit1.cmd' 1
    $exit2 = New-FakeBin 'exit2.cmd' 2
    # -1073741819 is 0xC0000005 (STATUS_ACCESS_VIOLATION) read as a signed
    # 32-bit exit code - the real shape a genuinely crashing exe reports.
    $exitCrash = New-FakeBin 'exitcrash.cmd' (-1073741819)

    if (Test-NodeSwapSupport $exit0) { Write-Host 'PASS Test-NodeSwapSupport: exit 0 is supported' }
    else { Write-Host 'FAIL Test-NodeSwapSupport: exit 0 is supported: returned false'; $fail++ }

    if (Test-NodeSwapSupport $exit1) { Write-Host 'PASS Test-NodeSwapSupport: exit 1 (validation error) is supported' }
    else { Write-Host 'FAIL Test-NodeSwapSupport: exit 1 (validation error) is supported: returned false'; $fail++ }

    if (Test-NodeSwapSupport $exit2) { Write-Host 'FAIL Test-NodeSwapSupport: exit 2 (unrecognized subcommand) is NOT supported: returned true'; $fail++ }
    else { Write-Host 'PASS Test-NodeSwapSupport: exit 2 (unrecognized subcommand) is NOT supported' }

    if (Test-NodeSwapSupport $exitCrash) { Write-Host 'FAIL Test-NodeSwapSupport: a crash exit code is NOT supported: returned true'; $fail++ }
    else { Write-Host 'PASS Test-NodeSwapSupport: a crash exit code is NOT supported' }

    # A plain text file renamed .exe is not a valid Win32 executable at all -
    # CreateProcess itself fails, so this never even reaches the point of
    # setting $LASTEXITCODE (unlike every .cmd case above, which all DO
    # start a real process). PowerShell's `&` surfaces that as a TERMINATING
    # ApplicationFailedException; without the try/catch in Test-NodeSwapSupport
    # this throws straight through and aborts the whole self-test run (and,
    # in production, the whole launcher) instead of returning $false (code
    # review finding, 2026-09-24 rollout PR).
    $notAnExe = Join-Path $fakeBinDir 'not-a-real.exe'
    Set-Content -Path $notAnExe -Value 'this is not a valid PE executable' -Encoding ASCII
    try {
      if (Test-NodeSwapSupport $notAnExe) { Write-Host 'FAIL Test-NodeSwapSupport: a non-PE file is NOT supported: returned true'; $fail++ }
      else { Write-Host 'PASS Test-NodeSwapSupport: a non-PE file is NOT supported (and does not throw)' }
    } catch {
      Write-Host "FAIL Test-NodeSwapSupport: a non-PE file is NOT supported (and does not throw): THREW instead - $($_.Exception.GetType().Name): $($_.Exception.Message)"
      $fail++
    }
  } finally {
    Remove-Item -Recurse -Force $fakeBinDir -ErrorAction SilentlyContinue
  }

  if ($fail -eq 0) { Write-Host 'ALL PASS'; exit 0 }
  Write-Host "FAILURES: $fail"
  exit 1
}

if (-not $Staged -or -not $Target -or -not $Sha256) {
  throw "-Staged, -Target and -Sha256 are required (or pass -SelfTest to run the argv-quoting unit checks only)"
}
$RunnerExe = Resolve-RunnerExe -RunnerExe $RunnerExe -Staged $Staged -Target $Target -Sha256 $Sha256 -SkipHashCheck:$SkipHashCheck `
  -HashFile { param($p) (Get-FileHash -Algorithm SHA256 -Path $p).Hash } `
  -SupportsNodeSwap ${function:Test-NodeSwapSupport} `
  -Log { param($m) Write-Host "[node-swap-launch] $m" }

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
