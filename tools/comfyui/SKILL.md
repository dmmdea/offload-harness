---
name: pp-comfyui
description: "Drive a local ComfyUI render server from the shell, with a durable record of every run the server itself forgets."
author: "Daniel Martinez"
license: "Apache-2.0"
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  openclaw:
    requires:
      bins:
        - comfyui-pp-cli
---

# ComfyUI — Printing Press CLI

## Prerequisites: Install the CLI

This skill drives the `comfyui-pp-cli` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via the Printing Press installer. It defaults binaries to `$HOME/.local/bin` on macOS/Linux and `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows:
   ```bash
   npx -y @mvanhorn/printing-press-library install comfyui --cli-only
   ```
2. Verify: `comfyui-pp-cli --version`
3. Ensure the reported install directory is on `$PATH` for the agent/runtime that will invoke this skill.

If the `npx` install fails before this CLI has a public-library category, install Node or use the category-specific Go fallback after publish.

If `--version` reports "command not found" after install, the runtime cannot see the binary directory on `$PATH`. Do not proceed with skill commands until verification succeeds.

ComfyUI keeps its render history in RAM and loses it on every restart. This CLI submits graphs, reads honest timings from the server's own execution timestamps, and keeps runs, graphs, node schemas and outputs in local SQLite so comparisons survive the restarts that tuning requires. It attaches to an in-flight render rather than starting a second one, and it answers questions the server cannot: why a model is invisible, what a loader will actually accept, and which configuration produced a given file.

## Unique Capabilities

These capabilities aren't available in any other tool for this API.

### Local state that compounds
- **`submit`** — Submitting a graph that is already rendering attaches to the running job instead of starting a second one.

  _Reach for this instead of POSTing /prompt directly whenever a render may already be in flight; a duplicate submit costs a full GPU render._

  ```bash
  comfyui-pp-cli submit examples/minimal-graph.json --json
  ```
- **`timing`** — Reports duration from the server's own execution timestamps, compared against the historical distribution for the same graph shape.

  _Use this rather than reading the server log when comparing configurations; log scraping has produced a false regression before._

  ```bash
  comfyui-pp-cli timing --last 20 --json
  ```
- **`sync-history`** — Copies the server's in-memory render history into the local store before a restart destroys it.

  _Run before restarting ComfyUI, or the runs you are comparing disappear._

  ```bash
  comfyui-pp-cli sync-history --json
  ```
- **`exp`** — Runs a parameter sweep as one durable object, with failed arms recorded as first-class rows rather than gaps.

  _Use for any comparison across settings; an OOM is a result worth keeping, not an absence._

  ```bash
  comfyui-pp-cli exp new demo-sweep --vary 3.steps=10,20 --graph examples/minimal-graph.json --json
  ```
- **`replay`** — Re-runs a stored graph and reports a before/after delta including which server configuration each ran under.

  _Use to confirm a change actually moved the number, rather than comparing against a stale baseline._

  ```bash
  comfyui-pp-cli replay --json
  ```

### Answers the server cannot give
- **`models why`** — Says why ComfyUI cannot see a model file, separating four causes that every other tool collapses into one.

  _Use this the moment a loader rejects a filename, before re-downloading anything._

  ```bash
  comfyui-pp-cli models why ltx-2.5-22b-dev-transformer-comfy-int8-convrot.safetensors --json
  ```
- **`nodes options`** — Prints the values a loader input will actually accept, reading both /object_info spec shapes.

  _Check a filename here before spending a render on a value the validator will reject._

  ```bash
  comfyui-pp-cli nodes options UNETLoader unet_name --json
  ```
- **`provenance`** — Given a produced file, reports the run, graph, experiment arm, timing, server identity, and staged input that made it.

  _Use when a rendered file needs to be traced back to the exact configuration that produced it._

  ```bash
  comfyui-pp-cli provenance MMX_smoke_00001_.mp4 --json
  ```
- **`outputs`** — Lists produced files and, with --probe, fills in resolution, frame rate, duration, and whether audio is present.

  _Use when comparing video models; the server cannot tell you whether a clip has sound._

  ```bash
  comfyui-pp-cli outputs --last 10 --probe --json
  ```

### Reproducible edits
- **`set`** — Applies overrides to a graph's inputs, refusing to write when a node no longer holds the class it did when the address was captured.

  _Use instead of hand-editing graph JSON; the class assertion is what makes a template update fail loudly rather than silently._

  ```bash
  comfyui-pp-cli set examples/minimal-graph.json 6@CLIPTextEncode.text="a brass sextant" --json
  ```
- **`validate`** — Checks a graph against the cached node schema before submitting — unknown classes, missing required inputs, and COMBO values that are not members of the option list.

  _Run before every submit; a rejection caught here costs nothing, one caught by the server has already consumed a queue slot._

  ```bash
  comfyui-pp-cli validate examples/minimal-graph.json --json
  ```
- **`stage`** — Stages a host file into ComfyUI's input directory and records its content hash, so archived runs stay reproducible.

  _Stage inputs rather than copying them by hand when a comparison needs to hold for months._

  ```bash
  comfyui-pp-cli stage examples/minimal-graph.json --json
  ```

## Recipes

### Check a filename before spending a render

```bash
comfyui-pp-cli nodes options UNETLoader unet_name --json --select results
```

Reads the COMBO option values from both possible spec shapes, so a name that would be rejected at submit is caught for free.

### Diagnose an invisible model

```bash
comfyui-pp-cli models why ltx-2.5-22b-dev-transformer-comfy-int8-convrot.safetensors --json
```

Separates unregistered model class from not-listed and no-such-input, each with its own remedy, instead of reporting a generic missing file.

### Preflight a graph offline

```bash
comfyui-pp-cli validate examples/minimal-graph.json --json
```

The server has no validate-only endpoint, so this is the only dry run that does not consume a queue slot.

### Patch a graph without silently hitting the wrong node

```bash
comfyui-pp-cli set examples/minimal-graph.json 6@CLIPTextEncode.text="a brass sextant" --json
```

Refuses the write when node 6 no longer holds the expected class, which is how a template revision fails loudly rather than quietly.

### Preserve history before a restart

```bash
comfyui-pp-cli sync-history --json --select results
```

The server's history is a RAM dict destroyed on restart; ingest it first or the runs being compared vanish.

## Known Gaps

Four cases where a plain reading of the output would mislead you. None affect the render path.

- **`queue clear` on its own clears nothing.** It posts an empty body, which the server accepts and ignores, and exits 0 — so never report the queue as cleared on the strength of that exit code. Send the body instead: `echo '{"clear": true}' | comfyui-pp-cli queue clear --stdin`, or `echo '{"delete": ["<prompt_id>"]}' | comfyui-pp-cli queue clear --stdin` for specific items. Neither stops the running prompt; that is `comfyui-pp-cli queue interrupt`.
- **`models why` is only definitive when scoped.** Unscoped, any empty model-folder COMBO anywhere on the server wins the verdict as `class-unregistered` (exit 12) even when `not-listed` is likelier — read `empty_combos` against `same_kind_loaders` before repeating the verdict. Scope it to settle the question: `comfyui-pp-cli models why <filename> --class CheckpointLoaderSimple --input ckpt_name`.
- **`sync` counts failed resources without naming them.** An `errored` count above zero has no accompanying event identifying which resource failed or why; sync one resource at a time when the answer matters.
- **`--json` unicode-escapes `<`, `>` and `&`** inside human-readable strings such as `timing.meta.outlier_rule`. Parse the JSON rather than pattern-matching its raw text and the values come back correct.

## Command Reference

**history** — Completed prompt records, including real execution timings.

- `comfyui-pp-cli history get` — Full record for one prompt: status (status_str, completed)
- `comfyui-pp-cli history list` — Recent prompt history, newest last.

**objectinfo** — The live node schema — the authority on class names and valid input values.

- `comfyui-pp-cli objectinfo all` — Every registered node class and its input schema.
- `comfyui-pp-cli objectinfo get` — Schema for one node class. Loader COMBO options live at index 1 of the input tuple as {'options' -> [...

**prompt** — Submit an API-format graph for execution.

- `comfyui-pp-cli prompt` — Queue an API-format graph. Returns {prompt_id, number, node_errors} on success.

**queue** — Pending and running work.

- `comfyui-pp-cli queue clear` — Clear pending items. Send {'clear': true}.
- `comfyui-pp-cli queue get` — Returns {queue_running, queue_pending}.
- `comfyui-pp-cli queue interrupt` — Interrupt the currently executing prompt. Does not clear pending items.

**system** — Server and device state.

- `comfyui-pp-cli system` — ComfyUI version, python version, and per-device vram_total / vram_free.

**upload** — Stage input images for LoadImage.

- `comfyui-pp-cli upload` — Upload an image into ComfyUI's input dir.

**userdata** — Server-side user files, including saved workflows.

- `comfyui-pp-cli userdata get` — Read a user file. Workflow paths are URL-encoded, e.g. workflows%2Fname.json.
- `comfyui-pp-cli userdata put` — Write a user file, creating parent dirs. Pass overwrite=true to replace.

**view** — Fetch rendered outputs.

- `comfyui-pp-cli view` — Fetch an output file by filename/subfolder/type.


### Finding the right command

When you know what you want to do but not which command does it, ask the CLI directly:

```bash
comfyui-pp-cli which "<capability in your own words>"
```

`which` resolves a natural-language capability query to the best matching command from this CLI's curated feature index. Exit code `0` means at least one match; exit code `2` means no confident match — fall back to `--help` or use a narrower query.

## Auth Setup

No authentication required.

Run `comfyui-pp-cli doctor` to verify setup.

## Agent Mode

Add `--agent` to any command. Expands to: `--json --compact --no-input --no-color --yes`.

- **Pipeable** — JSON on stdout, errors on stderr
- **Filterable** — `--select` keeps a subset of fields. Dotted paths descend into nested structures; arrays traverse element-wise. Critical for keeping context small on verbose APIs:

  ```bash
  comfyui-pp-cli history list --agent
  ```
- **Previewable** — `--dry-run` shows the request without sending
- **Offline-friendly** — sync/search commands can use the local SQLite store when available
- **Non-interactive** — never prompts, every input is a flag
- **Explicit retries** — use `--idempotent` only when an already-existing create should count as success

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

- Use `--home <dir>` for one invocation, or set `COMFYUI_HOME=<dir>` to relocate all four path kinds under one root.
- Use per-kind env vars only when a specific kind must diverge: `COMFYUI_CONFIG_DIR`, `COMFYUI_DATA_DIR`, `COMFYUI_STATE_DIR`, `COMFYUI_CACHE_DIR`.
- Resolution order is per-kind env var, `--home`, `COMFYUI_HOME`, XDG (`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`), then platform defaults.
- `config` contains settings like `config.toml` and profiles. `data` contains `credentials.toml`, `data.db`, cookies, and auth sidecars. `state` contains persisted queries, jobs, and `teach.log`. `cache` contains regenerable HTTP/cache files.
- Stored secrets live in `credentials.toml` under the data dir. Existing legacy `config.toml` secrets are read for compatibility and leave `config.toml` on the first auth write.
- Run `comfyui-pp-cli doctor --fail-on warn` to surface path and credential-location warnings. `agent-context` exposes a schema v4 `paths` block for agents that need the resolved dirs.
- For MCP, pass relocation through the MCP host config. The MCP binary does not inherit CLI flags:

  ```json
  {
    "mcpServers": {
      "comfyui": {
        "command": "comfyui-pp-mcp",
        "env": {
          "COMFYUI_HOME": "/srv/comfyui"
        }
      }
    }
  }
  ```

Fleet precedence: an inherited per-kind env var overrides an explicit `--home` for that kind. Use `COMFYUI_HOME` or per-kind vars as durable fleet levers, and use `--home` only for a single invocation. Relocation is not reversible by unsetting env vars; move files manually before clearing `COMFYUI_HOME`, or `doctor` will not find credentials left under the former root.

## Automatic learning

This CLI ships a self-capturing learning loop. The CLI does its own bookkeeping: every invocation is journaled locally, a failed flag followed by a corrected retry auto-derives a `flag_alias` candidate, and a `teach` on a query family without a playbook auto-synthesizes a `playbook_candidate` from the session's journal. Your job is judgment only: `recall` first, act on surfaced candidates, `teach` the final answer, `playbook amend` when you observe a correction. You never record failures by hand.

### Step 1: `recall` before any discovery

Before list/search/drill commands on a new user question, run:

```bash
comfyui-pp-cli recall "<user's question>" --agent
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
      "next_action": ["<trial command>", "comfyui-pp-cli learnings confirm 12"] }
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
       materially more, record the divergence via `comfyui-pp-cli playbook amend`
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

Candidate judgment details: `learnings confirm <id>` prints the candidate's full payload before materializing it - check that the printed payload matches the behavior you verified. `learnings reject <id>` tombstones the derivation signature so the same candidate does not resurface. The envelope carries only the few candidates worth acting on now; `comfyui-pp-cli learnings candidates` lists the full open set.

Graceful degradation: if `learnings confirm` is an unknown command, you are driving an older binary - ignore the candidates guidance and follow the rest of the protocol.

### Step 3: always read `warnings`

- `low_confidence`: row exists at `confidence<2`. Treat as a hint, not a skip-discovery hit.
- `resource_not_in_store`: the local store doesn't have the resource the learning points at. The match validator couldn't classify entities — direct-fetch and re-evaluate.
- `cross_alias_match` (per-result): the row was taught under a different alias and matched the live query's canonical via `entity_lookups` (e.g., a "USA" teach satisfying a "United States" recall). Trust the resource_id.
- `similar_shape_different_entity:<canonical>` (top-level): a structurally matching row exists but its canonical entity differs from the live query's. Treated as cold start; the warning carries the conflicting canonical as a hint, but the row is NOT promoted into Results.
- `ambiguous_alias` (top-level): a single query entity resolved to multiple canonicals (e.g., "Cards" → Arizona Cardinals + St. Louis Cardinals). Surface the ambiguity from context before committing to a resource.
- `candidates_present` (top-level): the envelope carries a `candidates` section. Handle it via the candidates branch in Step 2 before anything else.
- `lookup_refresh_available` (top-level): an entity in the query has no lookup row yet, but synced data could provide one. Run `comfyui-pp-cli sync` to refresh entity lookups.
- Top-level `no_learnings_for_query_family`: the table had no rows above the Jaccard floor. Pure cold start.

### Step 4: `teach &` after finalizing your response - always

Teaching is unconditional. After resolving a query the store could not answer, background-teach the final resource mapping - no call-count threshold, no judging whether it was "worth" learning. The teach is the anchor of the loop: it triggers playbook synthesis for a family without a playbook, and same-referent phrasings fold into one family so near-duplicate teaches do not fragment the store. Fire it after assembling your user-facing response but BEFORE emitting it, with a shell `&` so the call returns immediately:

```bash
comfyui-pp-cli teach --query "<user's question>" --resource-type <type> --resource <id1> --resource <id2>
# (append shell `&` to background it)
```

Silent on success. Errors only land in `teach.log` under the resolved state dir. Teach the **most specific** resource - if the user asked a broad question and you walked through parent records to find the specific answer, teach the leaf id, not the parent. The CLI uses seeded `entity_lookups` for cross-alias resolution at recall time, so a teach under one alias (e.g., "Niners") satisfies future queries under another alias (e.g., "49ers", "San Francisco") automatically.

PII rule: teach the structural question with identifiers stripped - never include names, emails, phone numbers, account ids, or other personal identifiers in taught queries or notes. The CLI scans teach queries for obvious email/phone shapes and warns, but does not block; strip before teaching rather than relying on the warning.

### Step 5: playbooks - optional flags, automatic synthesis

You do not need to decide whether a session "deserves" a playbook: a teach on a family without one auto-synthesizes a `playbook_candidate` from the session's journal, and the next session judges it via confirm/reject. Attach explicit playbook flags only when you already hold choreography worth recording verbatim - workarounds the CLI didn't surface (silently-dropped flags, undocumented params, pagination tricks, payload gotchas). Prefer the **integrated one-call form** - record the resource learning and the playbook in the same `teach` invocation:

```bash
# Common case: record both the resource learning AND the playbook in one call.
comfyui-pp-cli teach \
  --query "<user's question>" \
  --resource <id> \
  --playbook-file ~/playbooks/<shape>.json \
  --playbook-notes-file ~/playbooks/<shape>-notes.md
# (append shell `&` to background it)

# Alternate: playbook-only (no resource to record alongside).
comfyui-pp-cli teach-playbook \
  --query "<user's question>" \
  --playbook-file ~/playbooks/<shape>.json \
  --notes-file ~/playbooks/<shape>-notes.md
```

Playbook files are JSON with `steps`, `entity_slots`, `expected_tool_calls`. Notes files are markdown carrying the gotchas verbatim. File-free callers (MCP-only agents) pass the same content inline: `--playbook-json` and `--playbook-notes` on the integrated `teach` form, `--playbook-json` and `--notes` on `teach-playbook`. On the integrated `teach` form, the playbook flags are optional - omit them entirely for a resource-only teach. On the standalone `teach-playbook` form, at least one of the playbook and notes flags must be set; both empty is rejected. Playbooks are keyed on the structural query family (entities stripped) so a recipe taught from one entity-shaped query applies to every other query of the same shape, with `slots_resolved` binding the live query's canonical at recall time.

When you DO find a playbook on a future recall, treat it as ground truth: replay the steps with `slots_resolved` substitutions, skip the discovery that the choreography already documents, and read `notes` before any step.

### Step 6: `playbook amend &` when your debug response identifies a correction

If your debug-protocol response identifies a concrete correction the notes or playbook should know — a workaround, an undocumented endpoint shape, a stale field name, observed schema drift, an empty-payload fallback — fire `playbook amend` BEFORE emitting your user-facing response. Same fire-and-forget posture as `teach`.

```bash
comfyui-pp-cli playbook amend \
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

`comfyui-pp-cli learnings stats` reports recall hit rate, teach-to-reuse, playbook resolution rate, and candidate confirm/reject counts from the local `learn_events` table. Rates are null until they have a denominator; everything stays on this machine. Use it to check whether the loop is earning its keep for this CLI.

### Disabling learning

- `--no-learn` on a single command short-circuits both `recall` and the `teach` write path. Use for deterministic agent flows or tests that must not be affected by accumulated learnings.
- `COMFYUI_NO_LEARN=true` in the environment globally disables the pipeline.

## Agent Feedback

When you (or the agent) notice something off about this CLI, record it:

```
comfyui-pp-cli feedback "the --since flag is inclusive but docs say exclusive"
comfyui-pp-cli feedback --stdin < notes.txt
comfyui-pp-cli feedback list --json --limit 10
```

Entries are stored locally as `feedback.jsonl` under the resolved data dir. They are never POSTed unless `COMFYUI_FEEDBACK_ENDPOINT` is set AND either `--send` is passed or `COMFYUI_FEEDBACK_AUTO_SEND=true`. Default behavior is local-only.

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
comfyui-pp-cli profile save briefing --json
comfyui-pp-cli --profile briefing history list
comfyui-pp-cli profile list --json
comfyui-pp-cli profile show briefing
comfyui-pp-cli profile delete briefing --yes
```

Explicit flags always win over profile values; profile values win over defaults. `agent-context` lists all available profiles under `available_profiles` so introspecting agents discover them at runtime.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 2 | Usage error (wrong arguments) |
| 3 | Resource not found |
| 5 | API error (upstream issue) |
| 7 | Rate limited (wait and retry) |
| 10 | Config error |

## Argument Parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `comfyui-pp-cli --help` output
2. **Starts with `install`** → ends with `mcp` → MCP installation; otherwise → see Prerequisites above
3. **Anything else** → Direct Use (execute as CLI command with `--agent`)

## MCP Server Installation

Install the MCP binary from this CLI's published public-library entry or pre-built release, then register it:

```bash
claude mcp add comfyui-pp-mcp -- comfyui-pp-mcp
```

Verify: `claude mcp list`

## Direct Use

1. Check if installed: `which comfyui-pp-cli`
   If not found, offer to install (see Prerequisites at the top of this skill).
2. Match the user query to the best command from the Unique Capabilities and Command Reference above.
3. Execute with the `--agent` flag:
   ```bash
   comfyui-pp-cli <command> [subcommand] [args] --agent
   ```
4. If ambiguous, drill into subcommand help: `comfyui-pp-cli <command> --help`.
