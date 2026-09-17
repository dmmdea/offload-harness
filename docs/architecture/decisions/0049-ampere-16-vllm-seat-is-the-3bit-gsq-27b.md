---
status: Accepted
date: "2026-09-16"
---

# 0049 — The ampere-16 vLLM seat is Qwen3.8-27B 3-bit GSQ, chosen for concurrency and the cache-server path

> **Operator decision, 2026-09-16.** The tier's vLLM seat becomes `qwen38-27b-gsq-vllm`
> (ISTA-DASLab/Qwen3.8-27B-3Bit-GSQ), replacing the 4B `qwen3.5-4b-vllm`. It is chosen for **3.7x
> concurrency** and for being the tier's **only path to the LMCache cache server** — not on blind quality,
> where the llama.cpp IQ3_S+MTP arm still leads. The BOUND agent lane is unchanged: `config_seed.agent_model`
> stays on the llama.cpp fallback, per [ADR 0047](0047-ampere-16-agent-seat-reaudit.md).
>
> **Amended 2026-09-16 — the matched-window result is in and did not close the gap** (8.35 vs 9.29 at 49,152,
> gap 0.94, 21/24). The quality cost is recorded as real; see *Quality* below. The decision stands.

## Context — the quant nobody had found

[ADR 0047](0047-ampere-16-agent-seat-reaudit.md) left "a 27B under vLLM on one 16 GB card" recorded as
impossible. That was wrong twice over, and both errors are worth keeping because they are search errors, not
measurement errors:

1. *"No 27B vLLM quant exists"* — a search for `Qwen3.8-27B` returned four hits and none fit.
2. *"vLLM's quant ecosystem bottoms out around 4 bits, so a 27B on 16 GB must be llama.cpp"* — five
   W4A16/AWQ/GPTQ builds all price at **19.45–19.56 GB**, against 15.4 GB usable.

The operator asked whether a 3-bit existed "like we already did with llama.cpp". It does:
**`ISTA-DASLab/Qwen3.8-27B-3Bit-GSQ`, 11.85 GB** — transformer weights 3-bit GSQ (group 128), **embedding and
LM head 4-bit RTN (group 64)**. That last clause is the entire difference. Qwen3.8's ~248k vocabulary leaves
roughly 5 GB of embedding + `lm_head` at high precision in every W4A16 build; llama.cpp's IQ3_S quantizes them
too, which is how it lands at 14.4 GB. GSQ does the same thing for vLLM. **The 4-bit floor was a property of
what we had searched for, not of the engine.**

## The engine prerequisite, and why it is a first-class field

The checkpoint does not load on the vLLM we ran. On **0.28.0** its shipped `patch_vllm_qwen35_embedding.py`
applies cleanly (two lines, adding `quant_config` and `prefix` to the `VocabParallelEmbedding` construction in
`vllm/model_executor/models/qwen3_5.py`) and is still insufficient — the load fails with
`ValueError: There is no module or parameter named 'embed_tokens.weight_packed' in Qwen3_5Model`.

On **0.29.0** the same two-line patch works. The seat therefore declares its engine floor explicitly:

```
"engine_min_version": "0.29.0",
"engine_patch": "patch_vllm_qwen35_embedding.py (shipped in the checkpoint)"
```

This is recorded as seat data rather than tribal knowledge because the installer already DETECTS vLLM
prerequisites and falls back to the llama.cpp seat with a stated reason when they are absent (ADR 0035). A seat
that silently needs an engine the box does not run is the failure mode that rule exists to prevent. Note also
that the patch is **not** upstream: vLLM `main` still constructs a stock three-argument
`VocabParallelEmbedding` for Qwen3.5, so this is a carried patch, not a version wait.

## What was measured

Reference box: Lenovo M720q, NVIDIA A2 16 GB at the accepted 40 W / 1200 MHz profile. Both arms at the **same
vendor sampling** — `temperature 0.7 / top_p 0.80 / top_k 20 / presence_penalty 1.5`, the Qwen3.8 model card's
non-thinking values — so budgets match and the comparison is not confounded by sampling (INV-6).

| | vLLM 3-bit GSQ | llama.cpp IQ3_S + MTP |
|---|---|---|
| window served | **49,152** | 49,152 |
| weights | 11.85 GB | 14.4 GB |
| single-stream | 5.75 tok/s | 5.91 tok/s |
| **4-stream aggregate** | **19.53 tok/s** | 5.26 tok/s |
| TTFT p50 | 2.195 s | **0.408 s** |
| cache server | **yes** (ADR 0045) | no |

**Concurrency is the decision.** 19.53 against 5.26 is 3.7x, and it is what continuous batching exists to do.
Single-stream is a tie, so nothing is given up on serial work; TTFT is 5x worse, which matters for interactive
use and not for delegated contracts.

## Quality, stated against the decision rather than hidden behind it

At a **32,768** window the GSQ arm lost blind quality **8.42 to 9.28** (head-to-head 22/24, every lens agreeing).
That arm was handicapped: 32,768 was the top of the ladder that run, not the seat's limit. The entire gap sat in
**coverage** (7.69 vs 9.30) while **accuracy was HIGHER** (9.54 vs 9.47) — the signature of a model that is right
about less of the document, which is exactly what a smaller window produces.

The seat is wired at the **matched 49,152** window (65,536 refused: 2.3 GiB KV needed against 1.97 available,
estimated maximum 54,880). **The matched-window blind measurement governs this record, and it did not close the
gap.** Same instrument, same 8 packets, same vendor sampling, both arms at 49,152:

| arm | accuracy | specificity | coverage | OVERALL | fabrications (P/M) | degenerate | head-to-head |
|---|---|---|---|---|---|---|---|
| llama.cpp IQ3_S + MTP | 9.47 | 9.45 | 9.35 | **9.29** | 2 / 3 | 0 | **21/24** |
| vLLM 3-bit GSQ | **9.63** | 8.81 | 7.56 | 8.35 | **0 / 0** | 1 | 3/24 |

Gap **0.94**, every lens agreeing (faithfulness 9.26 vs 8.74, substance 9.34 vs 8.19, usefulness 9.28 vs 8.12) —
slightly WIDER than the 0.86 measured at 32,768. **The window hypothesis is refuted**: coverage did not move (7.69 → 7.56)
with 50 % more context, so the loss is a property of the arm (the 3-bit GSQ weights, or vLLM's decoding of
them), not of the budget it was given. What the GSQ arm keeps is the shape already seen at 32,768: **higher
accuracy and zero fabrications** on both counts, against lower coverage and one degenerate answer.

The quality cost of this seat is therefore real: **0.94 blind points, recorded as the cost of choosing
concurrency and the cache server**, exactly as ADR 0047 recorded the 27B's padding and its wall. It is the
reason the BOUND agent lane stays on the llama.cpp fallback: the seat that answers a default contract is the
higher-quality arm, and the vLLM seat earns its place on fan-out and on prefix reuse, not on a single answer.
Record: `Benchmarks and Optimizations/2026-09-16-16gb-tier-pass/judge-27b-49k`.

**This decision does not touch the agent lane.** `config_seed.agent_model` remains the llama.cpp fallback, so the
seat that ANSWERS a default contract is unchanged and no quality regression ships with this ADR.

## Consequences

- `profiles.json` → `profiles["ampere-16"].vllm_seat` becomes `qwen38-27b-gsq-vllm`, `max_model_len` 49,152,
  `gpu_memory_utilization` 0.92, `agent_ctx_tokens` 49,152. The **tier-level** `agent_ctx_tokens` stays 131,072,
  because it describes the BOUND lane (the fallback at the tier window) — `TestAgentWindowMatchesWhatTheAgentSeatServes`
  and `TestAmpere16AgentSeatIsTheMeasuredWinner` both enforce that distinction and both caught the error when this
  change first set it to 49,152.
- The 4B vLLM seat is no longer the tier's declared vLLM seat. The llama.cpp 4B remains `fallback_agent_model`,
  so a box without the venv, without 0.29.0, or without the patch still serves an agent lane.
- The cache-server binding for this tier is now possible for the first time since it became storeless: a binding
  is declared per vLLM seat (ADR 0045), and this seat is the tier's only vLLM-servable model of its class.
- **Not propagated** to blackwell-16 / volta-16. Neither has been measured on its own silicon and both still owe
  a vLLM seat ([ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md) counts the debt).

## Re-eval triggers

A vLLM release that upstreams the Qwen3.5 quantized-embedding path (the patch is carried, not merged); a W4A16
27B that fits 16 GB by quantizing its embedding; a vLLM-loadable 27B quant that closes the 0.94 coverage gap
(the matched-window result is in — a new quant, not a bigger window, is what would re-open this); and any change
to the reference card's free VRAM, since the 49,152 window was fitted against what the box actually had free.
