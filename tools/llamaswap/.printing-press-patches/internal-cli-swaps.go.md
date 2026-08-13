# Patch: `internal/cli/swaps.go`

**Wave:** A (spine)

## What was changed

The generated TODO scaffold was replaced with the real implementation: cold-load
percentiles per seat, minutes/day lost to swapping, and mutually-evicting model
pairs, computed from the mirrored domain tables.

`pp:data-source` was narrowed from the scaffold's `auto` to `local`.

`spineSwapsReport.Thrash` was changed from `[]spineThrashPair` with
`json:"thrash,omitempty"` to `*[]spineThrashPair` with the same tag, and
`spineBuildSwapsReport` now always materializes a non-nil (possibly empty)
slice when `--thrash` is passed. `spineWriteSwapsReport` dereferences it.

## Why

Swap economics is a question about HISTORY, and llama-swap exposes no endpoint
that answers it. `auto` would imply a live path exists; only the local mirror
has the timeline, so the declaration says `local`.

The `Thrash` pointer exists because `omitempty` drops an empty slice. That made
`swaps --thrash --json` emit output BYTE-IDENTICAL to plain `swaps --json`
whenever no mutual-eviction pair was found — the reader could not distinguish
"the thrash analysis ran and found nothing" from "the thrash analysis never
ran". An empty result is a finding and has to be visible. With a pointer, nil
means the flag was not passed (key absent) and a non-nil empty slice serializes
as `"thrash": []`.

## What a regen must preserve

1. The whole file body — a regen re-emits a TODO scaffold, which fails dogfood.
2. The `pp:data-source local` marker. Do NOT let a regen restore `auto`.
3. The command prints only. It must never edit groups, TTLs, or placement: the
   value of the report is that it is a report.
4. The `Thrash` POINTER and the non-nil materialization under `--thrash`. A
   regen that restores a plain `[]spineThrashPair` silently reintroduces the
   indistinguishable-output bug. Guarded by
   `TestSwapsThrashEmitsEmptyArrayOnColdMirror`.
