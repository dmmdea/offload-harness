# Phase S — ik_llama.cpp benchmark spike (RESOLVED — see RESOLUTION at end)

> Non-mutating spike, 2026-06-16. Built ik_llama.cpp in its own dir, benchmarked vs the live mainline engine on a scratch port, **touched nothing in the live `~/llama-swap/config.yaml`**. This is a proposal for Daniel — no binary was swapped.

## TL;DR — VERDICT: **REJECT ik_llama.cpp for the local-offload cascade.** One free mainline tuning win found.

ik is genuinely **faster on prompt-processing** (the result that *almost* justified a swap), and the feared #1765 token-generation regression **did not reproduce** on our current build. But ik **fails the sacred GBNF-grammar gate on the 26B-A4B CPU-MoE-offload path** — it returns HTTP 500 `invalid UTF-8` on trivial enum grammars and falls into repetition loops, where mainline produces valid JSON. The harness's grammar path is non-negotiable, so a faster engine that breaks it is unusable. The cascade shares one binary across all tiers, so the 26B breakage disqualifies ik for the whole cascade.

**Actionable takeaway (no ik needed):** mainline `--n-cpu-moe 24` lifts the live 26B-A4B escalation tier's token-gen ~**+11%** (15.4 → 17.2 t/s) for free, grammar intact — worth adopting after a quick verify.

## Setup (all non-mutating)
- `~/ik_llama.cpp` cloned + built CUDA, pinned commit **`11c9935c`** (2026-06-16), `sm_86`, flags `-DGGML_CUDA=ON -DGGML_NATIVE=ON -DCMAKE_CUDA_ARCHITECTURES=86-real -DLLAMA_CURL=OFF -DGGML_SCHED_MAX_COPIES=1`.
- Baseline = the exact mainline tree the live config uses: `~/llama.cpp-mtp` (`c34b922`). Built its `llama-bench` (additive; live `llama-server` untouched).
- A/B on scratch ports 18080/18081; `llama-bench` -p512 -n128 -r3 (median of 3), `GGML_CUDA_DISABLE_GRAPHS=1` on both, flash-attn on both, identical offload split. Grammar gate = the harness's own 4 GBNF tasks (summarize/classify/extract/triage) via its real `grammar`-field path, ik vs mainline serving the same model.

## Speed — 26B-A4B escalation tier (experts on CPU, f16 KV unless noted)
| Config | PP (pp512) t/s | TG (tg128) t/s | Note |
|---|---:|---:|---|
| **Mainline** `--cpu-moe` (= live) | 242.5 | 15.43 | baseline |
| ik `-ot exps=CPU` (`-fmoe 0`) | 316.0 | 15.48 | engine-matched |
| ik `-ot exps=CPU` (`-fmoe 1`) | 307.4 | 15.70 | +27% PP, TG parity |
| **ik `-ot exps=CPU` + q8_0 KV** | **583.2** | 14.44 | **+140% PP**, slight TG drop |
| **Mainline `--n-cpu-moe 24`** | 249.0 | **17.18** | **+11% TG** vs live — *free win* |
| ik `--n-cpu-moe 24` | 80.1 | 5.17 | ❌ ik's layer-offload path is broken-slow |

- **#1765 refuted on our build:** that issue measured ik ~4.4× *slower* TG (q8_0 KV) on a near-identical box; here ik holds TG parity (and is far faster on PP). Likely closed by ik's recent CUDA-FA TG work (#1973), which postdates #1765.
- **Don't use ik `--n-cpu-moe`** — its layer-based offload tanks here (80/5.17). ik's offload strength is the all-experts-on-CPU `-ot` path.

## Speed — E4B workhorse (fully on GPU, f16 KV unless noted)
| Config | PP t/s | TG t/s |
|---|---:|---:|
| Mainline | 3235.6 | 87.7 |
| ik f16 | 3621.5 | 96.2 |
| ik q8_0 KV | 3564.6 | 91.7 |

ik ≈ **+10–12%** on E4B (TG clearly faster, PP within noise). Modest — **below** the ~15–20% margin that would justify the operational cost of adopting a second engine.

## Grammar gate (SACRED) — the disqualifier
Same harness GBNF tasks, same model, f16 KV:

| Task | Mainline 26B | ik 26B (`--cpu-moe`) | ik E4B (GPU) |
|---|---|---|---|
| classify (enum) | ✅ `{label:technology, confidence:0.95}` | ❌ **HTTP 500 invalid-UTF-8** | ✅ valid |
| triage (enum) | ✅ `{decision:yes, reason:…}` | ❌ **HTTP 500 invalid-UTF-8** | ✅ valid |
| summarize | ✅ valid (clean truncation-defer) | ❌ **repetition loop**, no JSON | ✅ valid |
| extract | ✅ valid (grounding defer) | ✅ valid (grounding defer) | n/a |

- ik's grammar breaks **only on the 26B CPU-MoE-offload path** (the #1693-class hybrid-offload bug). On the fully-GPU E4B, ik's grammar is clean. So the defect is ik's CPU-resident-expert + grammar interaction, not grammar generally.
- Tested at **both** q8_0 and f16 KV → not a KV artifact; it's the engine path.

## Decision
1. **REJECT ik for the local-offload cascade.** The cascade runs one shared binary; the 26B escalation tier's grammar is broken on ik. A mixed setup (ik for E2B/E4B/12B + mainline for 26B) is technically possible but buys only ~10% on E4B (sub-threshold) while adding a second rolling-`main` engine to maintain and a live grammar landmine. Not worth it.
2. **PROPOSE: adopt mainline `--n-cpu-moe 24` for the live 26B-A4B escalation entry** (`~/llama-swap/config.yaml`, the `gemma4-26b-a4b` cmd: replace `--cpu-moe` with `--n-cpu-moe 24`). Measured ~+11% TG (15.4→17.2), grammar-safe (it's mainline). **Verify first:** (a) it still fits 8GB VRAM with the cascade's headroom, (b) the grammar gate passes at `--n-cpu-moe 24`. This is a free, low-risk win — Daniel's call (it's a live-config edit).
3. **Watch-list:** re-evaluate ik if/when the Gemma-4 CPU-MoE grammar/UTF-8 bug is fixed upstream — ik's PP is genuinely strong (583 t/s at q8_0 KV is 2.4× mainline). Worth filing/finding the upstream issue (`invalid UTF-8 byte 0xA0` on grammar-constrained Gemma-4 MoE with `--cpu-moe`).

## Why the spike was worth it (even ending in REJECT)
- **Verify-then-assert paid off twice:** the raw benchmark alone (ik +30–140% PP) would have said "adopt"; the sacred grammar gate revealed ik is unusable for the harness. And citing #1765 alone would have said "reject for TG" — wrong on our current build.
- Surfaced a **free mainline tuning win** (`--n-cpu-moe 24`).

## Artifacts / cleanup
- `~/ik_llama.cpp` (build kept for the watch-list re-test; ~a few GB — safe to remove if disk is needed).
- Spike test configs/inputs under `C:\Users\dmmde\ik-spike-*` + `.ik-spike\` (throwaway).
- No live config changed; live memory stack + Phase A.2 whisper-stt verified healthy after the spike.

---

## RESOLUTION (2026-06-16) — both proposals decided with real-flag data

The two proposals above were re-measured under the **exact live server flags** (POST `/v1/chat/completions` + grammar field + `--jinja`, ctx-8192, f16 KV, flash-attn) — NOT `llama-bench`, whose default ctx/batch misled the original numbers. Outcomes:

1. **`--n-cpu-moe 24` → revised to `--n-cpu-moe 28` and APPLIED.** The original "+11% TG" was a llama-bench artifact; real-flag TG curve on the 26B:
   - `--cpu-moe` (live baseline): 14.76 tok/s, ~2–3 GB free
   - `--n-cpu-moe 28` (2 expert layers on GPU): **15.94 tok/s mean = +8%**, ~2.2 GB headroom ✅
   - `--n-cpu-moe 26`: 15.81 (+7.5%), 1.4 GB free
   - `--n-cpu-moe 24` (6 layers): 16.66 (+13%) but only **634 MB free** — REJECTED as OOM-risky on the escalation tier under daytime desktop-floor swings (~1 GB).
   `--n-cpu-moe 28` is the safe optimum (≈24's speed, 3.5× the headroom). Applied via the ritual: backup `config.yaml.bak-20260616-125857`, surgical edit (only the 26B line), YAML validated, restart, verified embedder still 768 + 26B serves HTTP 200 + process runs `--n-cpu-moe 28` + 2 GB free. **Live.**

2. **Mixed-engine (ik for E2B/E4B/12B) → REJECTED with data.** Grammar gate PASSED (ik crash-free on the fully-GPU tiers: E2B 0/20, 12B 0/20, E4B 0/74 HTTP-500s incl. Spanish/UTF-8 — the crash mode is `--cpu-moe`-specific, GPU tiers dodge it). **But ik is SLOWER for token-gen** under real flags: E2B mainline ~79.6 vs ik ~44.6 tok/s (ik **44% slower** on the hot entry tier); 12B ~17.8 vs ~13.4. The "~10% faster" was a **PP-throughput** number (ik's genuine edge), not TG — and the harness is TG-bound. **Keep mainline on every tier.** Only revisit with ik-native IQ_K re-quantization (separate, speculative).

**Lesson (reinforced):** always measure real-flag TG; never trust llama-bench PP as a token-gen speed proxy. The watch-list (item 3) stands: re-file/track the upstream `invalid UTF-8 0xA0` CPU-MoE grammar bug.
