# opencode integration

## Purpose

Full local-offload harness support inside [opencode](https://opencode.ai) — feature parity with the
Claude Code integration (MCP tools, house rules, decision-point nudges, the dispatch instrument)
plus one lever Claude Code cannot offer: the three-lane dispatch protocol is injected into the
**system prompt of the turn that composes a fan-out**, and read-only subagent legs are
**rerouted automatically** to a free local seat.

## Source map

| Piece | Where | What |
|---|---|---|
| Harness MCP registration | `~/.config/opencode/opencode.jsonc` → `mcp.harness` | The SAME launch as Claude Code (`local-offload.exe mcp --config …`); every registered harness tool appears as `harness_<tool>` (the exact set is per-box: `agent_delegate` is gated on `agent_delegation_enabled` and the accelerator tools on the box listing the device, so do not hard-code a count); `timeout` raised to 600 s so `agent_run` / `agent_delegate` / `offload_ask` / `offload_review_diff` / media renders complete — `offload_ask` and `offload_review_diff` both inherit `core.AgentTimeoutSecDefault` (300 s) and measure 30-90 s in practice |
| House rules | `~/.config/opencode/AGENTS.md` | First in opencode's global rules list, so it **replaces** the `~/.claude/CLAUDE.md` fallback — intended on small-context local models, where that fallback carries Claude-only material. Keep one canonical rules file and sync the copy. Set `OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1` too: otherwise every skill under `~/.claude/skills` is listed (measured 199 skills, ~20.5k tokens, 31% of a 64k window) |
| Plugin | [`integrations/opencode/`](../../integrations/opencode/) in THIS repo (one harness for every agent — never a separate repo); a one-line loader in `~/.config/opencode/plugins/` re-exports `src/plugin.ts` from the checkout | Protocol injection, `task` description rewrite, read-only task reroute, H14 nudge, delegate placement digest, agent/command provisioning, instrument, `offload_plugin_status`; gated by the `integrations-opencode` CI job (tsc + bun test) |
| Primary tool exposure | plugin option `primaryTools` (default `"tier1"`) | opencode sends every enabled MCP tool schema up front — it has no deferred tool search — so all ~25 harness tools cost a 64k local primary ~18k tokens every session. `"tier1"` has the plugin's `config` hook write `permission` rules: `<mcp>_*` deny, the four Tier-1 tools (`offload_summarize` / `classify` / `extract` / `triage`) allow, and `<mcp>_*` allow on the offload subagent, which keeps the whole harness. Keys the user already set are never touched. The injected protocol switches to a Tier-1 variant that routes everything else to the offload subagent and never names a tool the primary cannot see. Measured on opencode 1.18.31: 30,856 → 12,528 first-turn input tokens (no harness MCP at all: 11,661). `"all"` restores the previous behaviour. |
| Offload subagent | `agent.offload` | `mode: subagent`, pinned to the local agent seat, `edit`/`bash`/`webfetch` denied, `external_directory` allowed (read-only recon anywhere) |
| Commands | `/offload-recon` `/offload-digest` `/offload-pair` | `subtask: true` on the offload agent |
| Instrument | `~/.claude/state/dispatch-log.jsonl` | Rows tagged `harness:"opencode"` — one adherence read across both harnesses; the Claude Code hooks own the file's rotation, the plugin is append-only |

## Behavior (verified live 2026-08-24, local primaries)

- `offload_plugin_status` called by a local model → `PLUGIN_OK 0.1.0`.
- `harness_offload_status` → endpoint + roster; the model quoted the injected protocol line verbatim.
- `harness_offload_summarize` on a real file → three summary points (cascade end-to-end).
- A `task` forced to `subagent_type "general"` with a read-only prompt → rerouted to `offload`
  (`task_reroute` row), which returned a line-numbered export inventory from the local 27B seat.
- `/offload-recon …` → `harness_agent_run` completed (~3.5 min) with a correct analysis.
- `harness_agent_delegate` `route:"spread"` with two contracts → `succeeded: 2, infrastructure: 0`.

## Caveats

- opencode inlines every enabled MCP tool schema per turn; the 24 harness tools are a context
  tax on small local models. Mitigation available: `enabled: false` globally + enable on the
  `offload` agent only — the plugin still routes read-only legs there.
- `opencode run` (non-interactive) auto-rejects permission asks; the primary agent keeps opencode's
  default `external_directory: ask`, so reads outside the project from the primary need the TUI
  or a permission override. The offload agent is exempt by design.
- Long harness calls need `mcp.harness.timeout` ≥ the call's wall time (set to 600 000 ms).

## Related

- [`../../tools/llamaswap/`](../../tools/llamaswap/) — the operator CLI the harness vendors.
- `~/.claude/rules/local-offload.md` — the three-lane dispatch protocol the plugin injects.
