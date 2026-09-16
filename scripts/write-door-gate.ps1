# write-door-gate.ps1 - the write door's four-task gate (ADR 0044, registers D-06 and D-114).
#
# Opening `agent_allow_write` on a node is a capability change; this is the proof that goes with it. It sends the
# four staged implementation legs under contracts/write-door/ (a one-file Go fix, a two-file Go fix + test case,
# a JSON + Markdown record edit, a ~3 KB whole-file rewrite asked for in ONE write_file call) to ONE seat - a fleet node (`-Remote http://<node>:18811`, route remote) or this
# box's own seat (no -Remote, route local) - then does what the caller of a write contract must always do: applies
# each returned unified diff to a FRESH copy of the task and proves it there. The harness never applies its own
# writes, so a green `summary.succeeded` is not the proof; the test run on the applied copy is.
#
#   t1  go test must FAIL before the patch and pass after it (the bug is the only red)
#   t2  go test passes before (the table had no case for the bug) and after, AND the patch must add the
#       "too large" table row so the new case exercises the fix
#   t3  nodes.json parses with exactly node-b flipped to true, nodes.md has exactly one row flipped to open
#   t4  release-notes.md comes back whole with EXACTLY its status line changed - the ~3 KB single-call write whose
#       cut argument the engine used to refuse (register D-114: llama.cpp answers 500 "Failed to parse tool call
#       arguments as JSON ... missing closing quote" when the step budget cuts the content mid-string)
#   all  the CLI exited 0 and the diff touches EXACTLY the task's expected files (the gate pins that set itself)
#
# Verdict: PASS only when all four tasks succeed, every diff applies cleanly and every proof holds; any deferred /
# failed_verification / refused (400: the node has not opted in) task is a FAIL that names the reason. Measured
# 2026-09-14 (one task) and 2026-09-15 (t1-t3, 18 / 45 / 48 s on a 4B seat; 10 s for t1 on a 27B seat); t4 is the
# shape measured on the Aorus 9B seat the same day, where a 1,024-token step budget cut the write mid-argument.
param(
  [string]$Binary = "$PSScriptRoot\..\bin\local-offload.exe",
  [string]$Config = "$env:USERPROFILE\.local-offload\config.json",
  [string]$Remote = "",
  [string]$OutDir = "$PSScriptRoot\..\write-door-gate-out"
)
$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
if (-not (Test-Path $Binary)) { throw "binary not found: $Binary" }
if (-not (Test-Path $Config)) { throw "config not found: $Config" }
$root = Join-Path $PSScriptRoot "..\contracts\write-door"
$ExpectedFiles = @{ t1 = @("clamp.go"); t2 = @("port.go", "port_test.go"); t3 = @("nodes.json", "nodes.md"); t4 = @("release-notes.md") }
New-Item -ItemType Directory -Force $OutDir | Out-Null
$stamp = Get-Date -Format "yyyyMMdd-HHmmss"

function Invoke-Task([string]$name) {
  $dir = Join-Path $root $name
  $cliArgs = @("delegate", "-config", $Config, "-contract", (Join-Path $dir "contract.json"), "-read-root", $dir)
  if ($Remote) { $cliArgs += @("-route", "remote", "-remote", $Remote) } else { $cliArgs += @("-route", "local") }
  $t = [DateTime]::UtcNow
  $raw = (& $Binary @cliArgs 2> (Join-Path $OutDir "$name.stderr.txt")) -join "`n"
  $cliExit = $LASTEXITCODE
  $wall = [int]([DateTime]::UtcNow - $t).TotalMilliseconds
  if ($cliExit -ne 0) { return @{ name = $name; ok = $false; why = "CLI exit $cliExit (transport/config failure or infrastructure defer; see $name.stderr.txt)"; wall_ms = $wall; cli_exit = $cliExit } }
  $raw | Out-File -Encoding UTF8 (Join-Path $OutDir "$name.raw.json")
  $start = $raw.IndexOf("{"); $end = $raw.LastIndexOf("}")
  if ($start -lt 0 -or $end -le $start) { return @{ name = $name; ok = $false; why = "no JSON in the CLI output"; wall_ms = $wall } }
  try { $j = $raw.Substring($start, $end - $start + 1) | ConvertFrom-Json } catch { return @{ name = $name; ok = $false; why = "CLI output is not JSON: $($_.Exception.Message)"; wall_ms = $wall } }
  $s = $j.summary; $r = @($j.results)[0]
  if (-not $s -or [int]$s.succeeded -ne 1) {
    $why = "summary $($s | ConvertTo-Json -Compress)"; if ($r -and $r.reason) { $why += " - $($r.reason)" }
    return @{ name = $name; ok = $false; why = $why; wall_ms = $wall }
  }
  if (-not $r.diff) { return @{ name = $name; ok = $false; why = "succeeded but the result carries no diff (write_note: $($r.write_note))"; wall_ms = $wall; cli_exit = $cliExit } }
  # the gate pins the touched set itself; the contract's diff_touches/diff_max_files are the delegator's check, not this proof's
  $expected = $ExpectedFiles[$name]
  $touched = @($r.diff_files | Sort-Object)
  if (($touched -join ",") -ne (($expected | Sort-Object) -join ",")) { return @{ name = $name; ok = $false; why = "diff touches [$($touched -join ',')], expected exactly [$($expected -join ',')]"; wall_ms = $wall; cli_exit = $cliExit; diff_files = $touched } }
  $patch = Join-Path $OutDir "$name.patch"
  [System.IO.File]::WriteAllText($patch, ($r.diff -replace "`r`n", "`n"), (New-Object System.Text.UTF8Encoding($false)))
  # a FRESH copy of the task sources (never the contract, never the fixture in place)
  $copy = Join-Path $OutDir "$name-$stamp"
  New-Item -ItemType Directory -Force $copy | Out-Null
  Get-ChildItem $dir -File | Where-Object { $_.Name -ne "contract.json" } | Copy-Item -Destination $copy
  return @{ name = $name; ok = $true; wall_ms = $wall; cli_exit = $cliExit; seat = $r.seat; node = $r.node; diff_files = $touched; patch = $patch; copy = $copy; bytes = $r.diff.Length }
}

function Test-Go([string]$dir) {
  Push-Location $dir
  try { $env:GOFLAGS = "-mod=mod"; $out = (& go test ./... 2>&1) -join "`n"; return @{ pass = ($LASTEXITCODE -eq 0); out = $out } }
  finally { Pop-Location }
}

function Prove([hashtable]$t) {
  if (-not $t.ok) { return $t }
  switch ($t.name) {
    "t1" {
      $before = Test-Go $t.copy
      if ($before.pass) { $t.ok = $false; $t.why = "t1 copy was GREEN before the patch - the fixture no longer carries its bug"; return $t }
      if ($before.out -notmatch 'above: Clamp\(42, 0, 10\) = 0, want 10') { $t.ok = $false; $t.why = "t1 copy is red for a reason other than the staged bug: $($before.out)"; return $t }
    }
    "t2" {
      $before = Test-Go $t.copy
      if (-not $before.pass) { $t.ok = $false; $t.why = "t2 copy was RED before the patch - the fixture's table must not cover the bug"; return $t }
    }
  }
  # The copy lives under this repository's work tree, and `git apply` run from a SUBDIRECTORY of a repository resolves
  # patch paths against the repository root and SKIPS every path outside the subdirectory - exit 0, "Skipped patch",
  # nothing changed (measured 2026-09-15 on the 0.125.0 gate: every seat diff was correct and t1/t3/t4 still "failed").
  # A throwaway repository in the copy makes the copy the root, so `a/release-notes.md` means the copy's file; and a
  # skip is a failure of the PROOF, never a pass.
  & git -C $t.copy init -q 2>&1 | Out-Null
  $apply = (& git -C $t.copy apply -p1 --verbose $t.patch 2>&1) -join "`n"
  if ($LASTEXITCODE -ne 0) { $t.ok = $false; $t.why = "git apply failed: $apply"; return $t }
  if ($apply -match 'Skipped patch' -or $apply -notmatch 'Applied patch') { $t.ok = $false; $t.why = "git apply changed nothing (the proof cannot run on an unpatched copy): $apply"; return $t }
  switch ($t.name) {
    "t1" { $after = Test-Go $t.copy; if (-not $after.pass) { $t.ok = $false; $t.why = "go test still red after the patch: $($after.out)" } else { $t.proof = "go test FAIL -> ok" } }
    "t2" {
      $after = Test-Go $t.copy
      $patchText = Get-Content $t.patch -Raw
      if (-not $after.pass) { $t.ok = $false; $t.why = "go test red after the patch: $($after.out)" }
      elseif (-not [regex]::IsMatch($patchText, '\+\s*' + [regex]::Escape('{name: "too large", in: "65536", wantErr: true}'))) { $t.ok = $false; $t.why = "the patch did not add the 'too large' table case, so nothing exercises the fix" }
      else { $t.proof = "go test ok -> ok with the new case; range check added" }
    }
    "t3" {
      try { $nodes = (Get-Content (Join-Path $t.copy "nodes.json") -Raw | ConvertFrom-Json).nodes } catch { $t.ok = $false; $t.why = "nodes.json no longer parses: $($_.Exception.Message)"; return $t }
      $flags = @{}; foreach ($n in $nodes) { $flags[$n.node_id] = [bool]$n.agent_allow_write }
      $md = Get-Content (Join-Path $t.copy "nodes.md") -Raw
      # patterns pre-escaped: Windows PowerShell 5.1 mis-tokenizes a literal '\| ... \|' inside an if-condition
      $openPat = [regex]::Escape('| open |'); $closedPat = [regex]::Escape('| closed |'); $rowPat = [regex]::Escape('| node-b | pool-b | 131072 | open |')
      $openRows = ([regex]::Matches($md, $openPat)).Count; $closedRows = ([regex]::Matches($md, $closedPat)).Count
      $flagsOk = ($flags['node-b'] -eq $true) -and ($flags['node-a'] -eq $false) -and ($flags['node-c'] -eq $false) -and (@($nodes).Count -eq 3)
      $rowOk = ([regex]::IsMatch($md, $rowPat)) -and ($openRows -eq 1) -and ($closedRows -eq 2)
      if (-not $flagsOk) { $t.ok = $false; $t.why = "nodes.json flags wrong: $($flags | ConvertTo-Json -Compress)" }
      elseif (-not $rowOk) { $t.ok = $false; $t.why = "nodes.md rows wrong (open=$openRows closed=$closedRows)" }
      else { $t.proof = "JSON parses, exactly node-b true; exactly one row open" }
    }
    "t4" {
      $orig = Get-Content (Join-Path (Join-Path $root "t4") "release-notes.md")
      $now = Get-Content (Join-Path $t.copy "release-notes.md")
      if ($now.Count -ne $orig.Count) { $t.ok = $false; $t.why = "release-notes.md came back with $($now.Count) lines, the fixture has $($orig.Count) - the single-call write did not carry the whole file"; return $t }
      $changed = @()
      for ($i = 0; $i -lt $orig.Count; $i++) { if ($orig[$i] -ne $now[$i]) { $changed += $i } }
      if ($changed.Count -ne 1) { $t.ok = $false; $t.why = "$($changed.Count) lines differ from the fixture, expected exactly the status line" }
      elseif ($orig[$changed[0]] -ne "Status: draft" -or $now[$changed[0]] -ne "Status: final") { $t.ok = $false; $t.why = "the changed line is '$($now[$changed[0]])' (was '$($orig[$changed[0]])'), expected 'Status: draft' -> 'Status: final'" }
      else { $t.proof = "the whole file came back with exactly the status line changed ($($t.bytes) B of diff)" }
    }
  }
  return $t
}

$results = @()
foreach ($name in "t1", "t2", "t3", "t4") {
  $t = Invoke-Task $name
  $t = Prove $t
  $results += [pscustomobject]$t
  $line = if ($t.ok) { "PASS  $name  $($t.wall_ms) ms  exit $($t.cli_exit)  $($t.node)/$($t.seat)  files=$($t.diff_files -join ',')  $($t.bytes) B  - $($t.proof)" } else { "FAIL  $name  $($t.wall_ms) ms  - $($t.why)" }
  Write-Host $line
}
$passed = @($results | Where-Object { $_.ok }).Count
$verdict = if ($passed -eq 4) { "PASS (4/4 tasks: succeeded, diff applied to a fresh copy, proof held)" } else { "FAIL ($passed/4)" }
$report = [pscustomobject]@{ binary = $Binary; remote = $Remote; route = $(if ($Remote) { "remote" } else { "local" }); ran_at = (Get-Date -Format s); results = $results; verdict = $verdict }
$report | ConvertTo-Json -Depth 6 | Set-Content -Encoding UTF8 (Join-Path $OutDir "report-$stamp.json")
Write-Host "VERDICT: $verdict"
if ($passed -ne 4) { exit 1 }
