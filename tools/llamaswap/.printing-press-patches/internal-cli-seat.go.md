# Patch: `internal/cli/seat.go`

**Waves:** B (config surface), D (final glue — `Example`)

## What was changed

The generated scaffold attached one subcommand. It now attaches three — `log`,
`show`, `try` — and carries a filled-in `Long` plus, from wave D, an `Example`
block.

## Why

Per-seat history, live-vs-file comparison, and flag experiments are three
different questions about one seat; they belong under one parent.

## What a regen must preserve

1. All three `addNovelCommandIfAbsent` calls.
2. The `Example` block and the `mcp:read-only` / `pp:parent-group` annotations —
   `internal/cli/handbuilt_audit_test.go` asserts both and will fail if a regen
   drops them.
3. **The statement that none of these writes the config**, and the fact behind
   it: `seat try` is PLAN-ONLY. It computes the flag change, prints the unified
   diff, the restart command, and the bound acceptance command. It never writes
   the YAML.
