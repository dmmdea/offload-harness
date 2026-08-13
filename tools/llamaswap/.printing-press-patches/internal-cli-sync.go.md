# Patch: `internal/cli/sync.go`

**Wave:** A (spine)

## What was changed

Five flags were added to the generated `sync` command — `--watch`,
`--interval`, `--seal`, `--event-window`, `--no-mirror` — plus three call sites
that route to the epoch-aware mirror:

- two short-circuits at the TOP of `RunE`, before the generated resource sync
  (`--seal` → `spineRunSeal`, `--watch` → `spineRunWatch`);
- one mirror pass after the generated sync completes (`spineRunMirror`).

The generated resource-sync body is otherwise untouched. Every piece of new
logic lives in `sync_mirror.go` and `internal/mirror/`; this file only holds
flag registration and the three calls.

## Why

llama-swap keeps request activity in a bounded in-memory ring and loses all of
it on restart. The generated `sync` mirrors framework resources but has no
concept of a server epoch, so it cannot tell "the ring rolled" from "the server
restarted and the id space reset to 1" — and silently merging two epochs
produces a history that never happened.

## What a regen must preserve

1. The five flag registrations, with their exact names and defaults.
2. The `--seal` / `--watch` short-circuits placed BEFORE any resource sync.
   `--seal` performs no network read; running the resource sync first would
   defeat that.
3. The `spineRunMirror` call after the resource sync, and its `--no-mirror`
   guard.
4. Nothing else. If the generator's own `sync` body changes, take the new body
   and re-apply these four points on top.
