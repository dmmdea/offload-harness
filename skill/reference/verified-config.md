# Verified config & measured numbers

Measured on an RTX 3070 Mobile (8GB) / i7-11800H / 64GB RAM laptop, llama.cpp
HEAD c34b922 (2026-06-13), Gemma-4 QAT `UD-Q4_K_XL` GGUFs, clean runs, temp 0,
GBNF grammar-constrained. **This harness uses no MTP** — see the rule below.

## The cascade (ascending capability)

| Tier | Model (QAT UD-Q4_K_XL) | Flags (+ common below) | GPU load | decode tok/s | Role |
|---|---|---|---:|---:|---|
| fast | `gemma-4-E2B-it-qat` | `-ngl 99` | ~3.35 GB | **~120–131** | triage, classify entry |
| work | `gemma-4-E4B-it-qat` | `-ngl 99 --parallel 1` | ~3 GB | **~70–83** | summarize, extract; default |
| escal | `gemma-4-26B-A4B-it-qat` (MoE) | `--cpu-moe -ngl 999 --parallel 1` + `GGML_CUDA_DISABLE_GRAPHS=1` | **~2.85 GB** | **~16** | climb on validation / low-confidence |
| (alt) | `gemma-4-12B-it-qat` (dense) | `-ngl 48` (`-ngl 99` OOMs on 8GB) | ~7.6 GB (edge) | ~15 | heavier, slower than the MoE — MoE preferred |

**Common flags (all tiers):** `--ctx-size 8192 --flash-attn on --cache-type-k f16 --cache-type-v f16 --threads 8 --jinja --reasoning off`

A 300-token summary lands in ~2–3s warm on E4B; a cold tier swap via llama-swap
adds ~5–18s. The 26B-A4B MoE is the sweet spot for the quality tier: faster than
the dense 12B **and** far lighter (experts in RAM), so it co-resides comfortably.

## Per-tier serving commands

```
# E4B workhorse (~70-83 tok/s)
llama-server --model gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf \
  --n-gpu-layers 99 --parallel 1 \
  --ctx-size 8192 --flash-attn on --cache-type-k f16 --cache-type-v f16 \
  --threads 8 --jinja --reasoning off

# E2B fast tier (~120-131 tok/s): same, model swapped, drop --parallel 1
# 26B-A4B MoE escalation (~16 tok/s, ~2.85GB GPU):
#   add  --cpu-moe --n-gpu-layers 999  and env  GGML_CUDA_DISABLE_GRAPHS=1
```

## Hard rules (cost hours to rediscover)

- **NO MTP in this harness.** `--spec-type draft-mtp` + a GBNF `grammar` field →
  llama.cpp 500 `"the current context does not logits computation. skipping"`.
  The harness *requires* grammar for structured JSON, so MTP is out. (MTP gives
  ~145 tok/s and is lossless, but only on the *free-form* path — e.g. a judge
  that doesn't constrain output. Keep it for that, never for the harness tasks.)
- **`--reasoning off` is mandatory.** Gemma-4 thinking mode otherwise eats the
  short output budget before answering → raw chat returns empty content with
  `finish=length`, `completion_tokens` spent, `content_len=0`. The grammar path
  mostly masks it; a free-form caller (the memory contradiction-sweep judge) hit
  it directly. Equivalent: `chat_template_kwargs:{enable_thinking:false}`.
- **f16 KV, not q8.** For Gemma-4, f16 KV is FASTER (~83 vs ~70 on E4B) *and*
  higher quality (q8 KV is materially lossy here). `-fa on` for the grammar path.
- **Structured output via GBNF `grammar`, never `--json-schema`** — Gemma-4
  crashes on `--json-schema`/`json_schema`/`response_format` (#22396, field #21571).
- **26B-A4B MoE: `--cpu-moe` + `GGML_CUDA_DISABLE_GRAPHS=1`.** Without the graphs
  flag the MoE path can crash; with `--cpu-moe` only attention is on the GPU.
- **12B dense: `-ngl 48`, not 99** — full offload OOMs an 8GB card (~400MB
  headroom at 48). The MoE is the better quality tier anyway.
- **Build with `-DCMAKE_CUDA_ARCHITECTURES=<cc>`**; the build is otherwise optimal
  (FA + CUDA graphs default-on). Don't add FORCE_MMQ/LTO.
- **CUDA 12.x; avoid 13.x** (MMQ crash → silent cuBLAS fallback regression).
- **Never run two model servers on one port** — a stale shared-GPU server
  silently halves throughput (caused a phantom "regression" in development).
- **Tiers are swap-EXCLUSIVE on 8GB** — one large model at a time (llama-swap
  group `exclusive: true`). Mixed-tier workloads pay a swap; batch by tier, or
  set `triage_model: ""` to let E4B handle triage without an E2B swap.

## Why these models
- QAT `UD-Q4_K_XL`: ~72% lower memory at near-BF16 quality; naive QAT→Q4_0 loses
  ~15 pts (use Unsloth dynamic UD).
- E2B → fast, high-interactivity triage. E4B → the quality/speed sweet spot for
  most grunt work. 26B-A4B MoE → near-frontier judgment at low VRAM for the rare
  case a smaller tier can't validate.
