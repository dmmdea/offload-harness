# opencode-local-offload

Full [local-offload harness](https://github.com/dmmdea/offload-harness) support inside
[opencode](https://opencode.ai) — feature parity with the Claude Code integration, plus the one
lever Claude Code cannot offer: the dispatch protocol lands in the **system prompt of the very turn
that composes a fan-out**, not in a hook that fires after the burst is already decided.

## What it does

| Surface | Hook | Behavior |
|---|---|---|
| Plan-time protocol | `experimental.chat.system.transform` | Appends the dispatch protocol + harness tool map to every primary turn (into the one system element opencode sends) |
| Child / title / compaction diet | `experimental.chat.system.transform` | Offload child sessions get no dispatch protocol (subagents cannot call `task`) and a 3-line read-only digest instead of the global rules file; title and compaction requests get nothing injected. Fails open: an unrecognised format is left as it is |
| Cheap title / compaction | `chat.params` | On Qwen-family models, title and compaction requests run with `chat_template_kwargs.enable_thinking: false`; titles are capped at 64 output tokens, compaction at 16,384. Keys the user set are never overwritten |
| Task routing | `tool.definition` | The built-in `task` description names the offload route |
| **Forcing function** | `tool.execute.before` (`task`) | Read-only-shaped subagent legs are rerouted to the `offload` subagent (free local seat), by changing the call's arguments in place. Judgment, network and (with `offloadTools: "recon"`) media-shaped legs are never touched. Option-gated. |
| H14 nudge | `tool.execute.after` | Read counter (12 / 40) appends the offload nudge; silent once the session delegates |
| Placement digest | `tool.execute.after` (`<mcp>_agent_delegate`) | States whether the local+server pair landed, flags `infrastructure`, counts defers. Added as the first text part of the MCP result, which is what opencode renders |
| Reroute note | `tool.execute.after` (`task`) | "This leg ran on the free local offload seat" — only when the child session the task created really is an `offload` session |
| Parity provisioning | `config` | Idempotently provides the `offload` agent (and `offload-media` with `offloadTools: "recon"`), `/offload-recon` `/offload-digest` `/offload-pair`, a local `small_model`, and the tool-scope `permission` rules |
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

Options:

| Option | Default | Meaning |
|---|---|---|
| `mcp` | `harness` | MCP server name the harness is registered under (tool prefix `<mcp>_`) |
| `offloadAgent` | `offload` | The read-only offload subagent; the media subagent is `<offloadAgent>-media` |
| `offloadModel` | `llamacpp/qwen3.8-27b` | Model for the offload subagents |
| `smallModel` | `llamacpp/gemma-4-e4b` | `small_model` applied when the config has none |
| `primaryTools` | `tier1` | Harness tools the PRIMARY agent sees: `tier1` = the four mechanical-text tools (`offload_summarize` / `classify` / `extract` / `triage`), everything else through the offload subagents; `all` = every harness tool |
| `offloadTools` | `recon` | Harness tools the OFFLOAD subagent sees: `recon` = the twelve read-and-digest lanes (`agent_delegate`, `agent_run`, `offload_ask`, `offload_status`, `offload_research`, the four cascade tools, `offload_ocr`, `offload_vqa`, `offload_extract_image`) and an `offload-media` subagent holding every other harness tool; `all` = the whole harness on `offload`, no media subagent |
| `routeReadOnlyTasks` | `true` | Reroute read-only-shaped `task` legs to the offload subagent |
| `systemProtocol` | `true` | Inject the dispatch protocol on primary turns |
| `nudges` / `readNudgeTiers` | `true` / `[12,40]` | H14-style read-counter nudges |
| `dispatchLog` | `~/.claude/state/dispatch-log.jsonl` | Cross-harness dispatch instrument |

The `permission` rules the plugin writes never change a key you set, and never shadow one: they
are inserted before your first `<mcp>_…` key, so your own harness rules keep winning (opencode
applies the last matching rule).

## Verify

Ask opencode: *"Call the offload_plugin_status tool"* → JSON with `plugin`, `version`, hooks, and
`diagnostics`. Every failure path the plugin fails open on is counted there, so "clean" means
clean:

| Field | Non-zero means |
|---|---|
| `configStepFailed[]` | a `config` step threw (`"<step>: <message>"`); every other step still ran. A `tier1Permissions` or `offloadToolScopes` entry means a tool scope was not applied |
| `permissionRejected[]` | a `permission` value is not an object (e.g. `"allow"`), so the plugin could not scope it; it was left as written |
| `systemTransform.protocol` / `child` / `childDigest` / `aux` | primary requests that got the protocol; offload child requests; child requests whose global rules file became the digest; title/compaction requests left untouched |
| `systemTransform.childFailOpen` | the rules segment did not match the file on disk (format or content drift); the child kept the full rules |
| `systemTransform.childHeaderMissing` | an offload child had no global `Instructions from:` header at all (no global rules file, or a changed header format) |
| `systemTransform.unknownSession` | a request came from a session no `session.created` event announced — the event arrived late or was missed, so child detection could not apply |
| `chatParams.applied` / `skippedNotAux` / `skippedNotQwen` / `skippedUserSet` | title/compaction requests given `enable_thinking: false`; requests skipped because they are not title/compaction, not a Qwen-family model, or the user set the kwargs |
| `auxAgreement.title` / `.compaction` `{byPrompt, byAgent}` | the same requests seen by prompt text (system transform) and by agent name (`chat.params`); `byPrompt < byAgent` means opencode reworded the prompt and the protocol is being injected into those requests again |
| `instrument.failures` | dispatch-log writes that failed |

`/offload-recon <question>` runs on the offload seat. A rerouted read-only `task` shows
`[local-offload] This leg ran on the free local "offload" seat` in its result — only when the child
session really is `offload`; a failure or an escalation is reported either way.
`opencode debug agent offload` prints the resolved permission rules.

## Develop

`bun run typecheck` · `bun test` (bun:test, hooks driven directly with synthetic inputs).

Apache-2.0.
