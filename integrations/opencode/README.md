# opencode-local-offload

Full [local-offload harness](https://github.com/dmmdea/offload-harness) support inside
[opencode](https://opencode.ai) — feature parity with the Claude Code integration, plus the one
lever Claude Code cannot offer: the dispatch protocol lands in the **system prompt of the very turn
that composes a fan-out**, not in a hook that fires after the burst is already decided.

## What it does

| Surface | Hook | Behavior |
|---|---|---|
| Plan-time protocol | `experimental.chat.system.transform` | Appends the three-lane dispatch protocol + harness tool map to every turn |
| Task routing | `tool.definition` | The built-in `task` description names the offload route |
| **Forcing function** | `tool.execute.before` (`task`) | Read-only-shaped subagent legs are rerouted to the `offload` subagent (free local seat). Judgment and network legs are never touched. Option-gated. |
| H14 nudge | `tool.execute.after` | Read counter (12 / 40) appends the offload nudge; silent once the session delegates |
| Placement digest | `tool.execute.after` (`<mcp>_agent_delegate`) | States whether the local+server pair landed, flags `infrastructure`, counts defers |
| Parity provisioning | `config` | Idempotently provides the `offload` agent, `/offload-recon` `/offload-digest` `/offload-pair`, and a local `small_model` |
| Instrument | `event` + hooks | Appends to `~/.claude/state/dispatch-log.jsonl` tagged `harness:"opencode"` — one adherence read across both harnesses |
| Doctor | tool `offload_plugin_status` | Load proof: version, options, per-session counters |

The classifier is a verbatim port of the Claude Code H15 hook's measured vocabularies (17/17
read-only catch, 0/12 judgment false positives on the fixture corpus). The harness tools
themselves come from the harness MCP server registered in `opencode.jsonc` (`mcp.harness`) — the
plugin never re-wraps them.

## Install

This directory is the harness's opencode integration path — it lives, ships and is versioned
WITH the harness (one harness for every agent; never a separate package to keep in sync).

1. Register the harness MCP in `~/.config/opencode/opencode.jsonc` (see `examples/opencode.jsonc`).
2. `bun install` in this directory (the harness checkout).
3. Drop a one-line loader in `~/.config/opencode/plugins/opencode-local-offload.ts` pointing at
   this checkout:
   ```ts
   export { LocalOffloadPlugin, default } from "<abs path to offload-harness>/integrations/opencode/src/plugin.ts";
   ```
4. Options (optional) via `OPENCODE_LOCAL_OFFLOAD_OPTIONS` (JSON), e.g. `{"routeReadOnlyTasks":false}`.

Options: `mcp` (default `harness`), `offloadAgent` (`offload`), `offloadModel`
(`llamacpp/qwen3.8-27b`), `smallModel` (`llamacpp/gemma-4-e4b`), `routeReadOnlyTasks` (true),
`systemProtocol` (true), `nudges` (true), `readNudgeTiers` ([12,40]), `dispatchLog`.

## Verify

Ask opencode: *"Call the offload_plugin_status tool"* → JSON with `plugin`, `version`, hooks.
`/offload-recon <question>` runs on the offload seat. A read-only `task` shows
`[local-offload] This leg ran on the free local "offload" seat` in its result.

## Develop

`bun run typecheck` · `bun test` (bun:test, hooks driven directly with synthetic inputs).

Apache-2.0.
