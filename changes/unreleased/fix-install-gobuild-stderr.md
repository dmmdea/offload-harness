### Fixed - `install.ps1` no longer dies when `go build` prints "go: downloading" on a fresh box

- **What it does.** Both Go builds in the Windows installer (the temporary renderer build in
  `Resolve-HarnessExe` and Step 7's `local-offload.exe` build) run with `$ErrorActionPreference`
  set to `Continue` for the native call and judge the build by its exit code; stderr lines go to the
  transcript.
- **Why.** On a box whose Go module cache is cold, which is every fresh install, `go build` writes
  `go: downloading <module>` to stderr. Under the installer's `$ErrorActionPreference = 'Stop'` a
  stderr line merged by `2>&1` is a terminating `NativeCommandError` in Windows PowerShell 5.1, so the
  install aborted at Step 6 with a healthy build still running. Every box that had installed before
  had a warm cache, which is why the installer's own runs never saw it. Found on the first fresh
  Windows install (an RTX 5080 desktop, 2026-11-04); the renderer call a few lines below already
  carried the same guard.
- **Check.** Re-running `setup\install.ps1` on that box after the fix went through Step 6 and Step 7
  and on to the final `{"installed":true,...}` line.
