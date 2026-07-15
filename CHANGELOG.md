# Changelog

All notable changes to `offload-harness` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [0.12.0] - 2026-07-15

### Added
- **Video quality options (universal, param-driven — never hardware-baked):**
  - **`hero` mode** — a native no-LoRA quality pass for the Wan builder (skips the distill LoRAs, native
    steps/cfg). Slower, but restores the camera/subject motion the 4-step LoRA trades for speed — the
    per-research win for realistic b-roll. `--hero` (CLI) / `hero:true` (tool). Fast 4-step stays default.
  - **`upscale`** — an optional post-decode ESRGAN upscale (`UpscaleModelLoader` → `ImageUpscaleWithModel`
    → optional resize), e.g. 720p→1080p. The upscale model + target are **per-machine config**
    (`videogen_upscale_model` / `videogen_upscale_width` / `videogen_upscale_height`) so no model name is
    baked into shared code; a machine with no upscale model just skips it. `--upscale` (CLI) / `upscale:true`.
  - (Frame count remains the per-machine `videogen_frames` knob — 81 ≈ a real 5s clip.)

## [0.11.0] - 2026-07-15

### Fixed
- **Video (`render/comfy-video.mjs` + `render/wf-wan22-i2v.mjs`): the default I2V path now works and is fast.**
  Default model flipped Hunyuan→**Wan 2.2** (Hunyuan needs 3 files absent on this box), with the JS-scoping
  bug fixed. Wired the on-disk **4-step lightx2v LoRAs** (HIGH on the high-noise expert, LOW on the low-noise
  expert) — ~91s for a 25-frame 480p clip vs ~12-25min native. Fixed a pre-existing wrong VAE default
  (`wan2.2_vae` is the 48-ch 5B VAE; the 14B A14B I2V needs the 16-ch `wan_2.1_vae` → the `patch_embed`
  36-vs-64-channel error). Live-verified end-to-end at 480p AND 720p.
- **Music (`render/wf-acestep.mjs`): rewritten from the retired v1 all-in-one to the ACE-Step v1.5 split
  stack** (UNETLoader + DualCLIPLoader type `ace` + VAELoader + the 1.5 encoder/latent/AuraFlow nodes), all
  on disk; every input verified against the live `/object_info`. Live-verified (10s FLAC).

### Added
- **Per-machine video resolution** (`videogen_width` / `videogen_height` / `videogen_frames` config): a 16GB
  box defaults to 720p while an 8GB box stays at the builder's 480p default (a per-request value still wins).
  Keeps the harness hardware-agnostic — resolution is config, not a constant. e.g. a 16GB box may set 1280×720.

## [0.10.2] - 2026-07-15

### Changed
- **`Default()` no longer ships phantom model names** (opt-in defaults): `vision_model` and
  `stt_model_hq` default to `""` instead of `qwen3vl-4b` / `whisper-stt-hq` (aliases served on no
  current machine). A machine that omits them now cleanly disables that route instead of inheriting a
  model that errors → cloud-defers. Configured machines are unaffected (they set both). Template +
  `config.example.json` updated.

## [0.10.1] - 2026-07-15

### Fixed
- **whisper-stt crash resilience** (`internal/sttclient`): the "whisper-server 502" was a whisper-server
  SIGSEGV, not a request bug (real speech transcribes fine). Two harness-side mitigations:
  - A real process-global serialization mutex around the inference request — whisper-server is
    single-slot and crashes on overlapping requests (the `Client` doc-comment claimed serialization but
    no mutex existed).
  - An empty-body 5xx (the crash signature) now yields a descriptive, diagnostic error (crash /
    near-silent audio / cold-restart) instead of a bare status code, so the defer reason is accurate.
  - (Machine-local `config.json`: `stt_unload_after:false` keeps whisper warm between back-to-back calls
    so no-speech input returns 200-empty instead of cold-crashing — not part of this repo diff.)
  - The full fix (whisper `ttl:-1` in the live llama-swap so it never cold-loads) needs operator
    approval and is not included. See docs/superpowers/evidence/2026-07-15-whisper-crash-resilience.md.

## [0.10.0] - 2026-07-15

### Fixed
- **Model-roster hardcodes removed from shared code** (keeps the harness hardware/model-agnostic — the
  roster is per-machine config, never baked in):
  - `internal/judge/embed.go` no longer hardcodes `"embeddinggemma"` — `NewEmbedder` takes the model,
    threaded from a new `Config.EmbedModel()` accessor (`MemoryStack[0]`, with the fallback living only
    in config). The genuine model-agnosticism violation.
  - `internal/mcpserver` `agent_run` planner default and `cmd/local-agent` `--model` / `--architect-model`
    / `--editor-model` now fall back to the configured model (`cfg.Model` / `cfg.EscalationModel`) instead
    of the literals `offload-e4b` / `gemma4-26b-a4b`.

### Added
- **`ocr_max_tokens` config** (default 1024): a machine with a strong VLM can raise the OCR output cap so
  a dense document page transcribes locally instead of hitting the 1024 ceiling (`finish_reason=length`)
  and deferring the whole OCR to cloud. Threaded into the vision dispatch; covers `extract_image` too.
- Guard tests: `TestEmbedUsesConfiguredModel`, `TestOCRHonorsConfiguredMaxTokens`, `TestModelFlagFallsBackToConfig`.

## [0.9.0] - 2026-07-14

### Fixed
- **Router entry tier is no longer hardcoded** (`internal/router`): `Train` takes the entry-tier
  model from config (`cfg.TriageModel`) instead of the literal `"gemma4-e2b"`. On any machine whose
  hardware-optimized roster names its triage model differently (e.g. `gemma-4-e2b`), the ledger rows
  never matched, so the self-optimization router silently collected 0 rows and never trained. The
  harness stays hardware/model-agnostic: the roster is per-machine config, never baked into shared code.

### Added
- **Per-machine image-model binding** (`internal/config`, `internal/imagegen`, `render/comfy-render.mjs`):
  `imagegen_ckpt` / `imagegen_vae` / `imagegen_steps` / `imagegen_cfg` / `imagegen_sampler` /
  `imagegen_scheduler`. A machine renders with the checkpoint its hardware can run — SDXL on an 8 GB
  box, an all-in-one DiT such as HiDream on a 16 GB box via `--vae builtin` (decodes with the VAE the
  checkpoint loader supplies; HiDream ships no VAE weights). Every field is optional and a zero binding
  emits no flags, so an unbound machine renders exactly as before.
- **Version-consistency guard** (`main_test.go` `TestVersionSourcesAgree`): the `VERSION` file, the
  compiled-in `version` const (advertised in the MCP handshake), and the top `CHANGELOG.md` entry must
  all name the same version — a build failure now catches drift. They had drifted to
  `VERSION` 0.7.0 / const 0.6.2 / CHANGELOG 0.7.0.

### Changed
- Version reconciled to **0.9.0** so this canonical private repo sits ahead of the public mirror
  (`offload-harness`, published at 0.8.0), per the versioning invariant. Folds in the 0.8.0 line already
  present in this tree — local coding-agent capabilities + the per-hardware optimization matrix — plus
  the CUDA-toolkit / Blackwell `detect` work the 0.8.0 publish did not carry.

## [0.8.0] - 2026-07-12

### Added — local coding agent capabilities (survive & drive complex multi-step tasks)
- **Transcript compaction + tool-result cap**: the agent loop stays within the served window (keep system + objective + recent turns full, elide older tool outputs, cap any single result), with a one-shot harder-compact retry on overflow instead of aborting — long tasks complete instead of crashing.
- **New tools**: `search_files` (worktree-confined regex grep, per-file grouped, 100-match cap), ranged `read_file` (offset/limit lines + continuation hints), `summarize_file` (digests a big file via the local offload cascade so its bytes never enter the transcript).
- **Working memory**: a per-workspace `AGENT.md` (loaded fenced-as-untrusted) and an `update_plan` scratchpad the loop re-injects on a cadence.
- **Per-task tool profiles** (`--profile edit|build|research|github`): a curated tool subset + tuned prompt + worked few-shot exemplars (small models select tools more reliably with fewer advertised); a profile can only narrow, never grant.
- **Restricted runner** (`--allow-run`, OFF by default): runs an allowlisted program directly (no shell) inside the OS sandbox — Linux (Landlock/seccomp) and **Windows (Job Object + low-integrity token, worktree-write-confined + transient relabel)**. `run_shell` (arbitrary shell) is **Linux-only**. Honest residual risk: on native Windows, writes/resources/allowlist are confined but **reads and network are not** — documented in the security section.
- **Architect/editor two-tier mode** (`--two-tier`): a planning model drafts a complete plan, a separate edit model executes it (aider-style one-shot handoff) — one cold swap, or zero-swap on a dual-GPU host.
- **Larger context**: `--ctx-tokens` + a 16K CUDA agent tier with q8_0 KV (measured throughput-neutral).

### Added — per-hardware optimization matrix
- `detect.ps1` classifies the host into one of 10 arch-profiles (Blackwell/Ampere/Volta/RDNA3/GCN × VRAM band + RAM tier + GPU count); `install.ps1` renders profile-driven serving (context, KV type, 26B-A4B placement, and a dual-GPU two-model-resident config); `selftest.ps1` **measures** the projections on the real box and records measured-vs-projected honestly.
- Documents the **Blackwell (sm_120) CUDA-12.8 build requirement** — neither stock Windows CUDA prebuilt works (12.4 lacks sm_120; 13.x segfaults the MMQ kernel ~5.6× slower).

### Changed
- README security section + OPERATOR-GUIDE + SETUP-AGENT + CLAUDE.md updated to document every tool, flag, the runner's honest posture, and the profile matrix.

## [0.7.0] - 2026-07-09

### Added
- **Cross-vendor Windows setup** (`setup/`): hardware detector (NVIDIA→CUDA, AMD→Vulkan incl. RDNA3 iGPUs like the Radeon 780M, CPU fallback), idempotent installer with pinned+SHA-verified assets and models, and a receipt-emitting selftest (per-tier swap-group exercise, deep-context Vulkan canary at ~7K depth, automatic `--cpu-moe` remediation, honest proves/does-not-prove scoping).
- **Local coding agent published** (`cmd/local-agent`): plan→tool loop over a local model with policy-brokered write/edit/search/GitHub tools, OpenAI-compatible `--serve` mode (loopback-only by default; `--listen-trusted-network` override), same-tool circuit breaker, and `--max-tokens` control.
- **Agent-facing docs**: `setup/SETUP-AGENT.md` install runbook for AI agents, `AGENTS.md`, `CLAUDE.md` orientation map, `docs/OPERATOR-GUIDE.md`.
- **Serving templates** for llama-swap on Windows (Vulkan / CUDA / CPU) with grammar-reliable flags.

### Changed
- README: cross-vendor requirements, agent setup entry, security posture section.
- `config.example.json`: escalation/reasoning tiers now reference the served `gemma4-26b-a4b`.

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
