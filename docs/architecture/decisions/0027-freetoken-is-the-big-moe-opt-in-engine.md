# 0027 — FreeToken is the big-MoE opt-in engine on the blackwell-2x16 tier

- Status: **Accepted** (operator-approved 2026-08-27)
- Deciders: operator (Daniel), on measured evidence from the 2026-08-27 nightshift
- Related: ADR 0018 (media lease), ADR 0025 (model residency), `docs/ROADMAP.md`
  nightshift + round-2 sections, mem0 `b7799371` / `18ac82da` / `af2fa8b9`

## Context

The tier's serving engine (llama.cpp via llama-swap) covers every roster seat, but
models materially larger than pooled VRAM have never had a usable home here. The
prior answer for the >RAM class was colibri (mem0: "colibri only >RAM engine"), and
the >VRAM-but-fits-RAM MoE class simply went unserved: DeepSeek V4-Flash ran at
12.56 t/s under `--n-cpu-moe` and was deleted for lack of consumers; Flash-Next
measured 0.44–0.46× the incumbent the same way.

FreeToken (FlashML-org, arxiv 2608.16157, Apache-2.0) is an edge-native MoE serving
engine whose thesis is bandwidth-adaptive expert streaming — exactly this niche. It
was evaluated on both nodes on 2026-08-26/27 with a calibrated exact-answer
instrument (proven able to score 0 on a dead endpoint).

**The deciding measurement:** `openai/gpt-oss-120b` (MXFP4) served on ONE 16 GB
card scored **18/18** on the differential suite (tool-call fidelity, 22k-token
multi-hop joins, Spanish) in 678 s total — faster than Flash-Next on two cards
(1828 s), on a model class this box previously could not usably serve at all. On
Lenovo's 6 GB card, `gpt-oss-20b` beat the resident 4B seat on both correctness
(36/36 vs 32/36) and wall (712 s vs 1368 s).

## Decision

FreeToken is adopted as the **big-MoE opt-in engine** on the blackwell-2x16 tier,
with `gpt-oss-120b` as its seated model.

- **Opt-in, hand-launched, never a service.** No scheduler, no daemon (house rule).
  It is started for a work session and killed after. It is NOT part of llama-swap
  and NOT part of the cascade.
- **Endpoint:** OpenAI-compatible `http://127.0.0.1:1919/v1` (Anthropic-style
  `/v1/messages` also served). The harness or any client reaches it as a plain
  endpoint — config, not code.
- **Install locations (both nodes, isolated from production):**
  - Qube: WSL distro `freetoken` (separate from the production WSL distro that hosts the memory stack), venv at
    `/opt/freetoken`, engine from git main.
  - Lenovo: the node's ZFS app pool under `offload-stack/freetoken` (pool storage;
    `UV_CACHE_DIR` and `HF_HOME` MUST live on the pool — a home-dir quota killed
    the first `[accel]` install).
- **Canonical launch (Qube):**
  ```
  wsl -d freetoken -u root -- bash /opt/freetoken/serve-bigmoe.sh
  # which runs:
  #   CUDA_VISIBLE_DEVICES=<card> ft serve --model openai/gpt-oss-120b \
  #     --port 1919 --num-tokens 32768
  ```

## Constraints that bound this seat (all measured, not read)

1. **No grammar / json_schema / response_format.** The GBNF cascade path can never
   ride this engine. Cascade seats stay on llama.cpp, permanently.
2. **`--num-tokens 32768` is mandatory.** `--moe-cache-auto` silently sizes KV to
   ~8.2k tokens; longer prompts get HTTP 400 ("Input sequence length … exceeds").
   The explicit 32k KV costs only 0.62 GiB.
3. **Device selection via `CUDA_VISIBLE_DEVICES`.** The git-main `--gpu` flag was
   present in a run that failed with "device not ready" on WSL; the env method is
   the proven path here.
4. **Mutual exclusion with pooled seats.** While the engine holds a card, pooled
   llama-swap seats (and on a 6 GB node, ANY seat) cannot co-reside — measured as
   502s on Lenovo. Callers take a text-class GPU lease for the session
   (`local-offload gpu reserve --class text …`).
5. **GGUF reuse is off the table today:** arch gate (`known: ['gemma4']`) and a
   dtype assert on gemma UD quants (both 0.1.2 and main@9ef3651). HF safetensors /
   MXFP4 / FP8 checkpoints are the real path.
6. **Qwen3.8-27B-FP8 does not load** (upstream fp8-loader bug, filed as
   FlashML-org/FreeToken#240) — the same-model llama.cpp-vs-FreeToken race stays
   open until it is fixed.

## Consequences

- The >VRAM MoE class (gpt-oss-120b now; GLM/M2.5-class NVFP4 checkpoints when
  disk allows) becomes usable on this tier at quality parity with the incumbent
  on everything measured.
- Ports: Qube `:1919` (WSL), Lenovo `:1920` — rows recorded in the port files.
- The engine tracks git main under the monthly whole-system update process
  (ROADMAP standing section), released-tag-first once upstream starts tagging
  meaningfully.
- Rollback: kill the process; delete the WSL distro / pool dir. Nothing in
  llama-swap, the harness config, or the cascade references it.
