# Patch: `internal/cli/upstream_metrics.go`

**Wave:** E (live-dogfood repair — both classes)

## What was changed

**Class 1 (text endpoint).** The generated JSON read path was replaced with a
raw text fetch through `rawTextGet` (`internal/cli/rawtext.go`), rendered by
`mcEmit`, behind an explicit status switch:

- 200 -> `{"schema_version":1,"metrics_enabled":true,"content_type":...,"lines":N,"body":"..."}`;
  interactive mode prints the exposition text verbatim.
- 501 **or** 404 -> **exit 0** with
  `{"schema_version":1,"metrics_enabled":false,"reason":"seat's llama-server lacks --metrics (HTTP 501/404) — a seat-flag absence, not an error","http_status":N}`.
- any other 5xx -> `ExitUpstream5xx` (27).
- anything else -> `apiErr` (5).

**Class 2 (live fixture).** `pp:happy-args: "--model;embeddinggemma"` was added
to the annotations and the `Example` placeholder `--model example-value` was
replaced with `--model embeddinggemma` (the generated `TODO: replace
placeholder example values` comment is therefore gone).

The generated required-input guard and the `--model` check were kept verbatim;
`--dry-run` short-circuits through `dryRunOK`/`writeDryRun`, which the
generated client used to provide. A `Long` description states the 501/404
contract and the exit codes, and `pp:typed-exit-codes` is `"0,2,4,5,27"`.

## Why

llama.cpp's server serves `/metrics` only when the seat was launched with
`--metrics`. Without the flag it answers 501 (some builds 404). Live-verified
on this deployment 2026-08-13: `embeddinggemma` answers 501 here. Raising that
as an error made a correct read of a correctly configured box look like a
failure, and it is the same class of finding the CLI already reports as data
(hardware 404 -> feature absent, prometheus 503 -> monitoring off).

The fixture was the second half of the failure: the generator emitted the
literal string `example-value`, which llama-swap correctly answered 404 for, so
`upstream metrics` failed live for a reason that had nothing to do with
metrics. `embeddinggemma` is the always-resident mem0 embedder (`ttl: -1`) and
this call is read-only, so a live dogfood can neither auto-start a model nor
evict one.

**Known trade-off, stated deliberately:** llama-swap also answers 404 with
`{"error":"model not found"}` when the *model* does not exist, so a typo'd
`--model` is reported here as `metrics_enabled:false` rather than as
not-found. The 404 arm exists for llama-server builds that answer 404 instead
of 501; `http_status` is in the envelope so a consumer can tell the two 404s
apart, and 501 is what this deployment actually returns.

## What a regen must preserve

1. The raw-text body. Guarded by `TestUpstreamMetricsServesPrometheusTextOn200`.
2. **The 501/404 -> exit 0 mapping**, guarded by
   `TestUpstreamMetricsFeatureAbsentExitsZero`.
3. **The 5xx -> exit 27 mapping**, guarded by
   `TestUpstreamMetricsUpstream5xxIsTyped` — the feature-off arm must never
   swallow a genuine upstream fault.
4. `pp:happy-args: "--model;embeddinggemma"` and the placeholder-free `Example`,
   guarded by `TestUpstreamListCommandsCarryLiveHappyArgs`.
