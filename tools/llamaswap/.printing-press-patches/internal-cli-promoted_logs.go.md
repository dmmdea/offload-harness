# Patch: `internal/cli/promoted_logs.go`

**Wave:** E (live-dogfood repair)

## What was changed

The generated JSON read path was replaced with a raw text fetch through
`rawTextGet` (`internal/cli/rawtext.go`), rendered by `mcEmit`.

- `--json` / piped stdout: `{"schema_version":1,"content_type":"text/plain","lines":N,"body":"..."}`.
- Interactive: the buffer itself, verbatim, with a trailing newline added only
  when the server omitted one.
- Any status other than 200 exits `ExitServerUnreachable` (4).
- `pp:typed-exit-codes: "0,4"` was added; `--dry-run` short-circuits through
  `dryRunOK`/`writeDryRun`.

The `// Wire sibling endpoints and sub-resources as subcommands` marker and the
plain `return cmd` were kept: the `triage` subcommand is attached to this
command from `glue_common.go` via `glueFindChild(root, "logs")`, so the command
must keep its name and stay attached to the root the same way.

## Why

`GET /logs` answers `text/plain`. The generated client decodes every response
as JSON, so `logs` and `logs --json` both failed with `API returned a non-JSON
response; expected JSON` and exit 5 in the 2026-08-13 live dogfood. The
`triage` subcommand had already solved this for itself (`glueFetchLogs`,
`logs_triage.go`), which left the parent command as the only broken half of the
same endpoint.

## What a regen must preserve

1. The raw-text body. Guarded by `TestLogsReadsPlainTextBuffer`.
2. The `triage` attachment point — do not restructure this file into something
   `glueFindChild` cannot find, and do not add `triage` here (that would
   double-register it).
3. The JSON envelope keys `content_type`, `lines`, `body`.
