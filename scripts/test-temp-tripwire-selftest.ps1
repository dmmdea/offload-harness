# test-temp-tripwire-selftest.ps1 - the temp-folder tripwire proves itself (GPU-I8c).
#
# The PowerShell twin of scripts/test-temp-tripwire-selftest.sh. Runs scripts/test-temp-tripwire.ps1 against the fixture
# module scripts/testdata/tripwirefixture and asserts, for every case, the verdict, the exit code and what was left on
# disk -- read from the disk here, not taken on the script's word:
#
#   CLEAN         ./clean    t.TempDir only                       exit 0, no folder left
#   LEAK          ./leaker   three folders never removed          exit 1, "3  tripwire-leak-*", the folder kept; -Remove takes it
#   PROCESSES     ./spawner  a copy of the test binary running    exit 2 with the pid and the image path
#                            from an image inside the folder
#   TESTS-FAILED  ./failing  a failing test, nothing left         exit 3, no folder left
#   REFUSED       the system drive                                 exit 4, nothing created
#   usage errors, a leak beside a failing package, the name-prefix rules, and -- when a Git bash is on PATH -- the shell
#   twin giving the same verdicts, exit codes and leak lines on the same fixture.
#
# Usage: pwsh -File scripts/test-temp-tripwire-selftest.ps1      (needs the Go toolchain on PATH; exits 1 if any check fails)
# Scratch: $env:TRIPWIRE_SELFTEST_DIR, else the system temp dir; it should be on a data drive (the script refuses the system
# drive, and the self-test then passes -AllowSystemDrive for the cases that need a folder). Removed on exit.

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$here = Split-Path -Parent $PSCommandPath
$tripwire = Join-Path $here 'test-temp-tripwire.ps1'
$shell = Join-Path $here 'test-temp-tripwire.sh'
$fixture = Join-Path $here 'testdata\tripwirefixture'
$psExe = (Get-Process -Id $PID).Path

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    [Console]::Error.WriteLine('selftest: the Go toolchain is not on PATH; the fixture is a Go module')
    exit 2
}
if (-not (Test-Path -LiteralPath $tripwire) -or -not (Test-Path -LiteralPath $fixture)) {
    [Console]::Error.WriteLine("selftest: cannot find $tripwire or $fixture")
    exit 2
}

$env:GOWORK = 'off'
$env:GOTOOLCHAIN = 'local' # the fixture is a module of its own; never resolve it through a workspace

$onWindows = ($PSVersionTable.PSVersion.Major -lt 6) -or [bool](Get-Variable -Name IsWindows -ValueOnly -ErrorAction SilentlyContinue)
$base = $env:TRIPWIRE_SELFTEST_DIR
if (-not $base) { $base = [IO.Path]::GetTempPath() }
$base = (Resolve-Path -LiteralPath $base).ProviderPath.TrimEnd('\', '/')
$root = Join-Path $base ('test-temp-tripwire-selftest.' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $root | Out-Null

$script:Pass = 0
$script:Fail = 0
$script:Skip = 0
$script:Out = ''
$script:Rc = 0
$script:Pids = @()

function Ok([string]$What) { $script:Pass++; Write-Host "ok    $What" }
function Bad([string]$What) { $script:Fail++; Write-Host "FAIL  $What" }
function Skipped([string]$What) { $script:Skip++; Write-Host "skip  $What" }
function Expect([string]$What, $Got, $Want) {
    if ("$Got" -eq "$Want") { Ok $What } else { Bad "${What}: got '$Got', want '$Want'" }
}
function Expect-Has([string]$What, [string]$Text, [string]$Pattern) {
    if ($Text.Contains($Pattern)) { Ok $What } else { Bad "${What}: output lacks '$Pattern'"; $Text -split "`n" | ForEach-Object { Write-Host "      | $_" } }
}

# Remove-Tree: removes a folder the self-test made, trying again for a while because an image that is still running cannot
# be deleted on Windows (the sleeper of the PROCESSES case ends by itself within a minute where nothing kills it).
function Remove-Tree([string]$Path) {
    for ($i = 0; $i -lt 45 -and (Test-Path -LiteralPath $Path); $i++) {
        Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath $Path) { Start-Sleep -Seconds 2 }
    }
    if (Test-Path -LiteralPath $Path) { [Console]::Error.WriteLine("selftest: could not remove $Path (an image in it is still running); remove it by an explicit list") }
}

function Stop-Sleepers([int[]]$ProcessIds) {
    foreach ($p in $ProcessIds) { Stop-Process -Id $p -Force -ErrorAction SilentlyContinue }
    foreach ($p in $ProcessIds) {
        for ($i = 0; $i -lt 20 -and (Get-Process -Id $p -ErrorAction SilentlyContinue); $i++) { Start-Sleep -Milliseconds 300 }
    }
}

function Run([string[]]$TripArgs) {
    $text = & $psExe -NoProfile -File $tripwire @TripArgs 2>&1 | Out-String
    $script:Rc = $LASTEXITCODE
    $script:Out = $text -replace "`r", ''
}
function Verdict-Of { if ($script:Out -match '(?m)^TRIPWIRE-VERDICT: ([A-Z-]+)') { return $Matches[1] } return '' }
function Folders-In { return @(Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count }
function Leak-Lines {
    return (($script:Out -split "`n" | Where-Object { $_ -match '^test-temp-tripwire:  +[0-9]+  ' } | ForEach-Object { $_ -replace '^test-temp-tripwire: *', '' }) -join "`n")
}
function Clean-Kept { Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue | ForEach-Object { Remove-Tree $_.FullName } }

try {
    # --- usage errors (no verdict line, exit 64)
    Run @()
    Expect 'no -Dir: exit' $script:Rc 64
    Run @('-Dir', (Join-Path $root 'does-not-exist'), '-Module', $fixture)
    Expect '-Dir that does not exist: exit' $script:Rc 64
    Run @('-Dir', $root, '-Module', $root)
    Expect 'no go.mod in -Module: exit' $script:Rc 64
    Expect 'usage errors print no verdict' (Verdict-Of) ''

    # --- the system drive is refused, and nothing is created
    $sysDir = if ($onWindows) { "$($env:SystemDrive)\" } else { '/' }
    $before = @(Get-ChildItem -LiteralPath $sysDir -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count
    Run @('-Dir', $sysDir, '-Module', $fixture, '-Pkgs', './clean')
    Expect 'system drive: exit' $script:Rc 4
    Expect 'system drive: verdict' (Verdict-Of) 'REFUSED'
    Expect 'system drive: nothing was created there' @(Get-ChildItem -LiteralPath $sysDir -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count $before

    # --- whether the scratch root itself is on the system drive decides whether the cases below need the override
    Run @('-Dir', $root, '-Module', $fixture, '-Pkgs', './clean')
    $common = @('-Dir', $root, '-Module', $fixture)
    if ($script:Rc -eq 4) {
        $common += '-AllowSystemDrive'
        Ok 'scratch root is on the system drive: refused without the override (the cases below pass it)'
    }

    # --- CLEAN
    Run ($common + @('-Pkgs', './clean'))
    Expect 'clean: exit' $script:Rc 0
    Expect 'clean: verdict' (Verdict-Of) 'CLEAN'
    Expect 'clean: the empty folder is gone' (Folders-In) 0

    # --- LEAK
    Run ($common + @('-Pkgs', './leaker'))
    Expect 'leak: exit' $script:Rc 1
    Expect 'leak: verdict' (Verdict-Of) 'LEAK'
    Expect 'leak: the three leaked folders are counted under one prefix' (Leak-Lines) '3  tripwire-leak-*'
    Expect 'leak: the folder is kept as evidence' (Folders-In) 1
    $kept = @(Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*')[0].FullName
    $inside = @(Get-ChildItem -LiteralPath $kept -Force)
    Expect 'leak: the folder holds exactly the three leaked entries on disk' $inside.Count 3
    Expect 'leak: the folder that was removed by its own test is not among them' @($inside | Where-Object { $_.Name -like 'tripwire-clean-*' }).Count 0
    Clean-Kept
    Run ($common + @('-Pkgs', './leaker', '-Remove'))
    Expect 'leak with -Remove: exit is still LEAK' $script:Rc 1
    Expect 'leak with -Remove: the folder is removed' (Folders-In) 0

    # --- TESTS-FAILED
    Run ($common + @('-Pkgs', './failing'))
    Expect 'failing: exit' $script:Rc 3
    Expect 'failing: verdict' (Verdict-Of) 'TESTS-FAILED'
    Expect-Has 'failing: the go test output is shown' $script:Out 'this test fails on purpose'
    Expect 'failing: the empty folder is gone' (Folders-In) 0

    # --- a leak beside a failing package is still a LEAK
    Run ($common + @('-Pkgs', './leaker ./failing'))
    Expect 'leak beside a failing package: exit' $script:Rc 1
    Expect 'leak beside a failing package: verdict' (Verdict-Of) 'LEAK'
    Clean-Kept

    # --- PROCESSES
    Run ($common + @('-Pkgs', './spawner'))
    $printed = @([regex]::Matches($script:Out, '(?m)^test-temp-tripwire: process   pid ([0-9]+) ') | ForEach-Object { [int]$_.Groups[1].Value })
    $script:Pids += $printed
    Expect 'spawner: exit' $script:Rc 2
    Expect 'spawner: verdict' (Verdict-Of) 'PROCESSES'
    Expect-Has 'spawner: the image under the folder is named' $script:Out 'tripwire-sleeper'
    if ($printed.Count -ge 1) { Ok 'spawner: a pid was printed' } else { Bad 'spawner: no pid was printed' }
    Expect 'spawner: the folder is kept while the process runs' (Folders-In) 1
    Stop-Sleepers $printed
    Clean-Kept
    # the folder of a run is never removed while a process runs from it, whatever was asked
    Run ($common + @('-Pkgs', './spawner', '-Remove'))
    $printed = @([regex]::Matches($script:Out, '(?m)^test-temp-tripwire: process   pid ([0-9]+) ') | ForEach-Object { [int]$_.Groups[1].Value })
    $script:Pids += $printed
    Expect 'spawner with -Remove: exit' $script:Rc 2
    Expect 'spawner with -Remove: the folder is still kept while the process runs' (Folders-In) 1
    Stop-Sleepers $printed
    Clean-Kept

    # --- the name-prefix rules (the shell twin is held to the same table)
    $table = @(
        @('lo-pipeline-lease-482910377', 'lo-pipeline-lease-*'),
        @('run-graph-12.json', 'run-graph-*.json'),
        @('run-graph-manifest-9981.json', 'run-graph-manifest-*.json'),
        @('abc.k3Jd9Q', 'abc.*'),
        @('lo-x-state-aB3dE9', 'lo-x-state-*'),
        @('plain-name', 'plain-name'),
        @('tripwire-sleeper.exe', 'tripwire-sleeper.exe'),
        @('tmp1', 'tmp*')
    )
    foreach ($row in $table) {
        $got = (& $psExe -NoProfile -File $tripwire -NamePrefix $row[0] 2>&1 | Out-String).Trim()
        Expect "prefix of $($row[0])" $got $row[1]
    }

    # --- the shell twin agrees on the same fixture (a Git bash; the WSL launcher in System32 is not it)
    $bash = @(Get-Command bash -All -ErrorAction SilentlyContinue | Where-Object { $_.Source -and $_.Source -notmatch '\\(System32|WindowsApps)\\' } | ForEach-Object { $_.Source })
    if ($bash.Count -ge 1 -and (Test-Path -LiteralPath $shell)) {
        $bashExe = $bash[0]
        $rootMixed = $root -replace '\\', '/'
        $fixtureMixed = $fixture -replace '\\', '/'
        $shellMixed = $shell -replace '\\', '/'
        foreach ($pk in './clean', './leaker', './failing') {
            Run ($common + @('-Pkgs', $pk))
            $psVerdict = Verdict-Of; $psRc = $script:Rc; $psLeaks = Leak-Lines
            Clean-Kept
            $shArgs = @($shellMixed, '--dir', $rootMixed, '--module', $fixtureMixed, '--pkgs', $pk)
            if ($common -contains '-AllowSystemDrive') { $shArgs += '--allow-system-drive' }
            $script:Out = (& $bashExe @shArgs 2>&1 | Out-String) -replace "`r", ''
            $script:Rc = $LASTEXITCODE
            Expect "twin agrees on ${pk}: exit" $script:Rc $psRc
            Expect "twin agrees on ${pk}: verdict" (Verdict-Of) $psVerdict
            Expect "twin agrees on ${pk}: leak lines" (Leak-Lines) $psLeaks
            Clean-Kept
        }
        foreach ($row in $table[0..1] + $table[3..5]) {
            $got = (& $bashExe $shellMixed --name-prefix $row[0] 2>&1 | Out-String).Trim()
            Expect "shell twin prefix of $($row[0])" $got $row[1]
        }
    } else {
        Skipped 'the shell twin is not on this host (no Git bash on PATH): its self-test runs there'
    }
} finally {
    Stop-Sleepers $script:Pids
    if ($root.StartsWith($base) -and (Split-Path -Leaf $root).StartsWith('test-temp-tripwire-selftest.')) { Remove-Tree $root }
}

Write-Host ''
Write-Host ("selftest: {0} passed, {1} failed, {2} skipped" -f $script:Pass, $script:Fail, $script:Skip)
if ($script:Fail -gt 0) { exit 1 }
exit 0
