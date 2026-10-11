# mutation-check-selftest.ps1 - the PowerShell mutation harness proves itself (H-17).
#
# The twin of scripts/mutation-check-selftest.sh. Runs scripts/mutation-check.ps1 against the tiny fixture module in
# scripts/testdata/mutfixture, on a COPY in a temp dir (the tracked fixture is never mutated), and asserts for every case
# the verdict, the exit code and that the mutated file came back byte for byte -- compared here against a snapshot taken
# before the run, not on the harness's word.
#
# The three verdicts the harness exists to tell apart, on the LF fixture:
#   RED          a mutation the tests catch                          Clamp: `v < lo` -> `v > lo`
#   SURVIVED     a mutation the tests do not catch (a real gap)      Band: `case n < 10:` -> `case n < 5:` (the "small" band is untested)
#   BAD-MUTANT   a mutation that does not compile (NOT a kill)       Clamp: `return lo` -> `return undefinedName`
# Then the traps that made the old proof scripts lie: a CRLF copy (the same mutations, the line endings must survive),
# whole-line and marker mutations, a mixed-ending file, an unterminated last line, a mutation that cannot land (text
# absent, ambiguous, a no-op, a multi-line pattern), an unmutated tree whose tests already fail, a restore that cannot
# succeed, and a stop (the pipeline stop that Ctrl-C causes) during the mutant's test run: the file must be restored by the
# finally block, and the wait must not outlast the stop.
#
# Usage: pwsh -File scripts/mutation-check-selftest.ps1     (needs the Go toolchain on PATH; exits 1 if any check fails)
# Scratch: $env:MUTATION_CHECK_TMPDIR, else the system temp dir; removed on exit.

[CmdletBinding()]
param()

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$Harness = Join-Path $PSScriptRoot 'mutation-check.ps1'
$Fixture = Join-Path (Join-Path $PSScriptRoot 'testdata') 'mutfixture'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    [Console]::Error.WriteLine('selftest: the Go toolchain is not on PATH; the fixture is a Go module')
    exit 2
}
if (-not (Test-Path -LiteralPath $Harness) -or -not (Test-Path -LiteralPath $Fixture)) {
    [Console]::Error.WriteLine("selftest: cannot find $Harness or $Fixture")
    exit 2
}

$env:GOWORK = 'off'      # the fixture is a module of its own; never resolve it through a workspace
$env:GOTOOLCHAIN = 'local'

$Latin1 = [Text.Encoding]::GetEncoding(28591)
$script:Pass = 0
$script:Fail = 0
$script:N = 0

$base = $env:MUTATION_CHECK_TMPDIR
if (-not $base) { $base = [IO.Path]::GetTempPath() }
$Root = Join-Path $base ('mutation-check-selftest.' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $Root | Out-Null

# --------------------------------------------------------------------------------------------------- helpers

function Read-Raw([string]$Path) { $Latin1.GetString([IO.File]::ReadAllBytes($Path)) }
function Write-Raw([string]$Path, [string]$Raw) { [IO.File]::WriteAllBytes($Path, $Latin1.GetBytes($Raw)) }
function ConvertTo-Lf([string]$Path) { Write-Raw $Path ((Read-Raw $Path).Replace("`r", '')) }
function ConvertTo-Crlf([string]$Path) { Write-Raw $Path ((Read-Raw $Path).Replace("`r", '').Replace("`n", "`r`n")) }

function Test-SameBytes([string]$A, [string]$B) {
    $x = [IO.File]::ReadAllBytes($A)
    if (-not (Test-Path -LiteralPath $B -PathType Leaf)) { return $false }
    $y = [IO.File]::ReadAllBytes($B)
    if ($x.Length -ne $y.Length) { return $false }
    for ($i = 0; $i -lt $x.Length; $i++) { if ($x[$i] -ne $y[$i]) { return $false } }
    return $true
}

function Get-TreeDigest([string]$Dir) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try {
        $all = @()
        foreach ($f in (Get-ChildItem -LiteralPath $Dir -File | Sort-Object Name)) { $all += [IO.File]::ReadAllBytes($f.FullName) }
        return (($sha.ComputeHash([byte[]]$all) | ForEach-Object { $_.ToString('x2') }) -join '')
    } finally { $sha.Dispose() }
}

function Expect([string]$What, $Got, $Want) {
    if ("$Got" -ceq "$Want") {
        $script:Pass++
        Write-Host ("selftest:   ok    {0} ({1})" -f $What, $Got)
    } else {
        $script:Fail++
        Write-Host ("selftest:   FAIL  {0}: got [{1}], want [{2}]" -f $What, $Got, $Want)
    }
}

function Invoke-Harness([hashtable]$P) {
    $global:LASTEXITCODE = $null
    $lines = @(& $Harness @P *>&1 | ForEach-Object { [string]$_ })
    $code = $global:LASTEXITCODE
    $verdict = 'none'
    $v = @($lines | Where-Object { $_ -like 'MUTATION-VERDICT:*' })
    if ($v.Count -gt 0) { $verdict = ($v[$v.Count - 1] -split ' ')[1] }
    return @{ Lines = $lines; Code = $code; Verdict = $verdict }
}

function Show-Lines($Lines, [bool]$Full) {
    foreach ($l in $Lines) {
        if ($l -like 'mutation-check: backup*') { continue }
        if (-not $Full -and $l -like 'mutation-check:           | *') { continue }
        Write-Host $l
    }
}

# Invoke-Case NAME WANT_VERDICT WANT_EXIT WATCHED FULL PARAMS: run the harness, assert verdict, exit code and a byte-exact restore
function Invoke-Case([string]$Name, [string]$WantVerdict, [int]$WantCode, [string]$Watched, [bool]$Full, [hashtable]$Params) {
    $script:N++
    $snap = Join-Path $Root ("case$($script:N).snap")
    Copy-Item -LiteralPath $Watched -Destination $snap
    Write-Host ''
    Write-Host ("selftest: ===== case {0}: {1}" -f $script:N, $Name)
    $r = Invoke-Harness $Params
    Show-Lines $r.Lines $Full
    Expect 'verdict' $r.Verdict $WantVerdict
    Expect 'exit code' $r.Code $WantCode
    Expect ((Split-Path -Leaf $Watched) + ' restored byte for byte (compared with the pre-run snapshot)') (Test-SameBytes $snap $Watched) $true
    return $r
}

# --------------------------------------------------------------------------------------------------- fixtures

try {
    $lf = Join-Path $Root 'lf'
    $crlf = Join-Path $Root 'crlf'
    Copy-Item -Recurse -LiteralPath $Fixture -Destination $lf
    foreach ($f in (Get-ChildItem -LiteralPath $lf -File)) { ConvertTo-Lf $f.FullName }   # a checkout may have made the fixture CRLF; start from LF
    Copy-Item -Recurse -LiteralPath $lf -Destination $crlf
    foreach ($f in (Get-ChildItem -LiteralPath $crlf -Filter '*.go' -File)) { ConvertTo-Crlf $f.FullName }
    $crOf = { param($p) ([IO.File]::ReadAllBytes($p) | Where-Object { $_ -eq 13 } | Measure-Object).Count }
    Write-Host ("selftest: fixture copies in {0} (lf: {1} CR bytes, crlf: {2} CR bytes in clamp.go)" -f $Root, (& $crOf (Join-Path $lf 'clamp.go')), (& $crOf (Join-Path $crlf 'clamp.go')))

    $fixtureDigest = Get-TreeDigest $Fixture

    # ------------------------------------------------------------------------------- the three headline verdicts

    $null = Invoke-Case "RED: the tests catch Clamp's lower bound flipped" 'RED' 0 (Join-Path $lf 'clamp.go') $true @{
        Dir = $lf; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Pkg = '.' }

    $null = Invoke-Case "SURVIVED: nothing tests Band's `"small`" band, so moving its upper edge goes unnoticed" 'SURVIVED' 1 (Join-Path $lf 'clamp.go') $true @{
        Dir = $lf; File = 'clamp.go'; Old = 'case n < 10:'; New = 'case n < 5:'; Pkg = '.' }

    $null = Invoke-Case 'BAD-MUTANT: an undefined name does not compile, so it is NOT a kill' 'BAD-MUTANT' 2 (Join-Path $lf 'clamp.go') $true @{
        Dir = $lf; File = 'clamp.go'; Old = 'return lo'; New = 'return undefinedName'; Pkg = '.' }

    # ------------------------------------------------------------------------------- marker mode

    $r = Invoke-Case 'RED, marker mode: the whole line is replaced and keeps its indentation' 'RED' 0 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Marker = 'case n == 0:'; New = 'case n == 1:'; Pkg = '.'; Run = 'TestBand' }
    $kept = @($r.Lines | Where-Object { $_.Contains("+ `tcase n == 1:") }).Count
    Expect 'the replaced line kept its tab indentation' $(if ($kept -gt 0) { 'kept' } else { 'lost' }) 'kept'

    $null = Invoke-Case "RED, marker picks the line and -Old narrows inside it ('lo' alone is ambiguous in the file)" 'RED' 0 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Marker = 'return lo'; Old = 'lo'; New = 'hi'; Pkg = '.'; Run = 'TestClamp' }

    # ------------------------------------------------------------------------------- the CRLF file (the old fake pass)

    $null = Invoke-Case 'RED on a CRLF file: the same mutation, and the CRLF endings must come back' 'RED' 0 (Join-Path $crlf 'clamp.go') $false @{
        Dir = $crlf; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Pkg = '.' }

    $null = Invoke-Case 'RED on a CRLF file, whole-line marker mutation' 'RED' 0 (Join-Path $crlf 'clamp.go') $false @{
        Dir = $crlf; File = 'clamp.go'; Marker = 'case n == 0:'; New = 'case n == 1:'; Pkg = '.'; Run = 'TestBand' }

    $crlfProbe = @'
$b = [IO.File]::ReadAllBytes('clamp.go')
$cr = @($b | Where-Object { $_ -eq 13 }).Count
$lf = @($b | Where-Object { $_ -eq 10 }).Count
if ($cr -ne $lf) { throw "not every line ends in CRLF: $cr CR, $lf LF" }
'@
    $null = Invoke-Case 'CRLF stays CRLF on the mutated line (the probe passes only if every line of the MUTANT ends in CRLF)' 'SURVIVED' 1 (Join-Path $crlf 'clamp.go') $false @{
        Dir = $crlf; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Compile = '$null = 1'; Test = $crlfProbe }

    $null = Invoke-Case 'the same probe on the LF copy fails its control arm, so the probe can tell the two apart' 'CONTROL-FAILED' 3 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Compile = '$null = 1'; Test = $crlfProbe }

    # ------------------------------------------------------------------------------- line-ending shapes outside Go

    $shapes = Join-Path $Root 'shapes'
    New-Item -ItemType Directory -Path $shapes | Out-Null

    Write-Raw (Join-Path $shapes 'unterminated.txt') "alpha=1`nbeta=2`ngamma=3"
    $unterminatedProbe = @'
$b = [IO.File]::ReadAllBytes('unterminated.txt')
$lf = @($b | Where-Object { $_ -eq 10 }).Count
if ($b[$b.Length - 1] -eq 10 -or $lf -ne 2) { throw "the last line is no longer unterminated ($lf LF)" }
'@
    $null = Invoke-Case 'an unterminated last line stays unterminated (probe: no final newline, 2 LFs, in both arms)' 'SURVIVED' 1 (Join-Path $shapes 'unterminated.txt') $false @{
        Dir = $shapes; File = 'unterminated.txt'; Old = 'gamma=3'; New = 'gamma=4'; Compile = '$null = 1'; Test = $unterminatedProbe }

    Write-Raw (Join-Path $shapes 'mixed.txt') "alpha=1`r`nbeta=2`ngamma=3`r`n"
    $mixedProbe = @'
$b = [IO.File]::ReadAllBytes('mixed.txt')
$cr = @($b | Where-Object { $_ -eq 13 }).Count
if ($cr -ne 2) { throw "expected exactly 2 CRs, found $cr" }
'@
    $null = Invoke-Case 'a mixed-ending file: the LF line stays LF, the CRLF lines stay CRLF (probe: exactly 2 CRs in both arms)' 'SURVIVED' 1 (Join-Path $shapes 'mixed.txt') $false @{
        Dir = $shapes; File = 'mixed.txt'; Old = 'beta=2'; New = 'beta=3'; Compile = '$null = 1'; Test = $mixedProbe }

    # ------------------------------------------------------------------------------- a mutation that cannot land

    $null = Invoke-Case 'NOT-APPLIED: the text is not in the file' 'NOT-APPLIED' 4 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Old = 'no such text anywhere'; New = 'x'; Pkg = '.' }

    $null = Invoke-Case 'NOT-APPLIED: the text matches several lines, so the harness will not guess' 'NOT-APPLIED' 4 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Old = 'return'; New = 'x'; Pkg = '.' }

    $null = Invoke-Case 'NOT-APPLIED: a no-op mutation changes nothing' 'NOT-APPLIED' 4 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Old = 'v < lo'; New = 'v < lo'; Pkg = '.' }

    $null = Invoke-Case 'NOT-APPLIED: the marker line does not hold the -Old text' 'NOT-APPLIED' 4 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Marker = 'return lo'; Old = 'hi'; New = 'x'; Pkg = '.' }

    # a multi-line pattern is refused outright: it is the shape that silently misses a CRLF file
    $script:N++
    Write-Host ''
    Write-Host ("selftest: ===== case {0}: a multi-line -Old is a usage error, not a pattern that quietly misses" -f $script:N)
    $snap = Join-Path $Root "case$($script:N).snap"
    Copy-Item -LiteralPath (Join-Path $lf 'clamp.go') -Destination $snap
    $r = Invoke-Harness @{ Dir = $lf; File = 'clamp.go'; Old = "v < lo {`n`t`treturn lo"; New = 'x'; Pkg = '.' }
    Expect 'exit code' $r.Code 64
    Expect 'no verdict is printed' $r.Verdict 'none'
    Expect 'clamp.go untouched' (Test-SameBytes $snap (Join-Path $lf 'clamp.go')) $true

    # ------------------------------------------------------------------------------- the control arm

    $broken = Join-Path $Root 'broken'
    Copy-Item -Recurse -LiteralPath $lf -Destination $broken
    Write-Raw (Join-Path $broken 'zz_always_fails_test.go') "package mutfixture`n`nimport `"testing`"`n`nfunc TestAlwaysFails(t *testing.T) { t.Fatal(`"this tree is red before any mutation`") }`n"
    $r = Invoke-Case 'CONTROL-FAILED: the tests already fail without the mutation, so nothing is attributed to one' 'CONTROL-FAILED' 3 (Join-Path $broken 'clamp.go') $false @{
        Dir = $broken; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Pkg = '.' }
    Expect 'the mutation was never applied' @($r.Lines | Where-Object { $_ -like 'mutation-check: applied*' }).Count 0

    $r = Invoke-Case 'CONTROL-FAILED: a mistyped -Run runs no test, and go test still exits 0 (else every mutant scores SURVIVED)' 'CONTROL-FAILED' 3 (Join-Path $lf 'clamp.go') $false @{
        Dir = $lf; File = 'clamp.go'; Old = 'v < lo'; New = 'v > lo'; Pkg = '.'; Run = 'TestNoSuchTest' }
    Expect 'the mutation was never applied' @($r.Lines | Where-Object { $_ -like 'mutation-check: applied*' }).Count 0

    # ------------------------------------------------------------------------------- a stop during the mutant's tests

    $stopDir = Join-Path $Root 'stop'
    New-Item -ItemType Directory -Path $stopDir | Out-Null
    Write-Raw (Join-Path $stopDir 'target.txt') "state=ok`n"
    $stopSnap = Join-Path $Root 'stop.snap'
    Copy-Item -LiteralPath (Join-Path $stopDir 'target.txt') -Destination $stopSnap
    $script:N++
    Write-Host ''
    Write-Host ("selftest: ===== case {0}: a pipeline stop (what Ctrl-C causes) while the mutant is under test; the finally block must restore the file" -f $script:N)
    $stopTest = @'
if ((Get-Content -Raw target.txt) -match 'hang') { New-Item -ItemType File -Path started | Out-Null; Start-Sleep -Seconds 120 }
'@
    $ps = [powershell]::Create()
    $null = $ps.AddScript({ param($h, $p) & $h @p }).AddArgument($Harness).AddArgument(@{
            Dir = $stopDir; File = 'target.txt'; Old = 'ok'; New = 'hang'; Compile = '$null = 1'; Test = $stopTest })
    $t0 = [DateTime]::UtcNow
    $async = $ps.BeginInvoke()
    $started = $false
    for ($i = 0; $i -lt 600; $i++) {
        if (Test-Path -LiteralPath (Join-Path $stopDir 'started')) { $started = $true; break }
        Start-Sleep -Milliseconds 100
    }
    Expect 'the mutant is under test (the mutation was on disk and the test command was sleeping)' $started $true
    $ps.Stop()
    for ($i = 0; $i -lt 300 -and -not $async.IsCompleted; $i++) { Start-Sleep -Milliseconds 100 }
    $elapsed = ([DateTime]::UtcNow - $t0).TotalSeconds
    $lines = @($ps.Streams.Information | ForEach-Object { [string]$_.MessageData })
    Show-Lines $lines $false
    Expect 'the run ended with a stop' ([string]$ps.InvocationStateInfo.State) 'Stopped'
    Expect 'the verdict line says INTERRUPTED' (@($lines | Where-Object { $_ -like 'MUTATION-VERDICT: INTERRUPTED*' }).Count -gt 0) $true
    Expect 'target.txt restored byte for byte after the stop' (Test-SameBytes $stopSnap (Join-Path $stopDir 'target.txt')) $true
    Expect ('the 120 s sleep was cut short ({0:N0} s in all)' -f $elapsed) ($elapsed -lt 60) $true
    $ps.Dispose()

    # ------------------------------------------------------------------------------- a restore that cannot succeed

    $rf = Join-Path $Root 'rf'
    New-Item -ItemType Directory -Path $rf | Out-Null
    Write-Raw (Join-Path $rf 'rf.txt') "x=1`n"
    $rfTest = @'
if ((Get-Content -Raw rf.txt -ErrorAction SilentlyContinue) -match 'x=2') { Remove-Item rf.txt; New-Item -ItemType Directory -Path rf.txt | Out-Null }
'@
    $script:N++
    Write-Host ''
    Write-Host ("selftest: ===== case {0}: RESTORE-FAILED: the test command replaces the file with a directory, so the restore cannot succeed" -f $script:N)
    $r = Invoke-Harness @{ Dir = $rf; File = 'rf.txt'; Old = 'x=1'; New = 'x=2'; Compile = '$null = 1'; Test = $rfTest }
    Show-Lines $r.Lines $false
    Expect 'verdict' $r.Verdict 'RESTORE-FAILED'
    Expect 'exit code' $r.Code 5
    $keptLine = @($r.Lines | Where-Object { $_ -like 'mutation-check: restore   the pristine copy is kept at: *' })
    $keptPath = ''
    if ($keptLine.Count -gt 0) { $keptPath = $keptLine[0].Substring($keptLine[0].IndexOf('kept at: ') + 9) }
    $holdsOriginal = $false
    if ($keptPath -and (Test-Path -LiteralPath $keptPath)) { $holdsOriginal = ((Read-Raw $keptPath) -ceq "x=1`n") }
    Expect 'the pristine copy it names holds the original bytes' $holdsOriginal $true
    # the kept copy is the harness's own temp dir (named mutation-check.XXXXXXXX); it is the one thing a failed restore leaves
    if ($keptPath -and ((Split-Path -Leaf (Split-Path -Parent $keptPath)) -like 'mutation-check.*')) {
        Remove-Item -LiteralPath (Split-Path -Parent $keptPath) -Recurse -Force -ErrorAction SilentlyContinue
    }

    # ------------------------------------------------------------------------------- the real command line

    $script:N++
    Write-Host ''
    Write-Host ("selftest: ===== case {0}: the same RED mutation through a real child process: powershell -File ... -Old ... -New ... -Pkg ." -f $script:N)
    $exe = (Get-Process -Id $PID).Path
    $childSnap = Join-Path $Root 'child.snap'
    Copy-Item -LiteralPath (Join-Path $lf 'clamp.go') -Destination $childSnap
    $childOut = @(& $exe -NoProfile -File $Harness -Dir $lf -File clamp.go -Old 'v < lo' -New 'v > lo' -Pkg . 2>&1 | ForEach-Object { [string]$_ })
    $childCode = $LASTEXITCODE
    Show-Lines $childOut $false
    Expect 'exit code' $childCode 0
    Expect 'the last line is the RED verdict' ($childOut[$childOut.Count - 1] -like 'MUTATION-VERDICT: RED*') $true
    Expect 'clamp.go restored byte for byte' (Test-SameBytes $childSnap (Join-Path $lf 'clamp.go')) $true

    # ------------------------------------------------------------------------------- summary

    Expect 'the tracked fixture was never touched' (Get-TreeDigest $Fixture) $fixtureDigest
}
finally {
    if (Test-Path -LiteralPath $Root) { Remove-Item -LiteralPath $Root -Recurse -Force -ErrorAction SilentlyContinue }
}

Write-Host ''
Write-Host ("selftest: {0} checks, {1} failed" -f ($script:Pass + $script:Fail), $script:Fail)
if ($script:Fail -eq 0) {
    Write-Host 'selftest: PASS'
    exit 0
}
Write-Host 'selftest: FAIL'
exit 1
