# mutation-check.ps1 - prove a test CAN fail: apply one single-line mutation, run the tests, restore the file (H-17).
#
# The PowerShell twin of scripts/mutation-check.sh: the same options (PascalCase), the same labels, the same verdicts and
# exit codes. Self-test: scripts/mutation-check-selftest.ps1. Windows PowerShell 5.1 and PowerShell 7 on any OS.
#
# The problem it closes. A proof that "this test catches that bug" used to be a throw-away script: apply a mutation,
# run the tests, count ANY failure as RED. Two ways that lied:
#   * a mutant that does not COMPILE also fails the test command, and was scored as a killed mutant;
#   * a mutation that never landed (an LF pattern on a CRLF file) changed nothing, and the unchanged tree's result was
#     reported as the mutant's.
#
# What a run does, in this order:
#   1. prepares the mutated bytes in memory and asserts the mutation LANDS: the bytes differ, they differ beyond line
#      endings, exactly one line differs, and every other line is identical. Nothing has been run and the file has not
#      been touched yet, so a bad mutation fails at once.
#   2. CONTROL arm: the unmutated tree is compiled and tested. Both must pass, or nothing can be proven. With the
#      default go test command a control run that executed no test at all (a mistyped -Run) is refused too:
#      it would otherwise score every mutant SURVIVED.
#   3. writes the mutation into the file and reads the file back to check the bytes on disk are the prepared ones.
#   4. COMPILES first (go test -c on the package, go vet for several, or your -Compile command). A build failure is
#      BAD-MUTANT, never RED: a mutant that does not build killed nothing.
#   5. runs the tests: they fail = RED (the mutant was killed), they pass = SURVIVED (a gap in the tests).
#   6. restores the file byte for byte and asserts it (the bytes read back are compared to the original, plus a SHA-256
#      compare), from a finally block, so it also runs on Ctrl-C and on an unexpected error.
#
# Usage (run it from the module root, or pass -Dir):
#   mutation-check.ps1 -File PATH (-Old TEXT | -Marker TEXT) -New TEXT (-Pkg PATTERN | -Test CMD) [options]
#
#   -File PATH      the source file to mutate; a relative path resolves against -Dir
#   -Old TEXT       exact text to replace; it must occur once on one line
#   -Marker TEXT    picks the ONE line that contains TEXT. With -Old, -Old is replaced inside that line; without -Old,
#                   the WHOLE line is replaced (its indentation and line ending are kept)
#   -New TEXT       the replacement; -New '' deletes the -Old text
#   -Pkg PATTERN    Go package pattern. Default compile: go test -c -o <tmp> PATTERN (one package), or go vet PATTERN
#                   (a "..." pattern or several, since go test -c takes one). Default tests: go test -count=1 [-run RE]
#   -Run RE         with -Pkg: go test -run RE
#   -Test CMD       the test command, run as a PowerShell script block; success = it throws nothing and, when a native
#                   command ran last, that command exited 0. Use a native command or `throw`; never `exit`
#   -Compile CMD    the command that must succeed on the mutant before the tests run (same rules). Required when there
#                   is no -Pkg. For other languages: a syntax check, a build, a type check
#   -Dir DIR        working directory for everything (default: the current directory)
#   -Tail N         lines of command output to show on a failure (default 20; 0 = none)
#   -Verbose        show the full output of every step
#   -Old, -Marker and -New must be one line (no CR or LF).
#
# Verdicts. The last line of the output is always "MUTATION-VERDICT: <verdict> ..." and the exit code follows:
#   RED              0   the mutant compiled and the tests failed: the tests catch this mutation
#   SURVIVED         1   the mutant compiled and the tests passed: the tests do NOT catch it
#   BAD-MUTANT       2   the mutant does not compile: not a kill, the proof is void; pick another mutation
#   CONTROL-FAILED   3   the unmutated tree failed to compile or test: nothing can be attributed to a mutation
#   NOT-APPLIED      4   the mutation did not land (text absent, ambiguous, a no-op, an unwritable file)
#   RESTORE-FAILED   5   the file is NOT back to its original bytes; the message names the pristine copy
#   INTERRUPTED          Ctrl-C or a stop ended the run; the file was restored first (the line is printed, the exit code
#                        is PowerShell's own for a stopped script)
#   (bad arguments exit 64 and print no verdict)
#
# Platform notes. All file I/O is [IO.File]::ReadAllBytes / WriteAllBytes: Get-Content, Set-Content and Out-File re-encode,
# strip a BOM and add newlines. The bytes are viewed through ISO-8859-1 (one char per byte, lossless for any content) so
# a search and a one-line splice never disturb a BOM, a non-UTF-8 byte or any other line, and the line's own terminator
# (CRLF, LF, or none on an unterminated last line) is kept. PowerShell cannot trap SIGTERM: a stop that is not a Ctrl-C
# (Stop-Process, a killed terminal) cannot be undone from inside, so the pristine copy's path is printed at the start.
# The temp dir is $env:MUTATION_CHECK_TMPDIR, else the system temp dir. Do not run two instances against the same file.

[CmdletBinding()]
param(
    [string]$File,
    [string]$Old,
    [string]$Marker,
    [string]$New,
    [string]$Pkg,
    [string]$Run,
    [string]$Test,
    [string]$Compile,
    [string]$Dir,
    [int]$Tail = 20
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$script:Prog = 'mutation-check'
$script:Latin1 = [Text.Encoding]::GetEncoding(28591)
$script:Utf8 = New-Object Text.UTF8Encoding($false)
$script:ShowAll = ($VerbosePreference -eq 'Continue')

# ------------------------------------------------------------------------------------------------- state

$script:Work = $null          # temp dir holding the pristine copy and every log
$script:OrigPath = $null      # the pristine copy of the file
$script:FullPath = $null      # the file being mutated, absolute
$script:OrigBytes = $null     # the pristine bytes, in memory
$script:OrigTime = $null      # its last write time
$script:OrigDigest = ''
$script:Finished = $false
$script:KeepWork = $false
$script:Prepared = $false
$script:LineNo = 0
$script:Content = ''
$script:NewContent = ''

# ------------------------------------------------------------------------------------------------- output

function Say([string]$Label, [string]$Text) {
    Write-Host ('{0}: {1,-9} {2}' -f $script:Prog, $Label, $Text)
}

function Exit-Usage([string]$Message) {
    [Console]::Error.WriteLine($script:Prog + ': usage error: ' + $Message)
    [Console]::Error.WriteLine($script:Prog + ': the options are listed in the header of this script')
    exit 64
}

function ConvertTo-Raw([string]$Text) { $script:Latin1.GetString($script:Utf8.GetBytes($Text)) }
function ConvertFrom-Raw([string]$Raw) { $script:Utf8.GetString($script:Latin1.GetBytes($Raw)) }

function Get-Digest([byte[]]$Bytes) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return (($sha.ComputeHash($Bytes) | ForEach-Object { $_.ToString('x2') }) -join '') } finally { $sha.Dispose() }
}

function Test-BytesEqual([byte[]]$A, [byte[]]$B) {
    if ($A.Length -ne $B.Length) { return $false }
    for ($i = 0; $i -lt $A.Length; $i++) { if ($A[$i] -ne $B[$i]) { return $false } }
    return $true
}

function Show-Log([string]$LogPath, [string]$How) {
    $lines = @()
    if (Test-Path -LiteralPath $LogPath) { $lines = @([IO.File]::ReadAllLines($LogPath)) }
    if ($script:ShowAll -or $How -eq 'all') {
        # everything
    } elseif ($Tail -le 0) {
        $lines = @()
    } elseif ($lines.Count -gt $Tail) {
        if ($How -eq 'head') { $lines = @($lines[0..($Tail - 1)]) } else { $lines = @($lines[($lines.Count - $Tail)..($lines.Count - 1)]) }
    }
    foreach ($l in $lines) { Write-Host ('{0}:           | {1}' -f $script:Prog, ($l -replace "`r", '')) }
}

# ---------------------------------------------------------------------------------------------- restore and finish

function Restore-Original {
    # Puts the pristine bytes back and proves it. Returns @{ Ok = bool; Note = string }. Safe to call more than once.
    if ($null -eq $script:OrigBytes -or $null -eq $script:FullPath) {
        return @{ Ok = $true; Note = 'no pristine copy was taken, so the file was never touched' }
    }
    $note = 'the bytes read back still differ after 5 attempts'
    $ok = $false
    for ($i = 1; $i -le 5; $i++) {
        try {
            $now = $null
            if (Test-Path -LiteralPath $script:FullPath -PathType Leaf) { $now = [IO.File]::ReadAllBytes($script:FullPath) }
            if ($null -eq $now -or -not (Test-BytesEqual $now $script:OrigBytes)) {
                [IO.File]::WriteAllBytes($script:FullPath, $script:OrigBytes)
                [IO.File]::SetLastWriteTimeUtc($script:FullPath, $script:OrigTime)
                $now = [IO.File]::ReadAllBytes($script:FullPath)
            }
            if (Test-BytesEqual $now $script:OrigBytes) {
                $after = Get-Digest $now
                if ($after -eq $script:OrigDigest) {
                    $ok = $true
                    $note = 'byte for byte: bytes read back equal the original, digest ' + $after.Substring(0, 12) + ' == ' + $script:OrigDigest.Substring(0, 12)
                } else {
                    $note = 'the bytes compare equal but the digests differ'
                }
                break
            }
        } catch {
            $note = 'could not write the file: ' + $_.Exception.Message
        }
        Start-Sleep -Seconds 1 # a scanner or an editor can hold the file for a moment
    }
    return @{ Ok = $ok; Note = $note }
}

function Remove-Work {
    if (-not $script:KeepWork -and $script:Work -and (Test-Path -LiteralPath $script:Work)) {
        try { Remove-Item -LiteralPath $script:Work -Recurse -Force -ErrorAction Stop } catch { }
    }
}

function Complete-Run([string]$Verdict, [int]$Code, [string]$Why) {
    if ($script:Finished) { exit $Code }
    $script:Finished = $true
    $r = Restore-Original
    if ($r.Ok) {
        Say 'restore' ('OK, ' + $r.Note)
    } else {
        $script:KeepWork = $true
        Say 'restore' ('FAILED: ' + $r.Note)
        Say 'restore' ('the pristine copy is kept at: ' + $script:OrigPath)
        Say 'restore' ("put it back by hand: Copy-Item -LiteralPath '" + $script:OrigPath + "' -Destination '" + $script:FullPath + "' -Force")
        $Verdict = 'RESTORE-FAILED'
        $Code = 5
        $Why = 'the file is not back to its original bytes'
    }
    if ($script:Prepared) {
        Say 'result' ($Verdict + ' -- ' + $File + ' line ' + $script:LineNo + ": '" + $script:Content + "' -> '" + $script:NewContent + "'")
    }
    Write-Host ('MUTATION-VERDICT: {0} ({1})' -f $Verdict, $Why)
    exit $Code
}

function Stop-NotApplied([string]$Why) {
    Say 'mutation' ('NOT APPLIED: ' + $Why)
    Complete-Run 'NOT-APPLIED' 4 $Why
}

# ---------------------------------------------------------------------------------------------- steps

function Invoke-Step([string]$Name, [scriptblock]$Block) {
    # Runs a compile or test step, capturing all output in a log. Returns @{ Code; Secs; Log }.
    $log = Join-Path $script:Work ($Name + '.log')
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $code = 0
    $text = @()
    try {
        $global:LASTEXITCODE = $null
        $out = & $Block 2>&1
        $ok = $?
        $native = $global:LASTEXITCODE
        $text = @($out | ForEach-Object { [string]$_ })
        if ($null -ne $native) { $code = [int]$native } elseif ($ok) { $code = 0 } else { $code = 1 }
    } catch [System.Management.Automation.PipelineStoppedException] {
        throw
    } catch {
        $text += ('EXCEPTION: ' + $_.Exception.Message)
        $code = 1
    } finally {
        $ErrorActionPreference = $prev
    }
    [IO.File]::WriteAllLines($log, [string[]]$text, $script:Utf8)
    return @{ Code = $code; Secs = [int]$sw.Elapsed.TotalSeconds; Log = $log }
}

# The default steps read script-scope variables instead of closing over locals: a script block made with
# GetNewClosure() is module-bound and does not see Invoke-Step's ErrorActionPreference = 'Continue', so on Windows
# PowerShell 5.1 the first line a native command writes to stderr (a compiler error) became an exception.
function Get-CompileBlock {
    if ($Compile) { return [scriptblock]::Create($Compile) }
    $script:CompiledOut = Join-Path $script:Work 'compiled.test'
    $script:PkgList = @($Pkg -split '\s+' | Where-Object { $_ })
    if ($Pkg -notmatch '\.\.\.' -and $Pkg -notmatch '\s') { return { $p = $script:PkgList[0]; & go test -c -o $script:CompiledOut $p } }
    return { $p = $script:PkgList; & go vet $p }
}

function Get-TestBlock {
    if ($Test) { return [scriptblock]::Create($Test) }
    $script:PkgList = @($Pkg -split '\s+' | Where-Object { $_ })
    $script:RunRe = $Run
    if ($Run) { return { $p = $script:PkgList; & go test -count=1 -run $script:RunRe $p } }
    return { $p = $script:PkgList; & go test -count=1 $p }
}

function Get-CompileText {
    if ($Compile) { return $Compile }
    if ($Pkg -notmatch '\.\.\.' -and $Pkg -notmatch '\s') { return 'go test -c ' + $Pkg }
    return 'go vet ' + $Pkg
}

function Get-TestText {
    if ($Test) { return $Test }
    if ($Run) { return 'go test -count=1 -run ' + $Run + ' ' + $Pkg }
    return 'go test -count=1 ' + $Pkg
}

# ---------------------------------------------------------------------------------------------- arguments

$haveOld = $PSBoundParameters.ContainsKey('Old')
$haveMarker = $PSBoundParameters.ContainsKey('Marker')
$haveNew = $PSBoundParameters.ContainsKey('New')

if (-not $File) { Exit-Usage '-File is required' }
if (-not $haveNew) { Exit-Usage "-New is required (use -New '' to delete the -Old text)" }
if (-not $haveOld -and -not $haveMarker) { Exit-Usage 'one of -Old or -Marker is required' }
if ($haveOld -and -not $Old) { Exit-Usage '-Old must not be empty' }
if ($haveMarker -and -not $Marker) { Exit-Usage '-Marker must not be empty' }
foreach ($t in @($Old, $Marker, $New)) {
    if ($t -and ($t.Contains("`n") -or $t.Contains("`r"))) {
        Exit-Usage '-Old, -Marker and -New must each be ONE line (no CR or LF): a mutation is a single-line replacement, and a multi-line pattern is how an LF pattern silently misses a CRLF file'
    }
}
if (-not $Pkg -and -not $Test) { Exit-Usage 'give -Pkg (Go) or -Test: what runs the tests?' }
if (-not $Pkg -and -not $Compile) { Exit-Usage 'give -Compile when there is no -Pkg: the compile step is what tells BAD-MUTANT from RED' }
if ($Run -and (-not $Pkg -or $Test)) { Exit-Usage '-Run only applies to the default test command (-Pkg without -Test)' }

$restoreLocation = $false
$prevCwd = [Environment]::CurrentDirectory
if ($Dir) {
    if (-not (Test-Path -LiteralPath $Dir -PathType Container)) { Exit-Usage ('-Dir not found: ' + $Dir) }
    Push-Location -LiteralPath $Dir
    $restoreLocation = $true
}
$cwd = (Get-Location).ProviderPath
[Environment]::CurrentDirectory = $cwd
if ([IO.Path]::IsPathRooted($File)) { $script:FullPath = $File } else { $script:FullPath = Join-Path $cwd $File }
if (-not (Test-Path -LiteralPath $script:FullPath -PathType Leaf)) {
    if ($restoreLocation) { Pop-Location }
    [Environment]::CurrentDirectory = $prevCwd
    Exit-Usage ('file not found: ' + $File + ' (a relative path resolves against -Dir, default the current directory)')
}

# ---------------------------------------------------------------------------------------------- set up

$tmpBase = $env:MUTATION_CHECK_TMPDIR
if (-not $tmpBase) { $tmpBase = [IO.Path]::GetTempPath() }
$script:Work = Join-Path $tmpBase ('mutation-check.' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $script:Work -Force | Out-Null

try {
    $script:OrigBytes = [IO.File]::ReadAllBytes($script:FullPath)
    $script:OrigTime = [IO.File]::GetLastWriteTimeUtc($script:FullPath)
    $script:OrigDigest = Get-Digest $script:OrigBytes
    $script:OrigPath = Join-Path $script:Work 'original'
    [IO.File]::WriteAllBytes($script:OrigPath, $script:OrigBytes)
    if (-not (Test-BytesEqual ([IO.File]::ReadAllBytes($script:OrigPath)) $script:OrigBytes)) {
        $script:OrigBytes = $null
        $script:Finished = $true
        Exit-Usage ('cannot take an exact copy of ' + $File)
    }

    $raw = $script:Latin1.GetString($script:OrigBytes)
    $lines = $raw.Split([char]10)                       # an element keeps the trailing CR of a CRLF line
    $rawNorm = $raw.Replace("`r", '')
    $linesNorm = $rawNorm.Split([char]10)

    $crCount = $raw.Length - $rawNorm.Length
    $lfCount = $lines.Count - 1
    if ($crCount -eq 0) { $style = 'LF' } elseif ($crCount -eq $lfCount) { $style = 'CRLF' } else { $style = "mixed ($crCount CR, $lfCount LF)" }
    if ($raw.Length -gt 0 -and $lines[$lines.Count - 1].Length -gt 0) { $style += ', no final newline' }

    Say 'file' ($File + ' (' + $style + ')')
    Say 'backup' ("pristine copy $($script:OrigPath) (digest $($script:OrigDigest.Substring(0, 12))); if this run is killed hard, put it back with: Copy-Item -LiteralPath '$($script:OrigPath)' -Destination '$($script:FullPath)' -Force")

    # ---------------------------------------------------------------------- 1. prepare and prove it lands

    if ($raw.IndexOf([char]0) -ge 0) { Stop-NotApplied 'the file holds NUL bytes: it is not a text source file' }

    $oldRaw = ''
    if ($haveOld) { $oldRaw = ConvertTo-Raw $Old }
    $newRaw = ConvertTo-Raw $New
    if ($haveMarker) { $selRaw = ConvertTo-Raw $Marker; $selName = '-Marker'; $selText = $Marker } else { $selRaw = $oldRaw; $selName = '-Old'; $selText = $Old }

    $hits = @()
    for ($i = 0; $i -lt $linesNorm.Count; $i++) {
        if ($linesNorm[$i].IndexOf($selRaw, [StringComparison]::Ordinal) -ge 0) { $hits += ($i + 1) }
    }
    if ($hits.Count -eq 0) { Stop-NotApplied ("the $selName text is not in ${File}: $selText") }
    if ($hits.Count -gt 1) { Stop-NotApplied ("the $selName text matches $($hits.Count) lines ($($hits -join ' ')): make it unique, or pick the line with -Marker") }
    $n = $hits[0] - 1
    $script:LineNo = $n + 1

    $line = $lines[$n]
    $cr = $line.EndsWith("`r")
    if ($cr) { $contentRaw = $line.Substring(0, $line.Length - 1) } else { $contentRaw = $line }
    $hasLf = ($n -lt $lines.Count - 1)
    if ($contentRaw.Contains("`r")) { Stop-NotApplied "line $($script:LineNo) holds a CR that is not its line ending: this harness mutates ordinary text lines" }
    if ($cr -and $hasLf) { $term = 'CRLF' } elseif ($hasLf) { $term = 'LF' } elseif ($cr) { $term = 'CR (unterminated last line)' } else { $term = 'none (unterminated last line)' }

    if ($haveOld) {
        $cnt = 0
        $pos = 0
        $first = -1
        while (($idx = $contentRaw.IndexOf($oldRaw, $pos, [StringComparison]::Ordinal)) -ge 0) {
            if ($cnt -eq 0) { $first = $idx }
            $cnt++
            $pos = $idx + $oldRaw.Length
        }
        if ($cnt -eq 0) { Stop-NotApplied "line $($script:LineNo) (picked by -Marker) does not contain the -Old text: $Old" }
        if ($cnt -gt 1) { Stop-NotApplied "the -Old text occurs $cnt times on line $($script:LineNo): make it unique" }
        $newContentRaw = $contentRaw.Substring(0, $first) + $newRaw + $contentRaw.Substring($first + $oldRaw.Length)
    } else {
        $indent = [regex]::Match($contentRaw, '^[ \t\v\f]*').Value
        $newContentRaw = $indent + $newRaw
    }
    $script:Content = ConvertFrom-Raw $contentRaw
    $script:NewContent = ConvertFrom-Raw $newContentRaw
    if ($newContentRaw -ceq $contentRaw) { Stop-NotApplied "the mutation is a no-op: line $($script:LineNo) would not change" }

    $mutLines = [string[]]$lines.Clone()
    if ($cr) { $mutLines[$n] = $newContentRaw + "`r" } else { $mutLines[$n] = $newContentRaw }
    $mutRaw = [string]::Join("`n", $mutLines)
    $mutBytes = $script:Latin1.GetBytes($mutRaw)

    $script:Prepared = $true
    Say 'mutation' ("$File line $($script:LineNo) (line ending kept: $term)")
    Say '' ('- ' + $script:Content)
    Say '' ('+ ' + $script:NewContent)

    # the landing assertions, on the prepared bytes, before anything runs
    if (Test-BytesEqual $mutBytes $script:OrigBytes) { Stop-NotApplied 'the mutated copy is byte-identical to the original' }
    $mutNorm = $mutRaw.Replace("`r", '')
    if ($mutNorm -ceq $rawNorm) { Stop-NotApplied 'only line endings differ: the mutation did not land' }
    $mutLinesNorm = $mutNorm.Split([char]10)
    if ($mutLinesNorm.Count -ne $linesNorm.Count) { Stop-NotApplied 'the line count changed' }
    $differ = @()
    for ($i = 0; $i -lt $linesNorm.Count; $i++) { if ($linesNorm[$i] -cne $mutLinesNorm[$i]) { $differ += ($i + 1) } }
    if ($differ.Count -ne 1 -or $differ[0] -ne $script:LineNo) { Stop-NotApplied ("expected exactly line $($script:LineNo) to differ, found: " + ($differ -join ' ')) }
    for ($i = 0; $i -lt $lines.Count; $i++) { if ($i -ne $n -and $lines[$i] -cne $mutLines[$i]) { Stop-NotApplied "bytes of line $($i + 1) changed" } }
    Say 'landed' "yes: exactly 1 line differs (line $($script:LineNo)), every other byte is identical, line endings compared normalised"

    # ---------------------------------------------------------------------- 2. control arm

    Say 'control' 'unmutated tree: compile, then tests; both must pass'
    Say '' ('compile: ' + (Get-CompileText))
    Say '' ('tests:   ' + (Get-TestText))
    $compileBlock = Get-CompileBlock
    $testBlock = Get-TestBlock
    $r = Invoke-Step 'control.compile' $compileBlock
    if ($r.Code -ne 0) {
        Say 'control' "the compile step FAILED on the unmutated tree (exit $($r.Code), $($r.Secs)s)"
        Show-Log $r.Log 'head'
        Complete-Run 'CONTROL-FAILED' 3 'the unmutated tree does not compile with the compile command, so no result can be attributed to the mutation'
    }
    if ($script:ShowAll) { Show-Log $r.Log 'all' }
    $r = Invoke-Step 'control.tests' $testBlock
    if ($r.Code -ne 0) {
        Say 'control' "the tests FAILED on the unmutated tree (exit $($r.Code), $($r.Secs)s)"
        Show-Log $r.Log 'tail'
        Complete-Run 'CONTROL-FAILED' 3 'the tests already fail without the mutation, so a failure with it proves nothing'
    }
    if ($script:ShowAll) { Show-Log $r.Log 'all' }
    # go test exits 0 when a mistyped -Run matches nothing ("ok  pkg  0.01s [no tests to run]") or the package has no tests
    # ("?  pkg  [no test files]"): a control arm that ran nothing would score every mutant SURVIVED
    if (-not $Test) {
        $realOk = @([IO.File]::ReadAllLines($r.Log) | Where-Object { $_ -match '^ok\s' -and -not $_.Contains('[no tests to run]') })
        if ($realOk.Count -eq 0) {
            Say 'control' 'go test exited 0 but ran no test: a mistyped -Run, or a package without tests'
            Show-Log $r.Log 'tail'
            Complete-Run 'CONTROL-FAILED' 3 'the control run executed no tests, so every mutant would score SURVIVED; fix -Pkg or -Run'
        }
    }
    Say 'control' "PASS: the unmutated tree compiles and its tests pass ($($r.Secs)s for the tests)"

    # ---------------------------------------------------------------------- 3. write the mutation

    try { [IO.File]::WriteAllBytes($script:FullPath, $mutBytes) } catch { Stop-NotApplied ("cannot write ${File}: " + $_.Exception.Message) }
    $disk = [IO.File]::ReadAllBytes($script:FullPath)
    if (-not (Test-BytesEqual $disk $mutBytes)) { Stop-NotApplied 'the bytes on disk are not the prepared mutation' }
    $diskLines = $script:Latin1.GetString($disk).Split([char]10)
    $bad = 0
    for ($i = 0; $i -lt $lines.Count; $i++) { if ($i -ne $n -and $lines[$i] -cne $diskLines[$i]) { $bad++ } }
    if ($diskLines.Count -ne $lines.Count -or $bad -ne 0) { Stop-NotApplied 'the file on disk differs from the original on more than the mutated line' }
    Say 'applied' ('the mutation is on disk (' + (Get-Digest $disk).Substring(0, 12) + ' now, ' + $script:OrigDigest.Substring(0, 12) + ' before)')

    # ---------------------------------------------------------------------- 4. compile the mutant

    $r = Invoke-Step 'mutant.compile' $compileBlock
    if ($r.Code -ne 0) {
        Say 'compile' "FAILED on the mutant (exit $($r.Code)): it does not build, so this is NOT a kill"
        Show-Log $r.Log 'head'
        Complete-Run 'BAD-MUTANT' 2 'the mutant does not compile, so a test failure here would prove nothing; choose a mutation that builds'
    }
    if ($script:ShowAll) { Show-Log $r.Log 'all' }
    Say 'compile' "ok: the mutant builds ($($r.Secs)s)"

    # ---------------------------------------------------------------------- 5. run the tests

    $r = Invoke-Step 'mutant.tests' $testBlock
    if ($r.Code -eq 0) {
        if ($script:ShowAll) { Show-Log $r.Log 'all' }
        Say 'tests' "PASSED on the mutant ($($r.Secs)s)"
        Complete-Run 'SURVIVED' 1 'the mutant compiles and the tests pass: they do not catch this mutation'
    }
    Say 'tests' "FAILED on the mutant (exit $($r.Code), $($r.Secs)s)"
    Show-Log $r.Log 'tail'
    Complete-Run 'RED' 0 'the mutant compiles and the tests fail: the tests catch this mutation'
}
finally {
    if (-not $script:Finished) {
        # Ctrl-C, a stop, or an unexpected error: put the file back before anything else
        $script:Finished = $true
        $r = Restore-Original
        if ($r.Ok) {
            Say 'restore' ('OK after an interruption or an unexpected error: ' + $r.Note)
            Write-Host 'MUTATION-VERDICT: INTERRUPTED (the run ended before a verdict; the file was restored first)'
        } else {
            $script:KeepWork = $true
            Say 'restore' ('FAILED after an interruption: ' + $r.Note + '; pristine copy: ' + $script:OrigPath)
            Write-Host 'MUTATION-VERDICT: RESTORE-FAILED (the file is not back to its original bytes)'
        }
    }
    Remove-Work
    if ($restoreLocation) { Pop-Location }
    [Environment]::CurrentDirectory = $prevCwd
}
