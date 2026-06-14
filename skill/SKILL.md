---
name: local-offload-setup
description: Use when setting up the "local-offload" harness — which lets Claude Code delegate short-context grunt work (summarize / classify / extract / triage) to a FREE local Gemma-4 model so those tokens never hit your cloud context. Targets a machine with an NVIDIA GPU (>=8GB), high system RAM (>=32GB), and a fast SSD. Builds a recent llama.cpp, pulls the Gemma-4 QAT family (E2B/E4B/26B-A4B), configures a grammar-reliable cascade via llama-swap, builds the Go CLI+MCP, and registers it with Claude Code. Triggers: "set up local offload", "install the local-offload harness", "offload model setup", "give Claude a free local model".
version: 1.2.0
---

# local-offload setup

Reproduces a verified local-offload harness on a compatible machine. The harness
is a Go CLI + MCP server that delegates summarize/classify/extract/triage to a
local **Gemma-4 QAT family cascade**: fast tasks enter at **E2B** (~120 tok/s),
the **E4B** workhorse (~83 tok/s) handles the rest, and on a validation failure
or a **low decision-confidence signal** (a token-logprob margin at the decision
token — catches genuinely torn calls) the request climbs to the near-frontier
**26B-A4B MoE** (~16 tok/s, experts in RAM) before ever giving up. It **NEVER
calls a cloud model** — when every local tier fails it returns a structured
`defer` so Claude handles the task itself. Telemetry goes to an append-only JSONL
ledger (`local-offload ledger`) that reads even while the MCP server is running.

> **Why no MTP / speculative decoding?** The harness produces structured JSON via
> GBNF **grammar-constrained** decoding (Gemma-4 crashes on `--json-schema`,
> llama.cpp #22396). MTP (`--spec-type draft-mtp`) is **incompatible with the
> grammar field** — llama.cpp returns a 500 "logits computation" error. So the
> verified serving config uses **no MTP**: `-fa on`, f16 KV, `--reasoning off`.
> (MTP ~145 tok/s is real but only for *free-form* generation, not this harness.)

## Hardware gate (check FIRST)

**Environment:** a **bash** shell on **Linux, macOS, or Windows + WSL2** (the scripts are POSIX/bash; native-Windows PowerShell building is out of scope). Every install location is **operator-chosen** via env vars (`MODELS_ROOT`, `LLAMACPP_DIR`, `HARNESS_SRC`, `CONFIG_OUT`, …) with sensible `$HOME`-relative defaults — the skill never hardcodes machine paths. Override any of them before running `setup.sh`.

Run `scripts/detect.sh` (or check manually). REQUIRED, else stop and tell the user what's missing:
- **NVIDIA GPU, >=8GB VRAM** (`nvidia-smi`). Report the compute capability (e.g. 8.6) — it becomes `CMAKE_CUDA_ARCHITECTURES`.
- **>=32GB system RAM** (>=64GB ideal — the 26B-A4B MoE keeps its experts in RAM via `--cpu-moe`).
- **Fast SSD** with >=20GB free (E4B ~4GB + E2B ~2.5GB + 26B-A4B ~14GB + build).
- **CUDA toolkit** with `nvcc` (any 12.x; AVOID 13.x — MMQ crash → cuBLAS fallback). If absent, point the user to install CUDA 12.4–12.8.
- **Go 1.26+** and **git, cmake, python3**.

On exactly 8GB VRAM all three tiers fit (one large model at a time — the group is
swap-exclusive). For <8GB or weaker GPUs set `WITH_FAMILY=0` to install only the
E4B workhorse (no cascade), or run the E2B tier alone.

## What it installs (all additive / reversible)

1. **Recent llama.cpp** built fresh into its own dir (does NOT touch any existing
   llama.cpp). Needs the `--reasoning` toggle (2026-06+) so Gemma-4 thinking mode
   can be disabled — otherwise short-budget grammar replies get eaten by the
   think phase (returns empty content, finish=length).
2. **Models** (Unsloth QAT dynamic `UD-Q4_K_XL` GGUFs): `gemma-4-E4B-it-qat`
   (workhorse), and with `WITH_FAMILY=1` also `gemma-4-E2B-it-qat` (fast tier) and
   `gemma-4-26B-A4B-it-qat` (MoE escalation tier).
3. **Serving config** — verified grammar-reliable, per tier (NO MTP):
   ```
   # common: --ctx-size 8192 --flash-attn on --cache-type-k f16 --cache-type-v f16 --jinja --reasoning off
   E4B : --n-gpu-layers 99 --parallel 1                 # ~70-83 tok/s
   E2B : --n-gpu-layers 99                               # ~120-131 tok/s
   26B-A4B (MoE): --cpu-moe --n-gpu-layers 999 --parallel 1   + env GGML_CUDA_DISABLE_GRAPHS=1   # ~16 tok/s, ~2.85GB GPU
   ```
   Emitted as an `offload-family.llama-swap.yaml` snippet (3 model defs + a
   swap-exclusive group) to merge into the user's llama-swap config.
4. **The harness CLI** (`local-offload`) built from the Go source.
5. **MCP registration** via `claude mcp add` (defaults already encode the cascade;
   `--config` is optional).

## Procedure

1. **Detect + gate.** Run `scripts/detect.sh`. Resolve `GPU_ARCH` (e.g. 86), `MODELS_ROOT`, `LLAMACPP_DIR`, `CUDA_HOME`. Stop if the gate fails.
2. **Confirm plan with the user** (full family vs E4B-only via `WITH_FAMILY`, install dirs, whether they run llama-swap). Use sensible defaults; only ask if ambiguous.
3. **Run `scripts/setup.sh`** with the resolved env vars. It is idempotent and:
   - clones the harness source from `HARNESS_REPO_URL` into `HARNESS_SRC` if absent (or uses an existing checkout — e.g. the repo you cloned this skill from),
   - builds llama.cpp (`-DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=$GPU_ARCH`),
   - downloads the QAT GGUFs via `hf download`,
   - builds the `local-offload` binary (`go build`),
   - writes `offload-family.llama-swap.yaml` (the cascade serving entries),
   - writes `config.json` with `model`/`triage_model`/`escalation_model` set.
4. **Serve the models.** Merge the snippet into your llama-swap config (e.g.
   `~/llama-swap/config.yaml`; back up first), restart llama-swap, verify
   `/health` (the harness talks to llama-swap's front-end port, default `11436`
   — override via `LLAMASWAP_PORT`). Without llama-swap the script writes a
   single-model E4B launcher (`serve-offload.sh`, no cascade).
5. **Smoke test:** `local-offload doctor` then `echo "<some text>" | local-offload summarize - --json`. Expect valid JSON. A `defer` with "model call failed" means the model isn't serving — check llama-swap / the launcher. `local-offload models` prints the active cascade.
6. **Register MCP** (needs a Claude Code restart to take effect):
   ```
   claude mcp add local-offload --scope user -- "<abs path>/local-offload" mcp
   ```
   Add `--config "<abs path>/config.json"` only for non-default endpoints/paths.
7. **Report** the install summary, the revert instructions, and the `ledger` command (proves token savings over time).

## Key gotchas (bake these in — they cost hours to rediscover)

- **MTP breaks the grammar path.** `--spec-type draft-mtp` + a GBNF `grammar`
  field → llama.cpp 500 "the current context does not logits computation".
  This harness ALWAYS uses grammar, so it runs **without MTP** (`-fa on`).
- **`--reasoning off` is mandatory.** Gemma-4's thinking mode otherwise consumes
  the (short) output budget before emitting the answer — raw chat returns empty
  content with `finish=length`. The grammar path masks it; free-form callers
  (e.g. a judge) hit it directly.
- **Structured output via GBNF `grammar`, never `--json-schema`** — Gemma-4
  crashes on `--json-schema`/`json_schema`/`response_format` (llama.cpp #22396,
  server field #21571). Don't "simplify" back to json_schema.
- **26B-A4B MoE needs `--cpu-moe` + `GGML_CUDA_DISABLE_GRAPHS=1`** — experts run
  in RAM; only ~2.85GB hits the GPU, so it's *lighter* than the dense 12B and a
  great escalation tier on 8GB.
- **QAT `UD-Q4_K_XL`**: ~72% lower memory at near-BF16 quality; a naive QAT→Q4_0
  loses ~15 pts — use the Unsloth dynamic UD build.
- **Build for your GPU arch** (`-DCMAKE_CUDA_ARCHITECTURES=<cc>`); the build is
  otherwise already optimal (FA + CUDA graphs default-on). Don't add FORCE_MMQ/LTO.
- **CUDA 13.x is a trap** (MMQ crash → silent cuBLAS fallback, decode regression). Prefer 12.4–12.8.
- **Never run two model servers on one port** — a stale shared-GPU server silently halves throughput (caused a phantom "regression" during development).
- **bbolt cache/ledger are single-writer.** The long-running MCP server holds the
  lock; a concurrent CLI run degrades to cache-less automatically (by design).

## Files
- `scripts/detect.sh` — hardware/toolchain detection.
- `scripts/setup.sh` — idempotent installer (build + pull family + configure + emit snippet).
- `reference/verified-config.md` — measured numbers (full QAT family) and flag rationale.
