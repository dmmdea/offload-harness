# CLAUDE.md — agent orientation map for offload-harness

Local-first Go harness that offloads grunt work (summarize/classify/extract/triage + vision/OCR/STT/
media-gen) to a free **Gemma-4 cascade** on llama.cpp. Ships as a CLI, an **MCP server**
(`local-offload`), and an optional local **coding agent** (`local-agent`). Never calls cloud; on low
confidence it returns a structured **defer** and the caller does the task. Every command below was
executed successfully while writing this file.

## Components & ports

| Binary / service | Role | Port | Config | Health check |
|---|---|---|---|---|
| `llama-swap` (fronts llama.cpp) | Serves the model tiers | `127.0.0.1:11436` | `$OFFLOAD_HOME\llama-swap.yaml` | `local-offload doctor` → `health: OK` |
| `local-offload` | Offload CLI + MCP (stdio) | — (talks to :11436) | `~/.local-offload/config.json` | `local-offload doctor` |
| `local-agent` | Coding-agent loop; one-shot, `--queue`, or `--serve` | `127.0.0.1:18800` (serve, OPTIONAL, OFF by default) | shares harness config; flags | `curl 127.0.0.1:18800/v1/models` |
| OpenWebUI | Chat GUI over the agent (OPTIONAL) | `127.0.0.1:8081` | env in `scripts/openwebui-stack.sh` | `curl 127.0.0.1:8081/health` |

The `--serve` endpoint is **unauthenticated** and drives write/GitHub tools → **loopback-only**;
`--listen-trusted-network` is required to bind beyond loopback (loud warning).

## Model tiers (served by llama-swap on :11436)

| Alias | Config key | Role |
|---|---|---|
| `gemma4-e2b` | `triage_model` | Fast entry tier — triage / classify. |
| `offload-e4b` (alias `gemma4-e4b`) | `model` | Workhorse — summarize / extract; default agent planner. |
| `gemma4-26b-a4b` | `escalation_model` / `reasoning_model` | MoE tier tried before deferring. |
| `embeddinggemma` | (memory stack) | Embeddings. |
| `qwen3vl-4b`, `whisper-stt`, `whisper-stt-hq` | `vision_model` / `stt_model[_hq]` | Vision + speech (optional; absent on a grunt-work-only install). |

The cascade enters small and escalates only on validation failure or low decision-confidence; all
tiers exhausted → **defer**. It is model-family, not vendor, specific — CUDA/Vulkan/CPU all serve the
same aliases via the templates in `setup/templates/`.

## Golden commands (all verified on this machine)

```bash
go build ./...                         # build everything — must stay green
go test ./...                          # test suite
go vet ./...                           # vet gate
```

```powershell
# Point at the setup config so the tier table reads the served offload-family aliases.
local-offload --config setup\templates\config.json models     # -> tier routing table
local-offload --config setup\templates\config.json doctor      # -> "health: OK" + each alias OK
local-offload --config setup\templates\config.json ledger --since 1   # token-savings JSON
echo "some text" | local-offload --config setup\templates\config.json summarize - --max-points 3 --json
```

```powershell
# Install / verify the stack (Windows):
pwsh -NoProfile -File setup\detect.ps1      # backend verdict JSON
pwsh -NoProfile -File setup\install.ps1     # pinned binaries + models + Go build
pwsh -NoProfile -File setup\selftest.ps1    # receipt JSON: verdict pass|warn|fail
# Start it:
& "$env:OFFLOAD_HOME\llama-swap\llama-swap.exe" --config "$env:OFFLOAD_HOME\llama-swap.yaml" --listen 127.0.0.1:11436
```

```bash
# Coding agent (from repo root, after `go build -o local-agent ./cmd/local-agent`):
local-agent --root . --base http://127.0.0.1:11436 --max-steps 4 "list the files and summarize README.md"   # read-only one-shot
local-agent --profile edit --allow-write --allow-overwrite "rename oldName to newName in util.go"            # narrowed toolset + few-shot
local-agent --two-tier --allow-write "add a pricing section to index.html"                                   # architect(26B)->editor(E4B), one swap
local-agent --serve --listen 127.0.0.1:18800 --base http://127.0.0.1:11436                                   # OpenAI server
# Legacy WSL/NVIDIA GUI stack:
bash scripts/openwebui-stack.sh
```

## Agent tools & flags (component map)

Tools live in `internal/agent/`. Read-only by default: `list_dir`, ranged `read_file`
(`offset`/`limit`, `tools.go`), `search_files` (regex/glob, 100-match cap, `greptool.go`),
`summarize_file` (offload digest, `tools.go`), the in-process `offload_*` cascade. Opt-in (each
behind an `--allow-*`): `write_file`/`edit_file`/`delete_file`, `web_fetch`, `web_search`, `run`
(`runtool.go`), `run_shell` (`shelltools.go`, **Linux only**), the `github_*` tools. Working memory:
`update_plan` + AGENT.md loading (`worktree_memory.go`, re-inject cadence). Profiles + exemplars in
`profiles.go`; two-tier architect/editor in `twotier.go`; transcript compaction in `compaction.go`.

Key flags (all `--allow-*` OFF by default): `-ctx-tokens` (default 16384; compaction budget = match
the served `--ctx-size`), `-profile general|edit|build|research|github` (narrows tools + adds a tuned
prompt/exemplars; can only narrow), `-allow-run` (the allowlisted direct-exec `run` tool),
`-allow-shell` (Linux-only `run_shell`), `-two-tier` + `-architect-model` (default `gemma4-26b-a4b`)
/ `-editor-model` (default `offload-e4b`). `--profile` and `--two-tier` are mutually exclusive.

## Invariants — DO NOT BREAK

1. **Grammar-reliable serving flags** (model-family, every backend): `--jinja --reasoning off`,
   f16 KV cache (`--cache-type-k/v f16`), `--flash-attn on` (except CPU + STT), **no MTP/draft**.
   Never `--json-schema` / `response_format` — they crash the model; the harness passes a raw GBNF
   `grammar` field. `--reasoning off` is mandatory or output comes back empty.
2. **Defer, never crash.** A `{"deferred":true,...}` result is a *valid* success signal (low
   confidence / over-long / all tiers failed), not an error. Do not "fix" defers by adding a cloud
   fallback — the harness holds no cloud credentials by design.
3. **Policy-broker order:** the agent's single broker enforces the step/tool caps (esp.
   `--max-same-tool`, the exact-repeat circuit breaker) *before* executing a tool. Capability flags
   (`--allow-write/-overwrite/-delete/-fetch/-search/-run/-shell/-github`) are all **OFF by default**.
   The `run` tool (`--allow-run`) execs an **allowlisted program directly, no shell** (`go`, `gofmt`,
   `python`, `python3`, `pytest`, `npm`, `node`, `cargo`, `git`; bare name only, resolved on the
   trusted PATH). **Confinement differs by OS:** Linux uses the Landlock+seccomp+userns cage (no
   network, FS-confined); native Windows uses a Job Object + low-integrity token — **writes** outside
   the worktree are blocked but **reads and network are NOT contained** (weaker than Linux; the tool
   description says so). `run_shell` (`--allow-shell`) is **Linux only**.
4. **Worktree confinement:** writes are confined to `--worktree` (default `--root`); the agent must
   never write outside it or into `.git`.
5. **Audit trail lives OUTSIDE any worktree** (`~/.local-offload/agent-audit.jsonl`) so a run cannot
   tamper with its own log.
6. **Loopback-only serve:** `local-agent --serve` refuses a non-loopback `--listen` unless
   `--listen-trusted-network` is passed. Keep it that way.

## Where things live

| Path | What |
|---|---|
| `main.go` | The `local-offload` CLI + MCP entry (subcommand dispatch, doctor/ledger/models). |
| `internal/pipeline/` | The offload cascade (tiers, escalation, grounding, recordless path for the agent). |
| `internal/agent/` | Agent loop, tools, and the policy broker (`Build()` is the shared constructor). |
| `internal/sandbox/` | OS cage for the runner: Landlock+seccomp+userns on Linux (`run` + `run_shell`); Job Object + low-integrity token on Windows (`run` only — writes confined, reads/network NOT). |
| `cmd/local-agent/` | The coding-agent CLI (`main.go`) + OpenAI server (`serve.go`, loopback guard). |
| `internal/mcpserver/` | MCP tool surface (incl. `agent_run`). |
| `setup/` | Cross-vendor installer: `detect.ps1`, `install.ps1`, `selftest.ps1`, `templates/`, `SETUP-AGENT.md`. |
| `skill/local-offload-setup/` | Thin setup-skill wrapper pointing at `setup/SETUP-AGENT.md`. |
| `config.example.json` | Full config with defaults (kept in lockstep via `go generate ./...`). |

## When lost

- **Installing?** → `setup/SETUP-AGENT.md` (agent runbook, decision tables keyed to script JSON).
- **Operating / diagnosing?** → `docs/OPERATOR-GUIDE.md` (task walkthroughs).
- **What a subcommand does?** → `local-offload` with no args prints usage; `README.md` has the full
  CLI + MCP tool tables.
