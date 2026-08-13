# Patch: `internal/cli/server_health.go`

**Wave:** E (live-dogfood repair)

## What was changed

The generated JSON read path (`resolveReadWithStrategyAndResponsePath` plus the
provenance/table/`printOutputWithFlagsMeta` pipeline) was replaced with a raw
text fetch through `rawTextGet` (`internal/cli/rawtext.go`), rendered by
`mcEmit`.

- `--json` / piped stdout: `{"schema_version":1,"status":"ok","body":"OK","latency_ms":N}`.
- Interactive: the body plus the resolved base URL and the round trip in ms.
- Any status other than 200 exits `ExitServerUnreachable` (4).
- `pp:typed-exit-codes: "0,4"` was added; `--dry-run` short-circuits through
  `dryRunOK`/`writeDryRun`, which the generated client used to provide.

## Why

`GET /health` answers the literal string `OK`, not JSON. The generated client
decodes every response as JSON, so against a perfectly healthy proxy this
command failed with `API returned a non-JSON response; expected JSON` and exit
5 — a client-side shape assumption reported as an API fault. Live dogfood
2026-08-13 failed both `server health` and `server health --json` on it.

Exit 4 rather than 5 for a non-200 because /health is the liveness probe for
the proxy itself: if it does not answer 200, the proxy is not serving, and
that is what `ExitServerUnreachable` means.

## What a regen must preserve

1. The raw-text body. A regen restores the JSON read path and reinstates the
   exit-5 failure against a healthy server. Guarded by
   `TestServerHealthReadsPlainOK`.
2. The non-200 -> exit 4 mapping, guarded by
   `TestServerHealthNonOKIsUnreachable`.
3. The JSON envelope keys `status`, `body`, `latency_ms` — a consumer branches
   on them.
