# `maxwell-3` — the 3 GB Maxwell tier, designed from the box

**Status:** in measurement — hardware truth and the engine proof are measured; seat bakes in progress; profile values below are PROJECTED until the matching row says MEASURED
**Date:** 2026-10-28
**Decides:** what a 3 GB NVIDIA card with host RAM to spare serves, on the first real hardware of its class to run the harness: `<node-g>`, a 2015 gaming laptop (GTX 970M 3 GB, 32 GB DDR4).

---

## 1. What I understood, what I assumed

The operator asked for a new tier for a specific machine: "build a new offload harness 2.5-3 GB tier that applies to that PC", with "the best model options (best quality that reasonably fits)", "every tier seat: embeddings, agent, media, etc.", configured and optimized from "frontier online research and testing as needed", and the reminder that the box "also has 32 GB dual channel RAM". The doctrine that governs every tier applies: all tiers are first-class, and a smaller model appears on a tier only because it is the best that fits.

Assumed (mine):

- The tier is a hardware CLASS, not this one laptop: NVIDIA, dedicated VRAM under 4 GB, any arch the pinned CUDA 12.4 build runs (sm_50 and up). The reference box is Maxwell, hence the name; a Pascal 3 GB card classifies here too and serves.
- The box is a standalone workstation for one user's Claude Code, not a fleet node: no `fleet-serve`, no fleet token, no cache server. Delegation OUT to a fleet is a later, separate decision.
- The engine stays the harness's pinned llama.cpp release family (`win-cuda-12.4`): no self-build, no CUDA 13 asset (it carries no Maxwell code).
- "32 GB dual channel" is the operator's belief; the box measures single-channel today (both DIMMs in channel A). The design is sized for single-channel and records what dual-channel changes.
- Host-RAM spill is admitted only in the two sanctioned forms: MoE experts through `--n-cpu-moe` (ADR 0080's gates), and sd.cpp weight staging (`--offload-to-cpu`). Nothing else runs on the CPU.

## 2. Intent and why

A 3 GB card today classifies as `ampere-6` and renders seats that cannot load (E4B needs 3,100 MiB at 16K; the 4B agent seat 3,681 MiB). The owner of such a box gets a roster that OOMs. The tier exists so the installer classifies the card honestly and renders only seats that fit, each the best measured for the class.

## 3. Goals, non-goals, anti-goals

Goals: (1) `detect.ps1` and `install detect` classify a <4 GB NVIDIA card as `maxwell-3`; (2) the rendered llama-swap config holds only seats that load on 3,072 MiB with the embedder resident; (3) every seat is the best quality that fits, chosen on measurements from the reference box (workhorse, agent, vision, STT, embeddings, reranker, image); (4) the install lands on the data drive, starts at boot, and a real offload call of every bound lane succeeds once, read back; (5) the installer's stale E2B/E4B pins are refreshed (found while staging: upstream size+sha moved).

Non-goals: fleet membership of the box; a RAM-spill seat as the DEFAULT agent; video generation as a bound seat (measured as experimental only); a global llama.cpp pin bump unless a seat that earns its place needs it (see Ruling R-ENGINE).

Anti-goals: a roster entry that OOMs when called; a seat that quietly runs dense layers on the CPU; a tier page that reads "second-class" (the doctrine forbids it); real machine or person names in the repo.

## 4. Constraints (measured on the reference box unless marked)

| Property | Value | How |
|---|---|---|
| GPU | GTX 970M, 3,072 MiB (llama.cpp sees 3,071), compute 5.2, driver 582.78 (R580, the last Maxwell branch), Optimus: the iGPU drives the panel, the card idles at 0 MiB and D3 | `nvidia-smi`, PnP power state |
| Engine | pinned `b11490` win-cuda-12.4: `ggml-cuda.dll` embeds PTX for sm_50 (144 modules), so sm_52 runs through the driver JIT; `b11946` keeps the same asset (146 modules, sm_50 present) | `strings` on both DLLs |
| JIT | first `llama-bench` 51 s vs 15 s warm (~36 s once), 50 MB cache; `CUDA_CACHE_PATH` on the data drive, `CUDA_CACHE_MAXSIZE=4294967296` | smoke 1 |
| Maxwell kernel paths (source-read, b11490) | no fp16 math, dp4a emulated, MMQ off below cc 6.1 (prompt processing = dequant-to-F32 + cuBLAS SGEMM), decode via MMVQ, flash attention via the f32 vec/tile kernels, CUDA graphs disabled below Volta | ggml-cuda sources |
| Measured E2B (UD-Q4_K_XL, -ngl 99) | pp512 374 t/s, tg128 39.2 (FA off) / 43.1 (FA on); server 32K q8_0 FA = 1,535 MiB, 35 t/s; 16K f16 FA = 1,517 MiB, 40.7 t/s; f16 KV costs +49 MiB over q8_0 at 16K and is 16% faster | smoke 1 |
| App-clock pin (`-ac 2505,1038`) | no effect (372.9/39.1 vs 374.1/39.2) — not used | smoke 1 |
| CPU | i7-6700HQ 4C/8T, AVX2+FMA+F16C, no AVX-512 | CIM |
| RAM | 2x16 GB DDR4-2400, BOTH in channel A (InterleavePosition 0): single-channel, ~11 GB/s copy, no scaling across threads | CIM + C# copy test |
| Disk | SATA SSD (~500 MB/s class); data drive with >500 GB free | CIM |
| Residency | embedder `embeddinggemma-300m` Q8_0 resident in every set; everything else ttl 300 | house rule |

Projected seat budget: 3,072 − WDDM/context reserve (measure) − embedder (measure) ≈ 2.4-2.5 GiB for weights + KV + compute of the swappable seat.

## 5. How it gets made

Go harness + PowerShell installer, as the repo already does: a `profiles.json` entry, a classification band in `internal/hwdetect` and its PowerShell twin, one new profile flag to omit the E4B entry, pins in `install.ps1`, generated tier docs, this spec, a `changes/` fragment. Ships as one PR on the public repo through `clean-ship`; measurements go to the benchmarks folder and the profile `notes`.

### Classification

`maxwell-3` is a VRAM band in the NVIDIA default branch (`0 < VRAMGb < 4`), placed before the `ampere-6` fall-through, in `classify.go` and `detect.ps1 Get-Profile`. The arch regex gains a `maxwell` label (GeForce GTX 9xx/9xxM, 750/745, 8xxM, Quadro M-series) so the unrecognised-GPU warning stops firing; the label never decides the band. `fleetnode.VendorArchFromProfile` learns the `maxwell-` prefix.

### Serving (projected; MEASURED rows replace these as the bakes land)

| key | value | status |
|---|---|---|
| backend | `cuda` | by construction |
| ctx_size / agent_ctx_tokens | 32768 | E2B at 32K q8_0 = 1,535 MiB MEASURED; f16 at 32K to be measured |
| kv_type | `f16` | f16 FA on is 16% faster than q8_0 at 16K for +49 MiB (MEASURED on E2B) |
| flash_attn | `on` | faster on Maxwell (MEASURED) |
| resident_tier | `gemma4-e2b` | E4B cannot load (3,100 MiB at 16K on the 6 GB reference box) |
| omit_e4b (new flag) | true | the E4B entry and its download are dropped on this tier |
| include_26b / moe_26b | false / drop | no path |
| include_embeddinggemma2 | false | the box is not the memory authority; the projector would starve the chat seat |
| disable_cuda_graphs | true | graphs are disabled below Volta anyway; keep the ampere-* default |
| agent_model | bake decides (see R-AGENT) | PROJECTED |
| media_seats | vision E2B + `mmproj-gemma-4-E2B-it-Q8_0.gguf`; stt whisper large-v3-turbo (q5_0 or q8_0, bake decides) + silero VAD | PROJECTED |
| config_seed | memory_stack [embeddinggemma, bge-reranker-v2-m3], escalation/reasoning "", agent_profile research, fleet_max_concurrent_jobs 1, imagegen sdcpp if smoke 4 passes | PROJECTED |

### Install on the reference box

`OFFLOAD_HOME` and the data home on the data drive; llama-swap on `127.0.0.1:11436`; a boot-triggered scheduled task with the hidden launcher starts llama-swap (the fleet-node launcher pattern, minus `fleet-serve`); the CUDA JIT cache env set for the task and for the user; whisper-server copied from the house cuda-12 build (the Windows installer ships no whisper component; every Windows tier provisions it by hand and the tier page says so).

## 6. Left to the builder

The measured numbers of every bake (the ledger in `Benchmarks and Optimizations/2026-10-28-maxwell3-tier/`), the final `notes` string, the exact `install.ps1` pin blocks for any new seat file (size + LFS sha from the HF tree API), and the render-test block mirroring the ampere-6 one.

## 7. Rulings

- Ruling: band on VRAM alone (`0 < VRAM < 4`) in the NVIDIA default branch, arch as a label - an arch-gated band would send an unrecognised 3 GB Pascal card to ampere-6, whose seats cannot load - a GTX 1060 3 GB lands in a Maxwell-named tier; it still serves.
- Ruling: tier id `maxwell-3`, reference box `<node-g>` - follows the arch-VRAM naming and the node-letter privacy rule - a renamed tier later is a profile id change plus docs.
- Ruling: workhorse is Gemma 4 E2B UD-Q4_K_XL unless the bake shows a fitting candidate beating it on both cascade shapes by more than the house bar - it is the only 3 GB-class number already measured and it is the family the cascade's grammar story was built on - a better 2B exists and we ship the second-best for one release.
- Ruling (R-E4B): a new inverted-polarity profile flag `omit_e4b` drops the `offload-e4b` entry, its matrix var, its set membership and its download - a roster entry the card cannot serve is the defect the closure gates exist for, and a 4.2 GB download for a dormant entry is waste - every template edit re-hashes the goldens and marks every rendered node STALE until re-rendered (the documented cost of any template change).
- Ruling (R-ENGINE): the tier ships on the pinned `b11490`; a seat that needs a newer build (LFM3 family: b11938+) is bound only if it wins its bake, and then the global pin moves to `b11946` in the same PR (every asset family re-pinned and hashed) - the pin policy is "a snapshot of frontier, refreshed on a cadence", and a per-tier second build is a bigger mechanism than the gain - a pin bump touches every fresh install; CI's render and audit gates are the regression gate.
- Ruling (R-AGENT): the dense agent seat is chosen by the house bake-off instrument (the two task shapes, fabrication as the disqualifier) among candidates that FIT with the embedder resident: E2B, MiniCPM5-2B Q4_K_M, LFM3-1.7B (b11946), Qwen3.5-4B UD-IQ3_XXS; the RAM-spill Qwen3.6-35B-A3B UD-IQ3_XXS is measured against ADR 0080's three gates and, if it passes, binds as an opt-in `agent-deep` alias, never the default - the operator pointed at the 32 GB on purpose, and the ADR requires a per-tier bake - a weak agent seat, or a spill seat that is too slow at ~11 GB/s, is recorded with its numbers and left out.
- Ruling: vision is E2B + the ggml-org Q8_0 projector (531 MiB) if it loads beside the embedder with 1K image tokens; LFM3-VL-2B is the challenger on b11946 - the E2B file is already the workhorse, so the seat adds one 531 MiB download - a vision seat that only fits at 4K ctx with 512 image tokens is bound at that geometry and documented.
- Ruling: KV f16 with flash attention on, ctx 32K - measured 16% faster than q8_0 for +49 MiB on the one model measured; q8_0 stays available per seat - a seat whose KV is large (non-SWA) takes q8_0 in its own entry.
- Ruling: `include_embeddinggemma2: false`; memory stack = embeddinggemma + bge-reranker-v2-m3 - the 1.2-1.5 GB projector cannot sit in the resident set on 3 GB - the box embeds text only; images embed on the memory authority.
- Ruling: STT = whisper-server from the house cuda-12 build (sm_50 PTX verified) with silero VAD; q5_0 vs q8_0 decided by the bake - the Windows installer has no whisper component, so the tier page documents hand provisioning like every Windows tier - a later installer component is a separate change.
- Ruling: image generation binds the house sd.cpp Vulkan pin with sdxl-turbo Q4_0 if smoke 4 renders a correct image on the card, with `--vae-tiling`; a bigger model with `--offload-to-cpu` only if measured on this card - quality-first, but the engine's Maxwell support is the gating risk, and the CUDA sd.cpp zip carries no Maxwell code - no image seat (NOT CONFIGURED) if the Vulkan build fails.
- Ruling: refresh the `model-e2b` and `model-e4b` pins (size + LFS sha) in this PR - a fresh install of any tier that downloads them fails today - none; the HF tree API is the pin source the file already names.
- Ruling: install home and data home on the data drive, llama-swap at boot through a hidden-launcher scheduled task, no fleet-serve - the C-92 rule and the house's own node pattern - a logon-only task would leave the box without a server after a reboot with no user session.
- Ruling: no new tests; rows are added only to the tables that already enumerate tiers, so the existing gates stay green - the repo's testing rule - a regression the tables do not cover is caught by the one real run the PR records.
- Ruling: privacy - `<node-g>` is the new letter, defined in the glossary as "the 2015 gaming laptop (GTX 970M 3 GB)" - V1/V4 - none.

Builder-check answers (one pass on a local seat; every question was answerable from the repo, settled here as minor, my call):

- Ruling: `omit_e4b` is a boolean in `profiles.json` read into `servingProfile`/`servingtmpl.Params` like `include_qwen35_4b`, inverted so the zero value renders as today; the renderer drops the `offload-e4b` entry and its `e4b` matrix var with `dropModel`, and the interactive set's leading `e4b | ` becomes a token (`__E4B_LEAD__`) that renders empty when omitted; `install.ps1` skips `model-e4b` on such a profile; `ParamsBasis` carries the flag so the provenance stamp changes - the smallest mechanism that mirrors an existing one - a dangling `e4b` in the set expression would refuse to render, which the render tests catch.
- Ruling: the agent bake-off instrument is the 2026-08-17 ampere-6 one (shape B: three documents to a structured 18-fact extraction; shape C: a five-file fixture with distractors, search+select, cross-file arithmetic, a fabrication trap), scored with the house gold sets and a typed judge; the bar is ADR 0080's 0.5 on the blind-pack scale, and fabrication disqualifies - one instrument across tiers keeps verdicts comparable - none.
- Ruling: ADR 0080's three gates, verbatim: adds capability over the incumbent by more than the bar; host RSS within the weights plus 1 GiB, released on unload, with a measured MemAvailable floor; no abort and no swap-in - the ADR is the authority - none.
- Ruling: `config_seed` keys are `config.Config` fields applied to a FRESH `config.json` only (tierseed); `memory_stack` names the llama-swap entries the memory stack keeps resident; nothing in it reaches the llama-swap yaml - the installer doc already says so - none.
- Ruling: the "matrix var" is llama-swap's `matrix.vars` id for an entry (`e4b: offload-e4b`) that the residency set expressions reference - the template's own comment explains it - none.
- Ruling: whisper-server on Windows comes from the same cuda-12 build family the house nodes run (sm_50 PTX verified by inspecting the DLL); the tier page records the copy step - `SETUP-AGENT.md` already documents hand provisioning for every Windows tier - none.

## Acceptance (real runs, output recorded in the PR)

1. `pwsh setup\detect.ps1` on `<node-g>` prints `"profile":"maxwell-3"`, `gpu_arch":"maxwell"`, no unrecognised-GPU warning; `local-offload install detect` agrees.
2. `install.ps1` end to end with the home on the data drive: the rendered yaml has no `offload-e4b`, the E4B GGUF is not downloaded, every bound seat's weights exist, `selftest.ps1` canaries pass, `local-offload doctor` is clean, `local-offload report` lists every bound lane as CONFIGURED.
3. One real call per bound lane from the box's own Claude Code through the MCP server (summarize, an agent run, vqa, transcribe, generate_image if bound), each output read back.
4. `go build ./...`, `go vet ./...`, the repo's CI; `go generate` committed; `render.tests.ps1` block for the tier passes; PR merged.
