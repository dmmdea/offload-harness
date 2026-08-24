# Patch: `internal/cli/residency.go`

**Wave:** D (analytics verbs)

## What was changed

Entirely new hand-authored file. The generator does not emit it (it is a novel
command, not an API endpoint mirror). Implements the `residency` command (hidden
alias `replay`): per-seat load/eviction reconstruction from mirrored request
gaps, plus an idle-TTL `--ttl model=seconds` counterfactual with structured
keep-set / eviction-group safety fields.

## Why

The deferred "replay" verb was defined here from 10 days of accumulated mirror
data. It answers the operator's own open question (was the uniform 5-minute TTL
right per seat) with measured numbers. Registered under the operator-intuitive
name `residency` with `replay` kept as an alias so the deferred name resolves.

## What a regen must preserve

1. The whole file — a regen emits nothing for it.
2. **The interval direction.** Idle is `next.start - prev.end` where
   `start = ts - duration` (llama-swap stamps `ts` at COMPLETION and
   `duration_ms` spans dispatch→done). This is centralized in
   `mirrorRequest.Start()/End()` in `analytics_common.go` and pinned by
   `TestResidency_IntervalDirectionIsCompletionStamped`. Reversing it silently
   turns real idle time into request duration and misclassifies evictions.
3. **The bound labels.** `reloads_avoided_ceiling` / `cold_minutes_saved_ceiling`
   are optimistic (the idle-only model ignores group-exclusivity and VRAM
   eviction); `resident_minutes_added_upper_bound` is an upper bound. The signs
   are load-bearing (the roast Logician flagged the opposite as worse than no
   caveat). Pinned by `TestResidency_WhatIfRaiseAvoidsReloadsWithCorrectBoundLabels`.
4. The `-1` (never-evict / keep-set) special case: it must never register a TTL
   eviction and must flag a what-if as moot. Pinned by
   `TestResidency_KeepSetSeatIsMootAndNotEvicted`.
5. PRINT-ONLY: never edits ttl, groups, or placement (`pp:data-source local`).
