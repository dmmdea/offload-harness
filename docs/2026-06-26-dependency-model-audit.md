<!-- Generated 2026-06-26 by an 8-agent research workflow (deps-model-audit, run wf_d3c26455-493). Baseline: llama-swap v202, llama.cpp c34b922, EmbeddingGemma+bge memory stack, go-sdk v1.6.1. -->

# Dependency & Model Update Audit (2026-06-26)

## ⚡ Execution addendum (actioned same day — read this first)
The raw research report follows below; this addendum records what was actually done and corrects findings after live verification.

**Applied:**
- **Go deps** — bbolt 1.4.3→1.5.0, x/text 0.14→0.38, x/sys, x/oauth2, segmentio/asm; **go-sdk held at v1.6.1** (v1.7 is a breaking pre-release). Full test suite green. Merged `e276dad`.
- **llama-swap v202→v230** — scratch-tested the config first, swapped with backup + auto-rollback, then verified grammar (GBNF yes/no) + memory stack (embeddinggemma 768-dim) on v230. Backup: `~/.local/bin/llama-swap.bak-v202-20260626`.
- **CUDA build env** — engine rebuilds had been failing at the executable link (`cublas` undefined). Root cause was NOT a missing toolkit: CUDA 12.4 is intact at `~/cuda-12.4`, just not in the default environment. Fix = `source ~/cuda-build-env.sh` before building (sets PATH/LD_LIBRARY_PATH/LIBRARY_PATH/CUDAToolkit_ROOT). llama.cpp + whisper are rebuildable again.
- **whisper.cpp v1.9.1** — rebuilt with the env fix (was blocked, now unblocked).

**No-op (already current):** Gemma-4 GGUFs (all Jun 12-13, post the Apr-11 chat-template fix); qwythos (the Jun-20 download is current — repo is days old).

**HELD — llama.cpp rebuild:** correct to hold. #23677 is open with no fix; there is nothing safe to upgrade *to*.

**CORRECTION — #23677 (the report below over-stated the live risk):** The bug is real/open, your build (Jun 13) IS in the affected range, and `gemma-4-E4B` (your workhorse) is the issue's reproduction model. BUT empirically the harness has **never** triggered it: 1,391 ledger calls (~420 grammar-constrained E2B/E4B) with `err_class` all-clean, zero "empty grammar stack" in the llama-swap journal, and a fresh 8-request high-temperature E4B grammar stress test → 0 crashes. Mechanism: the harness runs `E4B-it-qat UD-Q4_K_XL`, whereas the issue's repro is `E4B-it-Q5_K_M` (non-QAT) — the QAT/quant difference suppresses the `<unused49>` token. So: **real but latent, empirically dormant.** The `--logit-bias` workaround was the research agent's suggestion, NOT from the issue thread.

**CUDA version:** was building on **12.4** (`~/cuda-12.4`) / 12.8 runtime; latest is **13.3** (May 2026). (Initial advice was "not worth it for Ampere" — Daniel chose to upgrade to the latest + retire 12.4 anyway.)

**CUDA 13.3 migration (attempted 2026-06-26 per Daniel — currently BLOCKED, stack safely on 12.4):**
- ✅ CUDA 13.3 toolkit installed to `~/cuda-13.3` (runfile, no sudo, same method as 12.4; `nvcc V13.3.33`, runtime soname `.so.13`). `~/cuda-build-env.sh` repointed to 13.3.
- ✅ Prereqs verified: Ampere sm_86 supported in 13.3; driver shows **CUDA UMD 13.3** (610.62 ≥ 610.43.02 min); a minimal CUDA 13.3 program detects the RTX 3070 fine. So **basic CUDA 13.3 works on this WSL.**
- ❌ **BLOCKER:** the **ggml-based engines built on 13.3 crash at startup** — `munmap_chunk(): invalid pointer` (core dump) *before* CUDA init, even with the correct full env (`/usr/lib/wsl/lib` + `~/cuda-13.3/lib64`) and a real model. A **load-time `cublas-13`/glibc ABI issue** (the minimal program used only `libcudart`; the engines also link `libcublas.so.13`/`libcublasLt.so.13`). whisper canary on 13.3 crashes; llama.cpp shares `ggml-cuda` → presumed affected, so NOT attempted on the live engine.
- 🛡️ **Live stack UNTOUCHED + healthy on 12.4** — all 13.3 work was in fresh dirs (`build-13`) / alongside; `llama-swap` v230 still spawns the Jun-12 12.4-built `llama-server`; grammar + embeddinggemma verified working. **`~/cuda-12.4` NOT retired** (blocked on the above).
- **ROOT CAUSE — CONFIRMED (2026-06-26), and it's upstream:** NOT cublas/glibc (cublas-13 + `cublasCreate` work standalone; glibc 2.39, gcc 13.3 fine; no lib conflicts). It is a **known NVIDIA WSL bug**: the CUDA 13.3 `libcudart` + the WSL driver corrupt the heap inside `libnvidia-ptxjitcompiler` during `ggml_cuda_init`'s `cudaGetDeviceCount` — [unslothai/unsloth#6303](https://github.com/unslothai/unsloth/issues/6303) (CUDA 13.0 works, 13.3 doesn't on WSL) + [ikawrakow/ik_llama.cpp#1909](https://github.com/ikawrakow/ik_llama.cpp/issues/1909) (same crash, driver 610.47). Local workarounds tried + FAILED: `CUDA_MODULE_LOADING=EAGER/LAZY`. **610.62 is the latest Windows driver** (June 16 2026) — there is no newer one to fix it; the fix must come from a future NVIDIA driver/CUDA release.
- **DECISION: stay on CUDA 12.4** (works, proven; the live stack was never moved off it). `~/cuda-13.3` **removed 2026-06-26** (reclaimed 7 GB) per Daniel — we'll jump to a newer/fixed CUDA in the future, driven by the **`cuda-reforge`** skill (`~/.claude/skills/cuda-reforge/`), which codifies the full verified runbook (install → build-env → canary → version-pinned engine rebuild → llama-swap cutover → verify the sacred trio → retire) plus the WSL-ptxjit known-broken table. `build-13` artifacts removed; `~/cuda-build-env.sh` repointed to 12.4. CUDA **13.0** reportedly works (one report, on a 4090) but is non-latest + unconfirmed on the 3070 — not pursued.

---

## TL;DR
- **You are running on two known bugs in the exact `llama-swap` matrix feature you use** (v204 race fix + v211 logger fix never picked up). Upgrade to v230 — GBNF passthrough is untouched across all 28 releases.
- **The Gemma 4 grammar crash (`llama.cpp` issue #23677, `<unused49>` token crashes the GBNF sampler) is LIVE on your current build and has NO mainline fix.** Rebuilding does not fix it. This is your single biggest SACRED-constraint risk — flagged below with a workaround.
- **The memory stack is already best-in-class — do not touch it.** EmbeddingGemma-300M leads MTEB Eng v2 under 500M; the one tempting reranker upgrade (jina-reranker-v3) needs a non-mainline llama.cpp fork, which is disqualifying for a load-bearing path.
- **Two cheap correctness wins:** re-pull any Gemma 4 GGUF older than Apr 11 2026 (broken chat template), and bump the Go deps in one `go get -u` pass (go-sdk is already latest — hold the v1.7 pre-release).
- **Qwythos-5-1M v2** is a same-arch drop-in that *improves* GBNF sampler reliability — a rare upgrade that helps the sacred path rather than risking it.
- **whisper.cpp v1.9.1** is a low-risk CUDA-stability bump that also unlocks the Parakeet tier already on your roadmap.

## Do now (low-risk clear wins)
- **llama-swap — v202 → v230** — upgrade the binary; picks up v204 (matrix race), v211 (matrix logger), v224 (v219-refactor regressions). Verify Prometheus scrape (v225 adds /metrics auth) and confirm config uses `matrix` *or* `groups`, not both. — *risk: low.*
- **gemma-4 GGUFs — re-pull any tier pulled before Apr 11 2026** — `unsloth` UD-Q4_K_XL re-upload fixed a broken chat template that silently corrupts output. Correctness, not a quality bump. — *risk: low (must verify your pull date).*
- **go-deps — single `go get -u` pass** — bbolt v1.5.0, cobra v1.10.2 + pflag v1.0.10 (bump together), all `golang.org/x/*`, testify v1.11.1; then `go mod tidy`. go-sdk v1.6.1 is already latest — leave it. — *risk: low.*

## Do soon (worth a planned change)
- **llama.cpp — c34b922 (~b9628) → ~b9821** — rebuild (CUDA, same flags). Gains: grammar-generator/peg fixes (b9655/b9744/b9754), quant+MTP fix (b9789), sched CUDA perf (b9820). **Does NOT fix #23677.** Before upgrading, audit any path that sends grammar and expects silent fallback — **b9704 now returns HTTP 400 on invalid GBNF** instead of dropping the constraint. — *risk: medium.*
- **qwythos — original → Qwythos-9B-Claude-Mythos-5-1M v2** — same Qwen3.5-9B Q4_K_M, same VRAM (5.24–5.48 GiB). The v2 tokenizer/metadata fix *improves* GBNF sampler init on the dense-9B arch (already confirmed clean of the MoE enum bug). Released ~Jun 19; watch briefly since "v2" implies v1 had issues. — *risk: low.*
- **whisper.cpp — db5a84b → v1.9.1 (or HEAD)** — CUDA stability fixes (integer-overflow, cublasSgemmBatched) + unlocks the roadmap Parakeet tier in one binary. STT path uses no grammar, so SACRED #1 is not in play. **Verify the server `no_context=true` default** against your context assumptions before deploying. — *risk: low.*

## Watch (track, don't act yet)
- **llama.cpp #23677** — the Gemma 4 `<unused49>` GBNF crash. No fix as of b9821. Watch for a fix PR; this is the gating issue for grammar-constrained Gemma 4 reliability.
- **jina-reranker-v3** — a real +5.4 nDCG@10 BEIR gain over bge-reranker-v2-m3 at the same 0.6B. Blocked: llama.cpp mainline support (#17189) closed stale → needs a fork. Re-evaluate **only if/when mainline support lands.**
- **Parakeet-TDT-0.6B-v3** — now first-class in whisper.cpp v1.9.0; needs its own GGUF slot in llama-swap (~467MB Q4_K). Pull in when you actually build out the STT tier.
- **go-sdk v1.7.0-pre.1** — protocol 2026-07-28, stateless model, removed initialize handshake, multiple breaking API changes. **Do not adopt** until stable; would force a transport rewrite.
- **llama.cpp #24266** — Gemma4 MTP + n-gram speculative decoding interference (40+ → ~4 tok/s). Only relevant if you enable both at once.

## Skip (checked, not worth it)
- **Embedding swap (Qwen3-Embedding-0.6B / nomic-embed-v2-moe / snowflake-arctic-m-v2)** — all either trail EmbeddingGemma on English, are 2x the params, or carry an open llama.cpp crash bug. No reason to churn the index. EmbeddingGemma stays.
- **bge-reranker-v2.5-gemma2-lightweight** — 9B base, blows the 8GB VRAM ceiling. Out.
- **ik_llama.cpp** — already rejected for breaking enum grammars on the 26B CPU-MoE path. Stays rejected.
- **No Gemma 5 / no Gemma 4.x point release exists** — nothing to upgrade the model weights *to*; the only action is the chat-template re-pull above.

## ⚠️ Constraint warnings
- **GBNF GRAMMAR — LIVE RISK (llama.cpp #23677):** Gemma 4 can emit `<unused49>` (token ID 62) mid-generation and crash the GBNF sampler ("Unexpected empty grammar stack"). This affects the raw-grammar path the harness depends on, on the Gemma 4 QAT tiers. **No mainline fix exists on the current build or on b9821 — upgrading llama.cpp does not resolve it.** Workaround if it bites: `--logit-bias` to suppress the `<unused>` token IDs (e.g. ID 62). Treat any Gemma-4 grammar-constrained path as non-deterministically crash-prone until this PR merges. *Confidence: high that the bug is open; medium on real-world hit rate — verify against your own request volume.*
- **GBNF GRAMMAR — behavior change (llama.cpp b9704):** server now returns **HTTP 400 on malformed grammar** instead of silently dropping the constraint. If any harness path relied on silent fallback, it will now hard-error after the rebuild. Audit grammar validation before bumping llama.cpp.
- **GBNF GRAMMAR — net POSITIVE (Qwythos-5-1M v2):** the v2 tokenizer-metadata fix improves sampler init on the dense-9B arch. The enum-grammar bug (#20178) is MoE-35B-only; dense-9B is confirmed clean. This is the one model change that *helps* the sacred path.
- **GBNF GRAMMAR — verified SAFE (llama-swap):** zero grammar/GBNF commits across all 28 releases; grammar fields pass through to llama-server unchanged in v230. Upgrade does not touch the sacred path.
- **MEMORY STACK — do not churn:** no upgrade recommended touches EmbeddingGemma-300M or bge-reranker-v2-m3. The one attractive reranker (jina-v3) requires a non-mainline llama.cpp fork on a load-bearing path — **disqualified.** Any embedding swap would force a full mem0 re-index (768-dim) — not worth it given EmbeddingGemma already leads its weight class.
- **8GB VRAM — respected everywhere:** Qwythos stays on Q4_K_M (5.24–5.48 GiB; Q8_0 at ~9 GiB is borderline — avoid). bge-reranker-v2.5-gemma2 (9B) ruled out. Parakeet Q4_K (~467MB) fits trivially. No upgrade adds VRAM pressure.
- **NEVER-CALL-CLOUD — intact:** every recommended target is local GGUF/binary on :11436 or a pure-Go library. The go-sdk v1.7 pre-release (held) is the only item near the transport layer, and it is explicitly deferred.

## Per-dimension detail
**llama-swap (adopt now, risk low, confidence high):** Current v202 → latest v230. 28 releases in 10 weeks; three are load-bearing for you — v204 (matrix swap-race fix), v211 (matrix-logger regression from the v202 feature you run), v224 (fixes WebSocket + model-unload regressions from the v219 routing refactor). Staying on v202 means two active bugs in your exact matrix feature. GBNF passthrough is untouched (zero grammar commits repo-wide). Go straight to v230, which rolls up everything and adds purely-additive `-config-dir`. Only watch-outs: v225 added auth to /metrics (fix your Prometheus scrape if you have one), and matrix/groups remain mutually exclusive.

**llama.cpp (adopt soon, risk medium, confidence medium):** Current c34b922 (~b9628) → ~b9821. Rebuild buys grammar-generator/peg correctness fixes, the MoE quant+MTP fix (b9789), and a CUDA token-gen perf gain (b9820) — `--n-cpu-moe`/`-ot` semantics are unchanged, so the 26B CPU-MoE path is stable. **The call: rebuild, but understand it does not fix #23677** (the live Gemma 4 grammar crash). Two pre-flight items: audit for the new HTTP-400-on-bad-grammar behavior (b9704), and confirm any `--webui-*` flags still parse after the webui compat cleanup (b9726). With KV-cache quant + MTP, keep f16 KV cache (q8_0 drops MTP acceptance to ~0%).

**gemma-4 (adopt soon, risk medium, confidence high):** No new weights exist — Gemma 4 is unchanged since Apr 2 2026, no Gemma 5. The only action is a **correctness re-pull**: the Apr 11 `unsloth` re-upload fixed a broken chat template + llama.cpp parser bugs; GGUFs pulled earlier silently corrupt output. The Jun 9 MTP draft files are optional/low-priority. The grammar-crash risk lives in llama.cpp (#23677), not the weights — re-pulling does not change that. **Verify your actual pull dates** to know which tiers need re-pulling.

**embed-rerank (HOLD, risk low, confidence high):** EmbeddingGemma-300M (69.67 MTEB Eng v2, #1 under 500M) + bge-reranker-v2-m3 — both load-bearing across mem0, the harness B2 judge, and the meta-router. No challenger justifies the churn: Qwen3-Embedding-0.6B is 2x params and trails on English; nomic-v2-moe has an open llama.cpp crash; arctic-m-v2 is well below. jina-reranker-v3 is genuinely better (+5.4 BEIR) but needs a non-mainline fork — disqualifying on a load-bearing path. **Hold both;** re-open the reranker question only if jina-v3 lands in llama.cpp mainline.

**qwythos-qwenvl (adopt soon reasoning / hold vision, risk low, confidence high):** Reasoning tier → **Qwythos-9B-Claude-Mythos-5-1M v2** (empero-ai, ~Jun 19): same Qwen3.5-9B Q4_K_M, same VRAM, with a tokenizer-metadata fix that *improves* GBNF sampler init — a net positive for the sacred path on an arch already confirmed clean of the MoE enum bug. 1M context + better SFT are bonus. Adopt-soon (not now) only because it's ~7 days old and the "v2" label implies a v1 issue worth a brief watch. Vision tier: Qwen3-VL-4B-Instruct is still latest — **no action.**

**whisper (adopt soon, risk low, confidence medium):** db5a84b → v1.9.1 (or HEAD). Low-risk CUDA stability fixes for the running server (integer-overflow, cublasSgemmBatched) plus first-class Parakeet support inside the same binary — pre-empting a separate parakeet.cpp later. No grammar path in STT, so SACRED #1 is clear. The one behavioral item: **verify the server's new `no_context=true` default** doesn't break any context carry-over you rely on. Parakeet itself needs its own GGUF slot when you add the tier.

**go-deps (adopt soon, risk low, confidence high):** The critical dep — **go-sdk (MCP transport) is already at latest stable v1.6.1, no action.** Bundle the rest into one `go get -u` + `go mod tidy`: bbolt v1.5.0 (additive), cobra v1.10.2 + pflag v1.0.10 (bump together — pflag v1.0.8 renamed an export, v1.0.9 restored the alias), all `golang.org/x/*`, testify v1.11.1, regexp2 v1.12, segmentio/asm v1.2.1. **Hold go-sdk v1.7.0-pre.1** — its stateless protocol-2026-07-28 rewrite is breaking and would force a transport rewrite near the never-cloud boundary.