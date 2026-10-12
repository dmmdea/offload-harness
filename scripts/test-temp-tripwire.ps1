# test-temp-tripwire.ps1 - run the Go tests with the OS temp dir pointed at a fresh folder, then report what is left (GPU-I8c).
#
# The PowerShell twin of scripts/test-temp-tripwire.sh: the same options (PascalCase), the same labels, the same verdicts and
# exit codes. Self-test: scripts/test-temp-tripwire-selftest.ps1 over the fixture module scripts/testdata/tripwirefixture.
# Windows PowerShell 5.1 and PowerShell 7 on any OS.
#
# The problem it closes. A test that makes a file or folder in the OS temp dir and never removes it adds one more every run.
# That filled a system drive twice (a stand-in test binary left 22,434 folders and about 70 GB in under two hours), and the
# static lint (tempdir_lint_test.go, GPU-I8) cannot see a temp file made by PRODUCTION code that a test calls (F79's inline
# graph JSON), or a started copy of the test binary that exits before its cleanup. This script sees what is actually left:
# it sets TMP, TEMP, TMPDIR and GOTMPDIR to a folder it creates empty, runs go test, and looks in the folder afterwards.
#
# Usage (run it from the module root, or pass -Module):
#   test-temp-tripwire.ps1 -Dir PARENT [-Pkgs "PKG ..."] [options]
#
#   -Dir PARENT          a folder that exists and is NOT on the system drive; the script makes a fresh, empty
#                        test-temp-tripwire.XXXXXX folder inside it and points the temp dir there. Required: there is no
#                        default, because the OS default temp dir is the system drive, which is what this guards.
#   -Pkgs "PKG ..."      the packages, space separated (default ./...). A whole-module run loads the machine: run it
#                        at low priority on a spare host, or pass a subset.
#   -Module DIR          the Go module to test (default: the current directory)
#   -GoArgs "ARG ..."    extra go test arguments, space separated, e.g. "-p 1 -short" (-count=1 is always passed)
#   -IgnoreFile FILE     the reasoned ignore list (default: scripts\test-temp-tripwire.ignore in the module, when it exists):
#                        one entry per line, "NAME-PREFIX<TAB>reason", # comments and blank lines allowed. An entry left in the
#                        temp folder whose prefix is listed is not a leak; it is printed with its reason, never silently. A line
#                        with no reason is a usage error. For the leaver that is legitimate (a lock folder that must outlive a run).
#   -AllowSystemDrive    accept a PARENT on the system drive. Without it the script refuses and creates nothing.
#   -Remove              remove the folder at the end of a LEAK run too (a CLEAN run always removes its empty folder). Never
#                        done while a process still runs from it.
#   -Tail N              lines of go test output to show when it fails (default 20; 0 = none)
#   -Verbose             show the full go test output
#   -NamePrefix NAME     print the name prefix a leaked entry NAME is counted under, and exit (a check, no run)
#   -Help                this text
#
# Verdicts. The last line of the output is always "TRIPWIRE-VERDICT: <verdict> ..." and the exit code follows:
#   CLEAN         0   go test passed and the folder is empty (or holds only entries the ignore list names, each printed with
#                     its reason): nothing was left
#   LEAK          1   the folder holds entries; they are listed by name prefix with their count ("lo-x-state-*")
#   PROCESSES     2   a process still runs from an image inside the folder (listed with its pid); it wins over LEAK,
#                     the leaked entries are listed as well
#   TESTS-FAILED  3   go test failed and nothing was left: the tests that did not run were not watched
#   REFUSED       4   PARENT is on the system drive and -AllowSystemDrive was not given: nothing was created or run
#   (bad arguments exit 64 and print no verdict)
# A LEAK or PROCESSES run keeps the folder (it is the evidence) and prints its path; remove it by an explicit list, or
# run with -Remove. When go test fails and the folder is not empty, the verdict is still LEAK or PROCESSES.
#
# Platform notes. "System drive" means the drive named by $env:SystemDrive on Windows, and the filesystem that holds / elsewhere.
# The process check reads Win32_Process on Windows and /proc/PID/exe elsewhere. go test is started with -NoNewWindow, so
# no console window opens. A legitimate leaver is never ignored silently: fix the test, or list the file in the allowlist
# of tempdir_lint_test.go with its reason.

[CmdletBinding()]
param(
    [string]$Dir,
    [string]$Pkgs = './...',
    [string]$Module,
    [string]$GoArgs = '',
    [string]$IgnoreFile,
    [switch]$AllowSystemDrive,
    [switch]$Remove,
    [int]$Tail = 20,
    [string]$NamePrefix,
    [switch]$Help
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$script:Prog = 'test-temp-tripwire'
$script:OnWindows = ($PSVersionTable.PSVersion.Major -lt 6) -or [bool](Get-Variable -Name IsWindows -ValueOnly -ErrorAction SilentlyContinue)

function Say([string]$Label, [string]$Text) {
    Write-Host ('{0}: {1,-9} {2}' -f $script:Prog, $Label, $Text)
}

function Exit-Usage([string]$Message) {
    [Console]::Error.WriteLine($script:Prog + ': usage error: ' + $Message)
    [Console]::Error.WriteLine($script:Prog + ': -Help lists the options')
    exit 64
}

# Get-Prefix: a name with its random tail replaced by *, so a thousand leaked folders are one line. The rules are the ones
# the shell twin applies.
#   lo-pipeline-lease-482910377 -> lo-pipeline-lease-*      run-graph-12.json -> run-graph-*.json
#   abc.k3Jd9Q                  -> abc.*                    plain-name         -> plain-name
function Get-Prefix([string]$Name) {
    $ext = ''
    $m = [regex]::Match($Name, '^(?<stem>.*[0-9])(?<ext>\.[A-Za-z0-9]+)$')
    if ($m.Success -and $m.Groups['ext'].Value.Length -le 6) {
        $ext = $m.Groups['ext'].Value
        $Name = $m.Groups['stem'].Value
    }
    $t = [regex]::Match($Name, '[-_.][A-Za-z0-9]+$')
    if ($t.Success) {
        $tok = $t.Value.Substring(1)
        if ($tok.Length -ge 6 -and $tok -match '[0-9]') { return $Name.Substring(0, $t.Index + 1) + '*' + $ext }
    }
    $d = [regex]::Match($Name, '[0-9]+$')
    if ($d.Success) { return $Name.Substring(0, $d.Index) + '*' + $ext }
    return $Name + $ext
}

if ($Help) {
    Get-Content -LiteralPath $PSCommandPath | Where-Object { $_ -match '^#' } | ForEach-Object { $_ -replace '^# ?', '' }
    exit 0
}
if ($PSBoundParameters.ContainsKey('NamePrefix')) {
    Write-Output (Get-Prefix $NamePrefix)
    exit 0
}

# --------------------------------------------------------------------------------------------- arguments

if (-not $Dir) { Exit-Usage '-Dir is required' }
if (-not (Test-Path -LiteralPath $Dir -PathType Container)) { Exit-Usage "-Dir '$Dir' is not an existing folder" }
if ($Tail -lt 0) { Exit-Usage '-Tail needs a number' }
if (-not $Pkgs.Trim()) { Exit-Usage '-Pkgs is empty' }
if ($Module) {
    if (-not (Test-Path -LiteralPath $Module -PathType Container)) { Exit-Usage "-Module '$Module' is not an existing folder" }
} else {
    $Module = (Get-Location).Path
}
$Module = (Resolve-Path -LiteralPath $Module).Path
if (-not (Test-Path -LiteralPath (Join-Path $Module 'go.mod'))) { Exit-Usage "no go.mod in '$Module': run from the module root or pass -Module" }
$goCmd = Get-Command go -ErrorAction SilentlyContinue
if (-not $goCmd) { Exit-Usage 'the Go toolchain is not on PATH' }

$parent = (Resolve-Path -LiteralPath $Dir).ProviderPath

# the reasoned ignore list: read and checked before anything runs, so a malformed list never costs a run
$ignoreMap = @{}
$ignorePath = $null
if ($PSBoundParameters.ContainsKey('IgnoreFile')) {
    if (-not (Test-Path -LiteralPath $IgnoreFile -PathType Leaf)) { Exit-Usage "-IgnoreFile '$IgnoreFile' is not an existing file" }
    $ignorePath = (Resolve-Path -LiteralPath $IgnoreFile).ProviderPath
} elseif (Test-Path -LiteralPath (Join-Path $Module 'scripts\test-temp-tripwire.ignore') -PathType Leaf) {
    $ignorePath = Join-Path $Module 'scripts\test-temp-tripwire.ignore'
}
if ($ignorePath) {
    $lineNo = 0
    foreach ($line in (Get-Content -LiteralPath $ignorePath)) {
        $lineNo++
        if ($line -match '^\s*(#|$)') { continue }
        $parts = $line -split "`t", 2
        if ($parts.Count -lt 2 -or -not $parts[0].Trim() -or -not $parts[1].Trim()) {
            Exit-Usage "line ${lineNo}: $line of the ignore list has no name prefix, a tab and a reason"
        }
        $ignoreMap[$parts[0].Trim()] = $parts[1].Trim()
    }
}

# ------------------------------------------------------------------------------------------- system drive

function Test-SystemDrive([string]$Path) {
    if ($script:OnWindows) {
        $sys = $env:SystemDrive
        if (-not $sys) { return $false }
        $root = [IO.Path]::GetPathRoot($Path)
        return $root.TrimEnd('\', '/').Equals($sys.TrimEnd('\', '/'), [StringComparison]::OrdinalIgnoreCase)
    }
    $devP = (& stat -c %d $Path 2>$null)
    $devRoot = (& stat -c %d / 2>$null)
    return ($devP -and $devRoot -and ($devP -eq $devRoot))
}

if (-not $AllowSystemDrive -and (Test-SystemDrive $parent)) {
    Say 'refused' "'$parent' is on the system drive: the temp dir of a run is not pointed there, because a leak would fill it; give -Dir on a data drive, or -AllowSystemDrive to accept it"
    Write-Host "TRIPWIRE-VERDICT: REFUSED ($parent is on the system drive; nothing was created or run)"
    exit 4
}

# ------------------------------------------------------------------------------------------------- the run

$alphabet = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789'
$folder = $null
for ($try = 0; $try -lt 20 -and -not $folder; $try++) {
    $suffix = -join (1..6 | ForEach-Object { $alphabet[(Get-Random -Maximum $alphabet.Length)] })
    $candidate = Join-Path $parent ('test-temp-tripwire.' + $suffix)
    if (-not (Test-Path -LiteralPath $candidate)) {
        New-Item -ItemType Directory -Path $candidate | Out-Null
        $folder = (Resolve-Path -LiteralPath $candidate).ProviderPath
    }
}
if (-not $folder) { Exit-Usage "could not create a folder in $parent" }
Say 'folder' $folder

$outFile = $folder + '.go.out'
$errFile = $folder + '.go.err'
$saved = @{}
foreach ($v in 'TMP', 'TEMP', 'TMPDIR', 'GOTMPDIR') { $saved[$v] = [Environment]::GetEnvironmentVariable($v, 'Process') }
$rc = 1
$sw = [Diagnostics.Stopwatch]::StartNew()
$proc = $null
try {
    foreach ($v in 'TMP', 'TEMP', 'TMPDIR', 'GOTMPDIR') { [Environment]::SetEnvironmentVariable($v, $folder, 'Process') }
    $argList = @('test', '-count=1') + @($GoArgs -split '\s+' | Where-Object { $_ }) + @($Pkgs -split '\s+' | Where-Object { $_ })
    $proc = Start-Process -FilePath $goCmd.Source -ArgumentList $argList -WorkingDirectory $Module -NoNewWindow -PassThru `
        -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    $null = $proc.Handle # keeps the process handle, so ExitCode is readable after the wait
    $proc.WaitForExit()
    $rc = $proc.ExitCode
} finally {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
    foreach ($v in 'TMP', 'TEMP', 'TMPDIR', 'GOTMPDIR') { [Environment]::SetEnvironmentVariable($v, $saved[$v], 'Process') }
}
$secs = [int]$sw.Elapsed.TotalSeconds
Say 'go test' ("{0} (exit {1}, {2}s)" -f $Pkgs, $rc, $secs)
$goOut = @()
if (Test-Path -LiteralPath $outFile) { $goOut += Get-Content -LiteralPath $outFile }
if (Test-Path -LiteralPath $errFile) { $goOut += Get-Content -LiteralPath $errFile }
if ($VerbosePreference -eq 'Continue') {
    $goOut | ForEach-Object { Write-Host $_ }
} elseif ($rc -ne 0 -and $Tail -gt 0) {
    $goOut | Select-Object -Last $Tail | ForEach-Object { Write-Host ('  | ' + $_) }
}

# ------------------------------------------------------------------------------------------ what is left

$entries = @(Get-ChildItem -LiteralPath $folder -Force -ErrorAction SilentlyContinue | ForEach-Object { $_.Name } | Sort-Object)
$nTotal = $entries.Count
$nEntries = 0
$nPrefixes = 0
$nIgnored = 0
if ($nTotal -gt 0) {
    $groups = @($entries | ForEach-Object { Get-Prefix $_ } | Group-Object | Sort-Object @{ Expression = 'Count'; Descending = $true }, @{ Expression = 'Name'; Descending = $false })
    $leaks = @($groups | Where-Object { -not $ignoreMap.ContainsKey($_.Name) })
    $ignored = @($groups | Where-Object { $ignoreMap.ContainsKey($_.Name) })
    foreach ($g in $leaks) { $nEntries += $g.Count }
    foreach ($g in $ignored) { $nIgnored += $g.Count }
    $nPrefixes = $leaks.Count
    if ($nEntries -gt 0) {
        Say 'left' "$nEntries entries"
        foreach ($g in $leaks) { Write-Host ('{0}:   {1,5}  {2}' -f $script:Prog, $g.Count, $g.Name) }
    }
    foreach ($g in $ignored) { Write-Host ('{0}: ignored   {1,5}  {2}  ({3})' -f $script:Prog, $g.Count, $g.Name, $ignoreMap[$g.Name]) }
}

$procs = 0
$prefixDir = $folder.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
if ($script:OnWindows) {
    foreach ($p in @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, ExecutablePath -ErrorAction SilentlyContinue)) {
        if ($p.ExecutablePath -and $p.ExecutablePath.StartsWith($prefixDir, [StringComparison]::OrdinalIgnoreCase)) {
            Say 'process' ("pid {0}  {1}" -f $p.ProcessId, $p.ExecutablePath)
            $procs++
        }
    }
} else {
    foreach ($d in @(Get-ChildItem -Path /proc -Directory -ErrorAction SilentlyContinue | Where-Object { $_.Name -match '^[0-9]+$' })) {
        $target = $null
        try { $target = (Get-Item -LiteralPath (Join-Path $d.FullName 'exe') -ErrorAction Stop).LinkTarget } catch { continue }
        if ($target) {
            $target = $target -replace ' \(deleted\)$', ''
            if ($target.StartsWith($prefixDir, [StringComparison]::Ordinal)) {
                Say 'process' ("pid {0}  {1}" -f $d.Name, $target)
                $procs++
            }
        }
    }
}

if ($procs -gt 0) {
    $verdict = "PROCESSES ($procs process(es) run from images inside $folder; $nEntries entries left in $nPrefixes name prefix(es))"
    $code = 2
} elseif ($nEntries -gt 0) {
    $verdict = "LEAK ($nEntries entries left in $nPrefixes name prefix(es) under $folder)"
    $code = 1
} elseif ($rc -ne 0) {
    $verdict = "TESTS-FAILED (go test exited $rc and nothing was left: the tests that did not run were not watched)"
    $code = 3
} else {
    $ignoredNote = ''
    if ($nIgnored -gt 0) { $ignoredNote = "; $nIgnored entries named by the ignore list were not counted" }
    $verdict = "CLEAN (go test passed and nothing was left in the temp folder$ignoredNote)"
    $code = 0
}

# The empty folder of a run that left nothing goes. A folder with entries is the evidence: kept unless -Remove, and never
# removed while a process runs from it. The go test captures sit beside the folder, are the script's own, and always go.
if ($procs -eq 0 -and ($nEntries -eq 0 -or $Remove)) {
    if ((Split-Path -Parent $folder) -eq $parent -and (Split-Path -Leaf $folder).StartsWith('test-temp-tripwire.')) {
        for ($i = 0; $i -lt 10 -and (Test-Path -LiteralPath $folder); $i++) {
            Remove-Item -LiteralPath $folder -Recurse -Force -ErrorAction SilentlyContinue
            if (Test-Path -LiteralPath $folder) { Start-Sleep -Milliseconds 300 }
        }
        if (Test-Path -LiteralPath $folder) { Say 'kept' "$folder (a file is still held; remove it by an explicit list)" }
    }
} else {
    Say 'kept' "$folder (remove it by an explicit list when it has been read)"
}
foreach ($f in $outFile, $errFile) { if (Test-Path -LiteralPath $f) { Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue } }

Write-Host "TRIPWIRE-VERDICT: $verdict"
exit $code
