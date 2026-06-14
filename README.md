# local-offload

A local **offload harness** for [Claude Code](https://claude.com/claude-code) (and any MCP client): delegate short‑context, low‑judgment grunt work — **summarize / classify / extract / triage** — to a **free local Gemma‑4 cascade** served by [llama.cpp](https://github.com/ggml-org/llama.cpp). Your cloud model keeps all the judgment; the harness **never calls a cloud model**. When it can't do a task confidently it returns a structured **defer** so the caller handles it.

The point: bulk summarize/classify/extract/triage tokens never enter your cloud context. The built‑in `ledger` proves the savings over time.

```
┌─ Claude (Opus, etc.) ──────────────────────────────┐
│  "summarize this 40-page log"                       │
│        │  MCP call: offload_summarize               │
│        ▼                                             │
│   local-offload  ──►  Gemma-4 cascade (local, free) │
│        ▲                  E2B → E4B → 26B-A4B        │
│        └── result JSON, or {deferred:true} ─────────┘
```

## The cascade

Fast tasks enter at the small tier and climb only when genuinely uncertain:

- **triage / classify** → `gemma4-e2b` (~120 tok/s) — fast entry tier
- **summarize / extract** → `offload-e4b` (~83 tok/s) — the workhorse
- on a validation failure **or a low decision‑confidence signal** → escalate to `gemma4-26b-a4b` (MoE, ~16 tok/s, experts in RAM — near‑frontier quality)
- all local tiers fail → **defer** to the caller (the harness has no cloud credentials)

For triage/classify the harness requests per‑token logprobs and computes a **class‑mass margin** at the decision token; a margin below a learned threshold means the model was genuinely torn → escalate instead of guessing.

## Requirements

- **A bash environment**: Linux, macOS, or Windows + WSL2. (The installer scripts are POSIX/bash; native‑Windows building is out of scope.)
- **NVIDIA GPU, ≥ 8 GB VRAM** (all three tiers fit on 8 GB — one model on the GPU at a time, swap‑exclusive).
- **≥ 32 GB system RAM** (≥ 64 GB ideal — the 26B‑A4B MoE keeps its experts in RAM via `--cpu-moe`).
- **CUDA toolkit 12.x** with `nvcc` (avoid 13.x — an MMQ crash falls back to cuBLAS and regresses decode).
- **Go 1.26+**, plus `git`, `cmake`, `python3`, and the Hugging Face CLI (`pip install huggingface_hub`).
- ~20 GB free SSD (E4B ~4 GB + E2B ~2.5 GB + 26B‑A4B ~14 GB + build).

> **Models & license:** the cascade uses Google's **Gemma‑4** weights (Unsloth QAT `UD‑Q4_K_XL` GGUFs). Their use is governed by the [Gemma Terms of Use](https://ai.google.dev/gemma/terms); accept them on Hugging Face before downloading. This repository ships **no** model weights.

## Install

### Guided (recommended) — via the bundled Claude Code skill

This repo bundles a [`skill/`](skill/) that walks Claude Code through the whole install (hardware gate → build llama.cpp → pull models → build the CLI → wire serving → register MCP), baking in every hard‑won gotcha so the install is turnkey.

```bash
git clone https://github.com/dmmdea/local-offload.git ~/local-offload
# make the skill available to Claude Code (copy or symlink):
cp -r ~/local-offload/skill ~/.claude/skills/local-offload-setup
```

Then in Claude Code: **"set up the local-offload harness"**. The skill is operator‑driven — every install location is a prompt/env var with a sensible `$HOME`‑relative default; nothing is hardcoded.

### Manual

```bash
git clone https://github.com/dmmdea/local-offload.git ~/local-offload
cd ~/local-offload
bash skill/scripts/detect.sh          # checks GPU / CUDA / Go / RAM / disk
HARNESS_SRC=$PWD bash skill/scripts/setup.sh   # idempotent: build + pull family + configure
go build -o local-offload .           # (setup.sh also does this)
```

Override any install path via env var before `setup.sh`:
`MODELS_ROOT`, `LLAMACPP_DIR`, `HARNESS_SRC`, `CONFIG_OUT`, `LLAMASWAP_PORT`, `WITH_FAMILY` (set `0` for E4B‑only, no cascade), `GPU_ARCH`, `CUDA_HOME`.

## Serving (the model)

Verified **grammar‑reliable** serving — common flags per tier (**no MTP**, see below):

```
--ctx-size 8192 --flash-attn on --cache-type-k f16 --cache-type-v f16 --jinja --reasoning off
# E2B    : --n-gpu-layers 99
# E4B    : --n-gpu-layers 99 --parallel 1
# 26B-A4B: --cpu-moe --n-gpu-layers 999 --parallel 1   + env GGML_CUDA_DISABLE_GRAPHS=1
```

`setup.sh` emits an `offload-family.llama-swap.yaml` snippet (3 model defs + a swap‑exclusive group) to merge into your [llama-swap](https://github.com/mostlygeek/llama-swap) config. Without llama‑swap it writes a single‑model E4B launcher (`serve-offload.sh`, no cascade).

## Usage (CLI)

```bash
local-offload summarize notes.md --max-points 5 --json
local-offload classify ticket.txt --labels bug,feature,question --json
local-offload extract  invoice.txt --schema fields.json --json
local-offload triage   log.txt --question "Does this contain an error?" --json
local-offload ledger                 # token-savings report
local-offload doctor                 # endpoint health + config
local-offload models                 # show the active cascade
local-offload eval --dir testdata/eval   # code-based quality eval (selective accuracy, AURC, deferral-curve AUDC/QNC)
local-offload stats                  # observational per-task ledger telemetry
local-offload mcp                    # run as an MCP server (stdio)
```

Input is a file path or `-` for stdin. Config via `--config <path>` or `$LOCAL_OFFLOAD_CONFIG`.

## Usage (MCP)

```bash
claude mcp add local-offload --scope user -- "$HOME/local-offload/local-offload" mcp
```

Or in your MCP client config under `mcpServers`:

```json
"local-offload": {
  "command": "/absolute/path/to/local-offload",
  "args": ["mcp"]
}
```

Tools: `offload_summarize`, `offload_classify`, `offload_extract`, `offload_triage`. Each returns the result JSON or `{deferred:true, ...}`.

## Structured output (important)

Gemma‑4 **crashes** on llama.cpp `--json-schema` / `json_schema` / `response_format` ([#22396](https://github.com/ggml-org/llama.cpp/issues/22396)). This harness instead enforces a **GBNF grammar** via the `grammar` field, then forgivingly parses + schema‑validates the result in Go. Grammars are generated per request (no external dependency).

**Three serving rules the harness depends on** (they cost hours to rediscover):

- **No MTP / speculative decoding.** `--spec-type draft-mtp` + a GBNF `grammar` field → llama.cpp returns a 500 "logits computation" error. Serve with `-fa on`, f16 KV, no draft.
- **`--reasoning off` is mandatory.** Gemma‑4's thinking mode otherwise eats the short output budget before emitting the answer (empty content, `finish=length`).
- **Never `--json-schema`** — use the `grammar` field (above).

## Self‑learning (offline, inference‑free)

The harness logs rich signals per call (logprob margin, a deterministic grounding check, defer/retry/truncation, per‑tier infra errors, cheap input features) into an append‑only JSONL ledger, then tunes **its own** config from that data — pure Go stats over the ledger, **zero cloud tokens**. Run these on a schedule (cron / Task Scheduler):

```bash
local-offload calibrate     # per-task conformal margin thresholds -> thresholds.json
local-offload health        # per-tier EWMA/Page-Hinkley/CUSUM + P95 timeouts -> tier_overrides.json
local-offload train-router  # logistic entry-tier router from input features -> router-weights.json
local-offload optimize      # mine verified-good calls into BM25 few-shot exemplar pools
```

The pipeline loads the resulting JSON at startup. All four are idempotent and safe to re‑run.

### Learned correctness head (opt‑in)

`summarize`/`extract` have no decision‑token margin, so they get a separate pure‑Go logistic **correctness head** that predicts `p(correct)` from the call's features. It is adopted **only if it provably helps**, validated with a rigorous, leakage‑free gate:

```bash
local-offload confhead-eval       # out-of-fold AURC + AUGRC vs incumbent, paired-bootstrap CI -> ADOPT/REJECT per task
local-offload train-confhead      # fit the head over labeled ledger rows -> confhead-weights.json
local-offload confhead-calibrate  # conformal p(correct) thresholds at the target error rate -> confhead-thresholds.json
```

The adoption gate reports a per‑task verdict (ADOPT only when the 95% CI on ΔAURC excludes zero). When a head is adopted and you set `"confhead_enabled": true`, a call whose predicted `p(correct)` falls below its learned threshold escalates to the larger tier instead of being accepted — catching the inputs the workhorse is likely to get wrong. **Default off**; inert unless a head + threshold are present.

## Notes & limitations

- **bbolt cache is single‑writer**: when the long‑running MCP server holds it, a concurrent CLI run degrades to cache‑less automatically. The JSONL ledger has no such limit (both append concurrently).
- **Truncated output defers straight to the caller** (does not escalate) — every local tier shares the context window, so a bigger model can't help an over‑long input.
- A typo'd config key warns to stderr and is ignored rather than silently dropped.
- `extract` grammars cap at object‑of‑scalars / string‑arrays / enums; deeply nested schemas are not yet supported.

## License

[Apache License 2.0](LICENSE). The Gemma model weights it uses at runtime are governed separately by the [Gemma Terms of Use](https://ai.google.dev/gemma/terms).
