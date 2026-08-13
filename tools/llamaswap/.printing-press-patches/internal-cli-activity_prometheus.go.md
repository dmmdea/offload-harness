# Patch: `internal/cli/activity_prometheus.go`

**Wave:** E (live-dogfood repair)

## What was changed

The generated JSON read path was replaced with a raw text fetch through
`rawTextGet` (`internal/cli/rawtext.go`), rendered by `mcEmit`, plus an
explicit status switch:

- 200 -> `{"schema_version":1,"metrics_enabled":true,"content_type":...,"lines":N,"body":"..."}`;
  interactive mode prints the exposition text verbatim.
- 503 -> **exit 0** with
  `{"schema_version":1,"metrics_enabled":false,"reason":"llama-swap monitoring disabled (503)","http_status":503}`,
  and a one-line plain-text note in human mode.
- anything else -> `ExitServerUnreachable` (4).

A `Long` description was added stating the 503 contract and the exit codes, and
`pp:typed-exit-codes: "0,4"` was set. `--dry-run` short-circuits through
`dryRunOK`/`writeDryRun`.

## Why

`GET /metrics` answers Prometheus exposition text, not JSON, so the generated
client failed it with `API returned a non-JSON response; expected JSON` and
exit 5 in the 2026-08-13 live dogfood.

The 503 mapping is the more important half. Per the spec note, llama-swap
answers 503 on /metrics when it was started **without monitoring**. That is a
fact about the deployment, not an outage: an unattended agent that treats it as
an error retries a call that can never succeed on that process. The CLI already
uses this convention elsewhere — hardware 404 means the feature is absent — so
"switched off" is reported as data with `metrics_enabled:false` and exit 0.

`metrics_enabled` is emitted in BOTH the on and off envelopes on purpose: a
consumer branches on one boolean instead of on the presence of a key.

## What a regen must preserve

1. The raw-text body. Guarded by `TestActivityPrometheusServesExpositionOn200`.
2. **The 503 -> exit 0 mapping.** A regen that restores the generated error
   classification turns a documented feature switch back into a failure.
   Guarded by `TestActivityPrometheusMonitoringDisabledExitsZero`.
3. The `metrics_enabled` key in both envelopes, and the reason string constant
   `rawTextPrometheusOffReason` (rawtext.go) as its single source.
