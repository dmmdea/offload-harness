---
status: Accepted
date: "2026-09-15"
---

# 0046 — Ledger rows carry their origin and their job

Release: 0.124.0 (register D-101)

## Context

The savings ledger (`ledger.jsonl`) is one append-only file per box, written by every process that
runs the harness on it: each Claude Code session's MCP server, the CLI, the fleet node. Its rows said
WHAT ran (`task`, `model_tier`, tokens, latency, defer reason) and never WHO asked.

That was fine while the ledger was a savings report. It stopped being fine when the ledger became a
gate: the operator's harness-share floor (at least 30 % of a session's tokens through the cards) is
enforced by a Claude Code hook that sums this ledger over the session's time window. With no origin on
the rows, the hook summed EVERY row in the window, whoever wrote it. Measured on 2026-09-15 (a
live-usage report from a concurrent session): a session that made five harness calls read 26.5 → 35 %
"through the cards" on 259 rows of which roughly six were its own; the session doing the delegating read
its own figure diluted by the other's cloud tokens. The floor rewarded a session for its neighbour's
work and could block a session that had routed everything it had. Nothing about the floor was honest
until rows said who asked.

The same report listed what a row could not answer without opening the delegation-log corpus: which
job, how many steps, why it stopped, how long the structured re-pack took, whether acceptance passed.
And it noted that a delegate row keeps its prompt work in `seat_tokens_in` while `tokens_in` stays 0
(on purpose — `tokens_in` doubles as the savings column and a delegation must not double-count the
node-side agent row), so every reader reconstructed "tokens through the cards" from three columns and
an `input_chars / 4` guess.

## Decision

1. **`ledger.Record` stamps provenance on every row it writes.** `origin_session`, `origin_pid`,
   `origin_ppid`, resolved ONCE per process (`ledger.ProcessOrigin`): `LOCAL_OFFLOAD_ORIGIN` when a
   caller names itself, else `CLAUDE_CODE_SESSION_ID`, else empty. Claude Code exports that variable
   to every child process, MCP servers included (verified on three live servers on the reference
   workstation), and one MCP server is one session — so the session id is free at record time. The
   label is bounded the way the dispatch tenant is (printable ASCII, ≤ 96 bytes, else anonymous). A
   caller that sets its own origin on the entry keeps it. Stamping in `Record`, not at the record
   sites, is what makes the rule universal: the cascade, the agent loop, the delegate runner, the media
   lanes and any future writer are attributed by one line.
2. **One token figure per row: `cards_tokens`**, computed in `Record` when the caller left it 0: the
   seat's prompt work (`seat_tokens_in` on agent rows, `tokens_in` on cascade rows — the same
   measurement under two names) plus `tokens_out`; 0 on a cache hit (no card work) and on a defer that
   never reached a model; the chars/4 estimate only for a completed render that recorded no counts.
   The key is ALWAYS written, so its presence marks a row of this schema and a reader can stop
   reconstructing. `tokens_in` / `seat_tokens_in` keep their meanings and the savings summary is
   unchanged.
3. **The job rides on the row.** Delegate rows carry `job_id`, `route`, `placement` (the placement
   note, capped like `reason`), `steps`, `stop_reason`, `repack_ms`, `acceptance_result`
   (`pass` | `fail` | empty when nothing was evaluated — a deferred row must not read as a failed
   check). Agent rows carry `job_id`, `steps`, `stop_reason`, `repack_ms` through four new omitempty
   fields on `core.Meta`; vision rows carry the route's `placement`.
4. **Pre-0.124.0 rows read as UNATTRIBUTED, never as "some other session".** All new fields but
   `cards_tokens` are omitempty; old lines parse unchanged. The share hook (claude-config
   `harness-share-gate.js`) enforces the SESSION figure only when the window carries evidence the
   writers stamp origins — a row of the session's own, or a window in which every row names a session —
   and otherwise reports the fleet figure and says so. The transition (an MCP server still running the
   old binary next to new ones) is therefore visible, not silently wrong in either direction.

## Consequences

- The floor is attributable per session: `grep '"origin_session":"<id>"' ledger.jsonl` is the
  session's own ledger; the hook prints the session figure and the fleet figure side by side.
- A node's own rows carry pids and no session (a service inherits no Claude environment). Propagating
  the session through the dispatch envelope to the node's rows is a separate, additive change (a header,
  like the tenant — the envelope is a strict decoder) and is not part of this decision.
- Rows grow by ~60–120 bytes; they stay one small O_APPEND-atomic line.
- A process that changes its environment after start keeps its first origin; that is the intended
  reading (one process, one session), and a wrapper that needs to name itself sets
  `LOCAL_OFFLOAD_ORIGIN` before exec.

## Alternatives considered

- **Process-tree ancestry** (record `origin_pid`/`origin_ppid`, have the hook walk to the session's
  root process): works, but the hook runs under a shell of its own and the walk crosses `cmd.exe` /
  `pwsh.exe` layers that differ per host; the environment variable is the same fact with no walk. The
  pids are recorded anyway for a reader that groups by process.
- **A session id injected by the hook** (`MCP_SESSION_ID`): the hook does not spawn the MCP server;
  Claude Code does, and already exports the id.
- **Reusing the dispatch tenant** (`host-pid-start`): identifies the process, not the session, and
  its job is queue fairness on the node — a different contract with a different consumer.
