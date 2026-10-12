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
#   twin giving the same verdicts, exit codes and leak lines on the same fixture. Also pinned (review round): a failure followed
#   by a passing package is named although the tail shows one line; a missing SystemDrive refuses the folder; a process query
#   that fails is said and does not remove a folder with entries; a temp folder removed during the run exits 70; and a run whose
#   pipeline is stopped answers within 20 s while ./hanger (a test that sleeps 40 s) runs, leaves no test binary running from the
#   folder, keeps the folder and removes its captures.
#
# Usage: pwsh -File scripts/test-temp-tripwire-selftest.ps1      (needs the Go toolchain on PATH; exits 1 if any check fails)
# Scratch: $env:TRIPWIRE_SELFTEST_DIR, else the system temp dir; it should be on a data drive (the script refuses the system
# drive, and the self-test then passes -AllowSystemDrive for the cases that need a folder). Removed on exit.
# TRIPWIRE_SELFTEST_FAILFAST=1 ends the run at the first failed check (a mutation check needs only the first red).
# TRIPWIRE_SELFTEST_ONLY=tag,tag runs only the sections with those tags (base, digest, sysdrive, procquery, processes, interrupt,
# vanish, prefix); the setup that decides whether the scratch root needs the system-drive override always runs.

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
# a host whose execution policy is Restricted still runs the scripts under test
$psArgs = @('-NoProfile')
if ($onWindows) { $psArgs += @('-ExecutionPolicy', 'Bypass') }
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
function Bad([string]$What) {
    $script:Fail++
    Write-Host "FAIL  $What"
    if ($env:TRIPWIRE_SELFTEST_FAILFAST) { throw 'stopped at the first failed check (TRIPWIRE_SELFTEST_FAILFAST)' }
}
function Skipped([string]$What) { $script:Skip++; Write-Host "skip  $What" }
# Want TAG: true when TRIPWIRE_SELFTEST_ONLY is not set, or lists TAG (comma separated). A mutation check runs only the sections its
# mutant can fail; the setup that decides whether the scratch root needs the system-drive override always runs.
function Want([string]$Tag) {
    if (-not $env:TRIPWIRE_SELFTEST_ONLY) { return $true }
    return (($env:TRIPWIRE_SELFTEST_ONLY -split ',') -contains $Tag)
}
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

# Invoke-Native: runs a command and returns its output as plain text plus its exit code. Windows PowerShell 5.1 turns a native
# command's stderr lines into error records and, under -ErrorAction Stop, throws on the first one; a usage error is an
# expected output here, so the preference is relaxed for the call and the records are read back as their messages.
function Invoke-Native([scriptblock]$Command) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $text = (& $Command 2>&1 | ForEach-Object { "$_" } | Out-String)
        $script:Rc = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prev }
    return ($text -replace "`r", '')
}
function Run([string[]]$TripArgs) {
    $script:Out = Invoke-Native { & $psExe @psArgs -File $tripwire @TripArgs }
}
function Verdict-Of { if ($script:Out -match '(?m)^TRIPWIRE-VERDICT: ([A-Z-]+)') { return $Matches[1] } return '' }
function Folders-In { return @(Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count }
function Leak-Lines {
    return (($script:Out -split "`n" | Where-Object { $_ -match '^test-temp-tripwire:  +[0-9]+  ' } | ForEach-Object { $_ -replace '^test-temp-tripwire: *', '' }) -join "`n")
}
function Clean-Kept { Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue | ForEach-Object { Remove-Tree $_.FullName } }
# Stray-Captures: the go test capture files a run leaves beside its folder (the script owns them and always removes them)
function Stray-Captures { return @(Get-ChildItem -LiteralPath $root -File -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count }
# Get-RunningFrom: the processes (Windows) that run from an image inside DIR
function Get-RunningFrom([string]$Dir) {
    return @(Get-CimInstance -ClassName Win32_Process -Property ProcessId, ExecutablePath | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($Dir.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase) })
}

try {
    # --- usage errors (no verdict line, exit 64)
    if (Want 'base') {
        Run @()
        Expect 'no -Dir: exit' $script:Rc 64
        Run @('-Dir', (Join-Path $root 'does-not-exist'), '-Module', $fixture)
        Expect '-Dir that does not exist: exit' $script:Rc 64
        Run @('-Dir', $root, '-Module', $root)
        Expect 'no go.mod in -Module: exit' $script:Rc 64
        Expect 'usage errors print no verdict' (Verdict-Of) ''
        Run @('-Help')
        Expect '-Help: exit' $script:Rc 0
        Expect-Has '-Help: prints the header' $script:Out 'What it cannot see'
        if ($script:Out.Contains('Get-FailureDigest')) { Bad '-Help: prints comments from further down the file' } else { Ok '-Help: stops at the end of the header' }
    }

    # --- the system drive is refused, and nothing is created
    if (Want 'base') {
        $sysDir = if ($onWindows) { "$($env:SystemDrive)\" } else { '/' }
        $before = @(Get-ChildItem -LiteralPath $sysDir -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count
        Run @('-Dir', $sysDir, '-Module', $fixture, '-Pkgs', './clean')
        Expect 'system drive: exit' $script:Rc 4
        Expect 'system drive: verdict' (Verdict-Of) 'REFUSED'
        Expect 'system drive: nothing was created there' @(Get-ChildItem -LiteralPath $sysDir -Filter 'test-temp-tripwire.*' -ErrorAction SilentlyContinue).Count $before
    }

    # --- whether the scratch root itself is on the system drive decides whether the cases below need the override
    Run @('-Dir', $root, '-Module', $fixture, '-Pkgs', './clean')
    $common = @('-Dir', $root, '-Module', $fixture)
    if ($script:Rc -eq 4) {
        $common += '-AllowSystemDrive'
        Ok 'scratch root is on the system drive: refused without the override (the cases below pass it)'
    }

    # --- CLEAN
    if (Want 'base') {
        Run ($common + @('-Pkgs', './clean'))
        Expect 'clean: exit' $script:Rc 0
        Expect 'clean: verdict' (Verdict-Of) 'CLEAN'
        Expect 'clean: the empty folder is gone' (Folders-In) 0
    }

    # --- LEAK
    if (Want 'base') {
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
    }

    # --- the reasoned ignore list
    if (Want 'base') {
        $tab = [string][char]9
        [IO.File]::WriteAllText((Join-Path $root 'ignore.txt'), "# a comment`n`ntripwire-leak-*${tab}the fixture leaves these on purpose`n")
        Run ($common + @('-Pkgs', './leaker', '-IgnoreFile', (Join-Path $root 'ignore.txt')))
        Expect 'ignore list naming the leak: exit' $script:Rc 0
        Expect 'ignore list naming the leak: verdict' (Verdict-Of) 'CLEAN'
        Expect-Has 'ignore list naming the leak: the group is printed with its reason, not hidden' $script:Out 'ignored       3  tripwire-leak-*  (the fixture leaves these on purpose)'
        if ($script:Out.Contains('test-temp-tripwire: left')) { Bad 'ignore list naming the leak: it is reported as a leak' } else { Ok 'ignore list naming the leak: it is not reported as a leak' }
        Expect-Has 'ignore list naming the leak: the verdict says how many were not counted' $script:Out '3 entries named by the ignore list were not counted'
        Expect 'ignore list naming the leak: the folder goes' (Folders-In) 0
        [IO.File]::WriteAllText((Join-Path $root 'ignore-other.txt'), "something-else-*${tab}not what the fixture leaves`n")
        Run ($common + @('-Pkgs', './leaker', '-IgnoreFile', (Join-Path $root 'ignore-other.txt')))
        Expect 'ignore list naming something else: exit' $script:Rc 1
        Expect 'ignore list naming something else: verdict' (Verdict-Of) 'LEAK'
        Clean-Kept
        [IO.File]::WriteAllText((Join-Path $root 'ignore-noreason.txt'), "tripwire-leak-*`n")
        Run ($common + @('-Pkgs', './leaker', '-IgnoreFile', (Join-Path $root 'ignore-noreason.txt')))
        Expect 'ignore line with no reason: exit' $script:Rc 64
        [IO.File]::WriteAllText((Join-Path $root 'ignore-blank.txt'), "tripwire-leak-*${tab}   `n")
        Run ($common + @('-Pkgs', './leaker', '-IgnoreFile', (Join-Path $root 'ignore-blank.txt')))
        Expect 'ignore line with a blank reason: exit' $script:Rc 64
        Run ($common + @('-Pkgs', './leaker', '-IgnoreFile', (Join-Path $root 'no-such-file.txt')))
        Expect 'ignore file that does not exist: exit' $script:Rc 64
        Expect 'a rejected ignore list ran nothing' (Folders-In) 0
        # the default list: scripts\test-temp-tripwire.ignore in the module, found without being named
        $fixmod = Join-Path $root 'fixmod'
        Copy-Item -LiteralPath $fixture -Destination $fixmod -Recurse
        New-Item -ItemType Directory -Path (Join-Path $fixmod 'scripts') | Out-Null
        [IO.File]::WriteAllText((Join-Path $fixmod 'scripts\test-temp-tripwire.ignore'), "tripwire-leak-*${tab}the default list names it`n")
        $defaultArgs = @('-Dir', $root, '-Module', $fixmod)
        if ($common -contains '-AllowSystemDrive') { $defaultArgs += '-AllowSystemDrive' }
        Run ($defaultArgs + @('-Pkgs', './leaker'))
        Expect 'default ignore list: exit' $script:Rc 0
        Expect-Has 'default ignore list: the reason is printed' $script:Out '(the default list names it)'
    }

    # --- TESTS-FAILED
    if (Want 'base') {
        Run ($common + @('-Pkgs', './failing'))
        Expect 'failing: exit' $script:Rc 3
        Expect 'failing: verdict' (Verdict-Of) 'TESTS-FAILED'
        Expect-Has 'failing: the go test output is shown' $script:Out 'this test fails on purpose'
        Expect 'failing: the empty folder is gone' (Folders-In) 0
    }

    # --- a leak beside a failing package is still a LEAK
    if (Want 'base') {
        Run ($common + @('-Pkgs', './leaker ./failing'))
        Expect 'leak beside a failing package: exit' $script:Rc 1
        Expect 'leak beside a failing package: verdict' (Verdict-Of) 'LEAK'
        Clean-Kept
    }

    # --- a failure that is not at the end of the output is still named. go test prints one "ok" line per later package, so the
    # last lines of a whole-module run hold only "ok" lines; the failing test and its message have to be reported on their own.
    if (Want 'digest') {
        Run ($common + @('-Pkgs', './failing ./clean', '-Tail', '1'))
        Expect 'failure followed by a passing package, -Tail 1: exit' $script:Rc 3
        Expect 'failure followed by a passing package, -Tail 1: verdict' (Verdict-Of) 'TESTS-FAILED'
        Expect-Has 'failure followed by a passing package, -Tail 1: the failing test is named' $script:Out '--- FAIL: TestFails'
        Expect-Has 'failure followed by a passing package, -Tail 1: its message is shown' $script:Out 'this test fails on purpose'
        Expect 'failure followed by a passing package: the empty folder is gone' (Folders-In) 0
        Expect 'failure followed by a passing package: no capture is left beside it' (Stray-Captures) 0
        Run ($common + @('-Pkgs', './failing', '-Tail', '0'))
        if ($script:Out.Contains('this test fails on purpose')) { Bad '-Tail 0 shows no go test output: it shows the message' } else { Ok '-Tail 0 shows no go test output' }
        Clean-Kept
    }

    # --- a refusal that cannot be decided is a refusal. Without SystemDrive the guard cannot say which drive is the system drive,
    # and must not read that as "not the system drive". (A child process, so the variable is gone for the script only.)
    if (Want 'sysdrive') {
        if ($onWindows -and -not ($common -contains '-AllowSystemDrive')) {
            $cmd = "`$env:SystemDrive = ''; & '$tripwire' -Dir '$root' -Module '$fixture' -Pkgs './clean'; exit `$LASTEXITCODE"
            $script:Out = Invoke-Native { & $psExe @psArgs -Command $cmd }
            Expect 'no SystemDrive: exit (refused)' $script:Rc 4
            Expect 'no SystemDrive: verdict' (Verdict-Of) 'REFUSED'
            Expect-Has 'no SystemDrive: the output says it could not tell' $script:Out 'cannot tell'
            Expect 'no SystemDrive: nothing was created' (Folders-In) 0
        } else {
            Skipped 'no SystemDrive: Windows only, and the scratch root must be off the system drive'
        }
    }

    # --- a process check that cannot run is said, and the folder is not removed on its account. The query is made to fail by a
    # function of the same name in the caller's scope (a function wins over a cmdlet).
    if (Want 'procquery') {
        if ($onWindows) {
            $allowArg = if ($common -contains '-AllowSystemDrive') { ' -AllowSystemDrive' } else { '' }
            $cmd = "function Get-CimInstance { [CmdletBinding()] param([string]`$ClassName, [string[]]`$Property) Write-Error 'injected failure' }; & '$tripwire' -Dir '$root' -Module '$fixture' -Pkgs './clean'$allowArg; exit `$LASTEXITCODE"
            $script:Out = Invoke-Native { & $psExe @psArgs -Command $cmd }
            Expect 'process query fails, nothing left: exit' $script:Rc 0
            Expect-Has 'process query fails, nothing left: the verdict says the check was skipped' $script:Out 'process check skipped'
            Expect-Has 'process query fails, nothing left: the output says why' $script:Out 'injected failure'
            Expect 'process query fails, nothing left: the empty folder is gone' (Folders-In) 0
            $cmd = "function Get-CimInstance { [CmdletBinding()] param([string]`$ClassName, [string[]]`$Property) Write-Error 'injected failure' }; & '$tripwire' -Dir '$root' -Module '$fixture' -Pkgs './leaker' -Remove$allowArg; exit `$LASTEXITCODE"
            $script:Out = Invoke-Native { & $psExe @psArgs -Command $cmd }
            Expect 'process query fails, a leak, -Remove: exit' $script:Rc 1
            Expect-Has 'process query fails, a leak, -Remove: the verdict says the check was skipped' $script:Out 'process check skipped'
            Expect 'process query fails, a leak, -Remove: the folder is kept, because a process might run from it' (Folders-In) 1
            Clean-Kept
        } else {
            Skipped 'process query fails: the query is Win32_Process, Windows only'
        }
    }

    # --- PROCESSES
    if (Want 'processes') {
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
        $keptDirs = @(Get-ChildItem -LiteralPath $root -Directory -Filter 'test-temp-tripwire.*')
        if ($keptDirs.Count -eq 1) {
            Expect 'spawner with -Remove: nothing in the folder was touched (the note beside the running image is still there)' @(Get-ChildItem -LiteralPath $keptDirs[0].FullName -Force | Where-Object { $_.Name -eq 'tripwire-note.txt' }).Count 1
        } else {
            Bad 'spawner with -Remove: the kept folder is not there to look in'
        }
        Stop-Sleepers $printed
        Clean-Kept
    }

    # --- an interrupted run stops the test binaries it started, not only go, and answers the interrupt at once. The run is a script
    # in a runspace of this process that is stopped (as Ctrl-C stops a pipeline) while the ./hanger test binary, which sleeps 40 s,
    # runs from the watched folder. A wait that cannot be interrupted makes the stop take the whole 40 s; the survivors, the folder
    # and the capture files are read from the disk afterwards.
    if (Want 'interrupt') {
        if ($onWindows) {
            $jobAllow = ($common -contains '-AllowSystemDrive')
            $runner = [powershell]::Create()
            $null = $runner.AddScript({
                    param($tw, $dir, $mod, $allow)
                    if ($allow) { & $tw -Dir $dir -Module $mod -Pkgs './hanger' -AllowSystemDrive } else { & $tw -Dir $dir -Module $mod -Pkgs './hanger' }
                }).AddArgument($tripwire).AddArgument($root).AddArgument($fixture).AddArgument($jobAllow)
            $async = $runner.BeginInvoke()
            $running = $false
            for ($i = 0; $i -lt 240 -and -not $running; $i++) { Start-Sleep -Milliseconds 500; $running = @(Get-RunningFrom $root).Count -gt 0 }
            if (-not $running) {
                Bad 'interrupt: the hanger test binary never ran from the folder (waited 120 s)'
                $runner.Stop()
            } else {
                $watch = [Diagnostics.Stopwatch]::StartNew()
                $runner.Stop()
                $took = [int]$watch.Elapsed.TotalSeconds
                if ($took -le 20) { Ok "interrupt: the run stopped within 20 s of the signal (took $took s; the test sleeps 40 s)" } else { Bad "interrupt: the run took $took s to stop (the test sleeps 40 s): the wait for go test cannot be interrupted" }
                $survivors = @(Get-RunningFrom $root)
                $script:Pids += @($survivors | ForEach-Object { [int]$_.ProcessId })
                Expect 'interrupt: no process runs from the scratch root afterwards' $survivors.Count 0
                Expect 'interrupt: the folder is kept as evidence' (Folders-In) 1
                Expect 'interrupt: the go test captures are gone' (Stray-Captures) 0
                $said = (@($runner.Streams.Information | ForEach-Object { "$_" }) -join "`n")
                Expect-Has 'interrupt: it says the run did not finish' $said 'did not finish'
            }
            $runner.Dispose()
            Stop-Sleepers $script:Pids
            Clean-Kept
        } else {
            Skipped 'interrupt: the runspace-stop case is Windows only (the shell self-test stops its own twin on Linux)'
        }
    }

    # --- a temp folder that vanishes during the run is an error, not a clean run. A go that deletes the folder it was given stands
    # in for a test that removes its own temp dir; the script must say so (exit 70) instead of reading an empty listing as CLEAN.
    if (Want 'vanish') {
        if ($onWindows) {
            $stubGo = Join-Path $root 'stub-go'
            New-Item -ItemType Directory -Path $stubGo -Force | Out-Null
            [IO.File]::WriteAllText((Join-Path $stubGo 'go.cmd'), "@echo off`r`nrd /s /q `"%GOTMPDIR%`"`r`nexit /b 0`r`n")
            $allowArg = if ($common -contains '-AllowSystemDrive') { ' -AllowSystemDrive' } else { '' }
            $cmd = "`$env:PATH = '$stubGo;' + `$env:PATH; & '$tripwire' -Dir '$root' -Module '$fixture' -Pkgs './clean'$allowArg; exit `$LASTEXITCODE"
            $script:Out = Invoke-Native { & $psExe @psArgs -Command $cmd }
            Expect 'folder removed during the run: exit' $script:Rc 70
            Expect-Has 'folder removed during the run: the output says it is gone' $script:Out 'is gone'
            Expect 'folder removed during the run: no verdict is printed' (Verdict-Of) ''
            Expect 'folder removed during the run: no capture is left beside it' (Stray-Captures) 0
        } else {
            Skipped 'folder removed during the run: the stand-in go is a .cmd, Windows only'
        }
    }

    # --- the name-prefix rules (the shell twin is held to the same table)
    if (Want 'prefix') {
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
            $got = (Invoke-Native { & $psExe @psArgs -File $tripwire -NamePrefix $row[0] }).Trim()
            Expect "prefix of $($row[0])" $got $row[1]
        }
    }

    # --- the shell twin agrees on the same fixture (a Git bash; the WSL launcher in System32 is not it)
    if (Want 'prefix') {
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
                $script:Out = Invoke-Native { & $bashExe @shArgs }
                Expect "twin agrees on ${pk}: exit" $script:Rc $psRc
                Expect "twin agrees on ${pk}: verdict" (Verdict-Of) $psVerdict
                Expect "twin agrees on ${pk}: leak lines" (Leak-Lines) $psLeaks
                Clean-Kept
            }
            foreach ($row in $table[0..1] + $table[3..5]) {
                $got = (Invoke-Native { & $bashExe $shellMixed --name-prefix $row[0] }).Trim()
                Expect "shell twin prefix of $($row[0])" $got $row[1]
            }
        } else {
            Skipped 'the shell twin is not on this host (no Git bash on PATH): its self-test runs there'
        }
    }
} finally {
    Stop-Sleepers $script:Pids
    if ($root.StartsWith($base) -and (Split-Path -Leaf $root).StartsWith('test-temp-tripwire-selftest.')) { Remove-Tree $root }
}

Write-Host ''
Write-Host ("selftest: {0} passed, {1} failed, {2} skipped" -f $script:Pass, $script:Fail, $script:Skip)
if ($script:Fail -gt 0) { exit 1 }
exit 0
