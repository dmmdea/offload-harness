# Local-Offload — ROADMAP (decided order)

> Single source of truth for the BUILD SEQUENCE. The design detail lives in the chapter briefs and ADRs (linked at the bottom); this file owns the ORDER, and the order is **decided** — not a menu. **Last updated 2026-08-26** (frontier-update refresh; the file had gone ~100 minor versions stale at 2026-06-26 / 0.4.1).

## Framing
Give the harness local capability — text · image · audio · video, each across 3 angles (**understand · generate · edit**) — so grunt work never reaches a paid context. Dual-track: public generalist + a private **Danmar Auto Reviews**-optimized track. Hard rules carried through every phase:
- **never-call-cloud** (defer to Opus on low confidence; `offload_nim` is the one opt-in exception)
- **GBNF grammar path is sacred** (raw `grammar` field; never `--json-schema`)
- **zero-always-warm** (llama-swap `ttl:300` + force-unload; ComfyUI `--disable-smart-memory --cache-none` + `/free`)
- **editing is Claude-driven** (Opus decides cuts; local tools execute)
- **quality-first on every tier** — a slow tier gets its binding fixed, never a defer-to-cloud gate
- the load-bearing memory stack (embeddinggemma + bge-reranker) must never break.

### Scope correction (2026-08-26)
This file used to say the target was an "**8 GB single-user** box (RTX 3070 Mobile + 64 GB RAM)". **That has not been true since ~0.29.0.** The harness is now a **multi-node fleet** with a hardware-tier system (`docs/tiers/`), a fleet-node server, and cross-node delegation:

| node | tier | role |
|---|---|---|
| **Qube** | `blackwell-2x16` — RTX 5070 Ti 16GB + RTX 5060 Ti 16GB, 128 GB RAM, Win 11 | primary; agent seat `qwen3.8-27b`, all media engines, mem0 authority |
| **Aorus 15P-XD** | `ampere-8` — RTX 3070 8GB, 64 GB RAM | fleet node, agent seat `qwen3.5-9b-agent` |
| **Lenovo M720q** | `ampere-6` — RTX 3050 6GB, 32 GB RAM, Kubuntu | fleet node, agent seat `qwen3.5-4b-agent` |
| **Dell OptiPlex 7060** | `ampere-8` + Hailo-8L NPU | editor box / accelerator tier |

A tier is a **hardware class, not a Windows class** (0.29.0), and **deployment state is never a constraint** hardened into a spec — nodes are addressed by endpoint, never by assumed placement.

## Done — through 2026-06-28 (0.4.1 era)

> Kept verbatim for the verdicts it records (FLUX no-go, ik_llama reject, the qwythos bench). For everything shipped **after** 0.4.1, see "Status as of 2026-08-26" below and `CHANGELOG.md`.

- ✅ **SVG component kit** — a brand-agnostic, pure-Go parametric `internal/svgkit` (gauge, comparison-bar, chromatogram, icon set) exposed as the `offload_generate_svg` MCP tool + `generate-svg` CLI. The #1 image-gen item: crisp, 100% legible, FREE data-viz with **no model, no ComfyUI, no GPU lock, no grammar, no cache** — a dedicated `core.TaskGenerateSVG` branch (`runGenerateSVG`) renders the SVG from a JSON spec, writes it under `cfg.SVGDir`, and returns `{svg_path,width,height}`; any bad kind/spec/write **DEFERS** (consistent with the harness, never cloud). **Brand-agnostic invariant:** every color/datum is a caller input; defaults are a neutral slate palette (`#1e293b`/`#ffffff`/`#0ea5e9`/`#94a3b8`) with **zero brand tokens** — pass a `theme {fg,bg,accent,muted,font}` to brand it. Pure + deterministic (same spec → byte-identical SVG); all caller text XML-escaped. Visually verified by rasterizing each component to PNG (headless Edge) and eyeballing it: gauge dial sweeps proportionally with centered value, bars scale with a distinct highlight, chromatogram shows clean Gaussian peaks on a labeled-axis baseline, icons legible. One visual fix (right-align comparison-bar labels to stop gutter clipping). Commits `4981ea1`→`4babaff`. Plan: `docs/superpowers/plans/2026-06-27-svg-component-kit.md`.
- ✅ **Image** — vision (`vqa`/`ocr`/`extract-image`/`assess-image`) + **generation now a live MCP tool** (`offload_generate_image` / CLI `generate-image`, local ComfyUI SDXL — was only a `render/` runner). **5 image MCP tools live.** Gen runs the zero-always-warm GPU lifecycle (single-slot file-lock → free only the GPU-tier llama-swap models → ComfyUI cold-start `--disable-smart-memory --cache-none` → render → process-tree teardown + `/free`) and **DEFERS** on any failure (never cloud). Returns `{image_path,width,height,seed}`. Live-verified end-to-end (CLI + MCP, real PNGs, memory stack intact). Adversarial pre-commit review found + fixed 7 bugs, all re-verified: HIGH lock/VRAM leak on the Go timeout-kill (JS `finally` bypassed by `os.Kill` → now Go-side `taskkill /T` tree-kill + defer `/free`); HIGH gpu-lock 1h deadlock (now reclaims on confirmed-dead pid in 2 ms); MED `freeLlamaSwap` unload-ALL tore down the CPU memory stack (now per-model unload of GPU tiers only); MED `seed:0` (now mints + reports the real seed). Commits `9142987`. Plan: `docs/superpowers/plans/2026-06-16-phase2-video-gen.md`.
- ✅ **Video understanding** — `video-describe` (Phase A.1): ffmpeg frame-sampler → interleaved-timestamp Qwen3-VL path. Shipped, merged to `master`, live-verified (temporal understanding). Plan: `docs/superpowers/plans/2026-06-16-video-describe.md`.
- ✅ **Audio understanding / STT** — `transcribe` / `offload_transcribe` (Phase A.2): ffmpeg → 16 kHz mono WAV → whisper.cpp `whisper-server` (CUDA large-v3-turbo, `-nfa`, Silero VAD v6.2.0) via llama-swap `/upstream/whisper-stt` (own `exclusive:true` `ttl:300` group; `--hq` = large-v3). Returns `{gist, segments[]}` + SRT/txt/json pointers. Live-verified EN (JFK) + ES (es_ES 0% WER accent-folded) + Colombian (es_MX voice/CO vocab: turbo 6.5%, large-v3 `--hq` 0.0% folded). Ritual config edit done — memory stack intact. Adversarial review: 1 MED (media-filename collision) fixed. Merged to `master` + pushed (7b61ec7). Plan: `docs/superpowers/plans/2026-06-16-offload-transcribe.md`.
- ✅ **Research + architecture locked** — chapter briefs + addendum (below). Decisions: Resolve Studio approved (Phase 3), editing=Claude-driven, zero-always-warm, **vLLM=no**, **ik_llama.cpp=spike**, **fastcontext citation-pattern borrowed**, **DiffusionGemma=watch**.
- ✅ **Disk freed** — D: +18 GB; C: 61.5 → ~235 GB. The video-gen stack (Wan 2.2 14B, Hunyuan 1.5) is **already in `C:\ComfyUI`**; only smaller models remain to pull.
- ✅ **Text tiers — qwythos adoption + local reasoning tier (2026-06-21)** — (1) **escalation tier `gemma4-26b-a4b → qwythos`** (Qwen3.5-9B SFT, empero-ai "Claude-Mythos"; the "Fable distill" provenance is the author's **UNVERIFIED** claim — GGUF declares base=Qwen3.5-9B). Benched vs gemma4-26b-a4b + Qwen3.6-35B-A3B on the 74-case gold set: 3-way mechanical **wash**, qwythos ¼ the size (5.6 GB) + faster + 100% grammar coverage. Added to llama-swap via the ritual (`--reasoning off` = thinking off so raw GBNF lands in content); gemma4-26b-a4b retained for rollback. (2) **NEW local reasoning tier** (`ReasoningModel`, **on by default**): after the cascade defers on a grammar task, qwythos reasons under a think-wrapped GBNF (`gbnf.WrapThinking` → `<think>…</think>` then the JSON; `parser.StripThink` strips it) to reclaim the deferral before Opus. Adversarial review found + fixed **7 bugs** (StripThink first-tag, think-rule derail, truncation/confidence gates, token budget). Eval on a 29-case hand-hard label-verified gold set: reclaims deferred classify **2/2 correctly** (coverage 88→100%, accuracy held 100%), strictly non-negative. Commits `9493bdc`/`1ba5311`/`46ddaed`/`91d59c0`/`5b5c3ce`/`2754b53`. Plan: `docs/superpowers/plans/2026-06-20-qwythos-adoption.md`. Also fixed: the MCP-orphan leak (Claude Code re-spawns without closing stdin) via the dev-server reaper. **(3) Observability marker (2026-06-21):** a `reasoning` bool on the ledger entry makes a reclaim distinguishable from an escalation answer (both run on qwythos, so `ModelTier` collided) — `ledger` now reports `reasoning_reclaims`, `stats` a per-task `reasoning_reclaimed` (mutually exclusive with `escalation_resolved`, since the escalation tier *deferred* on a reclaim), and `models` lists the `reason` tier. Purely additive + back-compat (old lines → false). TDD'd (caught + fixed a double-count bug during impl); 3-lens adversarial review returned SHIP (0 correctness findings).
- ✅ **Shadow-labeling flywheel — Phase A + B (2026-06-26, LIVE)** — the harness now manufactures its own counterfactual training labels nightly, closing inference→labels→model-update for **both** self-learning heads (confhead + entry-tier router). **Phase A:** request-time `captureShadow` samples non-escalated classify/triage/extract rows into `shadow-queue.jsonl` (**off by default**, `shadow_rate` 0.10); nightly `shadow-label` drains (atomic rename-claim, cross-process safe), reruns counterfactual tiers via `RunTier` with **`record=false`** — structural guarantee: shadow runs **never** pollute the savings ledger / cache / exemplars — judges (classify/triage=`answersAgree`, extract=grounding oracle) → `confhead-labels.jsonl`; `offload-dream.ps1` trains + a **regression-aware fail-closed** confhead adoption gate (adopt iff ci_lo>0 on ≥1 task, block any ci_hi<0, neutral never vetoes; trust-region ±0.1/night; staging — no auto-adopt without `-AutoAdoptStats`). **Phase B (merged `main` `0420e0b`):** **B1** entry-tier ROUTER counterfactual labels — rerun E2B on router-skipped classify/triage rows, synthesize a `gemma4-e2b` router-sidecar label (agree→accept-at-E2B) merged by `router.Train`, so the router finally learns from its **own skip decisions** (those rows were E4B-tier + invisible to training, the gap that kept `internal/router` an empty stub). **B2** summarize judge via **embeddinggemma cosine** (new `internal/judge`, threshold 0.80) instead of exact-match. Subagent-driven (5 tasks, per-task + opus whole-branch review = SHIP); live dry-run exit 0 + **savings ledger byte-unchanged**; B2 judge verified live (paraphrase 0.95 vs unrelated 0.21). Live binary rebuilt 2026-06-26 (rename-then-build over the running MCP; nightly staging-mode picks it up next run; request-time summarize-capture on next CC restart). Data accrues from live use; router needs **≥200** rows before it trains. Plan: `docs/superpowers/plans/2026-06-26-shadow-flywheel-phase-b.md` (+ Phase A plan in the same dir). Tracked as **meta-router v2** in `D:\repos\meta-router\docs\MASTER-PLAN.md` §6.
  - ✅ **kNN entry-tier pre-filter (zero-training bridge, 2026-06-27)** — a `KNNPreFilterEnabled` (**off by default**) substrate that, *before* the LR router has its ≥200 rows, embeds a classify/triage input (embeddinggemma) and skips the E2B tier when the k nearest past inputs were mostly rejected at E2B. The substrate (`knn-index.jsonl`, `{task,vec,accept}`) accrues **only inside `shadow-label`** when the flag is on (reuses the existing B1 E2B-counterfactual labels — no new inference). It is **fail-open everywhere** (nil receiver / missing substrate / unreachable embedder / thin substrate → keep default E2B entry) and **yields to the LR router** once `HasTask` is true, so its hot-path embed cost disappears automatically. Never touches the GBNF/cache/savings-ledger paths. Plan: `docs/superpowers/plans/2026-06-27-knn-entry-tier-prefilter.md`.
  - ✅ **Flywheel actually flows now (0.4.1, 2026-06-28)** — diagnosed why the flywheel had manufactured ~0 labels for weeks despite "LIVE": **two compounding bugs.** (1) The MCP server (`local-offload.exe mcp`, registered with no `--config` and an empty env) silently ran on built-in **defaults with shadow capture OFF** — `~/.local-offload/config.json` was never loaded. Fixed: `loadCfg`/`resolveCfgPath` now auto-discovers `~/.local-offload/config.json` when neither `--config` nor `$LOCAL_OFFLOAD_CONFIG` is set. (2) `internal/health` had flagged **all three tiers DEGRADED on drift/throughput** (single-GPU non-stationarity), and the cascade routes *around* any DEGRADED tier — so the accurate E2B entry (eval: triage 100 %, classify ~90 %, ewma_margin 0.957) was being skipped to a larger, slower tier, starving the flywheel of E2B-entry rows. Fixed: health now route-skips **only on a genuine quality collapse** (`route_skip` = margin far below the tier's own baseline); drift/throughput stay observability-only. e2e-verified: live `health` degraded list `[…3 tiers] → []`; a bare invocation auto-loads config and captures an `entry_tier:gemma4-e2b` row; the drain manufactures confhead + router labels. Adoption (`-AutoAdoptStats`) enabled via the operator's nightly scheduler (published default stays OFF).

## Status as of 2026-08-26 (0.103.0)

The 2026-06 build order below is **closed**. What it called Phase 2 shipped in full, plus several arcs it never anticipated. Detail lives in `CHANGELOG.md`; this is the reconciliation.

**Closed since the last roadmap update (0.4.1 → 0.103.0):**
- **Generation — all three media, done.** image (0.16.0, quality-first), video, audio music + voice, `run-graph` primitive (0.18.0), warm batch (0.19.0), generative inpaint (0.20.0), deterministic edit-op pack (0.21.0), generative instruction edit (0.44.0), ESRGAN upscale (0.77.0). The GPU single-slot lock this file asked for became the **machine-wide media lease** (`internal/gpulease`, ADR 0018/0026).
- **The fleet.** fleet-node server (0.22.0), node acceptance gate (0.33.0), Linux install path (0.34.0), hardware tiers as a first-class concept (0.29.0–0.42.0), multi-node sub-agent delegation (0.66.0), `route: spread` + cross-seat retry (0.80.0), fleet-node job queue (0.100.0), fit-scored placement (0.99.0).
- **The agent loop.** dedicated planner seat (0.43.0), vision + hearing (0.24.0), the compaction ladder + eval harness (0.22.17–0.23.1), per-tier `agent_profile` (0.70.0).
- **Delegation entry points.** `offload_ask` (0.96.0) + result cache (0.98.0), `offload_review_diff` clean-context review lane (0.97.0), acceptance lint (0.88.0), live fleet roster in `offload_status` (0.95.0).
- **Beyond the original scope.** Hailo-8L accelerator tier (0.82.0–0.86.0, ADR 0024), vendored printed CLIs under `tools/` (0.50.0–0.54.0), `video_watch` end-to-end viewing (0.93.0), in-tree opencode integration (0.91.0), the repo-local docs system (0.22.1).

**Still open from the old order:** Phase 3 **video** editing / DaVinci Resolve (image editing shipped; the video cut-list path did not) and the **Danmar Auto Reviews capstone**. Both are parked below, behind the frontier update.

---

## CHECKPOINT 2026-08-28 — the post-approval execution sweep (T-items all closed; the board is clean)

Everything the operator approved on 2026-08-27/28 shipped, deployed, and behavior-verified in one arc — PRs #195–#203, each CI-green and merged by `dmmdea`:

- **T4 closed** (#195): MiniMax-H3 + WAN-Animate-2 wired from the official templates, bake-off verdicts recorded above. **H3 then SEATED** as the opt-in `model:"h3"` videogen family (#198, 0.107.0, deployed-path render verified); the **r2v DiT** (20.97 GB) is on disk for the reference-to-video lane. **WAN-Animate ROUTED** (#196, 0.106.0): `offload_animate_character` + `animate-character` + fleet task `animate`, live-verified; the template's `gpu/int8` pose cache hard-kills ComfyUI under ComfyUI-MultiGPU (filed as MultiGPU#219 after a clean-ComfyUI differential exonerated core).
- **Delegation durability shipped** (#199, ADR 0028, 0.108.0): the push-side intent ledger + orphan recovery. **Option B built DARK** (#203, ADR 0030, 0.109.0): `internal/fleetqueue` + claim loops + `route:"queue"`, inert until its three config keys are bound — enabling stays scoreboard-gated.
- **Lenovo FreeToken agent lane: attempted, REVERSED** (ADR 0029, #202): fully wired (pool install under systemd hardening, llama-swap-managed seat), then the real delegation path caught **cross-request response contamination** — reverted to the 4B seat (re-verified). The arc's harness fixes stay fleet-wide: grammar-free chat re-pack fallback (#200, 0.108.1) + schema-guided scalar coercion (#201, 0.108.2) — prerequisites for ANY OpenAI-only seat.
- **video_watch at scale proven**: a 44-min 4K published video fully analyzed (45 windows @1024px + whisper), catching a leftover Veo AI-stock watermark; the tail-window/ffmpeg-9 fixes + the WaitFree deflake shipped in #197 (0.106.1, closes #81).
- **Fleet parity**: Qube + Lenovo + Aorus all at **0.109.0** (Aorus topped up 2026-08-28 the moment it reappeared from a shared overnight outage: binary swap under `D:\offload-stack\bin` with the old process killed first — cycling the task does not reload, FLEET-NODE.md gotcha 3 — then `/fleet/health` 0.109.0 and a forced-remote `agent_delegate` contract passing on BOTH remote seats, 9B in 3.0 s / 4B in 15 s). Aorus native llama.cpp **b10621** + llama-swap v251. Its WSL llama stack is RETIRED (native flip 2026-08-25) — rebuilt as a spare, unit disabled; do not start it (WSL mirrored networking port-collides with the native listener). **The fleet-node boot trigger is PROVEN**: the 2026-08-25 S4U + `-AtStartup` registration fired on that reboot with nobody logged in (`LastRunTime` == boot time, launcher log "node UP") — the "admin run" item was already done, not pending.
- **sd.cpp on the reference box** (0.109.0 changelog): master-829-0a565f2 official win-cuda12 + the Lenovo's exact sdxl-turbo Q4_0, bound as `sdcpp_bin`/`sdcpp_model` in both deployed configs, render-verified; **fallback only** — `imagegen_engine` unchanged, H3-via-sd.cpp stays parked on upstream #1871 (open). *(The "sd.cpp on Qube" entry in Parked below is superseded by this.)*
- **T1-A hibernation verified LIVE on b10621** (operator-ordered check): seat_start → save → committed vault blob at `U:\kvstate`, no scheduled task involved; the old "one UAC consent blocks go-live" claim was stale.
- Also closed: disk reclaim (old llama.cpp trees + cu128 venv), leak-scan hooks on all 7 public-origin clones, Docker-leftovers resolved on the reference box, pull-queue decision doc retired into ADRs 0028/0030.

- **KV-layer research arc (2026-08-28, LMCache/vLLM) — RESEARCHED + MEASURED, design gate OPEN.** Handover: `Ecosystem/Benchmarks and Optimizations/2026-08-28-lmcache-kv-layer-research-handover.md` (operator drive). Verdicts: LMCache is vLLM-only (no llama.cpp/GGUF path exists or is tracked); its MP server must share the GPU host (CUDA IPC + `/dev/shm`), so a second box can only be an L2 tier (Valkey/RESP, NFS `fs`, S3) — that is the Lenovo's role once its A2 lands. Spike on Qube-WSL (Qwen3.5-9B FP8, 5060 Ti): a CPU KV tier turns the post-eviction re-prefill of a 12 k-token contract from **2.97 s into 0.33 s** — delivered by **vLLM's native `OffloadingConnector`**; LMCache itself cannot run its fast paths under WSL2 (legacy CUDA IPC is dead there, measured; the IPC-free path rejects hybrid KV groups and its pickle fallback measured 11.6 s, slower than recompute). Real-path `delegate` through vLLM passed (needs `--enable-auto-tool-choice --tool-call-parser qwen3_coder` and thinking off server-side). A vLLM side-process on a shared card loses to the 27B seat — a vLLM seat needs its own card. **Options awaiting the operator:** C = contract packing in the harness (docs-first, block-aligned; engine-independent) · A′ = a vLLM seat behind llama-swap with the native CPU tier on the third GPU · LMCache MP on the Linux Lenovo (network L2 / same-model sharing) · B = first-class `engine: vllm` with the pooled build. Nothing built until chosen.

- **HARNESS-FIRST ENFORCED (2026-08-30, operator order after the "three Opus agents, zero contracts" incident; ADR 0031).** Advisory nudges never moved the adoption metric (autopsy 2026-08-18: zero organic agent lanes; audit 2026-08-21: 72 % of bulk-read sessions used no offload); the guards that deny did. Shipped: (a) claude-config PR #33 — `offload-first-guard.js` DENIES a read-only subagent spawn unless the prompt carries `harness-exempt: <web|write|judgment|capacity> — <reason>` (H15 classifier reused; judgment/implementation spawns untouched; every decision in the dispatch log), `harness-loop-guard.js` DENIES bulk network loops in the session shell (≥ 8 iterations or list-driven); 21-case test suite; constitution rewritten to the enforced contract. (b) **0.110.0** (PR #208) — `offload_research` MCP tool + `local-offload research`: URLs in, pages fetched DELEGATOR-side under a public-web guard (no loopback/private/link-local/.local/tailnet/CGNAT; redirects re-checked; 2 MiB / 96 KiB caps), stripped to text, one grounded contract per page across the fleet (route spread) — "needs the web" is no longer a cloud-subagent reason. Contract rules measured live: the goal must name the MATERIALIZED context file and tell the seat to read it (a "do not open anything" goal made 3/3 seats report the document missing); acceptance = regex alternation of page-only tokens, hex blobs excluded; the structured re-pack budget went 512 → 1024 (root cause of the "invalid json: unexpected end of JSON input" abstention class, incl. the 2026-08-28 long-extraction ones). Live: 3 LMCache pages → faithful digests on Qube 27B (57–73 s) and Lenovo 4B (54 s). Deployed: Qube MCP binary + Lenovo 0.110.0; **Aorus pending (offline)**. Re-measure with the 2026-08-21 metric (organic `agent_delegate`/`offload_research` in non-harness-dev sessions) ~2026-09-13.

- **48 GB TIER OPENED (2026-08-31): third GPU installed (2× 5060 Ti + 5070 Ti = 48.9 GB); full-roster frontier research COMPLETE, candidates DOWNLOADED, testing next.** Research doc (proposal + §6.5 measurement gates): operator drive `Ecosystem/Benchmarks and Optimizations/2026-08-31-48gb-tier-roster-research.md`. Method: harness-first — 27 sources digested via `offload_research` on the local seats, zero cloud subagents. Headlines: Flash-Next both re-eval triggers FIRED (llama.cpp mainline merge 2026-08-27 ≥ b10664 + FreeToken qwen4_exp); proposal = new opt-in big seat Flash-Next UD-IQ4_XS with experts/PLE in RAM; the 27B stays the cascade agent seat (size-class outlier — no 2026 release in 40–150B beats it, 70B dense NOT seated); VL-32B for vision/ocr; escalation/reasoning promote within the Gemma-4 family; stt_hq = whisper-large-v3; Krea 2 RAW + LTX-2.5 bf16 quality lanes; Chatterbox v3; FLUX.2-dev/Ideogram license-gated OUT. ~320 GB of candidates downloaded and verified (incl. Qwen2.5-72B Q4 as the operator-requested 70B 3-card pooling reference, and the disk-swap-lost H3 r2v DiT RESTORED); manifest `V:\models\_48gb-download-manifest.txt`. **NEW PLACEMENT RULE (operator): 2-card models live on the 5060 Ti pair; the 5070 Ti is interactive-first and joins only for over-2-card work.** Testing session: llama.cpp ≥ b10666 first, then the §6.5 gates; tier matrix updates FIRST on every decision.

**Standing next actions:** the monthly sweep (first October nightshift) carries the watches — FreeToken#240, llama.cpp PR#27742 (Flash-Next), MultiGPU#219, sd.cpp#1871, ADR 0029's contamination differential, ADR 0030's enabling scoreboard. Aorus housekeeping, operator-decided 2026-08-28: Docker Desktop stays installed (service stopped/manual, `docker-desktop` WSL distro stopped — leave it); the 33 stale `local-offload*.exe` backups (631 MB) were deleted from `D:\offload-stack\bin`, leaving the live 0.109.0 + one pre-0.109.0 backup; the node's port file now carries its verified adapter facts. Its `D:\repos\local-offload` clone is **vestigial**, not "gated": nothing runs from it — the MCP server and the fleet task execute `D:\offload-stack\bin\local-offload.exe`, and the harness config's `imagegen_script` points at `D:\offload-stack\bin\render\` (all 65 render `.mjs` hash-identical to this repo at 0.109.0). The private repo went moat-only (its PR #22), so that clone can never fast-forward again; delete it or replace it with a public-canonical clone for on-box builds — an operator call, not a blocker.

**Contract-authoring note (measured 2026-08-28 on both remote seats):** a goal that says "Read the document…" sends the 9B seat hunting for a file and it fails acceptance; "The context document `<name>` is already provided to you…" passes on the 9B and the 4B alike. Inline context is not a file — say so in the goal.

---

## Decided order — frontier update (2026-08-26)

Source: `2026-08-26-offload-stack-frontier-update-handover.md` (research session). **Every version number below was re-verified live on Qube on 2026-08-26** before being written here — §8 of that handover warned its own numbers were single-sourced, and four of its cautions turned out to be already satisfied (recorded in "Corrections" at the end of this section).

### ROUND 2 — 2026-08-27 morning (operator review feedback, 9 items, all executed)

**Corrected claim** (operator caught an inference dressed as a fact): "GGUF is dead for us"
was wrong scope. Verified: qwen4exp GGUF → verbatim `GGUF architecture 'qwen4exp' is not
supported (known: ['gemma4'])`; gemma4 **UD** quants fail a dtype assert (`layers/base.py:45`)
on BOTH 0.1.2 and git main; standard Q4_0 QAT gemma untested (none on disk).

**Freshness (operator bet right twice):** FreeToken git main > pip (new `--gpu` flag, fla
l2norm perf fix) — both nodes upgraded to `main@9ef3651`. PR #27742 +3 commits (fused-QKV
segmentation for tensor split; still no MTP; still unmerged) — scratch build now
`build 511 / 250b614`, on CUDA 13.3.

**Same-model race blocked by a real upstream bug:** `ft serve Qwen/Qwen3.8-27B-FP8` is
ACCEPTED at config level (`cache_type=hybrid_radix` — it knows the GDN-hybrid arch) but the
fp8 weight loader dies (`qwen3_5_moe/weight.py:_iter_weights_fp8` → "CUDA driver error:
device not ready") while Qwen3.6-35B-FP8 loads fine in the same distro — an upstream-issue
candidate, surfaced (not filed unattended).

**Tuning fact that changed a verdict:** `--moe-cache-auto` sizes KV down to ~8.2k tokens —
long prompts 400 with "Input sequence length 22193 exceeds 8202". `--num-tokens 32768`
costs only 0.62 GiB and fixed it. Any FreeToken deployment here MUST set explicit KV.

**Suite v2** (built from the model card's OWN gap chart — JobBench +22.3, DeepSWE +16.5,
Toolathlon +6.4, SWE-multilingual +7.2 → the claimed edges are agentic/tool/multilingual):
8 tool-call-fidelity (OpenAI tools) + 5 multi-hop joins inside 22k-token docs + 5 Spanish.

| engine | score | total wall |
|---|---|---|
| incumbent qwen3.8-27b (llama.cpp, 2 cards) | 18/18 | **271 s** |
| FreeToken Qwen3.6-35B-FP8 (1 card, tuned) | 18/18 | 410 s |
| **FreeToken gpt-oss-120b (1 card)** | **18/18** | **678 s** |
| Flash-Next IQ4_XS (llama.cpp PR, 2 cards) | 18/18 | 1828 s |

> **APPROVED (operator, 2026-08-27): FreeToken seated as the big-MoE opt-in engine — ADR 0027.** Flash-Next: operator direction is to WAIT for upstream updates before a fair re-measure; re-eval triggers = PR #27742 merged to master, MTP implemented for qwen4exp, or FreeToken gaining the arch. The IQ4_XS weights stay on V: for that re-measure.

**The 120B row is the FreeToken verdict:** a model class this box could never usably serve
runs perfect-scoring on ONE 16 GB card, faster than Flash-Next on two. It earns the big-MoE
seat. Flash-Next: quality parity persists even on card-informed differential tiers; its
measured cost (6.7× wall vs incumbent) stands; verdict unchanged.

**Agentic tier: instrument invalid, honestly.** A throwaway-config `delegate --contract` run
produced EMPTY structured output on BOTH the candidate and the production incumbent —
both-arms-empty = my harness config, not the models (the incumbent passes real contracts
daily). A production-representative agentic A/B needs the real delegation path, which only
serves roster seats — recorded as the follow-up, not faked.

### STANDING PROCESS — monthly whole-system update & upgrade (operator, 2026-08-27)

On the first nightshift of each month, run the full stack sweep exactly as executed
2026-08-26/27: (1) re-read every component's RELEASED version (semver-blessed tag, never
newest nightly) — llama.cpp, llama-swap, whisper.cpp, ComfyUI + pinned frontend/kitchen/
aimdo + custom nodes, torch/CUDA, FreeToken, Go/Node/Python/ffmpeg/GIMP, Go module
advisories; (2) verify against live state, upgrade side-by-side with backups, one elevated
batch; (3) prove every upgrade with a named live check (not exit codes); (4) fold in the
deliberate holds — Go major (json v2) and go-sdk v1.7 (breaking MCP protocol) — as their
own tested migrations when their month comes; (5) update ROADMAP + tier matrix + port files
+ mem0 in the same pass. Fleet rule: Qube + Lenovo same night; Aorus the moment it answers.

### NIGHTSHIFT 2026-08-27 — FreeToken measured on both nodes; Flash-Next testing COMPLETE

**FreeToken** (FlashML-org, arxiv 2608.16157 — edge-native MoE serving; Apache-2.0, 8.5k stars),
tested per operator direction on Qube and Lenovo with a calibrated 18-task exact-answer
instrument (dead-endpoint 0/N proven before any number was trusted):

| node | model | result | comparator |
|---|---|---|---|
| Qube (1×16GB via WSL distro `freetoken`) | Qwen3.6-35B-A3B-FP8 | **24/24**, warm decode 15.5 tok/s | incumbent 27B (2 cards): 24/24, 20.2 tok/s |
| **Lenovo (6GB RTX 3050)** | **gpt-oss-20b MXFP4** | **36/36 in 712 s** | **4B seat: 32/36 in 1368 s** |

The Lenovo row is the headline: a **5× bigger model, more correct, half the wall, same 6 GB
card**. On Qube the wall gap vs the incumbent is Qwen3.6's 4.2× thinking-token verbosity, not
engine speed. Constraints that bound integration: **no grammar/json_schema** (the GBNF cascade
can never ride it); the GGUF loader rejects Unsloth UD quants (our whole GGUF library —
safetensors/MXFP4/q4_0 only); on a 6 GB box FreeToken and the llama-swap seat are mutually
exclusive tenants (measured: 502s while ft held the card); Linux-only CLI (Qube = dedicated WSL
distro, isolated from mem0's; Lenovo = ZFS pool install — home-dir quota kills `[accel]`,
`UV_CACHE_DIR`+`HF_HOME` must live on the pool). `--moe-backend` auto-selection is already
optimal for FP8 checkpoints (hybrid/cpu reject `fp8_block` experts).

**Integration verdict:** a real adoption case on the **ampere-6 tier agent/delegation lane**
(tool-call parser present, quality win measured) and as the **opt-in >VRAM MoE engine** on
Qube (the colibri niche). The harness reaches it as a plain OpenAI endpoint — config, not
code. Needs an ADR + port-file rows (Qube :1919 WSL, Lenovo :1920) before production wiring —
surfaced for the operator, not wired unattended.

**Flash-Next, completed on the correct quant.** UD-IQ4_XS (93.68 GB byte-verified, the best
quality that fits stably — not the Q2 the first pass used). Placement ladder (3 reps/arm,
expert-balanced splits, spill guards): best-safe **`-ncmoe 36 (42,6)` = 9.6 t/s**; fragile
10.4 at 172 MiB free; no Q2-style spill collapse. **Ready-to-adopt seat config** (full 131k
context, q8_0 KV, healthy 2.8/2.1 GB headroom):

```
-ngl 999 --n-cpu-moe 40 -sm layer --tensor-split 44,4 -c 131072
--cache-type-k q8_0 --cache-type-v q8_0 --flash-attn on --parallel 1 --load-mode auto
```
→ 8.3–8.7 t/s. **Quality: parity with the incumbent at every measurable difficulty** — base
(12), hard (6), and frontier competition-style (5, hand-verified) tiers, card sampling both
sides: 46/46 each. No separation found; the candidate costs ~2× wall and ~70 GB RAM for it.
**Verdict unchanged: no adoption today; the laguna rule holds (PR #27742 still unmerged).**
The config above is the drop-in seat entry if the PR merges and a real long-context workload
shows the 262k-native advantage. FreeToken is not an alternative path (arch unsupported).

### 48GB GATES + 3-CARD OPTIMIZATION — 2026-08-31 → 09-01 (operator-decided via lavish review)

Every §6.5 measurement gate ran; the operator approved seatings from the evidence; the
3-card placement law was enforced fleet-wide on Qube. Full ledger:
`Benchmarks and Optimizations/2026-08-31-48gb-gates-NOTES.md` (Drive). Canonical matrix
updated first per house rule. Highlights, each with its named check:

- **llama.cpp b10621 → b10720** (Flash-Next arch mainline). T1-A slot-restore proven by
  prompt_n 131→1 across an evict/reload; the pre-merge qwen4exp q8_0-KV crash is CLEARED.
- **Placement law (operator): 2-card seats on the 5060 Ti PAIR only; the 5070 Ti serves
  interactive first and joins only for 3-card work (first-class).** All incumbents re-pinned;
  singles balanced across both 5060s; verified by card telemetry (27B: 11.9+14.1 GB on the
  pair, 5070 Ti at desktop-idle).
- **Seated:** `qwen3.8-flash-next` (opt-in big seat, ncmoe28 28,10,10, q8 KV, 131k;
  12.5–12.9 t/s) + `qwen3.8-flash-next-262k` twin (native window, batch-class: pp 69 t/s) +
  `qwen3-vl-32b` (vision/vqa split seat) — the 8B keeps OCR (32B wins MMMU +11 but refuses
  verbatim transcription: invoice NED 0.773 vs 1.000). `qwen3.8-27b-262k` upgraded to q8_0 KV
  (drafter dropped — fidelity-over-speed on the longctx seat; q8+MTP does not fit the pair).
- **Real-path A/B (20 doc-grounded contracts/arm, CLI `delegate --contract`):** incumbent
  27B **20/20 PASS median 28 s**; Flash-Next **15/20 median 539 s (15× wall)** — slow decode
  compounds across loop steps into budget-defers. **Routing verdict: FN strictly opt-in;
  general delegation stays on the 27B.** The instrument-parity story (15/16 both) does not
  survive the production path — measure there before seating anything as a router target.
- **Rejected on evidence:** 27B Q6 re-quant (14/16, OOMs the seat contract), stt_hq
  large-v3 (loses to turbo on the house set), Krea 2 RAW (terminal noise under every recipe
  on ComfyUI 0.34; no official serving recipe exists — files deleted, 37 GB back).
- **Disk-swap casualties #2 and #3 found live and fixed:** krea2_turbo_bf16 restored into
  the wrong model-class dir (enum-invisible to UNETLoader) and the missing H3 turbo LoRA
  (re-fetched). H3 lane verified: 6.2 min joint-AV r2v on the 48 GB pool (was VRAM-tight).
- **UPSTREAM BUG (open): ComfyUI-MultiGPU `wrap_for_dlpack_with_device_guard` breaks int8
  DiTs on non-default CUDA devices** — loads `libcudart.so` by POSIX name on Windows, then
  cudaErrorIllegalAddress once shimmed. Caught via a ctypes diagnostic shim (kept in the
  ComfyUI venv, logs + maps POSIX CUDA names). Local == upstream HEAD; report queued.
  Interim law-exception: int8 video DiTs compute on the 5070 Ti; bf16 image pools run on the
  pair (proven). Fleet-node bug also open: the Lenovo seat intermittently answers a phantom
  "Go version" goal (7+ repros logged) — dispatch-path contamination, needs a fix here.

### T1. Qwen3.8-Flash-Next — **THE CURRENT FOCUS (operator, 2026-08-26)**

> Operator direction: *"qwen 3.8 NEXT is the focus right now..... careful...."* The caution is the **laguna-s-2.1 precedent** — a seat built on a fork binary whose arch mainline never absorbed, which produced non-terminating thinking (EOG tokens unregistered in the fork build) and was eventually deleted for 72.1 GB back.

**Verdict: build and measure it LOCALLY now; do not make it a seat until the PR merges.** The code-level objections have been resolved upstream (table below), so an early local read is worth having. What has *not* changed is the laguna rule: **nothing enters `llama-swap.yaml` until the arch is in mainline.**

**Live PR state** (`gh pr view 27742 --repo ggml-org/llama.cpp`, checked 2026-08-27):

| field | value |
|---|---|
| state | **OPEN**, `mergedAt: null` |
| mergeable / mergeStateStatus | MERGEABLE / **BLOCKED** |
| size | 2859 additions, 39 deletions, 23 files, 27 commits |
| activity | **76 comments in ~11 hours**; 20+ report crashes, asserts or garbage output |
| author | `danielhanchen` (Unsloth) |

**⚠️ Read the code, not the comment thread.** The 76-comment thread is a **lagging indicator**. Every blocker below was first recorded from the discussion, then re-checked against the actual source at PR head (`0b19188`, checked out locally 2026-08-27). **All four had already been fixed in code.** Anyone triaging this PR from its comments will reach a conclusion that is roughly a day out of date.

| blocker (from the thread) | state in the code at head `0b19188` | evidence |
|---|---|---|
| Quantized KV crashes — `build_attn_qsa` lacks Hadamard rotation, asserts at `qwen4exp.cpp:544` under `--cache-type-k q8_0` | ✅ **FIXED** | `build_attn_qsa` now rotates q/k/v via `llama_mul_mat_hadamard` before the quantized cache and un-rotates on the value side; the code comment reads *"rotate q/k/v before they reach a quantized cache, as the dense path does"* |
| `-np 2` crashes — `LLM_ARCH_QWEN4EXP` missing from the `graph_max_nodes()` large-budget list | ✅ **FIXED** | `src/llama-context.cpp:2304` now lists `LLM_ARCH_QWEN4EXP` alongside `QWEN3NEXT` |
| MTP is WIP / `--spec-type draft-mtp` unusable | ✅ **WIRED** | `COMMON_SPECULATIVE_TYPE_DRAFT_MTP` plumbed through `common/arg.cpp` (flag parse + `opts.download_mtp` sidecar auto-download) and `common/common.cpp`. Implemented — correctness still ours to measure |
| ggerganov: isolate the `llama-kv-cache` / `llama-memory-hybrid` changes into a new `llama-memory-hybrid-idx` | ✅ **DONE** | `src/llama-memory-hybrid-idx.{cpp,h}` exist (676 + 185 lines); `llama-kv-cache.cpp` **shrank by 307 lines** as the logic moved out |
| ngxson: `predecessors` must live in `llama_memory` or context save/load corrupts | ✅ **appears addressed** | `predecessors` now survives only as a **comment** at `qwen4exp.cpp:905` — no state variable outside the memory module |

**Why this mattered to us specifically:** the quantized-KV crash was not academic — **both** our Qwen3.8 seats run quantized KV (`qwen3.8-27b` at `q8_0/q8_0` 131k, `qwen3.8-27b-262k` at `q4_0/q4_0` 262k), so had it still been broken, serving Flash-Next the way we serve our current Qwen seats would have crashed on load. Both seats already run `--parallel 1`, so the `-np 2` bug never applied to us.

**What actually remains** is not a code defect: the PR is **unmerged and not yet approved** (`mergeStateStatus: BLOCKED` is branch protection awaiting review, not a conflict). The laguna rule therefore still holds — **keep it out of `llama-swap.yaml` until it is in mainline** — but an early **local** read is well justified, and is what we are doing.

**Adoption gate — every line must be true before this becomes a seat:**
- [x] quantized-KV path fixed — Hadamard rotation lands before the quantized cache (verified in source at `0b19188`)
- [x] `-np`/graph-budget crash fixed — `LLM_ARCH_QWEN4EXP` in the `graph_max_nodes()` large-budget list
- [x] MTP wired — `--spec-type draft-mtp` plumbed through `common/arg.cpp`
- [x] the ggerganov memory-hybrid refactor has landed — `llama-memory-hybrid-idx.{cpp,h}` exist
- [x] `predecessors` no longer lives outside the memory module
- [ ] **measured here:** decode t/s vs the incumbent `qwen3.8-27b` seat on **real harness agent contracts**, MTP draft depth swept at **2 and 3** (the card says acceptance collapses at ≥4)
- [ ] **quality** holds on our own contracts at UD-Q2_K_XL — 85.2% top-1 is the vendor's number, not ours
- [ ] PR #27742 **merged to master** — the laguna rule; a fork binary never becomes a seat
- [ ] re-verify the GGUF quants still load after merge — a re-shaped conversion path can invalidate quants published against the old one
- [ ] tier matrix updated **first** (house rule)

### T1 — MEASURED ON QUBE, 2026-08-26

**It runs.** Built PR head `0b19188` from source (MSVC 19.44 + CUDA 12.8, `CMAKE_CUDA_ARCHITECTURES=120`), pulled `unsloth/Qwen3.8-Flash-Next-GGUF` UD-Q2_K_XL and byte-verified all three shards (78,869,128,864 exact). Loads in 20.8 s under mmap and returns coherent output with visible reasoning.

**Architecture, from the GGUF header** (`general.architecture = qwen4exp`):

| | |
|---|---|
| layers / experts | **48** blocks, **512** experts, **10 used per token** |
| attention | 24 heads / **2** KV heads, key+value length 256 |
| **`full_attention_interval`** | **4** — only **12 of 48** layers carry a growing KV cache; the other 36 are Gated DeltaNet with fixed recurrent state |
| context | 262,144 native |
| size label | `512x56B` |

That hybrid layout makes long context genuinely cheap: ~24 KB/token, so **131k ≈ 3.2 GB and the full 262k ≈ 6.4 GB** of KV. On a dense model every layer would pay.

**Expert placement is the dominant throughput lever, and it has a cliff.** `--n-cpu-moe N` pins layers `[0,N)`'s experts to CPU. Measured, 8k ctx, mmap, 3 reps each:

| `-ncmoe` | `--tensor-split` | decode t/s | free VRAM 5060 / 5070 | verdict |
|---|---|---|---|---|
| 48 (all CPU) | default | 8.29 / 8.34 / 8.48 | ~7 GB | safe, slowest |
| 40 | default | 8.82 / 9.29 / 9.48 | ~4 GB | safe |
| 32 | `40,8` | 11.01 / 11.22 / 11.09 | 8545 / 2432 MiB | safe |
| **30** | **`37,11`** | **11.17 / 11.36 / 11.35** | 5010 / 3605 MiB | **safe, best balanced — recommended** |
| 28 | `38,10` | 11.71 / 12.15 / **12.24** | 5992 / **754 MiB** | fastest, **fragile** |
| 24 | `36,12` | **0.85** | — / 565 MiB | **silent spill** |
| 20 | `34,14` | — | — | hard OOM at load |

Three things that cost real time and are worth not re-learning:

1. **`--n-cpu-moe` disables llama.cpp's auto-fitter.** With `-ngl 999` it aborts with *"n_gpu_layers already set by user"*; with `-ngl auto` it aborts with *"tensor_buft_overrides already set by user"*. `-ngl auto` does **not** rescue you — `--n-cpu-moe` sets the overrides itself. Placement must be explicit.
2. **`-sm layer` splits by layer COUNT, but expert-bearing layers are ~10× heavier** (~0.9 GB each vs attention-only). An even split hands the whole heavy tail to one card and OOMs it at ~23 GB of 32.6 GB while the other sits half empty. `--tensor-split` must equalise *expert* layers, not layers.
3. **Between "fine" and "OOM" there is a silent-spill band.** `-ncmoe 24` loaded, answered correctly, and ran **13× slower** because one card had 565 MiB free and the driver fell back to host memory. **A successful load is not evidence of correct placement — only throughput is.** The threshold here sits between 565 and 754 MiB free.

**Recommended config if this is ever seated:** `-ngl 999 --n-cpu-moe 30 -sm layer --tensor-split 37,11 --load-mode auto --parallel 1`, `-fa` pinned, and **never `--load-mode none`** — see below.

**`--load-mode none` is barred on this box, and the handover's advice to use it was wrong.** `none` means *no mmap*, turning 78.87 GB of weights into private commit. Measured: `CommitLimit` 147.7 GB, `CommitFree` **56.8 GB**, `C:\pagefile.sys` fixed (`AutomaticManagedPagefile = False`). It does not fit, and Windows cannot grow a fixed pagefile; commit exhaustion lands on whichever process allocates next — most likely the WSL VM hosting mem0 (`:18791`) and Qdrant. `--no-mmap` and `--load-mode none` set the *same* enum, so the stated rationale ("`--no-mmap` has been ignored for weeks") described no real difference. Revisiting it requires raising the pagefile to ≥96 GB first — a consequential system change needing explicit approval, i.e. a separate experiment.

**MTP is architecturally absent — the ~2× claim cannot be realised on llama.cpp at all.** Not a missing download: `grep -ic "nextn|mtp" src/models/qwen4exp.cpp` returns **0**, so `hparams.n_layer_nextn` stays 0, the MTP guard returns `nullptr`, and the server aborts *after* loading ~73 GiB. Flash-Next's GGUF declares no `nextn`/`mtp` keys, while the **incumbent's** GGUF *does* declare `qwen35.nextn_predict_layers=1` — the incumbent is the seat that could use MTP, not the candidate. No GGUF MTP sidecar exists in any repo (only MLX/Apple-Silicon builds). Report this as *"MTP absent from the arch implementation"*, never as *"MTP did not help"*. Also: `qwen4exp` is absent from `llm_arch_supports_rs_rollback`, so a rejected draft would force a full checkpoint restore of 36 Gated-DeltaNet layers — speculation here is plausibly net-negative even once implemented. The viable substitute arm is `--spec-type ngram-mod`, which needs neither a draft model nor nextn tensors.

### T1 — HEAD-TO-HEAD vs THE INCUMBENT

The comparator runs the **incumbent on the same PR binary** (`B_pr`), so an A-vs-B gap is a *model* gap and not a compiler/CUDA-major gap. A second arm (`B_prod`) runs the incumbent on the production `b10435` prebuilt purely to size that confound. Neither goes through llama-swap `:11436` — that seat is `--parallel 1`, so benchmarking through it would serialise with live sessions in both directions.

| arm | binary | decode t/s (3 reps) | median |
|---|---|---|---|
| **A** Flash-Next, best safe (`-ncmoe 30`) | PR build | 11.17 / 11.36 / 11.35 | **11.36** |
| A Flash-Next, fastest (`-ncmoe 28`, fragile) | PR build | 11.71 / 12.15 / 12.24 | 12.15 |
| **B_pr** incumbent `qwen3.8-27b` | PR build | 24.88 / 25.03 / 24.95 | **24.95** |
| B_prod incumbent `qwen3.8-27b` | b10435 prebuilt | 25.12 / 25.12 / 25.09 | 25.12 |

**The build confound is measured at 0.7%** (`B_prod − B_pr`), far below the 10% that would have forced a Clang-cl rebuild of the PR to make the comparison safe. So the numbers stand as a model comparison.

**Flash-Next runs at 0.46× the incumbent's decode** (0.49× in the fragile config).

⚠️ **Do not quote a prefill ratio from the table above.** Those `pp_tps` figures come from 4–40 token prompts and are dominated by per-request overhead, not throughput — on the real 24k-token prompt Flash-Next's batched prefill measures **113 t/s**, an order of magnitude above the "~15 t/s" a short-prompt reading suggests. The only valid prefill comparison is the depth measurement below, at a realistic prompt length, with `cache_prompt: false`.

**Depth test — the last thing that could have saved it.** Flash-Next's `full_attention_interval = 4` means only 12 of 48 layers grow a KV cache, so in principle it should degrade *less* with depth than the dense incumbent. Measured on an identical **23,541-token** prompt, `cache_prompt: false`, `cache_n = 0` (no cache reuse), both on the PR binary:

| arm @ 24k depth | prefill t/s | decode t/s |
|---|---|---|
| Flash-Next `-ncmoe 30` | 159.5 / 177.9 | 9.61 / 10.30 |
| Incumbent `qwen3.8-27b` | **1382.4 / 1387.6** | **22.44 / 22.61** |

**It does not gain at depth — it loses slightly more.** 0.44× decode at 24k versus 0.46× at 8k, and **0.12× prefill** (8× slower to ingest). The KV advantage is real but irrelevant: the bottleneck is reading CPU-resident expert weights over DDR, not the KV cache. The "long-context specialist" outcome required **≥1.5×** decode at ≥32k depth; measured **0.44×**.

**Verdict: REJECT — both as the agent seat and as a long-context specialist.** It is roughly half the decode speed and an eighth the prefill speed of a model one quarter its size that is fully GPU-resident and multi-tenant friendly. The candidate holds ~70 GiB of RAM and makes the box effectively single-tenant while loaded.

Three limitations belong in the same sentence as any future re-read of this result: **Q2_K-only** (the only quant that fits), **PR-build-only** (unmerged, MSVC/CUDA 12.8), and **CPU-resident experts**.

*Retired from the decision criteria:* the "beat DeepSeek V4-Flash's 12.56 t/s" target. Different model, footprint, binary and depth — it is a sanity line, not a bar. Likewise the vendor's 85.2% top-1 figure is unreproducible here (the fp reference does not fit) and was never ours to accept.

**Model facts, for when the gate opens:** 125B total (+51B N-gram embedding table; HF counter shows 180B), **6B active per token**, 262,144 native context (1M via YaRN). 3 of every 4 layers are Gated DeltaNet, the 4th is Qwen Sparse Attention. Only two variants exist — `Qwen/Qwen3.8-Flash-Next` and `-FP8`; **no smaller variant.** vLLM is out of reach (FP8 needs 172.78 GiB). Sampling: thinking → temp 1.0 / top-p 0.95 / top-k 20; instruct → temp 0.7 / top-p 0.80 / top-k 20 / presence 1.5. Use `--load-mode none`, not `--no-mmap`. PLE/N-gram layers are 4-bit minimum.

### T1-adjacent. `Qwen3.8-27B-NVFP4-MTP` agent-seat A/B (downloaded, ready, not the focus)

**T1a. `Qwen3.8-27B-NVFP4-MTP-Q8attn` agent-seat A/B.** The cheapest large win on the text side and independent of everything else. An MTP drafter the uploader claims roughly **doubles** decode: 193 NVFP4 MLP tensors via NVIDIA ModelOpt, ~5.60 bpw, with attention projections deliberately held at Q8_0 to protect long-context accuracy.

⚠️ **Footprint correction (measured on disk 2026-08-26).** The handover called this "the same footprint" as the incumbent by comparing **17.81 GiB** against **17.92 GB** — different units. Actual bytes:

| seat | bytes | GiB | GB |
|---|---|---|---|
| candidate `Qwen3.8-27B-NVFP4-MTP-Q8attn.gguf` | 19,128,349,888 | 17.81 | 19.13 |
| incumbent `Qwen3.8-27B-UD-Q4_K_XL.gguf` | 17,923,394,624 | 16.69 | 17.92 |

The candidate is **~1.2 GB (6.7%) larger**, not equal. Still comfortably inside the 32.6 GB pooled tier alongside its 0.93 GB mmproj, so the A/B stands — but the win has to pay for real extra VRAM, and any "free speedup" framing is wrong.
- Add as a **side-by-side seat**, never in place of the incumbent — the A/B needs both.
- Flags: `--spec-type draft-mtp`, `--spec-draft-n-max <2|3>`, `-ngl 999`, `-fa` pinned explicitly.
- **Sweep draft depth 2 and 3.** The card states acceptance collapses at depth ≥4; treat depth as a swept parameter, not a default.
- Bench on **real harness agent contracts**, not a synthetic prompt.
- ⚠️ The ~2× is the uploader's claim, not our measurement. NVFP4 in *llama.cpp* is a different code path from ComfyUI's NVFP4 (which measured weak — see T3).
- Adopt only on a measured win, and **update the tier matrix FIRST** (house rule).


### T2. Security + toolchain — **run in parallel with everything below**

Promoted out of housekeeping by §11.1/§13.4: five Go stdlib CVEs and a sandbox that has been silently inert.
- **Go 1.26.5 → 1.26.7.** Fixes GO-2026-6218/6090/6089/5972/5026. **Skip 1.26.6 — it broke unencrypted HTTP/2.** Do **not** jump to 1.27.0: it switches to the `encoding/json` v2 engine, is *not* gated on go.mod, and this harness is JSON-heavy. That is a separate, tested migration.
- **`landlock-lsm/go-landlock` v0.9.0 → v0.10.0.** GHSA-vv6c-69r6-chg9 — best-effort mode **silently stopped restricting TCP bind/connect**. We have been running the vulnerable version.
- **`golang.org/x/sys` v0.46.0 → v0.47.0.** CVE-2026-39824, integer overflow in `windows.NewNTUnicodeString` — directly in scope on a Windows box.
- **`golang.org/x/text` v0.38.0 → v0.41.0** (CVE-2026-56852, infinite loop on invalid input). Plus `x/net` v0.58.0, `modernc.org/sqlite` v1.57.0, `jsonschema/v6` v6.0.3.
- **Hold `modelcontextprotocol/go-sdk` at v1.6.1.** We are already past its four advisories; v1.7.0 is a **breaking protocol change** and needs its own migration.
- **Node v24.18.0 → v24.20.0** (v24.18.1 fixed 11 CVEs, 3 HIGH).

### STACK UPDATE — EXECUTED 2026-08-27 (T2, T3, T5, T6 complete)

Operator direction: "the full system update — dependencies, libraries, EVERYTHING GETS
UPDATED RIGHT NOW." Executed same-night; every row below is live-verified, not assumed.

| component | before | after | verification |
|---|---|---|---|
| llama.cpp | b10435 nightly | **b10621** (= the nightly blessed by semver v0.3.0) | agent contracts ran on it (`seat_config_basis: b10621-c1d0e7a00`) |
| llama-swap | v249 | **v251** | serving; embeddinggemma child spawned from the b10621 path |
| whisper.cpp | 080bbbe8 (Jul) | **v1.9.3**, CUDA 13.3 build | JFK sample transcribed perfectly through the harness |
| torch | 2.11.0+cu128 | **2.13.0+cu130** (+tv 0.28.0, +ta 2.11.0) | kitchen CUDA gate open; 8/8 W4A4 shapes pass (cu128 failed 5/8) |
| ComfyUI | 0.32.0 | **v0.34.0** (frontend 1.49.6 as pinned) | E2E 1920×1088 image render through the harness |
| CUDA toolkit | 12.8 | **13.3.1** side-by-side (12.8 kept — see SageAttention) | nvcc V13.3.73; driver 616.56 untouched (13.x installer ships no driver) |
| Go / Node / Python / GIMP / ffmpeg | 1.26.5 / 24.18.0 / 3.14.6 / 3.2 / 7.1 | **1.26.7 / 24.20.0 / 3.14.7 / 3.2.4 / 9.0.1** | installers exit 0; ffmpeg out of the venv at `D:\Dev	oolsfmpeg-9.0.1`, config repointed |
| harness | 0.103.0 | **0.105.0** (PRs #187, #188) | deployed Qube + Lenovo, `fleet/health` verified |
| custom nodes | 4 stale | all at remote HEAD | Manager, VideoHelperSuite, Inpaint-CropAndStitch, RMBG |

**Version policy adopted (operator-prompted): take the project's RELEASED version, never
the newest build.** llama.cpp semver releases carry one asset — `nightly-tag.txt` naming
the blessed b-build (v0.3.0 → b10621). ComfyUI had a `v0.34.1` tag with **no release**
behind it. b10435 — an arbitrary nightly inside a 10–15× regression window — is the
cautionary tale.

**T3 payoff, measured.** The full native LTX-2.5 recipe — 1920×1088, 121 frames, joint
audio+video — now renders end-to-end through the harness: **"Prompt executed in 259.26 s"**
(326 s wall including ComfyUI cold start), producing a real 5.04 s A/V clip. Before cu130
this recipe was impossible at full resolution: the W4A4 DiT upcast-loaded at ~39 GB and
did not fit the 32.6 GB pool (the standing "pool at reduced resolution" workaround).
`scaled_mm_nvfp4` is also PRESENT in the kitchen CUDA backend (the research handover said
absent) — the W4A4-vs-NVFP4 question deserves a re-measure before any NVFP4 purchase.

**A second 0.34 break found and fixed (0.105.0, PR #188):** ComfyUI now hides every GPU
but the first on Windows (upstream #15737/#15813) — every pooled DisTorch2 graph failed
validation with `donor_device: 'cuda:1' not in ['cpu','cuda:0']`. `ensureComfy` restores
visibility via `cudaVisibleEnv()` (env-based, operator-overridable, multi-GPU spawns get
`--disable-pinned-memory` per upstream's own guidance).

**T6 applied:** deployed `imagegen_timeout_sec` 3600 → **600** — a wedged image render now
denies machine-wide media for at most 10 minutes, not 60.

**Deferred, with reasons:**
- ~~SageAttention cu130 rebuild~~ **SOLVED 2026-08-27 (operator: "try harder")**: the
  upstream `setup.py` uses GCC-style flags MSVC ignores — the source build was never
  going to work on Windows. The maintained Windows-wheel fork ships exactly our stack:
  `woct0rdho/SageAttention` `v2.2.0-windows.post6` →
  `sageattention-2.2.0+cu130torch2.10.0andhigher.post6-cp310-abi3-win_amd64.whl`
  (abi3 covers Python 3.14). Kernel verified on sm120 + ComfyUI boots "Using sage
  attention". **CUDA 12.8 then RETIRED the same night**: all five NVI2 packages
  uninstalled (0 registry entries remain), machine `CUDA_PATH` → v13.3, v12.8 PATH
  segments removed; a 12 KB empty dir husk remains (unregistered, harmless). nvcc
  13.3.73 is the only toolkit; torch cu130 + llama.cpp b10621 verified after.
- **Aorus** — offline throughout; parity debt now spans harness 0.102.0→0.105.0 AND the
  whole stack. First action when it answers.
- **Go 1.27.0** — deliberate hold (encoding/json v2 is its own migration).
- **go-sdk v1.7.0** — deliberate hold (breaking protocol change).

### T3. The cu130 chain — the hard gate for all media work

**Nothing in the media stack may be benchmarked or bake-off'd until this passes.** ComfyUI hard-disables `comfy_kitchen`'s CUDA backend whenever `torch.version.cuda < 13`, and that backend is the only provider of tensor-core kernels for NVFP4 / FP8 / ConvRot-W4A4 / INT8. With it off, our quantized checkpoints fall back to dequantize-to-bf16. Measured on this box: **W4A4 runs at 0.29× BF16 today and 1.85× with the backend on** — a 3.4–6.3× matmul penalty on the LTX-2.5 pipeline we already run.

**Do not work around the gate — satisfy it.** Force-enabling on cu128 throws `cuBLASLt 13.x library not found` on most layer shapes; the gate is correct.

1. **Copy the venv.** `C:\ComfyUI\.venv` → `.venv-cu130`. Side-by-side, never in place.
2. **torch 2.11.0+cu128 → 2.13.0+cu130** (+ torchvision 0.28.0+cu130). ⚠️ **torchaudio tops out at 2.11.0 on the cu130 index** — check what depends on it first. cu128 was removed in torch 2.13.
3. **Rebuild every compiled sidecar against cu130:** SageAttention (2.2.0 today), triton-windows, any custom-node CUDA extension.
4. ⚠️ **Wheel trap:** `comfy-kitchen` publishes both a compiled `cp312-abi3-win_amd64` wheel and a pure-Python `py3-none-any` fallback. If pip resolves the pure-Python one there are **no CUDA kernels at all**, regardless of torch. Verify from the startup log.
5. **HARD GATE — do not proceed until all three hold:**
   - the `You need pytorch with cu130 or higher` banner is **GONE**
   - the log contains `Found comfy_kitchen backend cuda: ...`
   - the microbenchmark shows **W4A4 ≥ 1.8× BF16 at 4096³**, and the previously-failing shapes (1024/2048/3072/5120/6144) now succeed
6. **ComfyUI 0.32.0 → 0.34.0**, frontend 1.49.6, comfy-kitchen 0.2.31, comfy-aimdo 0.4.15, and the four stale custom nodes (Manager, VideoHelperSuite, Inpaint-CropAndStitch, RMBG).
7. **Re-measure the real pipeline.** `render/wf-ltx25-i2v.mjs` at 1920×1088@24fps ×121 is the regression test. Record seconds-per-render and peak VRAM before/after. Expect LTX-2.5 22B to go from ~39 GB loaded (does not fit the 32.6 GB pool — hence today's reduced-resolution workaround) to ~20 GB.

**Format verdict — we already own the better one.** With the backend enabled, W4A4 measured **~1.75× faster than NVFP4** on this hardware, because W4A4 has a real kernel in `comfy_kitchen` and NVFP4 does not (`scaled_mm_nvfp4` is absent from the cuda backend's capability list). **Do not buy an NVFP4 LTX-2.5 checkpoint as a first move.** NVIDIA's "20% faster" claim is against bf16, not against W4A4. Revisit only if comfy-kitchen ships `scaled_mm_nvfp4` in its CUDA backend.

⚠️ ComfyUI issue **#11864** (open) reports native NVFP4 loading silently falling back to fp16/fp8 upcast. **Verify 4-bit residency from the load log; never assume it.**

### T4. The two new media models — strictly after T3's gate

- **MiniMax-H3.** Core weights have been on disk and unwired since download; ComfyUI core already ships `MiniMaxH3ImageToVideo` / `MiniMaxH3ReferenceToVideo` / `MiniMaxH3SigmaShift`. ⚠️ **The turbo LoRAs and style embeddings were MISSING** — they are the speed lever, and benchmarking without them measures the slow path against an LTX-2.5 recipe that is already distilled. Downloading now. Use the official `video_minimax_h3_{i2v,t2v,r2v}.json` templates — **do not hand-build the graph**. (Our installed `comfyui_workflow_templates` 0.11.40 ships zero JSON templates, so they arrive with the 0.34.0 upgrade or come from the repo.)
- **WAN-Animate-2.** Genuinely new capability — we have image-to-video but no character animation / video-driven retargeting. `wan_animate_2_distill_int8_convrot` (16.65 GB) is the only variant that fits a single 16 GB card, and only if it loads natively rather than upcasting. `WanAnimate2Cache` caches the pose branch and roughly halves generation time. Aux already on disk: `umt5_xxl` (both), `wan2.2_vae`, `wan_2.1_vae` — but **`clip_vision_h` is absent** and must come with it.
- ⚠️ **Do not chase WAN NVFP4** — the only published community NVFP4 (`lightx2v/Wan-NVFP4`) ships wan**2.1** files, i.e. the wrong model version.
- Bar for a roster slot, both models: **beat LTX-2.5 on something we care about, not merely work.**

### T4 — EXECUTED 2026-08-27. Both models wired from the official templates and validated live

**Corrections to the plan above, from disk:** the templates live in the **`comfyui_workflow_templates_json`** package (the old `comfyui_workflow_templates` package is an empty shell — that's why 0.11.40 "shipped zero JSONs"); `clip_vision_h.safetensors` is present (the "absent" warning is stale); all i2v/t2v aux files carry the exact template filenames. ⚠️ **`api_minimax_h3_*.json` are PAID Comfy-Cloud API-node workflows** (`MinimaxHailuo03*` nodes, no local models) — the local templates are `video_minimax_h3_*.json`. Never confuse the two.

**Wiring method (no harness code touched):** the official UI-format template (subgraph included) was converted 1:1 to an API-format graph — node ids preserved, only the two V3 dynamic nodes constant-folded (turbo switch, frame-count expression) — and run through `offload_run_graph`, which owns the ComfyUI lifecycle + GPU lock. Emitters + graphs archived in the session scratchpad (`t4/emit_h3_graph.py`, `t4/emit_wan_animate2.py`). Worked **first try** for H3.

**MiniMax-H3 bake-off vs the seated LTX-2.5** (same source still, same motion prompt + audio cue, same seed 757358688076805, ~5 s, ~0.9 MP, each on its own recipe — LTX deployed recipe vs H3 official turbo-8):

| axis | LTX-2.5 22B distilled (seated) | MiniMax-H3 fl2va int8-convrot turbo-8 |
|---|---|---|
| engine wall ("Prompt executed") | **233.7 s** @1280×704×121f | 457.8 s @1280×736×124f (DisTorch-pooled) |
| prompt adherence | never reached the scripted leap; wardrobe morph; source dusk grade lost | **full arc executed: turn → sprint → crouch → leap → lands on the next roof** (independent vision-seat confirmation, no artifacts noted) |
| i2v source fidelity | drifted dark, geometry ambiguous | **lighting/fog/wardrobe of the still preserved throughout** |
| audio (joint A/V both) | low-band wind/rumble wash, rolls off ~7 kHz | **structured sound design: footstep/impact transients, score swell into the leap, landing hit** (spectrogram-verified) |
| t2v / direction | not wired in our lane | **4-shot storyboard with hard cuts followed at 0.4 MP/207 s** — a capability the roster lacks entirely |

**Verdict: H3 clears the bar** — beats the incumbent on prompt adherence / multi-shot direction, i2v fidelity, and audio design; loses ~2× on speed. The matrix row-22 "3-way verdict (beat MiniMax-H3)" measured the **pre-turbo slow path** the plan warned about. Memory facts: H3 DiT 20.97 GB disk → **37.46 GB pooled** (DisTorch upcasts int8 to bf16, per the loaded-size rule) or runs single-card partial-offload (9.8 GB GPU + 10.2 GB CPU, 141 lowvram patches, native `convrot_w4a4` ops confirmed); text encoder `qwen3vl_32b nvfp4` 15.7 GB loads partially. Gap: the r2v lane needs the un-downloaded `minimax_h3_ref2va_pruned_int8_convrot` DiT (~21 GB) — optional.

**WAN-Animate-2 distilled: NEW capability validated.** Single-chunk Motion Transfer from the official `video_wan_animate2_distilled.json`: identity-preserving retargeting of a human dance driver onto a vinyl-toy character (radically different proportions) at 81f@480×854, 10-step lcm, **298.5 s**, loading **native int8 on one 16 GB card (15.9 GB total, no upcast)** — the plan's single-card fear cleared. ⚠️ **The template's `WanAnimate2Cache` `gpu`/`int8` setting hard-kills ComfyUI** mid-step-1 on this box (silent process death → harness 240 s watchdog abort); `cpu`/`default` works at both 49f and 81f. Aux used: `umt5_xxl_fp8_e4m3fn_scaled` + `clip_vision_h` + `wan_2.1_vae` (all on disk; template's `Wan2_1_VAE_bf16` name maps to our `wan_2.1_vae`).

**Follow-ups (not sprawled here):** (1) harness integration decision — H3 as a `videogen_family` option (code) and/or a WanAnimate media route; seating stays an operator call, matrix candidate rows added 2026-08-27; (2) optional r2v DiT download; (3) file the WanAnimate2Cache gpu-crash upstream once reproduced against a clean ComfyUI.

### T5. llama.cpp + llama-swap — one fleet-wide pass

- **llama.cpp b10435 → b10639.** Urgent for a second, unrelated reason: **b10435 sits inside a confirmed 10–15× generation-throughput regression window** (#27084/#27126); the fix `22b8e310b` is 12 commits past our build. Blast radius is CPU-side generation, so the **Lenovo 6GB tier, the Aorus node, and every `--n-cpu-moe` path** are directly exposed. Land on b10639, not an intermediate build — three merge/revert pairs sit inside the window. CLI surface is identical (two added flags, zero removals), so launch lines carry over verbatim. Rollback: b10435 and b10356 are still on disk.
  - Watch **#26347** (`/v1/models` now needs the API key when `--api-key` is set) and **#27626** (server rejects prefilled assistant messages carrying tool calls — an agent loop replaying a partial turn will error).
  - `-sm tensor` is new but still **EXPERIMENTAL** — do not adopt for the dual-GPU seats.
- **llama-swap v249 → v251.** Config schema identical. Fixes a v249 log-flood regression.

### T6. Config fix — the media lease TTL (§9)

`imagegen_timeout_sec` is passed **both** as the render's process-tree-kill timeout **and** as the media lease TTL (`internal/pipeline/pipeline.go` → `acquireMediaLease("image-gen", timeout, …)`). Deployed on Qube it is **3600** — a 60-minute machine-wide media lease for a job that finishes in one to two minutes. With 14 concurrent `local-offload` processes contending on one machine-wide lease, **one wedged render denies media to every other session for up to an hour.**

Set it to **600**. The code default is already 720 (`config.go:1102`); only the deployed `~/.local-offload/config.json` carries 3600. Config change, not code.

### Corrections to the handover, from live verification (2026-08-26)

Four of its cautions were already satisfied, and one number was wrong:

| Handover said | Live on Qube | Consequence |
|---|---|---|
| NVIDIA driver 610.88 → upgrade to 616.56 | **already 616.56** | §4 step 1 is **done**; drop it |
| Qube MCP process stale at 0.101.0 | delegation result reports **`harness_version: 0.103.0`** | §11.0 **resolved**; no restart needed |
| Pin `-fa` explicitly per seat (#27137, 2.3× risk) | **already pinned** — 11 seats carry `--flash-attn on`, **zero** `-fa auto` | no action |
| ffmpeg 9 removed `-vsync`/`-filter_complex_script`/etc. — grep first | **zero hits** across `render/` + `internal/` | ffmpeg bump is unblocked |
| llama-swap v251 error envelope will break string parsing | `gpugen.ClassifyErr` matches **substrings** (`out of memory`, `cudamalloc`) that survive JSON encapsulation; `"llama-server 5"` is our own `Error()` format | low risk — confirm, don't fear |

Still true and still blocking: **Aorus unreachable** (ping 100% loss, `fleet/health` deadline exceeded), so it is off parity at 0.102.0 while the fleet is at 0.103.0. House rule treats parity as first-class — deploy the moment it answers.

---

## Historical build order (2026-06 → 2026-07) — CLOSED, kept for the verdicts

### 1. Phase A.2 — STT / `offload_transcribe`  ✅ DONE (2026-06-16)
Completes the *understanding* angle (the lowest-risk, highest-reuse tier). Build whisper.cpp `whisper-server` (CUDA + `WHISPER_BUILD_SERVER`), download `ggml-large-v3-turbo.bin`, and **register it as a llama-swap `ttl:300` upstream** in the **LOAD-BEARING** `~/llama-swap/config.yaml` (do it ONLY via the backup → validate → restart → verify-memory-stack → **rollback-on-any-failure** ritual). Go `offload_transcribe` verb returns **`{gist, segments[]}`** (timestamped spans = the fastcontext citation-pattern, so Claude pulls only the spans it needs) + an SRT writer + MCP tool + CLI subcommand. Design: CHAPTER-audio brief, ANGLE 1 (whisper-large-v3-turbo; WhisperX/Parakeet as later quality/fast tiers).

### 2. Phase S — `ik_llama.cpp` infra spike  (standalone, non-mutating)  ✅ DONE (2026-06-16) — VERDICT: REJECT ik
**Benchmarked, did NOT adopt** (write-up: `docs/PHASE-S-ik_llama-benchmark-2026-06-16.md`). ik builds clean + is **faster on PP** (26B +27% f16 / **+140% q8_0 KV**; refutes #1765's TG regression on our build), BUT **fails the sacred GBNF gate on the 26B CPU-MoE path** (HTTP 500 invalid-UTF-8 on enum grammars + repetition loops; mainline produces valid JSON). Grammar is non-negotiable → reject. E4B (fully-GPU) grammar is clean but the speed win is only ~10% (sub-threshold). **Free actionable win surfaced:** mainline `--n-cpu-moe 24` lifts the live 26B TG ~+11% (15.4→17.2), grammar-safe — proposed for Daniel (live-config edit, verify VRAM+grammar first). Watch-list: re-eval ik when the Gemma-4 CPU-MoE grammar bug is fixed upstream.
Build `ik_llama.cpp` (ikawrakow fork), run it **on a scratch port — NOT the live config**, and benchmark the 26B-A4B `--cpu-moe` path + IQ_K quants vs mainline llama.cpp on this i7-11800H. Also test KV-cache quant on the Gemma cascade + tuned per-layer `-ot` offload. **Propose** a binary swap; do **not** auto-adopt (swapping the binary affects every model — that decision waits for Daniel). Adopt only what *provably* helps. Source: ADDENDUM.

### 3. Phase 2 — Generation
Video gen (Wan 2.2 14B / Hunyuan 1.5 — already on disk — via `comfy-render`-style runners) then audio gen (ACE-Step 1.5 music + Chatterbox V3 voice). **Standalone** (driven through ComfyUI `:8188`; does NOT touch llama-swap). Add the **GPU single-slot file-lock scheduler** here. Design: CHAPTER-video (gen) + CHAPTER-audio (gen).

#### Image-gen tooling — generation gaps + the right tool for each (Daniel, 2026-06-26)
**SDXL (current) is genuinely good at exactly one thing:** atmospheric / organic / molecular amber-on-ink heroes. Everything else it fakes badly. The gaps, and the right tool for each:

| Gap (SDXL is weak) | Best tool | Notes |
|---|---|---|
| Precise diagrams / charts / data-viz (chromatograms, gauges, comparison bars, mass-spec) | **Designed SVG/HTML in the builder** | Crisp, on-brand, 100% legible. No model — it's the right tool (e.g. the chromatogram). |
| Icons / pictograms (flask, checkmark, magnifier, shield) | **Open SVG icon set** (Lucide / Phosphor) | Drop into builders; free, consistent, recolor to amber. |
| Coherent recognizable objects/scenes (clean pen, lab glassware, a certificate) + legible baked-in text | **FLUX.1 [dev]** (local, quantized GGUF/fp8 in ComfyUI) | The single biggest model upgrade — far better prompt-adherence, object coherence, and in-image text than SDXL. Runs on the 3070 but slower (~1–2 min/img at low VRAM). |
| Soft / low-res renders | **Real-ESRGAN / SUPIR** upscale (ComfyUI node) | Optional crispness pass. |
| Fixing/compositing a render (artifact removal, inpaint) | **FLUX Fill / SDXL inpaint** | Polish, not generation. |

**Prioritized (Daniel's recommendation):**
1. ✅ **DONE — SVG component kit** (gauge, comparison-bar, chromatogram, icon set) shipped as `offload_generate_svg` + `generate-svg` (parametric `internal/svgkit`, brand-agnostic, pure-Go/free). Covers topic-legibility needs crisply, free, no new model. **Highest ROI.** See Done above.
2. **Add FLUX.1 [dev] (quantized) to ComfyUI** — for a coherent recognizable subject (pen, glassware, organ) or in-image text. **Verify the exact quantized setup works on the 8 GB 3070 before installing** (per "don't author configs unverified").

   **🔬 SPIKE RESULT (2026-06-26): FLUX.1-dev Q4_K_S works on the 8 GB 3070 but is IMPRACTICALLY SLOW (~13+ min/image).** Downloaded the GGUF stack (flux1-dev-Q4_K_S 6.8 GB + t5xxl-fp8 4.6 GB + clip_l + FLUX VAE) into `C:\ComfyUI\models`, ran it through the harness's **universal** `render/comfy-render.mjs --graph` (no brand/style baked in — the right mechanism). ComfyUI loaded it (LOW_VRAM, GGUF Q4_K, FLUX model type) and sampled at 100 % GPU, but the model **offloads ~1.2 GB** (FLUX-dev Q4 ~6.8 GB + encoders + VAE + compute > 8 GB) → every step thrashes VRAM↔RAM at ~40 s/step → ~13+ min for 20 steps. The "40–60 s/image" estimate assumed the model fits without offloading; **false on 8 GB**. **FLUX.1-schnell was then tested too (4-step) and is ALSO impractical:** its log showed **`1/4 [218 s/it]`** — schnell offloads the same ~1.1 GB, so each step thrashes just as hard; 4 steps × ~3.6 min ≈ 14 min, no better than dev. **Definitive verdict: FLUX (any variant) at Q4 is a NO-GO on the 8 GB 3070** — the offloading thrash dominates regardless of step count. To run FLUX you'd need it to FIT in VRAM (a Q2/Q3 quant — but below Q4 FLUX loses the object/text coherence that's its whole point) or more VRAM. **All FLUX files deleted (~12 GB reclaimed). Stay on SDXL** for image gen; revisit FLUX only on a bigger-VRAM GPU. The universal `render/comfy-render.mjs --graph` path IS proven (brand-agnostic) and works for any future model that fits.
3. **Real-ESRGAN upscale** only if renders look soft at publish size.

**Net:** keep SDXL for atmosphere, lean on the designed SVG kit for legibility (now delivered — see Done), add FLUX when we want real objects/text. **FLUX stays a NO-GO on the 8 GB 3070; the SVG kit covers legibility.** *(SVG kit: done. FLUX: revisit only on bigger-VRAM GPU.)*

### 4. Phase 3 — Editing  (needs the Resolve spend → last of the build)
Claude-driven cut-lists (WhisperX-JSON → OTIO/EDL + `auto-editor`) + cleanup (DeepFilterNet3 → MossFormer2; ffmpeg two-pass loudnorm → -14 LUFS) + **DaVinci Resolve Studio** (the $295 one-time spend, **approved but only purchased at this phase, with Daniel**). Design: CHAPTER-audio (edit) + CHAPTER-video (DaVinci).

### 5. Danmar Auto Reviews capstone  (last, deepest)
The private optimized track: no-avatar (chest-cam + b-roll), 6-month backlog, short+long form, two machines (3070 + editor's 5060). Deep channel analysis via the **Youtube-Analyst** skill. Built once the generalist capabilities exist.

### Parallel / needs Daniel (not on the critical path)
- **Docker leftovers** — RESOLVED on the reference box (verified 2026-08-27): Docker Desktop is not installed, no docker processes, no docker WSL distros — nothing to keep or kill there. Remaining: one check on the Aorus at its next parity pass.
- **Resolve purchase** — at Phase 3, still unpurchased.
- **DiffusionGemma** — WATCH only; re-eval when PR #24423 merges with `llama-server` AND grammar-under-diffusion lands in llama.cpp. *Merge status not re-checked in the 2026-08-26 refresh.*

---

## Parked (behind the frontier update)

- **Phase 3 — video editing / DaVinci Resolve.** ⚠️ Correction 2026-08-27 (operator): the Resolve Studio spend line was STALE — **Resolve Studio is purchased and ACTIVATED on the OptiPlex editor rig** (Sofia's account), the DaVinci bridge shipped (danmar-video-pipeline PR #3, `resolve.cmd build` live on the rig, identity-verified `f759aaa`), and the `davinci-resolve` skill drives real edits. What Phase 3 still holds is only the **automation half**: transcript-driven cut-lists (WhisperX-JSON → OTIO/EDL + `auto-editor` silence cuts), audio cleanup (DeepFilterNet3 → MossFormer2, two-pass loudnorm to −14 LUFS), and a harness editing verb. The image half of "edit" shipped long ago (op pack 0.21.0, generative edit 0.44.0, inpaint 0.20.0, upscale 0.77.0). Feeds the Danmar Auto Reviews capstone, whose review-side capability (44-min 4K `video_watch` + transcribe, catches details like leftover AI-stock watermarks) was proven 2026-08-27.
- **Danmar Auto Reviews capstone.** The private optimized track (no-avatar chest-cam + b-roll, 6-month backlog, short + long form, two machines). Built once the generalist capabilities exist — unchanged.
- **The empty `stt_hq` slot.** Empty **by decision**, not oversight: `qwen3-asr` was removed 2026-08-14 after tying whisper-turbo on every clean house sample, and `stt_model_hq` was deliberately cleared (empty falls back to `stt_model` by design). **Re-open only with field/noisy audio as the test set.** Candidates then: `nvidia/parakeet-tdt-0.6b-v3` (whisper.cpp has Parakeet code present but **not built** in our tree), `ggml-large-v3` full, `nvidia/canary-1b-v2`.
- **Roster challengers, none adopted, all need a bake-off.** Gemma-4 QAT chat-template re-pull (the unsloth repos were re-committed 2026-07-17 with a fix under the **same filenames** — verify by hub revision, not filename); MTP drafters exist for every Gemma-4 size but we wire one only on `gemma-4-12b`; `PaddlePaddle/PaddleOCR-VL-1.6-GGUF` vs `qwen3-vl-8b`; `nvidia/Nemotron-3-Embed-8B-BF16` as an embedding challenger.
- **sd.cpp on Qube.** Not installed (`sdcpp_bin` empty) though the harness has first-class config keys and `render/sdcpp-generate.mjs`; it **is** live on the Lenovo. Interesting only because sd.cpp has day-1 MiniMax-H3 support — but issue **#1871 "Poor video and audio quality with Minimax H3"** is **OPEN**, so treat it as a fallback, not the primary. Wire H3 in ComfyUI first (T4).

## Source briefs (design detail)
Kept in the operator's ecosystem notes, outside this repo (the previously-listed absolute path went stale when the drive letter changed):
- `CHAPTER-video-2026-06-15.md` — video understand · gen · avatar · DaVinci
- `CHAPTER-audio-2026-06-15.md` — audio listen · generate · edit (3 angles)
- `ADDENDUM-frontier-sources-2026-06-16.md` — ik_llama.cpp / DiffusionGemma / fastcontext verdicts
- `ROADMAP-frontier-2026-06-14.md` — the earlier roadmap
Repo plans: `docs/superpowers/plans/`. Continuation prompt: `docs/superpowers/plans/CONTINUATION-2026-06-16.md`.
