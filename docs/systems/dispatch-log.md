# Dispatch log

## Purpose

`~/.claude/state/dispatch-log.jsonl` is the instrument behind the weekly delegation-adherence read: one JSON
line per event, from every session on the box, in either of two harnesses. It is a cross-harness
contract. The Claude Code hooks write it and rotate it; the opencode plugin in this repo
(`integrations/opencode/`) appends to the same file, tagged `"harness":"opencode"`; the scoreboard reads
both with one arithmetic. A change to either writer that drops an event the reader divides by makes the
instrument fail in the reassuring direction: "nobody spawned a read-only agent" instead of "the
instrument is dead". The H15 hook rewrite did exactly that, removed `readonly_spawn` and `session`, and had
to put them back (register D-74).

## Questions this doc answers

- Which events are in the file, who writes each, and with which fields?
- Which events must a hook or plugin rewrite keep emitting?
- Who rotates the file, and why only one writer may?
- What may a new writer or a new event do without breaking the reader?

## Scope

The file's line format, the event vocabulary, the two writers and the rotation rule.

## Non-scope

How the scoreboard computes its percentages, and the hooks' nudge logic, live with the Claude Code
configuration, which is a separate repository. The opencode plugin's behaviour is in
[opencode-integration.md](opencode-integration.md).

## Key concepts

**Line.** One JSON object per line, UTF-8, newline-terminated. Every line carries `ts` (an ISO-8601 UTC
timestamp) and `event` (a string). Lines from the opencode plugin also carry `"harness":"opencode"`; a line
with no `harness` key is from the Claude Code hooks. `sid` is the first 8 characters of the session id, or
`unknown` when the writer had none.

**Denominator and numerator.** `readonly_spawn` counts the read-only sub-agent spawns a session made while
it could have delegated: the idle-waste denominator. `delegate` counts delegations: the numerator.
`session` is the heartbeat the reader uses to tell a quiet week from a dead instrument.

## How the system works

### Events

| event | written by | fields besides `ts`, `event`, `sid` | read by |
|---|---|---|---|
| `session` | hooks (first tool call of a session), opencode (a new top-level session; child sessions do not count) | none | scoreboard (heartbeat), dispatch report |
| `readonly_spawn` | hooks, opencode | `n` (number), `total` (hooks), `tier` (hooks), `desc` (string, at most 80 characters), `target` (opencode: the agent that would have run it) | scoreboard (the denominator) |
| `delegate` | hooks, opencode | `n` (subtask count, `-1` when unreadable), `route` (string), `tool` (hooks: the tool name) | scoreboard (the numerator), dispatch report |
| `lead_turn` | hooks | `reads`, `edits`, `calls`, `delegates` (numbers), `trajTier` | dispatch report (bulk-read sessions that never delegated) |
| `log_truncated` | hooks (rotation) | `prev_size` | dispatch report (the marker that earlier data was discarded) |
| `nudge` | opencode | `tier`, `reads` | recorded; neither script reads it |
| `task_reroute` | opencode | `from`, `to` (agent names), `desc` | recorded; neither script reads it |
| `task_reroute_skipped` | opencode | `reason`, `desc` | recorded; neither script reads it |
| `task_reroute_unconfirmed` | opencode | `child`, `agent` (string or null) | recorded; neither script reads it |
| `task_reroute_failed` | opencode | `desc` | recorded; neither script reads it |
| `config_default_applied` | opencode | `key`, `value` | recorded; neither script reads it |

"The scoreboard" and "the dispatch report" are the two reader scripts of the Claude Code configuration
(read on 2026-10-18); "neither script reads it" is true of that read and is the reason such an event is
safe to add or drop.

The hooks also write guard and mode events (denials, delegate-mode status); they are theirs, and a reader
ignores them. The events opencode writes are the `DispatchEvent` type in `integrations/opencode/src/instrument.ts`,
which a test holds this page to.

### Rotation

The Claude Code hooks own rotation: when the file passes 1 MiB the next write replaces it with a single
`log_truncated` line carrying `prev_size`. The opencode plugin is append-only. Two processes that both
rotate race each other into data loss, so exactly one writer does, and a new writer of any kind appends
and never truncates.

### Failure behaviour

Both writers fail open: a broken log channel writes one line to stderr and never throws. The plugin counts
its write failures and reports them in `offload_plugin_status`, so a dead instrument is distinguishable from
a quiet week.

## Important flows

1. A session's first tool call writes `session`; each qualifying read-only sub-agent spawn writes
   `readonly_spawn`; each delegation writes `delegate`.
2. The reader sums `readonly_spawn` and `delegate` per harness over the week and divides; `session` rows
   confirm the writer was running at all.
3. A rewrite of a writer keeps those three events and their meaning, or the reader's number moves for
   reasons that have nothing to do with delegation.

## Data and state

One append-only file under the user's `.claude/state` directory, shared by both harnesses on the box.
`DISPATCH_LOG_PATH` redirects the hooks' copy and must be set by tests and probes: the instrument was once
measured 96 percent test fixtures because fixtures and real sessions shared one path.

## Interfaces and entry points

Writers: the Claude Code hooks' `appendDispatchLog`, and `appendDispatchLog` in
`integrations/opencode/src/instrument.ts`. Reader: the scoreboard script in the Claude Code configuration.

## Dependencies

None at run time beyond the file system; the two harnesses do not call each other.

## Downstream effects

The weekly adherence number and any decision taken from it (nudge tiers, which tools are exposed to the
primary agent) read this file. An event added here is invisible to the reader until the reader is taught it.

## Invariants and assumptions

- **Hooks keep emitting `readonly_spawn` and the `session` heartbeat.** The reader's denominator and its
  liveness check depend on them. A hook test that pins the `session` row at the first call does not exist
  yet; it belongs to the hooks' repository, not to this one (register D-74).
- **The plugin stays append-only** and tags every line `"harness":"opencode"`.
- **The reader ignores unknown events and unknown fields**, so a writer may add an event without a
  reader change; it just will not be counted until the reader learns it.
- A field's meaning never changes under the same name.

## Error handling

| symptom | likely cause |
|---|---|
| the week reads zero spawns and zero sessions | a writer stopped emitting `session`: the instrument is dead, not the week quiet |
| a `log_truncated` row early in the week | the file passed 1 MiB; data before it is gone, and the reader should say so |
| opencode rows missing | the plugin's log writes are failing; `offload_plugin_status` reports the count |

## Source map

| piece | where |
|---|---|
| the opencode writer and the `DispatchEvent` type | `integrations/opencode/src/instrument.ts` |
| the opencode call sites | `integrations/opencode/src/plugin.ts` (`log({ event: ... })`) |
| the hooks' writer, rotation and event emitters | the Claude Code configuration's `hooks/` (separate repository) |
| the reader | the Claude Code configuration's `scripts/scoreboard.js` (separate repository) |
| the doc test | `dispatch_log_contract_docs_test.go` (this page against the plugin's event type, and the links to it) |
