# ComfyUI CLI

**Drive a local ComfyUI render server from the shell, with a durable record of every run the server itself forgets.**

ComfyUI keeps its render history in RAM and loses it on every restart. This CLI submits graphs, reads honest timings from the server's own execution timestamps, and keeps runs, graphs, node schemas and outputs in local SQLite so comparisons survive the restarts that tuning requires. It attaches to an in-flight render rather than starting a second one, and it answers questions the server cannot: why a model is invisible, what a loader will actually accept, and which configuration produced a given file.

## Install

The recommended path installs both the `comfyui-pp-cli` binary and the `pp-comfyui` agent skill (Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot, and other agents supported by the upstream [`skills`](https://github.com/vercel-labs/skills) CLI) in one shot:

```bash
npx -y @mvanhorn/printing-press-library install comfyui
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press-library install comfyui --cli-only
```

For skill only — installs the skill into the same agents as the default command above, but skips the CLI binary (use this to update or reinstall just the skill):

```bash
npx -y @mvanhorn/printing-press-library install comfyui --skill-only
```

To constrain the skill install to one or more specific agents (repeatable — agent names match the [`skills`](https://github.com/vercel-labs/skills) CLI):

```bash
npx -y @mvanhorn/printing-press-library install comfyui --agent claude-code
npx -y @mvanhorn/printing-press-library install comfyui --agent claude-code --agent codex
```

### Without Node (Go fallback)

If `npx` isn't available (no Node, offline), install the CLI directly via Go (requires Go 1.26.6 or newer):

```bash
go install github.com/mvanhorn/printing-press-library/library/ai/comfyui/cmd/comfyui-pp-cli@latest
```

This installs the CLI only — no skill.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/comfyui-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

Install the CLI binary first. The installer writes binaries to a per-user managed bin directory by default: `$HOME/.local/bin` on macOS/Linux and `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows.

```bash
npx -y @mvanhorn/printing-press-library install comfyui --cli-only
```

Then install the focused Hermes skill.

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-comfyui --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-comfyui --force
```

Restart the Hermes session or gateway if the newly installed skill is not visible immediately.

## Install for OpenClaw
Install both the CLI binary and the focused OpenClaw skill. The installer defaults binaries to a per-user bin directory (`$HOME/.local/bin` on macOS/Linux, `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows):

```bash
npx -y @mvanhorn/printing-press-library install comfyui --agent openclaw
```

Restart the OpenClaw session or gateway if the newly installed skill is not visible immediately.

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/comfyui-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


```bash
go install github.com/mvanhorn/printing-press-library/library/ai/comfyui/cmd/comfyui-pp-mcp@latest
```

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "comfyui": {
      "command": "comfyui-pp-mcp"
    }
  }
}
```

</details>


## Quick Start

```bash
# confirm the server is reachable before anything else
comfyui-pp-cli doctor

# see what the loader will actually accept, so a submit is not wasted on a rejected filename
comfyui-pp-cli nodes options UNETLoader unet_name

# capture the server's in-memory history into the local store before a restart destroys it
comfyui-pp-cli sync-history

# one view across the live queue, the live history, and the durable local record
comfyui-pp-cli jobs list

# durations from execution timestamps, compared against the same graph shape
comfyui-pp-cli timing --last 20

```

## Unique Features

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
  comfyui-pp-cli replay demo-run --json
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
  comfyui-pp-cli stage ./still.png --json
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

## Usage

Run `comfyui-pp-cli --help` for the full command reference and flag list.

## Paths & environment variables

This CLI separates local files into four path kinds:

| Kind | Contents |
|------|----------|
| `config` | User-editable settings such as `config.toml` and saved profiles |
| `data` | Durable local data such as `data.db` |
| `state` | Runtime state such as persisted queries, jobs, and `teach.log` |
| `cache` | Regenerable HTTP/cache files |

Each kind resolves independently. The ladder is:

1. Per-kind env var: `COMFYUI_CONFIG_DIR`, `COMFYUI_DATA_DIR`, `COMFYUI_STATE_DIR`, or `COMFYUI_CACHE_DIR`
2. `--home <dir>` for this invocation
3. `COMFYUI_HOME` for a flat relocated root
4. XDG env vars: `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`
5. Platform defaults matching existing installs

For containers and agent sandboxes, prefer a single relocated root:

```bash
export COMFYUI_HOME=/srv/comfyui
comfyui-pp-cli doctor
```

Under `COMFYUI_HOME=/srv/comfyui`, the four dirs resolve to `/srv/comfyui/config`, `/srv/comfyui/data`, `/srv/comfyui/state`, and `/srv/comfyui/cache`.

MCP servers do not receive CLI flags from the host. Put relocation in the host `env` block:

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

Precedence matters in fleets: an ambient per-kind variable such as `COMFYUI_DATA_DIR` overrides an explicit `--home` for that kind. Use `COMFYUI_HOME` or the per-kind variables for durable fleet relocation; treat `--home` as the weaker per-invocation lever.

Relocation is one-way. Unsetting `COMFYUI_HOME` does not move files back to platform defaults, and `doctor` cannot find files left under a former root. Move the files manually before unsetting relocation variables.

Existing installs keep working because the platform-default rung matches the legacy layout. Run `comfyui-pp-cli doctor --fail-on warn` to check path warnings in automation.

## Commands

### history

Completed prompt records, including real execution timings.

- **`comfyui-pp-cli history get`** - Full record for one prompt: status (status_str, completed), the messages array carrying execution_start / execution_success / execution_error timestamps in milliseconds, and the outputs map. The timestamp delta is the ONLY honest source of per-prompt duration — scraping "Prompt executed in N seconds" from the server log returns a stale line when the current run has not finished, and scraping an s/it progress bar returns a transient intra-run sample, not a rate.

- **`comfyui-pp-cli history list`** - Recent prompt history, newest last.

### objectinfo

The live node schema — the authority on class names and valid input values.

- **`comfyui-pp-cli objectinfo all`** - Every registered node class and its input schema.
- **`comfyui-pp-cli objectinfo get`** - Schema for one node class. Loader COMBO options live at index 1 of the input tuple as {"options" -> [...]}, NOT at index 0 — index 0 is the type string. An EMPTY options list means the model CLASS is unregistered (missing extra_model_paths.yaml key), not that the file is missing.


### prompt

Submit an API-format graph for execution.

- **`comfyui-pp-cli prompt`** - Queue an API-format graph. Returns {prompt_id, number, node_errors} on success. On validation failure returns HTTP 400 with an error object plus a node_errors map keyed by node id — that detail is the whole diagnostic and must be surfaced verbatim, never summarised.


### queue

Pending and running work.

- **`comfyui-pp-cli queue clear`** - Clear pending items. Send {"clear": true}.
- **`comfyui-pp-cli queue get`** - Returns {queue_running, queue_pending}. A non-empty queue_running with an idle GPU means the job is in a non-sampling stage (VAE decode, video assembly), not stalled.

- **`comfyui-pp-cli queue interrupt`** - Interrupt the currently executing prompt. Does not clear pending items.

### system

Server and device state.

- **`comfyui-pp-cli system`** - ComfyUI version, python version, and per-device vram_total / vram_free. Device ordering here is torch ordering, which can be the INVERSE of nvidia-smi ordering — trust this endpoint for cuda:N identity, not nvidia-smi index.


### upload

Stage input images for LoadImage.

- **`comfyui-pp-cli upload`** - Upload an image into ComfyUI's input dir. LoadImage takes a FILENAME inside that dir, never an absolute host path — passing an absolute path fails validation with "Invalid image file".


### userdata

Server-side user files, including saved workflows.

- **`comfyui-pp-cli userdata get`** - Read a user file. Workflow paths are URL-encoded, e.g. workflows%2Fname.json.
- **`comfyui-pp-cli userdata put`** - Write a user file, creating parent dirs. Pass overwrite=true to replace.

### view

Fetch rendered outputs.

- **`comfyui-pp-cli view`** - Fetch an output file by filename/subfolder/type.


### Self-learning loop

This CLI caches per-question discovery so repeat queries skip the walk and structurally similar queries get answered via entity substitution. The loop also self-captures: every invocation is journaled locally, and failed-flag corrections plus fresh teaches surface as candidates on the next `recall` for confirm/reject judgment. Agents call `recall` before discovery and fire `teach &` after answering. See the `## Automatic learning` section in `SKILL.md` for the full protocol.

- **`comfyui-pp-cli recall <query>`** - Look up cached resources for a query before running discovery
- **`comfyui-pp-cli teach`** - Record a query -> resource mapping (silent on success, safe to background with `&`)
- **`comfyui-pp-cli learnings list`** - Inspect taught rows
- **`comfyui-pp-cli learnings forget <query>`** - Undo a teach
- **`comfyui-pp-cli learnings candidates`** - List auto-captured candidates awaiting confirm/reject
- **`comfyui-pp-cli learnings stats`** - Local loop metrics: recall hit rate, teach-to-reuse, playbook resolution, candidate counts
- **`comfyui-pp-cli teach-pattern`** - Install a query/resource template up front
- **`comfyui-pp-cli teach-lookup`** - Add an entity mapping (e.g. country code, team alias) for pattern substitution

Pass `--no-learn` or set `COMFYUI_NO_LEARN=true` to disable the loop for deterministic flows.

The local store's schema version stamp is one-way: once this version of `comfyui-pp-cli` opens the database, older binaries refuse it with a version error — upgrade the binary rather than downgrading.

## Output Formats

```bash
# Human-readable table (default in terminal, JSON when piped)
comfyui-pp-cli history list

# JSON for scripting and agents
comfyui-pp-cli history list --json
# Filter to specific fields by name
comfyui-pp-cli history list --json --select <field>[,<field>...]

# Dry run — show the request without sending
comfyui-pp-cli history list --dry-run

# Agent mode — JSON + compact + no prompts in one flag
comfyui-pp-cli history list --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select <field>[,<field>...]` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **Explicit retries** - add `--idempotent` to create retries when a no-op success is acceptable
- **Explicit confirmation** - `--agent` does not imply `--yes`; pass `--yes` separately only after the target, arguments, and side effects are clear
- **Piped input** - write commands can accept structured input when their help lists `--stdin`
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set

Exit codes: `0` success, `2` usage error, `3` not found, `5` API error, `7` rate limited, `10` config error.

## Health Check

```bash
comfyui-pp-cli doctor
```

Verifies configuration and connectivity to the API.

## Configuration

Run `comfyui-pp-cli doctor` to see the resolved config, data, state, and cache directories. The platform-default config path is ``; `--home`, `COMFYUI_HOME`, and per-kind env vars can relocate it.

Static request headers can be configured under `headers`; per-command header overrides take precedence.

## Troubleshooting
**Not found errors (exit code 3)**
- Check the resource ID is correct
- Run the `list` command to see available items


### API-specific
- **A loader rejects a filename that is present on disk** — Run `comfyui-pp-cli models why <filename>`; an empty option list means the model class is unregistered, so add its key to extra_model_paths.yaml and restart ComfyUI.
- **A submit returned success but produced no file** — ComfyUI can return node_errors on HTTP 200 when only some output branches validate; re-check with `comfyui-pp-cli status <prompt_id>` for the partial-accept verdict.
- **Durations disagree with the server log** — Trust `comfyui-pp-cli timing`. The log's completion line is stale while a run is in flight, and a progress-bar sample is an instantaneous rate, not a duration.
- **Runs disappeared after restarting ComfyUI** — The server's history is in RAM. Run `comfyui-pp-cli sync-history` before restarting so the runs land in the local store.
- **A wait command timed out on a long render** — The prompt id is never lost on timeout; re-attach with `comfyui-pp-cli wait <prompt_id>` or check `comfyui-pp-cli status <prompt_id>`.

## Sources & Inspiration

This CLI was built by studying these projects and resources:

- [**comfy-cli**](https://github.com/Comfy-Org/comfy-cli) — Python (937 stars)
- [**artokun/comfyui-mcp**](https://github.com/artokun/comfyui-mcp) — TypeScript (573 stars)
- [**ComfyScript**](https://github.com/Chaoses-Ib/ComfyScript) — Python
- [**Comfy-Org/comfy-mcp**](https://github.com/Comfy-Org/comfy-mcp) — Python
- [**@stable-canvas/comfyui-client**](https://github.com/StableCanvas/comfyui-client) — TypeScript
- [**shawnrushefsky/comfyui-mcp**](https://github.com/shawnrushefsky/comfyui-mcp) — TypeScript

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
