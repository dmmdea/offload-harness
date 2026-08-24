# Patch: `internal/cli/analytics_common.go`

**Wave:** D (analytics verbs)

## What was changed

Entirely new hand-authored file shared by `residency.go` and `saturation.go`
(novel support code, not generated). Holds: `mirrorRequest` with the ONE correct
interval reconstruction (`Start()/End()`), `loadMirrorRequests`, the
`analyticsCoverage` honesty block (`fillAnalyticsCoverage`), YAML seat/TTL
lookup, and a local percentile.

## Why

Both verbs reconstruct request timing and both must state coverage identically.
Extracting the primitive once (the roast Expansionist's point) stops the two
verbs from silently disagreeing about the same slice of history, and puts the
load-bearing interval-direction fact in exactly one place.

## What a regen must preserve

1. The whole file — a regen emits nothing for it.
2. **`Start() = ts - duration`, `End() = ts`** (completion-stamped; see the file
   comment and `internal-cli-residency.go.md` item 2). This is THE fact the two
   verbs depend on; pinned by `TestResidency_IntervalDirectionIsCompletionStamped`.
3. The coverage block's honesty fields: `second_resolution_ts` (always true here),
   `prepoll_loss`, and a `coverage_pct` that is NULL when a hole makes the
   denominator unsound rather than a fabricated 100%. Pinned by
   `TestAnalytics_CoveragePctFromPrepollLoss`.
