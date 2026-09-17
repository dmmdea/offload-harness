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
>
> **Amended 2026-09-16 (later) — deployed live on the reference box; the bound lane stays the 4B.** The seat
> serves and passes the digest gate 8/8 at 900 s walls, and it cannot share the card with the embedder that backs
> the ecosystem's memory authority at its declared window — see *Live cutover* below. Binding it is an operator
> decision with two measured options, not a knob.
>
> **Amendment 3, 2026-09-16 (later still) — D5 = A, measured and wired.** The operator chose option 1: the GSQ is
> the bound lane at **32,768 @ util 0.90**, the window the card shares with the embedder. Measured before binding:
> KV 1.75 GiB = 43,690 tokens (1.33x), embedder HTTP 200 beside it idle and under 4-stream load, single-stream
> 7.17 tok/s, 4-stream 20.36 tok/s. The seat declaration now carries its bound-lane settings — see *Amendment 3*.

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

## Live cutover on the reference box (2026-09-16, same night)

Everything below is measured on the Lenovo M720q with the production venv (`vllm-env-s6`, upgraded vLLM 0.28.0 →
**0.29.0** by exact version with `uv`; torch 2.13.0 unchanged, LMCache 0.5.4 with its CUDA extensions intact,
`pip check` clean) and the checkpoint's embedding patch applied in that venv. The tier's OTHER Qwen3.5 seat, the 4B,
loads on 0.29 unpatched and patched and passed `contracts/digest-8.json` **8/8** (walls 42–108 s) — the patch is safe
for the whole tier. Record: `Benchmarks and Optimizations/2026-09-16-16gb-tier-pass/live-cutover/`.

| step | result |
|---|---|
| unit `vllm-agent-seat.service` + llama-swap entry `qwen38-27b-gsq-vllm` (aliases `a2-pool`, `agent-pool-a2`, `qwen38-27b-gsq`) | loads in **200 s** direct, **67 s** through the front door once cached; `Model loading took 10.4 GiB`, KV **2.46 GiB = 65,967 tokens**, 1.34x at 49,152; **14,694 of 15,356 MiB** |
| digest-8 as the bound lane, measured config (4,096 / thinking off / vendor sampling), 300 s wire default | **3/8** — five `wall timeout after 300s`, zero wrong answers |
| same, `timeout_sec: 900` | **8/8**, walls 129–516 s |
| decode-rate sample for the auto wall (D-03) | **none recorded** — no single completion reached 1,024 tokens; the seat needs `agent_seat_tok_s` seeded (single-stream measured 5.75 tok/s → the estimate clamps to the 900 s cap, which is what 8/8 needed) |
| embedder beside the seat at util 0.92 | `embeddinggemma /v1/embeddings → HTTP 500`; every mem0 write from every session returned 500 from the Lenovo authority while the seat was warm — the support group (`swap: false`) can neither evict it nor fit beside it |
| the obvious fix, util 0.90 | vLLM refuses: `1.82 GiB KV cache is needed … available 1.32 GiB`; **estimated maximum model length 32,928** |

**What this settles.** The seat is real, wired, callable by name and measured; it earns its keep on fan-out and on
the cache-server path exactly as decided. As the tier's BOUND agent lane it needs 900 s walls and it takes the
ecosystem's memory authority offline whenever it is warm, because the embedder mem0 relies on lives on this
box's llama-swap. So the bound lane went back to the 4B the same night (`config.json.bak-2026-09-16-pre-gsq-bind`
restored; the GSQ-bound copy kept as `config.json.gsq-bound-2026-09-16`), which is what this ADR already said
about the agent lane. The 4B's own numbers on the new engine: 8/8, 42–108 s, blind 5.39 (ADR 0047); the GSQ's:
8/8 at 900 s, 129–516 s, blind 8.35 — the quality argument for binding the GSQ is large and real, and it is
blocked by co-residency, not by the seat.

**The operator's decision (open, on the decision surface):**

1. bind the GSQ at **32,768 @ util 0.90** — the window the card can share with the embedder; already blind-measured
   at that window (**8.42**, 22/24 against the llama.cpp 27B, accuracy 9.54 vs 9.47, coverage 7.69), mem0 stays up,
   concurrency at 0.90 not yet measured; or
2. move mem0's embedder off the Lenovo (then the GSQ binds at 49,152 @ 0.92 as declared) — an architecture change
   this ADR does not make; or
3. keep the 4B bound (today's state) and call the GSQ by name for fan-out.

## Amendment 3 — D5 = A: the seat is the bound lane, at the operating point it shares the card at

The operator's answer to the three options above was **1**. Before binding, the arm was run at the proposed point
(record: `live-cutover/gsq-32k-u090-*`): 32,768 @ util 0.90 starts in 241 s, `Model loading took 10.4 GiB`, KV
**1.75 GiB = 43,690 tokens (1.33x)**, 13,852 MiB alone; **the embedder answers HTTP 200 beside it**, idle and
**12/12 times under the 4-stream load** (card peak 14,623 of 15,356 MiB); single-stream **7.17 tok/s**, TTFT 1.66 s,
4-stream aggregate **20.36 tok/s**, 0 failed — every side column slightly better than the 49,152 @ 0.92 shape, and
the blind quality at this window was already measured (8.42; accuracy 9.54, zero fabrications). Then it was bound
live: seat launch line 32,768 @ 0.90, the node's config at the measured lane settings, `doctor` clean, and the seat
joined llama-swap's `heavy` swap group — at 0.90 the ≤ 5.5 GB cascade seats no longer fit beside it (a request for
one returned HTTP 500 "upstream command exited prematurely"), and in the group the two SWAP instead (measured both
ways: cascade seat 200 with the GSQ swapped out; GSQ back through the front door in 67 s; embedder 200 beside it).

**What "wire it properly" means, and why this amendment changes the seat schema.** The installer binds a vLLM seat
as the agent lane whenever the venv exists (`Spec.Bindings()` → `agent_model`), but the lane's OTHER settings came
from the tier's `config_seed`, which describes the FALLBACK seat: `agent_max_tokens` 1,024, no sampling, the 300 s
default. That is precisely the configuration the gate measured failing (3/8). A seat declaration can now carry its
own bound-lane settings — `agent_max_tokens`, `agent_thinking`, `agent_sampling`, `agent_timeout_sec`,
`agent_seat_tok_s` — validated at render with the same rules the harness config applies, emitted by `Bindings()`
only when set, and left alone by `FallbackBindings()`. `profiles.json` declares the GSQ at 32,768 @ 0.90 with
4,096 / off / the vendor sampling / 900 s / 7.17 tok/s (the rate measured at this operating point, not the 5.75 of the 49,152 shape). The `agent_seat_tok_s` seed exists because the seat's
completions never reach the 1,024 tokens a rate sample needs, so without it the D-03 auto wall would run the 300 s
default it was measured failing at.

**Costs recorded.** The matched-window claim is forfeited by design (the seat's own shape, 49,152 @ 0.92, stays in
`measured` as history); walls are 129–516 s against the 4B's 42–108 s; the cache server stays blocked on the
LMCache × vLLM 0.29 connector (D-117), so the seat runs on VRAM only and the binding says so.

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
- **Superseded by Amendment 3:** the bound lane on the reference box is the GSQ at 32,768 @ 0.90, and
  `profiles.json` now declares that operating point plus the bound-lane settings; the 49,152 @ 0.92 shape is kept
  in `measured` as what the seat serves when it has the card to itself.
- **Not propagated** to blackwell-16 / volta-16. Neither has been measured on its own silicon and both still owe
  a vLLM seat ([ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md) counts the debt).

## Re-eval triggers

A vLLM release that upstreams the Qwen3.5 quantized-embedding path (the patch is carried, not merged); a W4A16
27B that fits 16 GB by quantizing its embedding; mem0's embedder leaving the Lenovo (removes the co-residency
block on binding the GSQ at its declared window); a vLLM-loadable 27B quant that closes the 0.94 coverage gap
(the matched-window result is in — a new quant, not a bigger window, is what would re-open this); and any change
to the reference card's free VRAM, since the 49,152 window was fitted against what the box actually had free.
