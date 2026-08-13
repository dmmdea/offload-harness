# Patch: `internal/cli/keepset_audit.go`

**Wave:** A (spine)

## What was changed

The generated TODO scaffold was replaced with the sampled custody audit:
residency over time, evictions attributed to their cause, and traffic counts
inside each degraded window, joined from the mirrored tables.
`pp:data-source` narrowed from `auto` to `local`.

## Why

No endpoint reports residency HISTORY. The answer can only come from the local
mirror, so the data-source declaration must say so rather than implying a live
path exists.

## What a regen must preserve

1. The whole body (a regen re-emits a TODO scaffold).
2. `pp:data-source local`.
3. **Sampled-window honesty in every output path.** The audit reports what the
   mirror actually observed, including its gaps. A ledger presented as complete
   when it has holes is worse than no ledger; the command name is "audit", not
   "custody", for that reason.
