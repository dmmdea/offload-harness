# Node swap

## Purpose

`local-offload node-swap` (`node_swap_cmd.go`, engine in `internal/nodeswap/`) is the one
reusable Windows fleet-node binary-swap tool every future deploy calls, replacing the family of
hand-adapted, per-release scripts (`aorus-swap-<sha>.ps1`, `deploy-node-exe.ps1`,
`fleet-node-restart.ps1` stitched together on the fly) that every deploy record before it used.

It exists because of a real outage. The 2026-09-24 Aorus 0.140.8 deploy ran its
restart-and-verify phase inside an interactive SSH session. Windows OpenSSH kills the whole
remote process tree when the client disconnects (the `windows-ssh-remote-ops-patterns` house
memory). The session dropped mid-script: the binary had already been swapped, but the script
never reached `schtasks /run` / health-verify / its own rollback path. The node sat down from
02:56 to ~05:30 with nobody watching, and was only recovered by a separate, short-lived
`schtasks /run` call made by hand. A second hazard the same day: an idle
`offload-harness.exe mcp` process on the OptiPlex held an OS-level file handle on the live exe
and silently blocked `Move-Item`/`Rename-Item`, even though nothing was actively using it.

`node-swap` fixes both: it is meant to be launched **detached** (see
[windows-node-swap-launch.ps1](#interfaces-and-entry-points)) so an SSH drop cannot orphan a
half-finished swap, and it diagnoses a rename failure instead of guessing — stopping only an
idle MCP helper on the same exe, never a live server.

## Questions this doc answers

- How do I swap a fleet node's binary (and, optionally, its render tree) without babysitting an
  SSH session for the whole restart-and-verify window?
- What happens if the connection that launched the swap drops mid-run?
- What happens if the swap fails partway through — does the node come back on the old binary?
- Why did a binary rename fail, and which processes does this tool stop to clear it?
- How do I add this to a new node, or wire it into an install/deploy script?

## Scope

The swap sequence for ONE Windows node's binary (verify hash, wait idle, backup, rename,
restart, verify, automatic rollback on any failure) and, as an option, its render tree
(tarball + backup + extract + hash-verify). The detached-launch wrapper
(`setup/windows-node-swap-launch.ps1`) that makes a run survive the launching session ending.

## Non-scope

- **Building or staging the new binary.** `node-swap` takes an already-staged exe and its
  expected sha256; it never builds, fetches, or verifies provenance beyond that hash.
- **A Linux process-holder diagnosis.** The `internal/nodeswap` engine itself is
  OS-agnostic Go and runs on Linux too (`GOOS=linux` builds ship it); what stays
  Windows-only is the rename-holder diagnosis (`FindProcessesByExe`/`StopProcess`,
  CIM-based) — a live Linux binary can be renamed out from under a running process
  with no lock at all, so that class of problem does not exist there. Linux deploys
  use [`setup/linux-node-swap-launch.sh`](#interfaces-and-entry-points), the
  detached-launch sibling of the PowerShell one, which drives the SAME engine —
  never a separate, hand-rolled polling implementation (the deploy-d5207011 Lenovo
  incident this fixes: an ad-hoc bash script's own health-poll hardcoded
  `127.0.0.1` while fleet-serve there bound only its tailnet address, and an
  unguarded fallback read every failed poll as "still busy" for ~46 minutes).
- **Deciding WHEN to deploy, or building the go binary/render tarball.** Those stay operator
  and deploy-record concerns; this tool is the mechanical last mile.
- **The OptiPlex's own campaign scheduling** (which release to deploy, when). What
  `node-swap` DOES now own for a standalone node (no `--health-url`, no
  `--restart-task`/`--restart-command`): it waits for this node's own GPU lease to
  clear before touching the binary — the check a standalone-node deploy previously
  left to the operator's own `gpu status` (deploy-d5207011 OptiPlex section). See
  "GPU-lease wait (standalone nodes)" below.

## Key concepts

- **Plan / Outcome** (`internal/nodeswap/nodeswap.go`) — a `Plan` is every input to one run,
  plain data with no live handles; `Outcome` is the full JSON-serializable audit trail
  (`Steps[]`, `OK`, `Error`, `RolledBack`, `RollbackOK`, the old/new sha256, the final PID and
  image hash). The `--result` file IS an `Outcome`.
- **Standalone node** — no `--health-url` and neither `--restart-task` nor `--restart-command`:
  the OptiPlex pattern (binary-only swap, no fleet-serve to wait on or restart). The health
  half of post-restart verification is skipped (the swap is still proven by hash), but the
  wait step is NOT skipped: it waits for this node's own GPU lease to clear instead (see
  "GPU-lease wait" below) — a standalone node has no queue depth to read, but it can still
  be mid-render under a caller's own `gpu reserve`.
- **GPU-lease wait (standalone nodes)** — `Plan.GPULockPath`/`GPUStateDir`
  (`internal/gpulease.OpenAt` under the hood, the same resolution `gpu status` uses) are
  polled with the SAME timeout/interval shape as the fleet-serve idle-wait, refusing to
  proceed while the lease is held. `Deps.InspectGPULease == nil` (an older caller) skips
  this exactly as before it existed — never a nil-function panic.
- **Auto-resolved `--health-url`** — when the caller leaves `--health-url` empty,
  `runNodeSwap` reads THIS node's own config `fleet_listen` (`--config`, same resolution
  precedence as every other command) and fills it in automatically — but ONLY when that
  address already clears loopback/wildcard (`resolveNodeSwapDefaults`,
  `loopbackOrWildcardHost` in `node_swap_cmd.go`). A config that still carries the
  built-in loopback default gets no health URL, never a wrong one that reads as a false
  "not idle" forever — the exact failure class of the Lenovo deploy-d5207011 incident. An
  explicit `--health-url` always wins outright.
- **Idle MCP holder** — a Windows-only class of rename blocker: a `local-offload.exe mcp`
  process from another session holds an OS-level handle on the exe with no active job. Only a
  process matching `--mcp-match` (default `" mcp"`) and **not** also matching `--process-match`
  (default `"fleet-serve"`) is ever stopped to clear a rename; a live fleet-serve holder is
  reported and left alone by this path (it was already handled by the earlier stop-node step).

## How the system works

`Run` (`internal/nodeswap/nodeswap.go`) executes a fixed sequence over the `Deps` seam (every
OS operation — hashing, health reads, CIM process enumeration, renames, tarball extraction,
`Start`/`Stop-ScheduledTask` — is a function value, so the whole state machine including every
rollback branch is unit-tested with fakes, no real Windows box required):

1. **Verify hash** — the staged binary's sha256 must equal `--sha256` before anything is
   touched.
2. **Wait idle** — poll `--health-url`'s `/fleet/health` until `running=0, queued=0` (skipped
   for a standalone node). `--dry-run` stops here, after step 1–2, having touched nothing.
3. **Stop the node** — `Stop-ScheduledTask` (if `--restart-task` is set) and stop any process
   whose command line matches `--process-match`, waiting for full exit.
4. **Backup the old binary** — rename `Target` to `Target.bak-<suffix>`. On failure, enumerate
   holders via CIM (`Win32_Process` filtered by `ExecutablePath`); stop only an idle MCP
   holder and retry once; anything else is reported, never touched.
5. **Install the new binary** — rename `Staged` over `Target`.
6. **Optional render-tree swap** — backup the render dir, extract the tarball, verify at least
   one file landed; any failure here rolls the whole run back (binary included).
7. **Restart** — `Start-ScheduledTask`, or run `--restart-command` (e.g. the Qube's
   WMI-launching `fleet-node-restart.ps1`), or nothing at all for a standalone node.
8. **Verify** — poll for a process matching `--process-match` whose OWN running image hashes
   to what was just installed, and (when configured) a healthy `/fleet/health`, within
   `--verify-timeout`.

**Any failure from step 4 onward triggers automatic rollback** — including backing up the old
exe (step 4) and installing the new one (step 5), both of which happen AFTER the node was
already stopped in step 3: leaving those failures unrecovered would strand a stopped node with
nobody restarting it, exactly the outage class this tool exists to prevent. Recovery stops
whatever is running, restores the render tree and binary backups (skipping the binary restore
when step 4 itself failed — nothing was ever moved, so the original binary is still sitting at
`Target` untouched), restarts, and re-verifies the OLD binary is back up.
`Outcome.RolledBack`/`RollbackOK` record whether that recovery itself succeeded.

## Important flows

- **Detached launch across an SSH drop** — see
  [windows-node-swap-launch.ps1](#interfaces-and-entry-points): the launcher starts
  `node-swap` via `Win32_Process.Create` (never `Start-Process`, which dies with the SSH
  session per `windows-ssh-remote-ops-patterns`), prints the `--log`/`--result` paths, and
  returns immediately. The swap keeps running, keeps appending to `--log`, and still reaches
  its own rollback path — because it is not a child of the session that launched it.
- **Rollback-itself-fails** — if restoring the backup, restarting, or re-verifying the OLD
  binary fails during rollback, `RolledBack`/`RollbackOK` say so explicitly rather than
  reporting a false recovery; whatever succeeded (e.g. the binary restore) is never undone
  again by a later failed step.

## Data and state

- `--result <path>` — the full `Outcome` as JSON, written atomically (`.tmp` then rename) so a
  poller reading it over a fresh connection never observes a partial write.
- `--log <path>` — a timestamped, append-only text log of every step, written as the swap
  progresses (so `Get-Content -Tail` shows live progress, not just the final result).
- Backups (`Target.bak-<suffix>`, `RenderDir.bak-<suffix>`) are left in place on success —
  never deleted by this tool — matching every prior deploy record's convention.

## Interfaces and entry points

- `local-offload node-swap --staged NEW.exe --target LIVE.exe --sha256 HEX [--restart-task
  NAME | --restart-command "..."] [--health-url URL] [--render-tarball t.tar.gz --render-dir
  DIR] [--dry-run] [--result out.json] [--log out.log] [--json]` — the engine; runs
  synchronously, returns its own exit code.
- [`setup/windows-node-swap-launch.ps1`](../../setup/windows-node-swap-launch.ps1) — the
  Windows detached launcher: registers the above via `Win32_Process.Create`, prints the
  log/result paths, and returns immediately so the caller's session may disconnect safely.
- [`setup/linux-node-swap-launch.sh`](../../setup/linux-node-swap-launch.sh) — the Linux
  sibling: `setsid nohup <node-swap engine binary> node-swap ... &`, disowned, with the same
  post-launch liveness check and log/result-path printout. Its `--restart-command` is the
  Linux equivalent of the Windows launcher's `-RestartTask` (a systemd unit restart, e.g.
  `systemctl restart offload-fleet-node.service`, matching every deploy record's own
  pattern); omitting it (and `--health-url`) is the standalone/OptiPlex-on-Linux shape.

## Dependencies

- `GET /fleet/health` ([fleet-node.md](fleet-node.md)) for idle-wait and post-restart
  verification.
- CIM (`Get-CimInstance Win32_Process`) for process enumeration/holder diagnosis — Windows
  only (`internal/nodeswap/deps_windows.go`); `internal/nodeswap/deps_other.go` stubs this
  clearly on other OSes rather than faking a story this tool does not implement there.
- `Start`/`Stop-ScheduledTask` or an arbitrary `--restart-command` for the restart step.

## Downstream effects

A rollback restarts the node with the OLD image — any code relying on the NEW release having
landed (a follow-up smoke test, a dependent config change) must check `Outcome.OK` /
`FinalImageSHA256` before assuming the swap took, never just that the tool exited without a
Go panic.

## Invariants and assumptions

- The staged binary's sha256 is verified BEFORE any file or process is touched.
- A rename failure never stops a process matching `--process-match` (a live server) — only an
  idle helper matching `--mcp-match` and not also `--process-match`.
- Every failure from step 4 onward (the node already stopped) attempts an automatic rollback —
  backup-old and install-new included, not only the render-tree swap and later steps;
  `--no-rollback` exists for tests only and is never set in production.
- `Deps` has no direct filesystem/process calls outside `internal/nodeswap/deps*.go` — the
  sequencing in `nodeswap.go` is OS-agnostic and fully fake-testable by design.

## Error handling

Every step's failure is captured in `Outcome.Steps[]` (name, ok, detail, timestamp) and
`Outcome.Error`. A holder that blocks a rename is reported by PID and command line, never
guessed at. `writeSwapResult` still writes whatever `Outcome` was produced even when a later
step (e.g. rollback) also failed, so a poller always has something to read.

## Security and privacy notes

`--restart-command` runs whatever string it is given via `powershell -Command` with no
sandboxing beyond what the caller already has on that box — it is meant for a fixed,
operator-authored restart script path (e.g. `fleet-node-restart.ps1`), never untrusted input.

## Observability and debugging

- `--log` is human-readable, timestamped, append-only; safe to `Get-Content -Tail -Wait` while
  a detached run is in progress.
- `--result` is the full `Outcome`; `rolled_back: true, rollback_ok: true` means the swap
  failed safely and the box is back on the old binary, running.
- A holder-diagnosis failure names the blocking PID(s) and their command lines directly in
  `Outcome.Error` — no separate log grep needed.

## Testing notes

`internal/nodeswap/nodeswap_test.go` and `deps_test.go` cover the sequence (happy path, hash
mismatch, never-idle timeout, rename-retry with an idle MCP holder vs. a live fleet-serve
holder left alone, backup-old failure -> restart-only recovery (nothing was ever moved), 
install-new failure -> restore + restart, restart failure -> rollback, verify failure ->
rollback, a rollback whose OWN restore fails surfacing `RollbackOK:false` rather than a false
recovery, render-tree swap rolled back on a later failure, standalone-node hash-only
verification, the standalone GPU-lease wait clearing/timing out, `backupPathFor`'s
doubled-`bak-` guard) and the real
cross-platform primitives (hashing, health-read incl. the pre-0.100.0 `queue_depth` fallback,
tar.gz extraction incl. a path-escape refusal). `node_swap_cmd_test.go` covers CLI flag
parsing. Everything above builds and passes on both `GOOS=linux` (CI) and `GOOS=windows`
(cross-compiled) — see `internal/nodeswap/deps_windows.go` / `deps_other.go` for the
platform split `crossplatform_lint_test.go` expects.

## Common pitfalls

- Passing `--target` as the binary currently running the CLI itself — this is EXPECTED and is
  how every deploy record already worked (a running Windows exe can be renamed away as long as
  no process holds a blocking handle on it); the tool's own holder diagnosis exists for the
  case where that assumption breaks.
- Forgetting `--health-url` on a real fleet node — auto-resolve now fills it in from this
  node's own config `fleet_listen` WHEN that config already names a real (non-loopback)
  bind address; a node whose config was never told its real address still skips the
  idle-wait exactly as before (hash-only verification, no false sense of safety). Passing
  `--health-url` explicitly always overrides the auto-resolve either way.
- Setting both `--restart-task` and `--restart-command` — rejected up front
  (`nodeswap.validatePlan`); pick one restart mechanism per node.
- Passing a `--backup-suffix` that already starts with `bak-` (e.g.
  `--backup-suffix bak-2026-09-24-pre-<sha>`) — `backupPathFor` strips one redundant
  leading `bak-` before prepending its own, so this no longer doubles into
  `<target>.bak-bak-...`; harmless either way (the backup is still found and restored by
  its exact name), but the plain suffix (`2026-09-24-pre-<sha>`) reads cleaner.

## Source map

- `internal/nodeswap/nodeswap.go` — the sequence, `Plan`/`Outcome`/`Deps`, rollback, the
  standalone GPU-lease wait (`waitGPUFree`), `backupPathFor`'s doubled-`bak-` guard.
- `internal/nodeswap/deps.go` — cross-platform real implementations (hash, health, rename,
  tar.gz extraction, `InspectGPULease` via `internal/gpulease`).
- `internal/nodeswap/deps_windows.go` / `deps_other.go` — the CIM process-enumeration /
  `taskkill` split.
- `node_swap_cmd.go` — the CLI (`local-offload node-swap`), the config-driven
  `--health-url`/GPU-lease-path auto-resolve (`resolveNodeSwapDefaults`).
- `setup/windows-node-swap-launch.ps1` — the Windows detached launcher.
- `setup/linux-node-swap-launch.sh` — the Linux detached launcher.

## Related docs

- [fleet-node.md](fleet-node.md) — the `/fleet/health` contract this tool polls, and the
  existing "Running as a Windows scheduled task" registration recipe.
- [`../FLEET-NODE.md`](../FLEET-NODE.md) — the operator quickstart for running a fleet node.
