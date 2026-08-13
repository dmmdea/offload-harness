---
name: pp-llamaswap
description: "The llama-swap operations console — durable history, drain-aware control, and measurement commands with three specific guarantees: keep-set unloads are refused statically by id AND alias (never from server ttl), `--drain` fails closed when slot state is unreadable, and fit/ctx refuse to answer inside their uncertainty band instead of guessing. Trigger phrases: `what models are loaded`, `free up VRAM`, `unload a model safely`, `check llama-swap`, `is the memory stack ok`, `will this model fit`, `llama-swap history`, `use llamaswap`, `run llamaswap-pp-cli`."
author: "Daniel Martinez"
license: "Apache-2.0"
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  openclaw:
    requires:
      bins:
        - llamaswap-pp-cli
---

# llama-swap — Printing Press CLI

## Prerequisites: Install the CLI

This skill drives the `llamaswap-pp-cli` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via the Printing Press installer. It defaults binaries to `$HOME/.local/bin` on macOS/Linux and `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows:
   ```bash
   npx -y @mvanhorn/printing-press-library install llamaswap --cli-only
   ```
2. Verify: `llamaswap-pp-cli --version`
3. Ensure the reported install directory is on `$PATH` for the agent/runtime that will invoke this skill.

If the `npx` install fails before this CLI has a public-library category, install Node or use the category-specific Go fallback after publish.

If `--version` reports "command not found" after install, the runtime cannot see the binary directory on `$PATH`. Do not proceed with skill commands until verification succeeds.

llama-swap forgets everything on restart and ships no CLI at all. This one mirrors activity and events into local SQLite before they vanish (sync), refuses to unload the protected keep-set by id or alias (sourced from config, never from the server's ttl), fails closed rather than guessing when a drain cannot read slot state, prints config diffs and restart commands instead of ever rewriting your hand-commented YAML, and encodes the measurement traps — the seat's real `n_ctx` over the assumed one, tokenizer counts over chars/4 — into bench, fit, and ctx.

## When to Use This CLI

Reach for this CLI whenever a task touches the local llama-swap serving stack: checking what models are loaded or configured, safely freeing VRAM without killing in-flight work or the memory stack, validating or diffing the serving config before a restart, measuring throughput/VRAM/context fit for a seat change, or answering historical questions (swap costs, error rates, keep-set residency) from the local mirror. It replaces raw curl against :11436 and produces typed exit codes and stable JSON for unattended runs.

## Unique Capabilities

These capabilities aren't available in any other tool for this API.

### Local state that compounds
- **`keepset audit`** — See when the memory-stack models were actually resident, every eviction attributed to its cause, and how much embed/rerank traffic landed inside a degraded window.

  _Reach for this after any restart or seat change to prove the memory stack never silently degraded._

  ```bash
  llamaswap-pp-cli keepset audit --since 7d --json
  ```
- **`seat log`** — A per-model chronology of every flag change mined from the dated config-backup series, with the YAML comments as the reasoning and the live cmd as the current truth.

  _Answers 'when did this seat get that flag and what else landed in the same edit' without opening 19 backups by hand._

  ```bash
  llamaswap-pp-cli seat log gemma-4-e4b --json
  ```
- **`sync`** — Pull the server's in-memory request activity and event stream into local SQLite before a restart destroys them, sealing each server epoch and quantifying what the ring buffer already dropped.

  _Run it before asking any historical question; every other analytics command reads what sync stored._

  ```bash
  llamaswap-pp-cli sync --json
  ```
- **`swaps`** — Cold-load percentiles per model, time lost to swapping, and which model pairs repeatedly evict each other.

  _Use it to decide seat groupings and TTLs from measured cost instead of intuition._

  ```bash
  llamaswap-pp-cli swaps --thrash --json
  ```

### Guardrails with teeth
- **`verify`** — Embeds a fixed probe on the embedder and reranks a fixed pair on the reranker, asserting both against stored calibrated baselines — catching dropped flags that roster counting cannot.

  _Run after every config change or restart; it is the step of the manual ritual that has historically caught real outages._

  ```bash
  llamaswap-pp-cli verify --probe --json
  ```

## When NOT to Use This CLI

These are hard boundaries of the tool, not preferences. Do not attempt a workaround:

- **It never writes the llama-swap YAML.** `config apply` and `seat try` PLAN a change — unified diff, content-addressed backup, the exact restart command — and stop. Editing the hand-commented config is the operator's job.
- **It never restarts the service.** `service status` reads; when a restart is required the CLI prints the elevated command for you to run.
- **There is no `models add` / `models rm`.** Fragment mode is not enabled on this deployment, so the roster is only editable in the YAML.
- **It does not download models.** No pull, no fetch. `models list` is CONFIG-derived: a seat whose GGUF was never downloaded still appears and fails only when called.
- **It is not for ComfyUI or image/video generation.** That stack is owned by `comfyui-pp-cli`; this CLI only knows the llama-swap proxy on this box.

## Command Reference

Generated from the shipped binary's own command tree. Run `llamaswap-pp-cli <command> --help` for flags on any line.

### Operations — what is loaded, what happened, and what to do about it

- `llamaswap-pp-cli ps` — Models currently holding VRAM: NAME, STATE, CTX, TTL, PORT, UPTIME. TTL comes from the YAML, not the server.
- `llamaswap-pp-cli load` — Warm a model into VRAM on purpose, with progress and a swap report.
- `llamaswap-pp-cli kill` — Cancel in-flight requests by id or by model — the polite alternative to unloading mid-generation.
- `llamaswap-pp-cli models list` — Every CONFIGURED model (from config YAML), not just loaded ones. Per-entry keys: `id`, `object`, `created`, `owned_by`, `name`, `meta.llamaswap.aliases`, `status.value`. Match on `id` AND the aliases — an id-only check reports a correctly-served alias as missing.
- `llamaswap-pp-cli models running` — Only the models currently holding VRAM: {running:[{model, state, cmd, proxy, ttl, name, description}]}.
- `llamaswap-pp-cli models unload` — Unload ONE model by id or alias (keep-set protected, optionally drain-aware).
- `llamaswap-pp-cli models unload-all` — Unload every running model EXCEPT the keep-set, as one per-model unload each. The non-selective bulk route is used ONLY when no keep-set member is resident or `--force-keepset` was passed; otherwise it refuses and says why.
- `llamaswap-pp-cli keepset status` — Is each keep-set model resident, and is it actually answering?
- `llamaswap-pp-cli keepset audit` — See when the memory-stack models were actually resident, every eviction attributed to its cause. Requires accumulated `sync` history; a cold mirror reports UNKNOWN, not clean.
- `llamaswap-pp-cli sync` — Sync API data to local SQLite for offline search and analysis.
- `llamaswap-pp-cli swaps` — Cold-load percentiles per model, time lost to swapping, and which model pairs repeatedly evict each other. Requires accumulated `sync` history; a cold mirror reports UNKNOWN, not clean.
- `llamaswap-pp-cli events` — Consume the /api/events SSE stream as NDJSON — drain briefly by default, follow with `-f`.
- `llamaswap-pp-cli logs` — Buffered recent logs as plain text (proxy + upstream stdout/stderr interleaved). One-shot: there is no follow mode here — use `events -f`.
- `llamaswap-pp-cli logs triage` — Classify the buffered proxy log into the error taxonomy, with counts and buffer positions.
- `llamaswap-pp-cli captures` — Full request/response capture for one activity entry — the body-level diagnostic for "what did the client actually send".
- `llamaswap-pp-cli captures export` — Bulk-export request/response captures to JSONL — the eval/regression dataset the UI cannot produce. `--out` is required.
- `llamaswap-pp-cli activity log` — Per-request activity log: id, timestamp, model, HTTP status, cached/prompt/generated token counts, prompt speed.
- `llamaswap-pp-cli activity performance` — Performance metrics feed backing the UI's Performance page (per-model speed distributions).
- `llamaswap-pp-cli activity prometheus` — Prometheus exposition of proxy metrics.
- `llamaswap-pp-cli activity stats` — Aggregated activity stats (request counts, token totals) since proxy start.
- `llamaswap-pp-cli inflight` — Cancel one in-flight request by its id (ids visible in the activity feed / UI in-flight table).
- `llamaswap-pp-cli service status` — Is llama-swap up, which process is it, and what did the launcher last do.
- `llamaswap-pp-cli doctor` — Check CLI health.
- `llamaswap-pp-cli verify` — Post-restart verification: roster count, keep-set ANSWERING, and calibrated memory-stack probes. `--probe` compares against baselines that `verify --probe --init` must record ONCE first; without that calibration it exits 24, and exit 26 means the stack answered but DEGRADED.

### Config intelligence — read-only, never rewrites your YAML

- `llamaswap-pp-cli config validate` — Check a config against the embedded llama-swap JSON schema, plus nearest-key hints for unknown top-level keys.
- `llamaswap-pp-cli config lint` — Semantic checks llama-swap's own boot validation does not make: macros, aliases, ports, ttl semantics, missing files, routing coherence.
- `llamaswap-pp-cli config explain` — The fully resolved view of one seat: raw block with comments, macro-expanded cmd, aliases, ttl, env, seat kind.
- `llamaswap-pp-cli config diff` — Semantic diff between two configs: per-model added/removed/changed flags, with the comment blocks that changed alongside.
- `llamaswap-pp-cli config drift` — Compare every RUNNING seat's live command line against the config file, flag by flag.
- `llamaswap-pp-cli config backup` — Copy the live config to a content-addressed backup and record it in a sidecar index; report dedup and orphan backups.
- `llamaswap-pp-cli config apply` — Plan a config change: unified diff vs live, a content-addressed backup, the exact elevated restart command, and the post-restart verify plan. Never writes.
- `llamaswap-pp-cli config testinstance` — Boot a throwaway llama-swap on a scratch port, count the models it registers, then kill it.
- `llamaswap-pp-cli seat log` — A per-model chronology of every flag change mined from the dated config-backup series, with the comment that landed alongside each one.
- `llamaswap-pp-cli seat show` — One seat's live command line vs the file, flag by flag.
- `llamaswap-pp-cli seat try` — PLAN a seat flag change: the would-be command, a unified diff, the restart command, and an acceptance probe. Never writes.
- `llamaswap-pp-cli bind check` — Every model name a consuming tool is configured with must resolve in the live roster; report dangling bindings and unbound seats.

### Measurement — numbers with their traps encoded

- `llamaswap-pp-cli bench` — Benchmark seats through the production route, with the serving config identity attached.
- `llamaswap-pp-cli bench aux` — Latency and throughput for the resident embedder and reranker.
- `llamaswap-pp-cli vram` — Per-GPU-UUID VRAM snapshot with explicit baseline/after/delta.
- `llamaswap-pp-cli fit` — Will this model at this context fit the cards, as an interval with a refuse-to-answer band. Takes a loaded model id or a `.gguf` path; it never starts a model to answer.
- `llamaswap-pp-cli ctx` — Real tokens vs the seat's live n_ctx: room left, and KV cost at a target context.
- `llamaswap-pp-cli gguf` — Read a GGUF file's header: architecture, layers, GQA heads, native context, quantization.
- `llamaswap-pp-cli gate grammar` — Does this seat actually enforce a GBNF grammar on the chat route?
- `llamaswap-pp-cli gate tools` — Does this seat emit a well-formed tool call?
- `llamaswap-pp-cli scratch` — Run an ephemeral eval seat derived EXACTLY from a production command line.
- `llamaswap-pp-cli build check` — Proxy version plus the llama.cpp build actually serving each loaded seat.

### API passthrough — thin wrappers over the proxy's own routes

- `llamaswap-pp-cli server health` — Liveness of the llama-swap proxy itself (plain 'OK'). Says NOTHING about any model being loaded.
- `llamaswap-pp-cli server version` — Returns {version, commit, build_date}. The capability gate for everything else.
- `llamaswap-pp-cli server hardware` — Hardware detection as llama-swap sees it (GPUs, VRAM). 404 on pre-v247 servers.
- `llamaswap-pp-cli profiles list` — List configured profiles and which one is active. 404 on pre-v241 servers.
- `llamaswap-pp-cli profiles activate` — Switch the active profile. Body is the profile selection object.
- `llamaswap-pp-cli upstream props` — llama-server properties for one model: build_info, model_path, chat_template, modalities, total_slots.
- `llamaswap-pp-cli upstream slots` — Per-slot state incl. is_processing — the drain signal before unload.
- `llamaswap-pp-cli upstream health` — Per-model readiness ({'status':'ok'} once loaded; 503 while loading).
- `llamaswap-pp-cli upstream tokenize` — Tokenize text with the model's own tokenizer.
- `llamaswap-pp-cli upstream detokenize` — Convert token ids back to text.
- `llamaswap-pp-cli upstream apply-template` — Render OpenAI-style messages through the model's chat template WITHOUT generating — returns the exact prompt string.
- `llamaswap-pp-cli upstream embeddings` — Embeddings from an embedding-capable model (on this deployment embeddinggemma, 768-dim, resident).
- `llamaswap-pp-cli upstream rerank` — Rerank documents against a query with a reranker model (on this deployment bge-reranker-v2-m3, resident, CPU-bound).
- `llamaswap-pp-cli upstream metrics` — Per-model llama-server Prometheus metrics (needs --metrics on the seat; 404 otherwise).
- `llamaswap-pp-cli upstream lora-adapters` — List LoRA adapters loaded on this model and their scales.
- `llamaswap-pp-cli upstream lora-set` — Set LoRA adapter scales on this model.
- `llamaswap-pp-cli upstream open` — Print a model's llama.cpp passthrough URL — or open it in a browser with `--launch`.

**TRAP:** any `upstream` request AUTO-STARTS the target model if it is not running — a multi-GB load triggered by a "probe". Gate inspection on `ps` first.

### Learning and framework

- `llamaswap-pp-cli which` — Find the command that implements a capability.
- `llamaswap-pp-cli search` — Search locally synced data.
- `llamaswap-pp-cli analytics` — Run analytics queries on locally synced data.
- `llamaswap-pp-cli export` — Export data to JSONL or JSON for backup, migration, or analysis.
- `llamaswap-pp-cli import` — Import data from JSONL file via API create/upsert calls.
- `llamaswap-pp-cli recall` — Check prior learnings for a query before running discovery (LLM-fired, pre-discovery).
- `llamaswap-pp-cli teach` — Record a query -> resource mapping for future recall (LLM-fired, silent).
- `llamaswap-pp-cli teach-playbook` — Record a CLI playbook + free-text notes for a query family.
- `llamaswap-pp-cli teach-pattern` — Install a manual generalization pattern (query_template, resource_template, entity_kind).
- `llamaswap-pp-cli teach-lookup` — Install a manual entity-lookup row (kind, canonical, value).
- `llamaswap-pp-cli learnings list` — List recorded learnings.
- `llamaswap-pp-cli learnings candidates` — List auto-captured improvement candidates awaiting judgment.
- `llamaswap-pp-cli learnings confirm` — Confirm an open candidate: print its full payload, then materialize it.
- `llamaswap-pp-cli learnings reject` — Reject a candidate and tombstone its derivation signature.
- `llamaswap-pp-cli learnings forget` — Delete learnings matching a query (needs `--all`, `--resource`, or `--action`).
- `llamaswap-pp-cli learnings purge` — Delete settled candidate rows (expired and confirmed; `--tombstones` adds rejected).
- `llamaswap-pp-cli learnings stats` — Report learn-loop effectiveness metrics from the local events table.
- `llamaswap-pp-cli playbook list` — List stored playbooks (query_family, content presence, last observed).
- `llamaswap-pp-cli playbook amend` — Append a note to an existing playbook (LLM-fired self-correction, silent).
- `llamaswap-pp-cli profile save` — Save the current invocation's non-default flags as a named profile.
- `llamaswap-pp-cli profile list` — List saved profiles.
- `llamaswap-pp-cli profile show` — Show a profile's values as JSON.
- `llamaswap-pp-cli profile use` — Print the flag values a profile will apply (does not execute anything).
- `llamaswap-pp-cli profile delete` — Remove a profile.
- `llamaswap-pp-cli configure` — Emit ready-to-paste client configuration pointing at this llama-swap.


### Finding the right command

When you know what you want to do but not which command does it, ask the CLI directly:

```bash
llamaswap-pp-cli which "<capability in your own words>"
```

`which` resolves a natural-language capability query to the best matching command from this CLI's curated feature index. Exit code `0` means at least one match; exit code `2` means no confident match — fall back to `--help` or use a narrower query.

## Recipes

### Free VRAM safely before a render job

```bash
llamaswap-pp-cli models unload-all --drain --json
```

Drains in-flight work first and always excludes the protected keep-set, then confirms via /running.

### Audit a seat's request history without burning context

```bash
llamaswap-pp-cli activity log --model gemma-4-e4b --agent --select id,resp_status_code,tokens.output_tokens,duration_ms
```

The activity records are deeply nested; --select narrows to the four fields that matter for triage.

### Will this model fit at a bigger context?

```bash
llamaswap-pp-cli fit V:/models/gemma-4-31B-it-qat-GGUF/gemma-4-31B-it-qat-UD-Q4_K_XL.gguf --ctx 40960 --json
```

GGUF weights plus KV math (GQA-aware) against real per-card capacity net of the keep-set, answered as an interval. Takes a loaded model id or a GGUF path; the path form works for a model that is not currently loaded.

### Prove the memory stack survived a config change

```bash
llamaswap-pp-cli verify --probe --json
```

Asserts embedder cosine and reranker score against stored calibrated baselines — catches dropped flags roster checks miss. Requires a one-time calibration first: run 'llamaswap-pp-cli verify --probe --init' once on a known-good stack to record the baselines, or --probe exits 24 with no baseline to compare against.

### What did tonight's swaps cost?

```bash
llamaswap-pp-cli swaps --since 24h --json
```

Cold-load percentiles and eviction pairs from the local mirror, not from memory.

## Auth Setup

No authentication required.

Run `llamaswap-pp-cli doctor` to verify setup.

## Agent Mode

Add `--agent` to any command. Expands to: `--json --compact --no-input --no-color --yes`.

- **Pipeable** — JSON on stdout, errors on stderr
- **Filterable** — `--select` keeps a subset of fields. Dotted paths descend into nested structures; arrays traverse element-wise. Critical for keeping context small on verbose APIs:

  ```bash
  llamaswap-pp-cli captures --id 597 --agent
  ```

  Capture ids are the INTEGER activity-row ids, not UUIDs. List the ones that actually have a stored body first:

  ```bash
  llamaswap-pp-cli activity log --json --select id,has_capture
  ```
- **Previewable** — `--dry-run` shows the request without sending
- **Offline-friendly** — sync/search commands can use the local SQLite store when available
- **Non-interactive** — never prompts, every input is a flag
- **No write endpoints** — this API is read-and-control only, so the global `--idempotent` flag is inert here; there is nothing to re-create

### Response envelope

Commands that read from the local store or the API wrap output in a provenance envelope:

```json
{
  "meta": {"source": "live" | "local", "synced_at": "...", "reason": "..."},
  "results": <data>
}
```

Parse `.results` for data and `.meta.source` to know whether it's live or local. A human-readable `N results (live)` summary is printed to stderr only when stdout is a terminal AND no machine-format flag (`--json`, `--csv`, `--compact`, `--quiet`, `--plain`, `--select`) is set — piped/agent consumers and explicit-format runs get pure JSON on stdout.

## Paths and state

Agents should treat the CLI's path resolver as part of the runtime contract:

- Use `--home <dir>` for one invocation, or set `LLAMASWAP_HOME=<dir>` to relocate all four path kinds under one root.
- Use per-kind env vars only when a specific kind must diverge: `LLAMASWAP_CONFIG_DIR`, `LLAMASWAP_DATA_DIR`, `LLAMASWAP_STATE_DIR`, `LLAMASWAP_CACHE_DIR`.
- Resolution order is per-kind env var, `--home`, `LLAMASWAP_HOME`, XDG (`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`), then platform defaults.
- `config` contains settings like `config.json` and saved profiles. `data` contains `data.db` and the local mirror. `state` contains persisted queries, jobs, and `teach.log`. `cache` contains regenerable HTTP/cache files.
- Run `llamaswap-pp-cli doctor --fail-on warn` to surface path warnings. `agent-context` exposes a schema v4 `paths` block for agents that need the resolved dirs.
- For MCP, pass relocation through the MCP host config. The MCP binary does not inherit CLI flags:

  ```json
  {
    "mcpServers": {
      "llamaswap": {
        "command": "llamaswap-pp-mcp",
        "env": {
          "LLAMASWAP_HOME": "/srv/llamaswap"
        }
      }
    }
  }
  ```

Fleet precedence: an inherited per-kind env var overrides an explicit `--home` for that kind. Use `LLAMASWAP_HOME` or per-kind vars as durable fleet levers, and use `--home` only for a single invocation. Relocation is not reversible by unsetting env vars; move files manually before clearing `LLAMASWAP_HOME`, or `doctor` will not find the data left under the former root.

## Automatic learning

This CLI ships a self-capturing learning loop. The CLI does its own bookkeeping: every invocation is journaled locally, a failed flag followed by a corrected retry auto-derives a `flag_alias` candidate, and a `teach` on a query family without a playbook auto-synthesizes a `playbook_candidate` from the session's journal. Your job is judgment only: `recall` first, act on surfaced candidates, `teach` the final answer, `playbook amend` when you observe a correction. You never record failures by hand.

### Step 1: `recall` before any discovery

Before list/search/drill commands on a new user question, run:

```bash
llamaswap-pp-cli recall "<user's question>" --agent
```

The response envelope:

```json
{
  "query": "...",
  "normalized": "<normalized form>",
  "query_entities": ["..."],
  "found": true | false,
  "match_score": 0.0,
  "results": [
    { "resource_id": "...", "resource_type": "...", "venue": "...",
      "confidence": 2, "entity_match": "exact|partial|unknown",
      "source": "taught|preseed|pattern", "warnings": ["..."] }
  ],
  "mismatches": [ /* only when --debug-mismatches */ ],
  "warnings": [ /* top-level */ ],
  "candidates": [
    { "id": 12, "class": "flag_alias | playbook_candidate",
      "summary": "...", "sightings": 3, "last_seen": "...",
      "rationale": "...",
      "next_action": ["<trial command>", "llamaswap-pp-cli learnings confirm 12"] }
  ],
  "playbook": {
    "query_family": "...",
    "playbook": {
      "steps": [ { "cmd": "<command with {slot} substitution>", "purpose": "..." } ],
      "entity_slots": ["$ENTITY"],
      "expected_tool_calls": 3
    },
    "slots_resolved": { "$ENTITY": { "token": "<live token>", "canonical": "<canonical>" } },
    "notes": "<workarounds + gotchas for this query family>"
  },
  "notes": "<duplicate surface for non-playbook callers>"
}
```

Empty-store short-circuit: if the store has no learnings, playbooks, or candidates yet (recall finds nothing and `learnings list` and `learnings candidates` are both empty), skip recall for the rest of this session instead of taxing every query; resume recall-first once something has been taught.

### Step 2: decision tree

Read `candidates`, `playbook`, `notes`, `results[0]`, and warnings in that order:

```
if Candidates present (warnings include "candidates_present"):
    -> candidates are try-then-confirm, never facts. Follow each candidate's
       two-step next_action verbatim: run the trial command first, then run
       `learnings confirm <id>` only after the trial verified the behavior.
       Reject a wrong candidate with `learnings reject <id>`.
    -> NEVER re-teach something recall surfaced as a candidate; confirm or
       reject that candidate instead of teaching a duplicate.
    -> candidates ride alongside playbooks and resource hits, not instead of
       them; continue with the branches below after acting on them.

if Playbook present:
    -> READ Playbook.notes verbatim FIRST (workarounds + gotchas the CLI surface doesn't expose)
    -> replay Playbook.steps in order, substituting Playbook.slots_resolved entries
       for the entity slot tokens. If a step's slot is unresolved, fall back to
       discovery for that step only.
    -> the Playbook's expected_tool_calls is a budget; if you find yourself running
       materially more, record the divergence via `llamaswap-pp-cli playbook amend`
       at end-of-session.

elif Notes present (no Playbook):
    -> read Notes verbatim before any discovery step; they carry known gotchas
       for this query family even when no structured choreography exists yet.

elif Found AND Results[0].EntityMatch == "exact" AND Results[0].Confidence >= 2:
    -> skip discovery; fetch live data for Results[*].ResourceID in parallel

elif Found AND Results[0].EntityMatch == "partial":
    -> candidate hint, NOT a hit; read the resource title to validate before trusting

elif (any row in Mismatches[] when --debug-mismatches was passed):
    -> treat as cold start; the stored learning is for a different entity
       (different canonical resolved from query_entities)

else:  // Found == false, no playbook, no notes
    -> cold start; run discovery normally; teach the answer afterward (Step 4).
       If the family has no playbook yet, that teach auto-synthesizes a
       playbook candidate from this session's journal - you do not need to
       record one by hand.
```

Playbook and Notes are orthogonal to the per-resource path. A recall response can carry both a Playbook AND a `Results[]` hit - use both: the Playbook tells you which choreography to run; the resource hits short-circuit specific steps. Default to skipping `mismatches`; pass `--debug-mismatches` only when investigating cold-start surprises.

Candidate judgment details: `learnings confirm <id>` prints the candidate's full payload before materializing it - check that the printed payload matches the behavior you verified. `learnings reject <id>` tombstones the derivation signature so the same candidate does not resurface. The envelope carries only the few candidates worth acting on now; `llamaswap-pp-cli learnings candidates` lists the full open set.

Graceful degradation: if `learnings confirm` is an unknown command, you are driving an older binary - ignore the candidates guidance and follow the rest of the protocol.

### Step 3: always read `warnings`

- `low_confidence`: row exists at `confidence<2`. Treat as a hint, not a skip-discovery hit.
- `resource_not_in_store`: the local store doesn't have the resource the learning points at. The match validator couldn't classify entities — direct-fetch and re-evaluate.
- `cross_alias_match` (per-result): the row was taught under a different alias and matched the live query's canonical via `entity_lookups` (e.g., a "USA" teach satisfying a "United States" recall). Trust the resource_id.
- `similar_shape_different_entity:<canonical>` (top-level): a structurally matching row exists but its canonical entity differs from the live query's. Treated as cold start; the warning carries the conflicting canonical as a hint, but the row is NOT promoted into Results.
- `ambiguous_alias` (top-level): a single query entity resolved to multiple canonicals (e.g., "Cards" → Arizona Cardinals + St. Louis Cardinals). Surface the ambiguity from context before committing to a resource.
- `candidates_present` (top-level): the envelope carries a `candidates` section. Handle it via the candidates branch in Step 2 before anything else.
- `lookup_refresh_available` (top-level): an entity in the query has no lookup row yet, but synced data could provide one. Run `llamaswap-pp-cli sync` to refresh entity lookups.
- Top-level `no_learnings_for_query_family`: the table had no rows above the Jaccard floor. Pure cold start.

### Step 4: `teach &` after finalizing your response - always

Teaching is unconditional. After resolving a query the store could not answer, background-teach the final resource mapping - no call-count threshold, no judging whether it was "worth" learning. The teach is the anchor of the loop: it triggers playbook synthesis for a family without a playbook, and same-referent phrasings fold into one family so near-duplicate teaches do not fragment the store. Fire it after assembling your user-facing response but BEFORE emitting it, with a shell `&` so the call returns immediately:

```bash
llamaswap-pp-cli teach --query "<user's question>" --resource-type <type> --resource <id1> --resource <id2>
# (append shell `&` to background it)
```

Silent on success. Errors only land in `teach.log` under the resolved state dir. Teach the **most specific** resource - if the user asked a broad question and you walked through parent records to find the specific answer, teach the leaf id, not the parent. The CLI uses seeded `entity_lookups` for cross-alias resolution at recall time, so a teach under one alias (e.g., "Niners") satisfies future queries under another alias (e.g., "49ers", "San Francisco") automatically.

PII rule: teach the structural question with identifiers stripped - never include names, emails, phone numbers, account ids, or other personal identifiers in taught queries or notes. The CLI scans teach queries for obvious email/phone shapes and warns, but does not block; strip before teaching rather than relying on the warning.

### Step 5: playbooks - optional flags, automatic synthesis

You do not need to decide whether a session "deserves" a playbook: a teach on a family without one auto-synthesizes a `playbook_candidate` from the session's journal, and the next session judges it via confirm/reject. Attach explicit playbook flags only when you already hold choreography worth recording verbatim - workarounds the CLI didn't surface (silently-dropped flags, undocumented params, pagination tricks, payload gotchas). Prefer the **integrated one-call form** - record the resource learning and the playbook in the same `teach` invocation:

```bash
# Common case: record both the resource learning AND the playbook in one call.
llamaswap-pp-cli teach \
  --query "<user's question>" \
  --resource <id> \
  --playbook-file ~/playbooks/<shape>.json \
  --playbook-notes-file ~/playbooks/<shape>-notes.md
# (append shell `&` to background it)

# Alternate: playbook-only (no resource to record alongside).
llamaswap-pp-cli teach-playbook \
  --query "<user's question>" \
  --playbook-file ~/playbooks/<shape>.json \
  --notes-file ~/playbooks/<shape>-notes.md
```

Playbook files are JSON with `steps`, `entity_slots`, `expected_tool_calls`. Notes files are markdown carrying the gotchas verbatim. File-free callers (MCP-only agents) pass the same content inline: `--playbook-json` and `--playbook-notes` on the integrated `teach` form, `--playbook-json` and `--notes` on `teach-playbook`. On the integrated `teach` form, the playbook flags are optional - omit them entirely for a resource-only teach. On the standalone `teach-playbook` form, at least one of the playbook and notes flags must be set; both empty is rejected. Playbooks are keyed on the structural query family (entities stripped) so a recipe taught from one entity-shaped query applies to every other query of the same shape, with `slots_resolved` binding the live query's canonical at recall time.

When you DO find a playbook on a future recall, treat it as ground truth: replay the steps with `slots_resolved` substitutions, skip the discovery that the choreography already documents, and read `notes` before any step.

### Step 6: `playbook amend &` when your debug response identifies a correction

If your debug-protocol response identifies a concrete correction the notes or playbook should know — a workaround, an undocumented endpoint shape, a stale field name, observed schema drift, an empty-payload fallback — fire `playbook amend` BEFORE emitting your user-facing response. Same fire-and-forget posture as `teach`.

```bash
llamaswap-pp-cli playbook amend \
  --query "<exact recall query string>" \
  --add-note "<your concrete correction>"
# (append shell `&` to background it)
```

What counts as worth amending: a behavior you OBSERVED this session that future-you would benefit from knowing. Examples worth amending:

- A workaround for a CLI surface that silently drops or misorders a flag.
- An undocumented endpoint shape (response wrapped in `{meta, results}`, payload nested two levels deeper than the docs claim).
- Observed schema drift (a field renamed, an index that shifted between seasons, a category label that the API now returns lower-cased).

What does NOT belong in notes:

- The year-specific or entity-specific answer to the user's question. That's the response, not a learning.
- Per-team / per-athlete / per-row data the playbook already retrieves at runtime.
- Statements that paraphrase what the existing notes already say.

The amend command appends to the family's existing notes with a timestamped marker (`[amend YYYY-MM-DDTHH:MMZ]: <text>`). Multiple amends accumulate; the audit trail is visible. If no playbook exists yet for the family, amend creates a notes-only one (so cold-start corrections still land).

#### PII discipline for amend notes

`playbook amend` notes are designed to potentially flow upstream as shared knowledge in future versions of the Printing Press. Keep them clean of user-identifying content so the upstream-contribution path stays open without retroactive scrubbing:

- **Do NOT embed** paths to user filesystems, personal API keys or tokens, user email addresses, user GitHub handles, or specific query histories tied to a single user.
- **Acceptable**: endpoint shapes, undocumented field names, API gotchas, observed schema drift, workarounds for CLI surfaces, generalizable pagination or retry tactics.

If a correction is only meaningful with user-specific context, it belongs in a personal note, not in the playbook amend.

### Measuring the loop

`llamaswap-pp-cli learnings stats` reports recall hit rate, teach-to-reuse, playbook resolution rate, and candidate confirm/reject counts from the local `learn_events` table. Rates are null until they have a denominator; everything stays on this machine. Use it to check whether the loop is earning its keep for this CLI.

### Disabling learning

- `--no-learn` on a single command short-circuits both `recall` and the `teach` write path. Use for deterministic agent flows or tests that must not be affected by accumulated learnings.
- `LLAMASWAP_NO_LEARN=true` in the environment globally disables the pipeline.

## Agent Feedback

When you (or the agent) notice something off about this CLI, record it:

```
llamaswap-pp-cli feedback "the --since flag is inclusive but docs say exclusive"
llamaswap-pp-cli feedback --stdin < notes.txt
llamaswap-pp-cli feedback list --json --limit 10
```

Entries are stored locally as `feedback.jsonl` under the resolved data dir. They are never POSTed unless `LLAMASWAP_FEEDBACK_ENDPOINT` is set AND either `--send` is passed or `LLAMASWAP_FEEDBACK_AUTO_SEND=true`. Default behavior is local-only.

Write what *surprised* you, not a bug report. Short, specific, one line: that is the part that compounds.

## Output Delivery

Every command accepts `--deliver <sink>`. The output goes to the named sink in addition to (or instead of) stdout, so agents can route command results without hand-piping. Three sinks are supported:

| Sink | Effect |
|------|--------|
| `stdout` | Default; write to stdout only |
| `file:<path>` | Atomically write output to `<path>` (tmp + rename) |
| `webhook:<url>` | POST the output body to the URL (`application/json` or `application/x-ndjson` when `--compact`) |

Unknown schemes are refused with a structured error naming the supported set. Webhook failures return non-zero and log the URL + HTTP status on stderr.

## Named Profiles

A profile is a saved set of flag values, reused across invocations. Use it when a scheduled or recurring agent reuses the same saved flags while providing different input each run.

```
llamaswap-pp-cli profile save briefing --json
llamaswap-pp-cli --profile briefing captures --id 597
llamaswap-pp-cli profile list --json
llamaswap-pp-cli profile show briefing
llamaswap-pp-cli profile delete briefing --yes
```

Explicit flags always win over profile values; profile values win over defaults. `agent-context` lists all available profiles under `available_profiles` so introspecting agents discover them at runtime.

## Exit Codes

Framework codes:

| Code | Meaning |
|------|---------|
| 0 | Success |
| 2 | Usage error (wrong arguments) |
| 3 | Resource not found — the named model/alias resolves to nothing in the roster (checked against ids AND `meta.llamaswap.aliases`) |
| 4 | Server unreachable — the proxy did not answer on 127.0.0.1 |
| 5 | API error (upstream issue) |
| 7 | Rate limited (wait and retry) |
| 10 | Partial failure |

llama-swap-specific codes (from `internal/cli/exitcodes.go`) — these are the ones an unattended run branches on:

| Code | Meaning |
|------|---------|
| 20 | Keep-set refusal — the operation would touch a protected keep-set member (matched by id or alias, sourced from config, never server ttl) |
| 21 | Drain timeout — `--drain` could not confirm idle within the timeout; nothing was unloaded (fail closed) |
| 22 | Drain unobservable — /slots was unreadable (timeout/5xx); nothing was unloaded (fail closed) |
| 23 | Port conflict — a scratch/test instance port is already listening or sits inside the startPort span / reserved band |
| 24 | Config invalid — schema or semantic validation failed. Also the SETUP-GAP code: `verify --probe` with no baseline recorded yet |
| 25 | Drift — live process flags diverge from the file (`config drift`, `seat show --diff-yaml`). Not an error; a finding |
| 26 | Probe failed — `verify --probe` found the memory stack answering but DEGRADED (cosine/score outside stored tolerance) |
| 27 | Upstream 5xx — the upstream model server answered 5xx |
| 28 | Fit refusal — `fit`/`ctx` lands inside the uncertainty band and refuses to answer rather than guess |

## Argument Parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `llamaswap-pp-cli --help` output
2. **Starts with `install`** → ends with `mcp` → MCP installation; otherwise → see Prerequisites above
3. **Anything else** → Direct Use (execute as CLI command with `--agent`)

## MCP Server Installation

Install the MCP binary from this CLI's published public-library entry or pre-built release, then register it:

```bash
claude mcp add llamaswap-pp-mcp -- llamaswap-pp-mcp
```

Verify: `claude mcp list`

## Direct Use

1. Check if installed: `which llamaswap-pp-cli`
   If not found, offer to install (see Prerequisites at the top of this skill).
2. Resolve the query to a command. The CLI's own resolver is the authority — prefer it over guessing from the section headings:
   ```bash
   llamaswap-pp-cli which "<capability in your own words>"
   ```
   Exit `0` means at least one match; exit `2` means no confident match — then fall back to the Unique Capabilities and Command Reference above.
3. Execute with the `--agent` flag:
   ```bash
   llamaswap-pp-cli <command> [subcommand] [args] --agent
   ```
4. If still ambiguous, drill into subcommand help: `llamaswap-pp-cli <command> --help`.
