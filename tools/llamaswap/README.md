# llama-swap CLI

**The llama-swap operations console — durable history, drain-aware control, and measurement commands with three specific guarantees: keep-set unloads are refused statically by id AND alias (never from server ttl), `--drain` fails closed when slot state is unreadable, and fit/ctx refuse to answer inside their uncertainty band instead of guessing.**

llama-swap forgets everything on restart and ships no CLI at all. This one mirrors activity and events into local SQLite before they vanish (sync), refuses to unload the protected keep-set by id or alias (sourced from config, never from the server's ttl), fails closed rather than guessing when a drain cannot read slot state, prints config diffs and restart commands instead of ever rewriting your hand-commented YAML, and encodes the measurement traps — the seat's real `n_ctx` over the assumed one, tokenizer counts over chars/4 — into bench, fit, and ctx.

## Install

The recommended path installs both the `llamaswap-pp-cli` binary and the `pp-llamaswap` agent skill (Claude Code, Codex, Cursor, Gemini CLI, GitHub Copilot, and other agents supported by the upstream [`skills`](https://github.com/vercel-labs/skills) CLI) in one shot:

```bash
npx -y @mvanhorn/printing-press-library install llamaswap
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press-library install llamaswap --cli-only
```

For skill only — installs the skill into the same agents as the default command above, but skips the CLI binary (use this to update or reinstall just the skill):

```bash
npx -y @mvanhorn/printing-press-library install llamaswap --skill-only
```

To constrain the skill install to one or more specific agents (repeatable — agent names match the [`skills`](https://github.com/vercel-labs/skills) CLI):

```bash
npx -y @mvanhorn/printing-press-library install llamaswap --agent claude-code
npx -y @mvanhorn/printing-press-library install llamaswap --agent claude-code --agent codex
```

### Without Node

The generated install path is category-agnostic until this CLI is published. If `npx` is not available before publish, install Node or use the category-specific Go fallback from the public-library entry after publish.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/llamaswap-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

Install the CLI binary first. The installer writes binaries to a per-user managed bin directory by default: `$HOME/.local/bin` on macOS/Linux and `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows.

```bash
npx -y @mvanhorn/printing-press-library install llamaswap --cli-only
```

Then install the focused Hermes skill.

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-llamaswap --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-llamaswap --force
```

Restart the Hermes session or gateway if the newly installed skill is not visible immediately.

## Install for OpenClaw
Install both the CLI binary and the focused OpenClaw skill. The installer defaults binaries to a per-user bin directory (`$HOME/.local/bin` on macOS/Linux, `%LOCALAPPDATA%\Programs\PrintingPress\bin` on Windows):

```bash
npx -y @mvanhorn/printing-press-library install llamaswap --agent openclaw
```

Restart the OpenClaw session or gateway if the newly installed skill is not visible immediately.

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/llamaswap-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "llamaswap": {
      "command": "llamaswap-pp-mcp"
    }
  }
}
```

</details>

## Quick Start

```bash
# Verify the proxy is reachable and see deployment findings (version, keep-set residency, config-dir mode)
llamaswap-pp-cli doctor

# The configured roster with live status and aliases — config truth vs VRAM truth in one table
llamaswap-pp-cli models list

# What actually holds VRAM right now, with TTL deadlines
llamaswap-pp-cli ps

# Mirror the server's ephemeral request history into local SQLite before the next restart zeroes it
llamaswap-pp-cli sync

# Diff every live seat's running flags against the YAML on disk — catches edits that never landed
llamaswap-pp-cli config drift

```

## Unique Features

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
- **`residency`** — Reconstruct each seat's load/evict timeline from request gaps, and cost a different idle-TTL with keep-set and eviction-group safety.

  _Answers "what would a longer/shorter TTL on this seat save or cost" with measured numbers before you edit the YAML. A simulation over history, not a traffic re-run; `replay` is a hidden alias._

  ```bash
  llamaswap-pp-cli residency --ttl qwen3.8-27b=900 --since 7d --json
  ```
- **`saturation`** — Per-seat error and load pressure: 429/5xx counts and rates, request volume, and the hourly load curve.

  _Reach for it during or after a 429 storm to see which seats are rejecting and when the load lands. In-flight concurrency is intentionally omitted — llama-swap timestamps are whole-second, so depth would be a clock artifact._

  ```bash
  llamaswap-pp-cli saturation --since 24h --json
  ```

### Guardrails with teeth
- **`verify`** — Embeds a fixed probe on the embedder and reranks a fixed pair on the reranker, asserting both against stored calibrated baselines — catching dropped flags that roster counting cannot.

  _Run after every config change or restart; it is the step of the manual ritual that has historically caught real outages._

  ```bash
  llamaswap-pp-cli verify --probe --json
  ```

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

## Usage

Run `llamaswap-pp-cli --help` for the full command reference and flag list.

## Paths & environment variables

This CLI separates local files into four path kinds:

| Kind | Contents |
|------|----------|
| `config` | User-editable settings such as `config.json` and saved profiles |
| `data` | Durable local data such as `data.db` |
| `state` | Runtime state such as persisted queries, jobs, and `teach.log` |
| `cache` | Regenerable HTTP/cache files |

Each kind resolves independently. The ladder is:

1. Per-kind env var: `LLAMASWAP_CONFIG_DIR`, `LLAMASWAP_DATA_DIR`, `LLAMASWAP_STATE_DIR`, or `LLAMASWAP_CACHE_DIR`
2. `--home <dir>` for this invocation
3. `LLAMASWAP_HOME` for a flat relocated root
4. XDG env vars: `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`
5. Platform defaults matching existing installs

For containers and agent sandboxes, prefer a single relocated root:

```bash
export LLAMASWAP_HOME=/srv/llamaswap
llamaswap-pp-cli doctor
```

Under `LLAMASWAP_HOME=/srv/llamaswap`, the four dirs resolve to `/srv/llamaswap/config`, `/srv/llamaswap/data`, `/srv/llamaswap/state`, and `/srv/llamaswap/cache`.

MCP servers do not receive CLI flags from the host. Put relocation in the host `env` block:

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

Precedence matters in fleets: an ambient per-kind variable such as `LLAMASWAP_DATA_DIR` overrides an explicit `--home` for that kind. Use `LLAMASWAP_HOME` or the per-kind variables for durable fleet relocation; treat `--home` as the weaker per-invocation lever.

Relocation is one-way. Unsetting `LLAMASWAP_HOME` does not move files back to platform defaults, and `doctor` cannot find files left under a former root. Move the files manually before unsetting relocation variables.

Existing installs keep working because the platform-default rung matches the legacy layout. Run `llamaswap-pp-cli doctor --fail-on warn` to check path warnings in automation.

## Commands

Run `llamaswap-pp-cli <command> --help` for flags on any line below.

### Operations commands

What is loaded, what happened, and what to do about it. These are hand-built on top of the proxy's routes — they are where the local mirror, the keep-set protection, and the drain logic live.

- **`llamaswap-pp-cli ps`** - Models currently holding VRAM: NAME, STATE, CTX, TTL, PORT, UPTIME. TTL is read from the llama-swap YAML, not from `/running` (see `models running` below for why).
- **`llamaswap-pp-cli load`** - Warm a model into VRAM on purpose, with progress and a swap report.
- **`llamaswap-pp-cli kill`** - Cancel in-flight requests by id or by model — the polite alternative to unloading mid-generation.
- **`llamaswap-pp-cli keepset status`** - Is each keep-set model resident, and is it actually answering? "Listed" is not "resident" is not "answering"; this asserts the third. A configured-resident member that is missing is drift (exit 25).
- **`llamaswap-pp-cli keepset audit`** - When the memory-stack models were actually resident, every eviction attributed to its cause, and how much embed/rerank traffic landed inside a degraded window. Requires accumulated `sync` history: a cold mirror reports UNKNOWN, not clean.
- **`llamaswap-pp-cli sync`** - Pull the server's in-memory activity and event stream into local SQLite, sealing each server epoch and quantifying what the ring buffer already dropped. Every historical query below reads what `sync` stored.
- **`llamaswap-pp-cli swaps`** - Cold-load percentiles per model, minutes lost to swapping, and which model pairs repeatedly evict each other (`--thrash`). Requires accumulated `sync` history: a cold mirror reports UNKNOWN, not clean.
- **`llamaswap-pp-cli events`** - Consume the /api/events SSE stream as NDJSON — drains briefly by default, follows with `-f`. This is the live-follow surface.
- **`llamaswap-pp-cli service status`** - Is llama-swap up, which process is it, and what did the launcher last do. Read-only: the CLI prints the elevated restart command rather than running it.
- **`llamaswap-pp-cli doctor`** - Check CLI health: resolved paths, proxy reachability, and deployment findings.
- **`llamaswap-pp-cli verify`** - Post-restart verification: roster count, keep-set ANSWERING, and calibrated memory-stack probes. `--probe` compares against baselines recorded once by `verify --probe --init`; without that one-time calibration it exits 24, and exit 26 means the stack answered but DEGRADED.
- **`llamaswap-pp-cli models list`** - Every CONFIGURED model (from config YAML), not just loaded ones. Keys per entry: `id`, `object`, `created`, `owned_by`, `name`, `meta.llamaswap.aliases`, `status.value`. CRITICAL: `id` is the canonical id and harness-bound aliases hide in `meta.llamaswap.aliases` — matching id alone reports a correctly-served alias as missing (the exact bug in 2 of 3 clients in the offload-harness today). This roster is CONFIG-derived: a model whose GGUF file was never downloaded still appears here and fails only when called.
- **`llamaswap-pp-cli models running`** - Only the models currently holding VRAM: {running:[{model, state, cmd, proxy, ttl, name, description}]}. `state` is "ready" once health-checked. Do NOT read keep-set membership out of this `ttl`: the server reports `ttl: 0` for seats configured `ttl: -1` (verified live on this deployment), so 0 means "no value the server will vouch for", not "deliberately resident". Read the keep-set from `keepset status` or `ps`, both of which take TTL from the YAML. This is still the ground truth for *what is loaded* — /v1/models is config, /running is VRAM.
- **`llamaswap-pp-cli models unload`** - Unload ONE model by id or alias. Use `--drain` (with `--drain-timeout`, default 30s) to wait for in-flight work first; it fails closed — exit 21 on timeout, exit 22 when slot state is unreadable — and unloads nothing in either case. Keep-set members are refused (exit 20) by id or alias unless `--force-keepset`. Without `--drain`: measured on this deployment, an unload issued during an in-flight generation returns in ~1.3s WITHOUT draining and the in-flight request dies with 502. Route exists on v24x+.
- **`llamaswap-pp-cli models unload-all`** - Unload every running model EXCLUDING the keep-set, issued as one per-model unload per target. The non-selective bulk routes (POST /api/models/unload, legacy GET /unload) take the keep-set down with everything else, so they are used ONLY when no keep-set member is currently resident, or when `--force-keepset` was passed; otherwise the command refuses and says why rather than quietly widening the blast radius. On this deployment the keep-set is the mem0 memory stack (embeddinggemma + bge-reranker-v2-m3).
- **`llamaswap-pp-cli activity log`** - Per-request activity log: id, timestamp, model, HTTP status, cached/prompt/generated token counts, prompt speed, generation speed, duration. Model-scoped, paginated and sortable via query params (verified live against the v249 UI). This history is IN-MEMORY — a proxy restart zeroes it, which is exactly why the CLI mirrors it into SQLite.
- **`llamaswap-pp-cli activity performance`** - Performance metrics feed backing the UI's Performance page (per-model speed distributions).
- **`llamaswap-pp-cli activity prometheus`** - Prometheus exposition of proxy metrics. Returns 503 when monitoring is disabled in config — treat 503 as "feature off", not proxy-down.
- **`llamaswap-pp-cli activity stats`** - Aggregated activity stats (request counts, token totals) since proxy start.
- **`llamaswap-pp-cli captures`** - Full request/response capture for one activity entry — the body-level diagnostic for "what did the client actually send and what came back". 404 when capture wasn't enabled or the buffer rotated past this id.
- **`llamaswap-pp-cli captures export`** - Bulk-export captures to JSONL — the eval/regression dataset the UI cannot produce. `--out` is required; narrow with `--model`, `--since`, `--status`, `--last`.
- **`llamaswap-pp-cli inflight`** - Cancel one in-flight request by its id (ids visible in the activity feed / UI in-flight table). The polite alternative to unloading a model out from under a stuck request.
- **`llamaswap-pp-cli logs`** - Buffered recent logs as plain text (proxy + upstream stdout/stderr interleaved). One-shot only — this command has NO follow mode. For live tailing use `events -f`; to make sense of a large buffer use `logs triage`.
- **`llamaswap-pp-cli logs triage`** - Classify the buffered proxy log into the error taxonomy, with counts and buffer positions — the way to read a log buffer without pulling all of it into context.

### Config intelligence commands

Read-only analysis of the llama-swap YAML. None of these ever writes your hand-commented config; the write-shaped ones produce a plan.

- **`llamaswap-pp-cli config validate`** - Check a config against the embedded llama-swap JSON schema, plus nearest-key hints for unknown top-level keys.
- **`llamaswap-pp-cli config lint`** - Semantic checks llama-swap's own boot validation does not make: macros, aliases, ports, ttl semantics, missing files, routing coherence.
- **`llamaswap-pp-cli config explain`** - The fully resolved view of one seat: raw block with comments, macro-expanded cmd, aliases, ttl, env, seat kind.
- **`llamaswap-pp-cli config diff`** - Semantic diff between two configs: per-model added/removed/changed flags, with the comment blocks that changed alongside.
- **`llamaswap-pp-cli config drift`** - Compare every RUNNING seat's live command line against the config file, flag by flag. Exit 25 when they diverge — a finding, not an error.
- **`llamaswap-pp-cli config backup`** - Copy the live config to a content-addressed backup and record it in a sidecar index; report dedup and orphan backups.
- **`llamaswap-pp-cli config apply`** - PLAN a config change: unified diff vs live, a content-addressed backup, the exact elevated restart command, and the post-restart verify plan. Never writes.
- **`llamaswap-pp-cli config testinstance`** - Boot a throwaway llama-swap on a scratch port, count the models it registers, then kill it. Exit 23 on a port conflict.
- **`llamaswap-pp-cli seat log`** - A per-model chronology of every flag change mined from the dated config-backup series, with the comment that landed alongside each one.
- **`llamaswap-pp-cli seat show`** - One seat's live command line vs the file, flag by flag.
- **`llamaswap-pp-cli seat try`** - PLAN a seat flag change: the would-be command, a unified diff, the restart command, and an acceptance probe. Never writes.
- **`llamaswap-pp-cli bind check`** - Every model name a consuming tool is configured with must resolve in the live roster; reports dangling bindings and unbound seats.

### Measurement commands

- **`llamaswap-pp-cli bench`** - Benchmark seats through the production route, with the serving config identity attached — so a number is never separated from the flags that produced it. Prompt processing and generation are reported **separately**, each as `mean ± sample stddev (n-1)` over `--runs`, and a spread above 3% of the mean is flagged `UNSTABLE` rather than averaged away.
  - `--depth N[,N...]` measures at one or more **KV depths** (llama-bench's `-d`): N tokens are prefilled before the timed window opens, and the prefill is excluded from the timing. The observed `cache_n` is reported, so a prefill that failed to stick is called out as `PARTIAL depth` instead of being published as a deep-context rate. Benching at depth 0 only **overstates** a seat — both rates decay as the cache fills.
  - `--standard` emits the community-canonical **pp512 / tg128** markdown row with the build, hardware line, and comparability sha.
  - Every row records a **comparability key** over the llama.cpp build, the host, the weights file, and every seat flag that moves a number.
- **`llamaswap-pp-cli bench compare`** - Diff two recorded bench rows, **refusing (exit 29)** when their comparability keys differ, and naming the fields that differ. A delta smaller than the two rows' combined run-to-run spread is reported as noise, not as a regression.
- **`llamaswap-pp-cli bench aux`** - Latency and throughput for the resident embedder and reranker.
- **`llamaswap-pp-cli metrics <model>`** - Parse a seat's llama-server Prometheus exposition into typed telemetry. `--delta 5s` scrapes twice and reports counters as a **windowed rate** (a single scrape of a counter is a lifetime total, not a rate). `requests_deferred > 0` is surfaced as a `slots_too_low` finding. Serves `metrics_enabled:false` and **exit 0** when the seat lacks `--metrics`. Note `kv_cache_usage_ratio` no longer exists upstream; its absence is reported as a removal, not a fault.
- **`llamaswap-pp-cli vram`** - Per-GPU-UUID VRAM snapshot with explicit baseline/after/delta.
- **`llamaswap-pp-cli fit`** - Will this model at this context fit the cards, as an interval with a refuse-to-answer band (exit 28 inside the band). Takes a loaded model id or a `.gguf` path; it never starts a model to answer, so a bare id that is not loaded exits 3. Four header facts also make it refuse rather than emit a confident wrong number: a **sharded** model (it sums every sibling shard when the whole set is on disk, and refuses when one is missing), **MLA** (compressed latent KV), **SSM/Mamba** (fixed recurrent state), and a **non-model** GGUF (adapter / imatrix / mmproj).
- **`llamaswap-pp-cli ctx`** - Real tokens vs the seat's live n_ctx: room left, and KV cost at a target context. Also reports the model's **native (pre-extension) window** beside its declared one, so serving a YaRN-extended model past its training context is visible as the quality decision it is. Uses `meta.n_ctx` from the roster as a fast path on servers newer than v249, falling back to the `/props` round trip with an explicit note.
- **`llamaswap-pp-cli gguf`** - Read a GGUF file's header: architecture, layers, GQA heads, native context, quantization. Reports a **measured bits-per-weight histogram** derived from the tensor table (which catches a file whose `general.file_type` label disagrees with what is stored), **shard membership** (`split.count` / `split.no`, with the sibling shards resolved and summed), **MoE total-vs-active parameters**, **RoPE scaling**, `pooling_type` (RANK ⇒ reranker), and `general.type` (adapter / imatrix / mmproj are identified, never treated as models). The `LLAMA_FTYPE_GUESSED` bit is masked and labelled `(guessed)`.
- **`llamaswap-pp-cli gate grammar`** - Does this seat actually enforce a GBNF grammar on the chat route?
- **`llamaswap-pp-cli gate tools`** - Does this seat emit a well-formed tool call?
- **`llamaswap-pp-cli scratch`** - Run an ephemeral eval seat derived EXACTLY from a production command line.
- **`llamaswap-pp-cli build check`** - Proxy version plus the llama.cpp build actually serving each loaded seat.

### API passthrough commands

Thin wrappers over the proxy's own routes.

- **`llamaswap-pp-cli server hardware`** - Hardware detection as llama-swap sees it (GPUs, VRAM). Added in v247 — returns 404 on older servers; treat 404 as "feature absent", not an error.

- **`llamaswap-pp-cli server health`** - Liveness of the llama-swap proxy itself (plain "OK"). This says NOTHING about any model being loaded or ready — a green /health with an empty /running is the normal idle state. For per-model readiness use upstream health.

- **`llamaswap-pp-cli version drift`** - Compares the llama-swap surface this CLI was verified against (`v249`) with the live server, and reports **which backend actually answered**. llama.cpp's own `llama-server` has shipped a native model-swapping router mode since Dec 2025 that serves a similarly shaped `/models`; pointing this CLI's admin commands at one produces 404s that look like faults, and the detector names it instead. Exit 25 when the server is older than the verified surface (a finding, not an error). Read-only.

- **`llamaswap-pp-cli server version`** - Returns {version, commit, build_date}. The capability gate for everything else: per-model unload routes need v24x+, /api/hardware needs v247+, profiles/selectors need v241+. Cache this and gate optional commands on it instead of guessing from 404s.

- **`llamaswap-pp-cli profiles activate`** - Switch the active profile. Body is the profile selection object.
- **`llamaswap-pp-cli profiles list`** - List configured profiles and which one is active. 404 on pre-v241 servers.

TRAP for every `upstream` command: any request here AUTO-STARTS the target model if it is not running — a multi-GB load triggered by a "probe". Gate inspection on `ps` first.

- **`llamaswap-pp-cli upstream apply-template`** - Render OpenAI-style messages through the model's chat template WITHOUT generating — returns the exact prompt string. Pair with tokenize for precise budget math.

- **`llamaswap-pp-cli upstream detokenize`** - Convert token ids back to text.
- **`llamaswap-pp-cli upstream embeddings`** - Embeddings from an embedding-capable model (on this deployment embeddinggemma, 768-dim, resident). Also dispatchable proxy-wide at /v1/embeddings; the upstream form pins the exact model.

- **`llamaswap-pp-cli upstream health`** - Per-model readiness ({"status":"ok"} once loaded; 503 while loading).
- **`llamaswap-pp-cli upstream lora-adapters`** - List LoRA adapters loaded on this model and their scales.
- **`llamaswap-pp-cli upstream lora-set`** - Set LoRA adapter scales on this model.
- **`llamaswap-pp-cli upstream metrics`** - Per-model llama-server Prometheus metrics (needs --metrics on the seat; 404 otherwise).
- **`llamaswap-pp-cli upstream open`** - Print a model's llama.cpp passthrough URL — or open it in a browser with `--launch`.
- **`llamaswap-pp-cli upstream props`** - llama-server properties for one model: build_info, model_path, chat_template, modalities, total_slots, default_generation_settings (incl. the REAL loaded n_ctx — the authority for context-window sizing; assuming ctx from config has killed real runs with exceed_context_size 400s).

- **`llamaswap-pp-cli upstream rerank`** - Rerank documents against a query with a reranker model (on this deployment bge-reranker-v2-m3, resident, CPU-bound). Note the harness config declares rerank as a first-class roster member but today only a bench script can reach it — this command closes that gap.

- **`llamaswap-pp-cli upstream slots`** - Per-slot state incl. is_processing — the drain signal before unload. CAVEATS: needs llama-server started with --slots (404/501 otherwise — on this deployment most seats do not pass it); on non-llama.cpp upstreams (whisper-server) 404 is ambiguous with "not loaded" and must be cross-checked against /running; slot state exposes prompt content, so treat output as sensitive.

- **`llamaswap-pp-cli upstream tokenize`** - Tokenize text with the model's own tokenizer. The honest answer to "does this fit the context window" — chars/4 estimates are ~2x off on Gemma-family tokenizers (measured).

### Framework commands

- **`llamaswap-pp-cli which`** - Resolve a natural-language capability query to the command that implements it. Exit 0 = at least one match, exit 2 = no confident match.
- **`llamaswap-pp-cli search`** - Search locally synced data.
- **`llamaswap-pp-cli analytics`** - Run analytics queries on locally synced data.
- **`llamaswap-pp-cli export`** - Export data to JSONL or JSON for backup, migration, or analysis.
- **`llamaswap-pp-cli import`** - Import data from JSONL file via API create/upsert calls.
- **`llamaswap-pp-cli configure`** - Emit ready-to-paste client configuration pointing at this llama-swap.
- **`llamaswap-pp-cli profile save|list|show|use|delete`** - Named sets of flags saved for reuse.


### Self-learning loop

This CLI caches per-question discovery so repeat queries skip the walk and structurally similar queries get answered via entity substitution. The loop also self-captures: every invocation is journaled locally, and failed-flag corrections plus fresh teaches surface as candidates on the next `recall` for confirm/reject judgment. Agents call `recall` before discovery and fire `teach &` after answering. See the `## Automatic learning` section in `SKILL.md` for the full protocol.

- **`llamaswap-pp-cli recall <query>`** - Look up cached resources for a query before running discovery
- **`llamaswap-pp-cli teach`** - Record a query -> resource mapping (silent on success, safe to background with `&`)
- **`llamaswap-pp-cli learnings list`** - Inspect taught rows
- **`llamaswap-pp-cli learnings forget <query> --all`** - Undo a teach. Needs a scope: `--all` wipes every rule for that query, or narrow with `--resource <id>` / `--action <boost|hide|alias_of>`
- **`llamaswap-pp-cli learnings candidates`** - List auto-captured candidates awaiting confirm/reject
- **`llamaswap-pp-cli learnings stats`** - Local loop metrics: recall hit rate, teach-to-reuse, playbook resolution, candidate counts
- **`llamaswap-pp-cli teach-pattern`** - Install a query/resource template up front
- **`llamaswap-pp-cli teach-lookup`** - Add an entity mapping (e.g. country code, team alias) for pattern substitution

Pass `--no-learn` or set `LLAMASWAP_NO_LEARN=true` to disable the loop for deterministic flows.

The local store's schema version stamp is one-way: once this version of `llamaswap-pp-cli` opens the database, older binaries refuse it with a version error — upgrade the binary rather than downgrading.

## Output Formats

Capture ids are the INTEGER activity-row ids, not UUIDs. Find ones that actually have a stored body first:

```bash
llamaswap-pp-cli activity log --json --select id,has_capture
```

Then, using `597` as an example id:

```bash
# Human-readable table (default in terminal, JSON when piped)
llamaswap-pp-cli captures --id 597

# JSON for scripting and agents
llamaswap-pp-cli captures --id 597 --json
# Filter to specific fields by name (capture fields: id, req_path, req_headers, req_body, resp_headers, resp_body)
llamaswap-pp-cli captures --id 597 --json --select id,req_path,resp_body

# Dry run — show the request without sending
llamaswap-pp-cli captures --id 597 --dry-run

# Agent mode — JSON + compact + no prompts in one flag
llamaswap-pp-cli captures --id 597 --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select <field>[,<field>...]` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **No write endpoints** - this API is read-and-control only, so the global `--idempotent` flag is inert here; there is nothing to re-create
- **Confirmable** - `--yes` for explicit confirmation of destructive actions
- **Piped input** - commands accept structured input when their help lists `--stdin`
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set
- **Structured errors** - every non-zero exit under `--json`/`--agent` emits one machine-readable envelope:

```json
{"ok": false,
 "error": {"code": "server_unreachable", "category": "unavailable", "retryable": true,
           "http_status": 0, "message": "...", "remediation": "...", "exit_code": 4}}
```

  `category` is one of `fix_request`, `retry`, `unavailable`, `refusal`, `internal` — the four decisions a caller can make. Branch on `code`, never on `message`. The envelope covers **every** exit path, including cobra's pre-parse flag errors (detected by scanning argv, because a parse failure aborts before flags are bound) and dial failures. It goes to **stdout** while stdout is clean, and to **stderr** once a command has already written a result document there — so a refusal that printed its report first (`bench compare`) still leaves exactly one JSON document on stdout. Human mode is unchanged: no envelope, same stderr line, same exit code.

### Exit codes

Framework codes: `0` success, `2` usage error, `3` not found (model/alias not in the roster, checked against ids AND `meta.llamaswap.aliases`), `4` server unreachable, `5` API error, `7` rate limited, `10` partial failure.

llama-swap-specific codes — the ones an unattended run branches on:

| Code | Meaning |
|------|---------|
| 20 | Keep-set refusal — the operation would touch a protected keep-set member (matched by id or alias from config, never server ttl) |
| 21 | Drain timeout — `--drain` could not confirm idle within the timeout; nothing was unloaded (fail closed) |
| 22 | Drain unobservable — /slots was unreadable (timeout/5xx); nothing was unloaded (fail closed) |
| 23 | Port conflict — a scratch/test instance port is already listening or sits inside the startPort span / reserved band |
| 24 | Config invalid — schema or semantic validation failed. Also the setup-gap code: `verify --probe` with no baseline recorded yet |
| 25 | Drift — live process flags diverge from the file (`config drift`, `seat show --diff-yaml`). Not an error; a finding |
| 26 | Probe failed — `verify --probe` found the memory stack answering but DEGRADED (outside stored tolerance) |
| 27 | Upstream 5xx — the upstream model server answered 5xx |
| 28 | Refused to guess — `fit`/`ctx` lands inside the uncertainty band, OR the GGUF makes the standard KV formula inapplicable (incomplete shard set, MLA, SSM, or a non-model file). The message names the measurement that would settle it |
| 29 | Not comparable — `bench compare` was asked to diff two rows with different comparability keys; their difference would measure the configuration change, not the thing being compared |

## Health Check

```bash
llamaswap-pp-cli doctor
```

Verifies configuration and connectivity to the API.

## Configuration

Run `llamaswap-pp-cli doctor` to see the resolved config, data, state, and cache directories. The platform-default config path is `~/.config/llamaswap-pp-cli/config.json`; `--home`, `LLAMASWAP_HOME`, and per-kind env vars can relocate it.

Static request headers can be configured under `headers`; per-command header overrides take precedence.

## Troubleshooting
**Not found errors (exit code 3)**
- Exit 3 means the model or alias you named is not in the roster. It is checked against both `id` and `meta.llamaswap.aliases`, so an exit 3 means neither matched.
- Run `llamaswap-pp-cli models list` to see the configured roster and every alias, or `llamaswap-pp-cli ps` for just what is resident.

### API-specific
- **unload refuses with a keep-set error** — That model backs the memory stack (matched by id or alias from config, never server ttl). Override requires --force-keepset; check `keepset status` first.
- **slots returns 404 for a model** — Either the seat lacks --slots in its cmd or the model is not loaded — cross-check with `ps`; a cold /upstream probe would auto-start the model, so this CLI gates on /running first.
- **activity/stats show far fewer requests than expected** — Server metrics reset on every restart, and `activity stats` reads the server. Run `sync` regularly, then ask the local mirror instead: `analytics` for request/error rates, `swaps` for cold-load and eviction cost, `keepset audit` for residency history. There is no top-level `stats` command.
- **commands hang ~21s then fail** — Something resolved localhost to ::1. This CLI pins 127.0.0.1; if you overrode --host, use the IP form.

## Sources & Inspiration

This CLI was built by studying these projects and resources:

- [**ollama**](https://github.com/ollama/ollama) — Go (100000 stars)
- [**llama.cpp router mode**](https://github.com/ggml-org/llama.cpp) — C++ (75000 stars)
- [**llama-swap (server + web UI)**](https://github.com/mostlygeek/llama-swap) — Go (5341 stars)
- [**lms (LM Studio CLI)**](https://github.com/lmstudio-ai/lms) — TypeScript (3000 stars)
- [**llama-swappo**](https://github.com/kooshi/llama-swappo) — Go (30 stars)
- [**mcp-llama-swap**](https://github.com/oussama-kh/mcp-llama-swap) — Python (5 stars)

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
