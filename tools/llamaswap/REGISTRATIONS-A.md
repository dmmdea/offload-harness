# REGISTRATIONS-A — wave A (spine)

Registration notes from the spine build. **No edit to `internal/cli/root.go` is
required for any of it** — everything below wires itself through the generated
extension points, which is what those points exist for. The exact `AddCommand`
lines are recorded anyway, so a coordinator can make the wiring explicit if the
project prefers that.

## New root command: `ps`

Registered from `internal/cli/ps.go` via the generated novel-command hook:

```go
func init() {
	registerNovelCommand(func(root *cobra.Command, flags *rootFlags) {
		addNovelCommandIfAbsent(root, newSpinePsCmd(flags))
	})
}
```

`registerNovelCommand` hooks run inside `newRootCmd` (root.go:399) before the
`addNovelCommandIfAbsent` block, and registration is additive, so this cannot
collide with a sibling wave's hook.

If explicit registration in root.go is preferred instead, delete the `init()` in
`ps.go` and add this line beside the other novel commands (root.go ~line 401):

```go
	addNovelCommandIfAbsent(rootCmd, newSpinePsCmd(flags))
```

## New subcommand: `keepset status`

Attached inside `newNovelKeepsetCmd` in `internal/cli/keepset.go` (the same file
that already attaches `keepset audit`), so root.go is untouched:

```go
	addNovelCommandIfAbsent(cmd, newNovelKeepsetStatusCmd(flags))
```

## Commands whose registration was already present (implemented in place)

| Command | Registered at | Wave A change |
|---|---|---|
| `sync` | root.go:376 `rootCmd.AddCommand(newSyncCmd(flags))` | added `--watch/--interval/--seal/--event-window/--no-mirror` and the epoch-aware mirror pass |
| `swaps` | root.go `addNovelCommandIfAbsent(rootCmd, newNovelSwapsCmd(flags))` | TODO scaffold replaced with the real implementation |
| `residency` (alias `replay`) | root.go `addNovelCommandIfAbsent(rootCmd, newNovelResidencyCmd(flags))` | wave D: new — idle-TTL what-if over mirror gaps (`internal/cli/residency.go` + `analytics_common.go`) |
| `saturation` | root.go `addNovelCommandIfAbsent(rootCmd, newNovelSaturationCmd(flags))` | wave D: new — per-seat 429/5xx + load pressure (`internal/cli/saturation.go`) |
| `keepset` / `keepset audit` | root.go:401 | TODO scaffold replaced; `status` added |
| `models unload` / `models unload-all` | via `newModelsCmd` | generated endpoint mirrors replaced with keep-set- and drain-aware versions |

Wave D added two novel commands and one shared helper file (`internal/cli/analytics_common.go`).
The `whichIndex` (`internal/cli/which.go`) now holds SEVEN entries (was five): the two new verbs
were added alongside `keepset audit`, `seat log`, `sync`, `swaps`, `verify`.

## Files created by wave A

```
internal/fakeswap/fakeswap.go
internal/fakeswap/fakeswap_test.go
internal/mirror/client.go
internal/mirror/mirror.go
internal/mirror/keepset.go
internal/mirror/mirror_test.go
internal/mirror/keepset_test.go
internal/cli/sync_mirror.go
internal/cli/ps.go
internal/cli/spine_acceptance_test.go
REGISTRATIONS-A.md
```

## Files modified by wave A

```
internal/cli/sync.go              (flags + Long help + two call sites; generated body otherwise untouched)
internal/cli/swaps.go             (scaffold -> implementation)
internal/cli/keepset.go           (scaffold -> implementation + status subcommand)
internal/cli/keepset_audit.go     (scaffold -> implementation)
internal/cli/models_unload.go     (generated mirror -> hardened)
internal/cli/models_unload_all.go (generated mirror -> hardened)
```

## Handoff notes for whoever owns repo-level bookkeeping

1. **`.printing-press-patches/` is still empty.** `AGENTS.md` asks that every
   edit to a generated file be recorded there so a reprint carries the intent
   forward, but it points at the public library's `AGENTS.md` for the entry
   shape, which is not in this tree. Wave A modified six generated files (listed
   above); each needs a reprint guard. Not written here rather than guessed at
   in the wrong format.

2. **`internal/config/config.go:27` hardcodes the loopback HOSTNAME as the
   default base URL.** House rule for this CLI is the literal `127.0.0.1`: a
   loopback hostname can stall ~21s on an `::1` first attempt in this
   environment. Wave A works around it — `spineBaseURL` in `sync_mirror.go`
   resolves the configured host and rewrites it to `127.0.0.1` when every
   resolved address is loopback — but the generated client still dials the
   configured value directly, so the default itself should be changed at the
   source. That file is outside wave A's ownership.

3. **`whichIndex` (`internal/cli/which.go`) has no entry for `ps` or
   `keepset status`.** Verified: the index holds exactly five entries
   (`keepset audit`, `seat log`, `sync`, `swaps`, `verify`).
   `TestWhichIndex_ExistsAndIsWellFormed` only checks index-to-tree resolution,
   so nothing fails today, but `which "what is loaded"` will not surface `ps`
   until an entry is added by whoever owns that file.
