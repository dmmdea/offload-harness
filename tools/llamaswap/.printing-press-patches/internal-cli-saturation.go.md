# Patch: `internal/cli/saturation.go`

**Wave:** D (analytics verbs)

## What was changed

Entirely new hand-authored file (novel command, not generated). Implements the
`saturation` command: per-seat 429/5xx counts and rates, request volume, busiest
hour, and the hourly load curve, from the local mirror.

## Why

The deferred "saturation" verb. Its originally-imagined headline — in-flight
concurrency depth — was found unreconstructable: llama-swap stamps activity at
whole-second resolution, so an overlap sweep of (ts, duration) invents depth from
same-second collisions (a measured slots=1 seat "showed" depth 6). So this verb
reports only what is ground-truth at second resolution: rejections, error rates,
volume, and when the load lands.

## What a regen must preserve

1. The whole file — a regen emits nothing for it.
2. **The deliberate absence of any in-flight/concurrency/queue-depth field.**
   Re-introducing one would print a timestamp artifact as capacity. Guarded by
   `TestSaturation_CountsRejectionsAndOmitsConcurrency`, which fails if any JSON
   key matches in_flight/inflight/concurren/depth.
3. Rejection/error counts come straight from status codes (429, 5xx) — exact,
   not modelled.
4. PRINT-ONLY: never edits ttl, groups, or placement (`pp:data-source local`).
