# Patch: `internal/cli/seat_log.go`

**Wave:** B (config surface)

## What was changed

The generated TODO scaffold was replaced with the per-seat change chronology:
the dated config-backup series on disk, deduplicated by content sha, joined to
the live command line, with the YAML comments carried through as the reasoning.
`pp:data-source` is `local`. Writes the `seat_config_history` table
(`ON CONFLICT(content_sha, model) DO NOTHING`).

## Why

The filesystem holds a change history the API has never seen. The backup series
plus the comments in it IS the operator's decision record.

## What a regen must preserve

1. The whole body (a regen re-emits a TODO scaffold).
2. **Filenames are labels, not timestamps.** Ordering is by mtime, identity is
   by sha256, and a filename date that disagrees with the mtime is REPORTED,
   never trusted — five such disagreements exist in the reference corpus.
3. The idempotent insert. Re-running must not duplicate the timeline.
4. `--no-record` and verify mode must both suppress the write.
