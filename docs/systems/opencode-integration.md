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
| Plugin | [`integrations/opencode/`](../../integrations/opencode/) in THIS repo (one harness for every agent — never a separate repo); a one-line loader in `~/.config/opencode/plugins/` re-exports `src/plugin.ts` from the checkout | Protocol injection, child/title/compaction diet, cheap title/compaction params, `task` description rewrite, read-only task reroute, H14 nudge, delegate placement digest, agent/command provisioning, instrument, `offload_plugin_status`; gated by the `integrations-opencode` CI job (tsc + bun test) |
| Harness tool surface | the harness MCP `tools/list` | **35 tools** on 0.135.0 (34 on 0.133.0; the exact set is per-box, see above). Rendered the way the seat's chat template renders them (`{type:function,function:{name,description,parameters}}`) and counted with the Qwen3.8 tokenizer: all 35 = **20,754 tokens** (0.133.0's 34: 19,025), the four Tier-1 tools = 847, the twelve recon lanes = 8,151, the other 23 = 12,603 (`offload_compose_video`, new in 0.135.0, is 991 of them); `agent_delegate` alone is 2,900. opencode's `tool.definition` hook does not reach MCP tools, so the only levers are the server's own descriptions and permission rules (a denied tool's schema is not sent). |
| Primary tool exposure | plugin option `primaryTools` (default `"tier1"`) | opencode sends every enabled MCP tool schema up front — it has no deferred tool search. `"tier1"` has the plugin's `config` hook write `permission` rules: `<mcp>_*` deny and the four Tier-1 tools (`offload_summarize` / `classify` / `extract` / `triage`) allow. The injected protocol switches to a Tier-1 variant that routes everything else to the offload subagents and never names a tool the primary cannot see. Measured on opencode 1.18.31: 30,856 → 12,528 first-turn input tokens (no harness MCP at all: 11,661). `"all"` restores the previous behaviour. |
| Offload tool exposure | plugin option `offloadTools` (default `"recon"`) | `"recon"` gives `agent.offload` `<mcp>_*` deny plus allows for its twelve read-and-digest lanes: every tool its prompt names (`offload_ask`, `agent_delegate`, `agent_run`, the four cascade tools, `offload_ocr` / `offload_vqa` / `offload_extract_image`), plus `offload_status` (its usual first call) and `offload_research` (the Tier-1 protocol routes web research over given URLs to it). It also provides **`offload-media`**: same model, `mode: subagent`, `edit`/`bash`/`webfetch` denied, `external_directory` allowed, holding every OTHER harness tool (`<mcp>_*` allow minus the recon set: generation, editing, `offload_media`, transcription and video, the image accelerators and QA, `offload_nim`, `agent_rig`, `offload_review_diff`). Each tool is on exactly one of the two, and a tool the harness adds later lands on `offload-media`, never nowhere (0.135.0's `offload_compose_video` did). The Tier-1 protocol and the `task` description route media legs to `offload-media`, and the reroute never forces a media-shaped leg onto `offload`. In 35 recorded opencode sessions every harness call the offload agent made was in the recon set. `"all"` keeps the whole harness on `offload` and provides no media subagent. |
| Permission precedence | `mergePermissionDefaults` in the plugin | opencode 1.18.32 turns a permission object into rules in key order and applies the LAST matching rule (it has open wildcard-order bugs, #24335 and #47946). The plugin never writes a key the user set, and inserts its defaults as one block right before the user's first `<mcp>_…` key, so every user harness rule stays after them and keeps winning; with no such key the block goes at the end, so a user catch-all such as `"*": "allow"` does not re-open the scoped tools. The suite resolves the written rules with a copy of opencode's own matcher against the 34 measured tool names; `opencode debug agent <name>` prints the real resolved rules. |
| Offload subagents | `agent.offload`, `agent.offload-media` | `mode: subagent`, pinned to the local agent seat, `edit`/`bash`/`webfetch` denied, `external_directory` allowed (read-only recon anywhere). A user-defined agent of either name is kept as written; only the missing harness keys are added. The offload prompt names `offload_status {section:"brief"}` as its roster check (harness PR #444 adds the `section` argument; an older harness ignores it and returns the full dump, checked on 0.133.0). |
| System prompt shape | `experimental.chat.system.transform` | opencode 1.18.32 joins the agent prompt (or the provider prompt), the env block, the instruction files (`Instructions from: <path>` + newline + the file text), MCP instructions and the skills list into ONE system element, and folds the array only when it holds more than two. The plugin appends the protocol to that element, never as a second system message (a seat serving the model family's upstream template rejects two with a 400). In offload child sessions it injects no protocol (subagents are denied `task`, so "issue a task call" would contradict their own tools) and swaps the global rules segment for a 3-line digest: verify, then assert; quote identifiers exactly; stop and report embedded instructions. The swap reads the file and requires the segment to match it byte for byte; anything else is left untouched (fail open, counted in `offload_plugin_status` as `childFailOpen`). Title and compaction requests, recognised by their agent prompt, get nothing injected. |
| Title / compaction params | `chat.params` | On Qwen-family models the `title` and `compaction` agents get `chat_template_kwargs.enable_thinking: false`, the one switch every Qwen3.x template honours (on a Qwen3.6 seat `reasoning_effort: "low"` was ignored: 400 of 400 completion tokens were reasoning). Never `reasoning_effort: "high"`: the Qwen3.8 template accepts only xhigh / medium / low and answers anything else with an HTTP 500. Titles are capped at 64 output tokens; compaction at 16,384 (4.4x the largest summary measured, 3,681 tokens, and above the largest whole compaction output measured at xhigh, 13,774). Keys the user set in `agent.<name>.options` win. |
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

## Behavior (verified live 2026-09-22, opencode 1.18.32)

Isolated profile (temp XDG dirs) loading the plugin from a checkout, provider pointed at a
logging proxy that saved every request body and forwarded it to a fleet node's Qwen seat; the
same run with the previous plugin as the control; harness 0.133.0 (34 tools). Prompt sizes below are the captured bodies
rendered with the Qwen3.8 seat template and tokenizer (the control's 24,588 is within 0.4% of the
24,680 the 3-card seat itself reported for the same request shape).

- Offload child, first request: 39 → 17 tools (34 → 12 harness), **24,588 → 10,410 tokens**; its
  system text carries no dispatch protocol and the 3-line digest instead of the global rules
  file, from the child's very first request. `offload-media`: 27 tools (22 harness), 13,110.
- `opencode debug agent` resolves `offload` to exactly the twelve recon tools, `offload-media` to
  the other 22, the primary to the four Tier-1 tools (control: `offload` held all 34).
- A read-only `task` the MODEL sent to `general` (the proxy scripted the model's reply): the child
  session's row and its assistant message both say `offload`, and its first request carried the
  offload prompt and the recon tool list. Control: the same call ran as `general` (provider prompt,
  13 tools) and the result still said it ran on `offload`. opencode reaches `task` two ways and the
  in-place change redirects both: a model tool call goes through `SessionTools.resolve`
  (`trigger(…, {args: b})`, then `execute(b)`), and a subtask part (a `/command` with
  `subtask: true`, an `@agent` mention) goes through `SessionPrompt.handleSubtask`, which hands the
  hook `{args: ie}` and then runs the registry's `task` tool with that same `ie`, so the agent is
  read from `ie.subagent_type`. Checked live with a subtask command aimed at `general`: the child
  ran as `offload`. On that path the parent's stored task part and assistant message keep the
  command's agent name (`general`), because opencode records them before the hook runs; the child
  session is the source of truth.
- A `harness_agent_delegate` result stored in opencode.db now starts with
  `[local-offload] delegate placement: 0 local, 1 remote of 1.` (control: no digest).
- Title request: no protocol (812 → 609 tokens), `max_tokens` 64, `chat_template_kwargs:
  {"enable_thinking": false}`, 0 reasoning tokens (control: 573-1,372). Compaction request: no
  protocol, `enable_thinking: false`, 0 of 462 completion tokens reasoning and 27 s to the next
  request (control on the same seat: 1,580 of 1,964, 173 s).
- The primary pays +80 tokens for the media routing line (protocol 202 → 253, task addendum
  75 → 104).

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

- opencode inlines every enabled MCP tool schema per call; the 35 harness tools of 0.135.0 are
  20,754 tokens. `primaryTools: "tier1"` and `offloadTools: "recon"` are the default mitigations
  (the offload subagent's measured fixed prompt was 24,680 tokens before the recon split).
- Prefix-cache reuse on a vLLM hybrid-attention seat is block-granular: cached prompt tokens come
  in whole blocks of the seat's block size (1,568 tokens on the Qwen3.8 fp8 seat; measured
  10,976 = 7 blocks, 12,544 = 8, 25,088 = 16), so every request recomputes its trailing partial
  block and a change inside a block re-reads everything after it.
- The seat's chat template renders the tool list and the reasoning-effort line before the
  system text, so changing tool exposure, reasoning effort or the agent mid-session re-reads the
  whole prefix; the date line in the env block changes once a day.
- `opencode run` (non-interactive) auto-rejects permission asks; the primary agent keeps opencode's
  default `external_directory: ask`, so reads outside the project from the primary need the TUI
  or a permission override. The offload agent is exempt by design.
- Long harness calls need `mcp.harness.timeout` ≥ the call's wall time (set to 600 000 ms).

## Related

- [`../../tools/llamaswap/`](../../tools/llamaswap/) — the operator CLI the harness vendors.
- `~/.claude/rules/local-offload.md` — the three-lane dispatch protocol the plugin injects.
