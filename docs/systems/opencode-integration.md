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
| Context instrument | [`cmd/opencode-context`](../../cmd/opencode-context/) over [`internal/occontext`](../../internal/occontext/) | Read-only per-call token, cache, growth, compaction and TTFT report from a copy of opencode's session db — see [Measuring context](#measuring-context) |

## Behavior (verified live 2026-08-24, local primaries)

- `offload_plugin_status` called by a local model → `PLUGIN_OK 0.1.0`.
- `harness_offload_status` → endpoint + roster; the model quoted the injected protocol line verbatim.
- `harness_offload_summarize` on a real file → three summary points (cascade end-to-end).
- A `task` forced to `subagent_type "general"` with a read-only prompt → rerouted to `offload`
  (`task_reroute` row), which returned a line-numbered export inventory from the local 27B seat.
- `/offload-recon …` → `harness_agent_run` completed (~3.5 min) with a correct analysis.
- `harness_agent_delegate` `route:"spread"` with two contracts → `succeeded: 2, infrastructure: 0`.

## Measuring context

`go run ./cmd/opencode-context` is the before/after gate for anything that changes what opencode
sends a seat (tool allowlists, the child-session diet, prune and compaction settings, the rules
file). It never opens opencode's database: it copies `opencode.db` with its `-wal` and `-shm` into a
temp dir (a live session keeps its newest rows in the WAL, so the main file alone is behind), copies
again when the source changed during the copy, requires `PRAGMA quick_check` = `ok` on the copy, and
deletes the copy on exit.

```
go run ./cmd/opencode-context --last 5 --cache-block 1568 --cache-valid-since qwen3.8-27b-vllm-3card=2026-09-22T17:31:00Z
go run ./cmd/opencode-context --session <ses_id> --json > before.json
```

The default db is `$XDG_DATA_HOME/opencode/opencode.db`, else `~/.local/share/opencode/opencode.db`
(on Windows `%USERPROFILE%\.local\share\opencode\opencode.db`); `--db` overrides it. `--last N`
takes the N newest primary sessions with their child sessions; `--session`, `--since` and `--model`
narrow further.

Per session (primary, or child through `session.parent_id` — never through the agent name) and per
agent it reports:

| Figure | How |
|---|---|
| Per-call prompt / cached / output / reasoning | The message's `tokens`: one assistant message is one LLM call; prompt = `input + cache.read + cache.write`; opencode's `output` excludes `reasoning`. A zero-token assistant message (the subtask launcher, an abort) is a stub, not a call. When a server reports reasoning 0 but stored reasoning text (llama.cpp counts it inside output), the row shows a `~` byte estimate beside the server's figures |
| First-call prompt | The first call per session and agent: the fixed prefix (system prompt, rules, tool schemas) |
| Cache-read % | cached / prompt over calls whose cache figures count: overall; excluding each session's first call; excluding only cold first calls (cached 0), since a warm first call is a cross-session prefix hit |
| Growth split | prompt(n) − prompt(n−1) = the previous call's reasoning and output (server counts, replayed into history when the chat template preserves thinking) + tool outputs + user text (byte estimates: `--tool-bytes-per-token 2.7`, `--text-bytes-per-token 3.5`, calibrated on the Qwen3.8 tokenizer over 84 tool outputs and 83 reasoning texts) + a residual (template wrapping, attachments, estimate error). Pairs across a compaction or a model switch are skipped. Session and group lines give each category's share, so "reasoning's share of growth" and "tool-output share" read off directly |
| Compaction events | The `compaction` agent's summary call: the prompt and total before it (the total is what opencode compares with the usable window), the summary call's prompt, output, reasoning and wall time, and the prompt of the call after it |
| TTFT | First reasoning or text part start − message created; it includes a cold seat load after an idle unload |
| Model | provider/model per call |

Two server facts shape the cache figures; both are parameters, not constants:

- `--cache-valid-since [MODEL=]TIME` (repeatable). A server that does not return
  `prompt_tokens_details` logs cached 0 even on a hit. The blackwell-3x16 3-card seat began reporting
  cached tokens only after `enable_prompt_tokens_details` (`--enable-prompt-tokens-details`) went live,
  at 2026-09-22 17:31 UTC; its earlier rows are reporting artifacts. Calls before a cutoff stay listed
  (marked `*`) but leave every ratio. With no cutoff, the report prints a hint when a model's warm calls
  logged 0 before its first cache hit, with the interval the cutoff lies in.
- `--cache-block N`. vLLM caches whole blocks, so cached counts are multiples of the block (1,568
  tokens on the 3-card seat: the hybrid model's unified block at fp8 KV) and every call recomputes its
  trailing partial block. A cached count that is not a whole number of blocks is flagged: the block is
  wrong for that seat.

The report carries ids, counts and times only — no session text or tool output — so it is safe to
paste; `--titles` adds session titles.

Baseline read by the tool from the 2026-09-22 db (opencode 1.18.32, 3-card seat), matching the hand
audit it replaces: main-agent first call 11,773 and 12,265 tokens; offload subagent first call 24,680
(24,639–24,800 across four sessions); cache 66.0% of prompt tokens overall and 92.2% excluding the
cold first calls (93.0% excluding every session's first call — one of them was a warm cross-session
hit); every cached count a whole number of 1,568-token blocks.

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
