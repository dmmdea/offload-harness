# window-watch.ps1 - log every window that appears on the desktop while something runs.
#
# The no-visible-windows rule (a console program started by a process with no console of its own gets a NEW
# console window, a Windows Terminal window by default) is checked by tests that read flags and by this watcher,
# which looks at the desktop itself: it polls EnumWindows and writes one line for each top-level window that was
# not there when it started. scripts/window-watch-run.mjs starts it, runs a command with NO console of its own
# (the case that opens windows) and reads the log.
#
#   powershell -NoProfile -File window-watch.ps1 -OutFile watch.log -StopFile watch.stop [-IntervalMs 30] [-IncludeHidden]
#
# It runs until StopFile exists (or MaxSeconds passes). Log lines:
#   # watching baseline=<n> interval_ms=<n>      the first line: the watcher is polling
#   <ms since start>|<hwnd>|<pid>|<process>|<class>|<title>|<visible>
#   # done polls=<n> new=<n> console=<n>         the last line (console = Windows Terminal, conhost, pseudo console)
# Only visible windows are logged unless -IncludeHidden is given (the positive control in the test of this
# script creates a hidden window, because nothing may be put on the screen to prove the watcher can see it).
param(
  [Parameter(Mandatory = $true)][string]$OutFile,
  [Parameter(Mandatory = $true)][string]$StopFile,
  [int]$IntervalMs = 30,
  [int]$MaxSeconds = 3600,
  [switch]$IncludeHidden
)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;
public static class WinWatch {
  delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetClassNameW(IntPtr h, StringBuilder sb, int n);
  [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetWindowTextW(IntPtr h, StringBuilder sb, int n);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  public static List<string[]> Snapshot(bool includeHidden) {
    var found = new List<string[]>();
    EnumWindows(delegate (IntPtr h, IntPtr l) {
      bool visible = IsWindowVisible(h);
      if (!visible && !includeHidden) return true;
      var cls = new StringBuilder(256); GetClassNameW(h, cls, 256);
      var title = new StringBuilder(256); GetWindowTextW(h, title, 256);
      uint pid; GetWindowThreadProcessId(h, out pid);
      found.Add(new string[] { h.ToInt64().ToString(), pid.ToString(), cls.ToString(), title.ToString().Replace("|", "/").Replace("\r", " ").Replace("\n", " "), visible ? "visible" : "hidden" });
      return true;
    }, IntPtr.Zero);
    return found;
  }
}
"@
$consoleClasses = @('ConsoleWindowClass', 'CASCADIA_HOSTING_WINDOW_CLASS', 'PseudoConsoleWindow')
$seen = New-Object 'System.Collections.Generic.HashSet[string]'
foreach ($w in [WinWatch]::Snapshot($IncludeHidden.IsPresent)) { [void]$seen.Add($w[0]) }
$log = New-Object System.IO.StreamWriter($OutFile, $false, (New-Object System.Text.UTF8Encoding($false)))
$log.AutoFlush = $true
$log.WriteLine("# watching baseline=$($seen.Count) interval_ms=$IntervalMs")
$clock = [System.Diagnostics.Stopwatch]::StartNew()
$polls = 0; $new = 0; $consoleCount = 0
while (-not (Test-Path -LiteralPath $StopFile) -and $clock.Elapsed.TotalSeconds -lt $MaxSeconds) {
  $polls++
  foreach ($w in [WinWatch]::Snapshot($IncludeHidden.IsPresent)) {
    if (-not $seen.Add($w[0])) { continue }
    $new++
    if ($consoleClasses -contains $w[2]) { $consoleCount++ }
    $name = ''
    try { $name = (Get-Process -Id ([int]$w[1]) -ErrorAction Stop).ProcessName } catch { $name = '(gone)' }
    $log.WriteLine(("{0}|{1}|{2}|{3}|{4}|{5}|{6}" -f $clock.ElapsedMilliseconds, $w[0], $w[1], $name, $w[2], $w[3], $w[4]))
  }
  Start-Sleep -Milliseconds $IntervalMs
}
$log.WriteLine("# done polls=$polls new=$new console=$consoleCount")
$log.Close()
