# NIM research analyst (overnight sweeps)

## Purpose

The usage pattern for pushing bulk, low-judgment RESEARCH work — long-document synthesis,
option comparison matrices, literature triage, draft analyses — through the explicit remote
NIM surface (`offload_nim` MCP tool / `local-offload nim` CLI) during autonomous overnight
runs, so that work costs neither session tokens nor local GPU time. This is a usage/config
pattern, not a code path: everything it uses already exists (`internal/nimclient`, the `nim`
command, the `offload_nim` tool).

## Trigger

An autonomous run (nightshift or similar) needs an LLM pass over research material that is
(a) mechanical enough to delegate — summarize/compare/enumerate, not decide — and (b) too
large or too numerous to burn session context on, and (c) NOT servable by the local cascade
(over the local context window, or the local seats are busy with render/eval work the run
needs them for).

## Participants

`offload_nim` (MCP) or `local-offload nim` (CLI); `internal/nimclient` (the one remote
client); the configured `nim_endpoint`/`nim_model`/`nim_max_tokens`/`nim_timeout_sec`;
`$NVIDIA_API_KEY` or `$NGC_API_KEY` (env only, never config).

## The pattern

1. **Chunk at the source.** One document / one comparison axis per call. Per-call failure
   isolation (and the `nim_timeout_sec` bound, default 120 s) argues for many small calls
   over one giant one.
2. **Instruct for structure.** Ask for the exact shape the run will consume ("reply with
   ONLY a JSON object {...}" / a fixed markdown table). Free-form prose answers cost a
   second pass to digest. For reasoning models, leave `--max-tokens` headroom — thinking
   tokens bill against the same budget, and a truncated reply arrives with
   `truncated: true` in the `--json` output (always present in the MCP result; check it —
   a cut-off answer parses as plausible prose).
3. **Fan out, then synthesize locally.** N chunk-calls to NIM, then ONE synthesis step in
   the session (or a final `nim` call over the collected structured outputs). The judgment
   step — what the research MEANS for the decision at hand — stays with the session model;
   NIM produces material, not verdicts.
4. **Write results to disk as they arrive** (the run's notes/ledger dir), not into session
   context. The session reads back only the structured synthesis.

```
# CLI shape (per chunk; --json to capture telemetry + truncation flag)
local-offload nim paper-section.md --system "Reply with ONLY a JSON object {claims: [...], evidence: [...], relevance_to: <topic>}" --max-tokens 2000 --json

# Browse the catalog when picking a sweep model
local-offload nim --list-models
```

## Constraints (these are the rules, not advice)

- **Content leaves the box.** `offload_nim` is the harness's only remote surface and it is
  explicit/opt-in ([mcp-server.md](../systems/mcp-server.md), ADR 0001's defer-never-cloud
  rule is about the CASCADE — this surface is exempt precisely because it is explicit).
  Dev/research content is operator-accepted; brand-sensitive or private material is NOT,
  absent explicit authorization for that material.
- **Never in the savings ledger.** NIM calls are deliberate experiments/escalations, not
  defer-avoidance; nothing here touches savings accounting.
- **Key from env only** (`NVIDIA_API_KEY`/`NGC_API_KEY`); `nimclient.KeyForBase` refuses to
  transmit it to non-NVIDIA bases. A self-hosted NIM via `--base` is keyless.
- **Not for judgment calls.** Seat decisions, plan verdicts, anything the operator will act
  on — the session model owns those. NIM output is input material, always attributed
  ("per the NIM sweep, unverified") until the session verifies it.

## Failure modes

A transient 5xx from the hosted catalog is normal overnight — retry once, then record the
chunk as unswept and move on (the sweep is additive; a missing chunk is visible in the
synthesis, a silently dropped one is not). `truncated: true` means raise `--max-tokens` or
pick a non-reasoning model, not re-ask the same call. An empty `content` with populated
`reasoning_content` is the reasoning-model budget failure — same remedy.

## Success behavior

The run's notes dir holds one structured artifact per chunk + one synthesis; the session
context carried only the synthesis; the local seats stayed free for render/eval work.

## Source map

- `internal/nimclient/nimclient.go` — the one remote client (`Chat`, `ListModels`,
  `KeyForBase`'s key-scoping rule)
- `main.go` `runNim` — the `nim` CLI command (`--json` telemetry incl. the `truncated` flag)
- `internal/mcpserver/mcpserver.go` — the `offload_nim` MCP tool registration
- `internal/config/config.go` — `nim_endpoint` / `nim_model` / `nim_max_tokens` /
  `nim_timeout_sec`
