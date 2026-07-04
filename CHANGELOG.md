# Changelog

All notable changes to `local-offload` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [0.6.0] — 2026-06-29

### Added
- **Remote NVIDIA NIM tool** (`nim` CLI subcommand + `offload_nim` MCP tool) — an explicit, opt-in path to any OpenAI-compatible NVIDIA NIM endpoint: NVIDIA's hosted [build.nvidia.com](https://build.nvidia.com) catalog (dozens of **free** models — nemotron-3-ultra-550b, llama-3.3-70b, gpt-oss, qwen, deepseek, glm, kimi…) by default, or a **self-hosted** NIM container via `--base http://host:8000/v1`. It lets the harness reach frontier models a small local GPU can't run, used deliberately rather than for routine grunt work.
  - Model-agnostic: pick any model per call (`--model` / `model`), or browse the catalog with `nim --list-models` (`list_models:true`).
  - **The API key is read from `$NVIDIA_API_KEY` (or `$NGC_API_KEY`) only — never a config field**, so a secret never lands in a tracked file or the public repo. A self-hosted NIM is keyless.
  - **Stays out of the cascade and the savings ledger:** NIM calls are deliberate remote experiments/escalations, not local defer-avoidance, so they are never recorded as Opus-tokens-saved. The local Gemma cascade and its sacred GBNF grammar path are untouched.
  - Defers-not-crashes: an unset key on the hosted endpoint, a down endpoint, or a bad model id returns a clean error (CLI) / `{deferred:true, reason}` (MCP), never a panic.
- New `internal/nimclient` package (pure `net/http`, fully unit-tested) and read-only `Pipeline.Cfg()` accessor for side-channel tools.

### Changed
- Config gains `nim_endpoint` / `nim_model` / `nim_max_tokens` / `nim_timeout_sec` (all defaulted; the hosted endpoint + nemotron-3-ultra-550b are the defaults). No existing behavior changes.

### Fixed (packaging & robustness patch, 2026-07-03 — ships in the v0.6.0 tag)
- **Module path now matches the repository** (`github.com/dmmdea/offload-harness`), so `go install github.com/dmmdea/offload-harness@latest` works. If you installed under an older module path, reinstall with the command above.
- Relative render-script paths (`videogen_script` etc.) resolve against the executable's directory, so generation tools work from any working directory.
- `~/` expands in all path-typed config fields, and `./config.json` joined the config search order (`--config` > `$LOCAL_OFFLOAD_CONFIG` > `./config.json` > `~/.local-offload/config.json` > defaults).
- Bad MCP tool arguments surface as a structured defer with the argument error instead of a generic failure.
- `doctor` diffs the live `/v1/models` roster against every configured model alias; `models` output is data-driven.
- The ledger records each defer's reason; `ledger` reports the top defer reasons.
- The llama client's request budget is split from tier timeouts, and cold-model-swap timeouts no longer trip the circuit breaker.
- Context-budget trimming is rune-safe (no more mid-UTF-8 cut points).
- The savings ledger labels its estimate honestly (`est_value_kept_local`).

## [0.5.0] — 2026-06-29

### Added
- **Local media generation** on the single 8 GB GPU, behind a generalized single-slot scheduler (each is opt-in; the default text/vision/STT runtime is unchanged, and every path defers cleanly when its model/ComfyUI/script is absent):
  - **Voice / TTS** — `offload_generate_audio` `kind=voice` (CLI `generate-audio`): Chatterbox Multilingual (commercial-safe, default Spanish, zero-shot voice cloning via `clone=`). Verified end-to-end (a real 2.84 s WAV through the harness).
  - **Music** — `offload_generate_audio` `kind=music`: ACE-Step 1.5 text-to-music (style prompt + optional lyrics, seeded). Verified end-to-end (a real 7.99 s FLAC).
  - **Video** — `offload_generate_video` (CLI `generate-video`): Hunyuan 1.5 480p image-to-video. Wiring complete and the ComfyUI graph executes cleanly. **Caveat:** a quality render (`steps=50`) is throughput-gated on the 8 GB 3070 — it exceeds the worker's ~20-min window — so video is wired but impractical on this card until a step-distilled checkpoint / a fast tier (LTX) / a larger-VRAM GPU.
- **Generalized GPU single-slot scheduler** (`render/gpu-lock.mjs` `withGpuSlot` + shared `render/comfy-lifecycle.mjs`): one cross-process lock serializes image/video/audio generation; the guarded lifecycle (freeLlamaSwap → ensureComfy → guarded teardown + signal handlers) is centralized; new `internal/gpugen` adds a Windows process-tree-kill on timeout so a gen run can't orphan a VRAM-pinning ComfyUI child. `MEMORY_STACK` (the CPU models never unloaded) is now config/env-sourced.

### Changed
- `internal/imagegen` is now a thin caller of `internal/gpugen`; image-generation behavior is unchanged (byte-equivalent).

## [0.4.2] — 2026-06-29

### Added
- **Live hot-reload of self-learning artifacts.** The long-running MCP server now picks up nightly-retrained weights/thresholds/overrides without a restart — a stdlib content-hash poll reloader (fail-open last-good; the confhead head+thresholds are swapped atomically as one snapshot; the append-grown kNN index is excluded; all artifact writers are atomic tmp+rename). Starts only in `mcp` mode; CLI one-shots are byte-identical.
- **`offload eval --confhead-ab`** — a paired A/B decision-gate that replays a held-out gold set with the confidence head OFF vs ON (staged weights via a temp config, never touching live) and reports per-task selective-accuracy / cost / AUDC frontier dominance plus a calibrated-margin baseline. The reusable gate for deciding whether enabling the head is a net win.
- **Calibration diagnostics:** AUGRC + ECE reported alongside the confhead-eval AURC verdict; realized-accepted-error vs target in confhead-calibrate. Diagnostics only — they never change the adoption verdict.
- A larger, **unambiguous, consistently-labeled** classify/triage eval gold corpus (162/158 train + 45/40 disjoint held-out) with an explicit `testdata/eval/LABELING-RUBRIC.md`.

### Fixed
- **Router/kNN label feeder revived.** The shadow drain's router-label + kNN-substrate synthesis was structurally dead (it only fired for non-E2B-entry rows, which capture never produces). It now derives router + kNN labels from the escalation-agreement signal already computed for E2B-entry rows — zero extra inference, savings ledger untouched.

### Changed
- Confhead/calibration emission floor `minRows` 100 → 60 (emission gate only; the OOF paired-bootstrap CI remains the adoption guard). `alpha`, `target_error_rate`, and the conformal CRC are unchanged.

### Notes
- **The confidence head was evaluated end-to-end and deliberately left DISABLED (`confhead_enabled=false`).** On the current local classify/triage workload the small E2B tier is ~98–100% accurate, so there are almost no "should-escalate" negatives, and a label-validity probe found the self-agreement label (E2B vs the larger local tier) is ~77% backwards on disagreements (the larger tier is the *less* accurate one here). The adoption gates correctly returned REJECT. The plumbing is built, reviewed, and ready for a workload where escalation actually pays off (e.g. cloud-vs-local quota routing); it changes no default behavior today.

## [0.4.1] — 2026-06-28

### Fixed
- **The shadow-labeling flywheel now actually manufactures counterfactual labels.** Two compounding bugs had left it producing ~0 labels:
  - **Config silently ignored by the MCP server.** A bare `local-offload mcp` (host passes neither `--config` nor `$LOCAL_OFFLOAD_CONFIG`) ran on built-in defaults with shadow capture **off**. `loadCfg` now also auto-discovers `~/.local-offload/config.json` when both are unset (new `resolveCfgPath`; precedence: flag → env → conventional path → defaults).
  - **Healthy entry tiers were route-skipped.** `internal/health` flagged tiers DEGRADED on margin/throughput **drift** (CUSUM/Page-Hinkley) or throughput collapse, and the cascade routed *around* any DEGRADED tier — so an accurate small entry tier that was merely non-stationary (single-GPU throughput variance) got skipped to a larger, slower one, starving the flywheel of entry-tier data. Health now separates a `route_skip` signal (true only on a genuine **quality collapse** — confidence margin far below the tier's own baseline) from the observability `Status` (any drift/throughput anomaly); only `route_skip` populates the routing skip-list. Drift/throughput remain visible for timeout tuning.
- The CLI `version` string now matches the `VERSION` file (was a stale `0.1.0`).

## [0.4.0] — 2026-06-28

### Added
- First public release. 0.4.0 reflects the already-mature harness (core text offload + full self-learning cascade + flywheel + kNN + vision/STT/video understanding + image & SVG generation); media generation, DaVinci editing, and the capstone remain on the roadmap.
- Text offload tools — **summarize, classify, extract, triage** — on a free local Gemma-4 cascade via llama.cpp; never calls a cloud model (returns a structured **defer** on low confidence).
- **MCP server** (stdio) exposing 12 tools, plus a Go CLI.
- **Vision**: VQA, OCR, image field-extraction, and render QA (`assess-image`).
- **Speech-to-text** via whisper.cpp (`transcribe`) and **video understanding** (`video-describe`).
- **Image generation** (SDXL via ComfyUI) and a dependency-free **SVG component kit** (gauge / comparison-bar / chromatogram / icons).
- **Self-learning cascade**: confidence-gated escalation, conformal thresholds, a logistic entry-tier router, health/circuit-breakers, few-shot exemplars, and an offline shadow-labeling flywheel — all inference-free over the token ledger.
- Append-only JSONL **token-savings ledger**.
