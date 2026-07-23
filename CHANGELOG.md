# Changelog

All notable changes to `offload-harness` are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [SemVer](https://semver.org/).

## [0.22.18] - 2026-07-23

### Added — GCF lossless columnar compaction (`internal/gcf`) — OmniRoute harvest Phase A
- New codec: eligible JSON arrays (≥8 flat objects, scalar values only) re-encode columnar —
  keys stated once in a `[N]{fields}` header, pipe-joined rows, `~`=missing/`-`=null sentinels,
  ambiguous strings JSON-quoted. **Losslessness is the contract**: the decoder ships as the
  round-trip ORACLE (production never decodes; the model reads the compact form) and every
  encoder path must deep-equal back through it in tests and fuzz — three defects were found and
  fixed BEFORE first ship (fuzz: Sscanf header fragility on field names with spaces; review,
  reproduced: all-empty-objects arrays decoding a phantom field; by test: trailing-content
  acceptance that would have eaten text after the array), crashers kept in the fuzz corpus.
  Anything outside the contract fails closed to the original text: under min rows, non-object
  elements, nested values, duplicate keys, unsafe field names, or no strict size win.
  ≥30% savings test-asserted on typical tool-output arrays. Format attribution (gcf-typescript
  via OmniRoute, both MIT) added to NOTICE; independent reimplementation, no code copied.
- **Agent ladder**: new LOSSLESS rung 2a via `--gcf-compact` (default off) — over budget, older
  JSON-array tool bodies re-encode before the skeleton/marker/drop rungs run. `compact()` now
  takes a `compactOpts` struct; zero value = the original pinned ladder.
- **Pipeline context assembly**: new `gcf_compact` config field (default off) — an over-budget
  input's eligible JSON is compacted losslessly BEFORE the head/tail context trim, converting
  would-be truncations into full-fidelity completions. In-budget inputs are never touched
  (happy-path bytes stable, pinned by test).

## [0.22.17] - 2026-07-23

### Added — skeleton rung in the agent's compaction ladder (`--skeleton-prune`, default off)
- The transcript compactor jumped straight from "full tool body" to "bare size marker", so the
  first budget crossing in a long agent task destroyed ALL information in every older tool
  result at once. A new flag-gated rung sits between the two: older verbose bodies are reduced
  to deterministic **skeletons** — the head/tail windows plus buried
  error/failure/warning/test-summary lines, elided runs replaced by counted
  `[... n lines elided ...]` markers, opened by a disclosure prefix that also makes the pass
  idempotent. Bare markers and whole-turn drops remain the fall-through when skeletons alone
  cannot reach the budget (a fallen-through marker reports the ORIGINAL body size, parsed from
  the skeleton prefix); with the flag off the older rungs run untouched (pinned by test on the
  off-path's shape). Surface: the `local-agent` CLI (`--skeleton-prune`), all drive modes incl.
  two-tier. The MCP `agent_run` path has no per-call knob yet — it gains the rung only if the
  default flips after broader measurement.
- Deliberately model-free: the local cascade costs ~4 s warm / ~11 s cold per ~2k-token call
  (measured 2026-07-23 on the 16 GB box), which would serialize every over-budget step; the
  rules pass costs microseconds, is unit-testable, and produces identical bytes on every
  re-compaction (KV-prefix friendly). A cascade-refined or lossless-structural skeletonizer can
  slot into the same seam later if measurement earns it (the queued OmniRoute study).
- Origin: the swe-pruner-pro *policy* (prune consumed tool outputs into skeletons) adopted
  WITHOUT its mechanism (SGLang hidden-state serving + per-backbone trained heads — verified
  against its README 2026-07-23); advisory proposal reconciled against this repo's existing
  compaction design rather than the proposal's insertion-time sketch.

## [0.22.16] - 2026-07-22

### Fixed — TTS venv setup docs generalized beyond the original machine
- `render/README.md`'s `.tts-venv` setup note documented one machine's path (ComfyUI's
  Python 3.12, `torch==2.6.0` cu124) — a recipe that fails outright on Blackwell GPUs
  (RTX 50xx, sm_120: cu124 ships no sm_120 kernels; torch ≥2.7.0 cu128 required) and
  omitted a universal trap: `resemble-perth` (chatterbox's watermarker dep) imports
  `pkg_resources`, removed in setuptools 81+ and absent from uv venvs, so
  `perth.PerthImplicitWatermarker` silently resolves to `None` and model init crashes with
  `TypeError: 'NoneType' object is not callable`. The note now covers both GPU generations,
  the `uv pip` install path (uv venvs ship no pip), and the setuptools<81 requirement, with a
  second VERIFIED line from the Blackwell machine (Qube, RTX 5060 Ti: real 4.0 s 24 kHz WAV
  end-to-end through `generate-audio`). Stale one-machine comment in `render/tts.mjs` retired
  with it.

## [0.22.15] - 2026-07-22

### Added — the HQ STT tier can be an llama-server mtmd model (`stt_hq_api: "openai"`)
- The HQ transcribe path spoke only whisper-server's `/inference` multipart protocol, so binding
  an llama-server-served STT model (Qwen3-ASR, mtmd) to `stt_model_hq` deferred with a
  whisper-endpoint 404 (live finding 2026-07-21, binding rolled back). The client now also speaks
  the OpenAI `/v1/audio/transcriptions` shape through the same `/upstream/<model>/` passthrough —
  verified live on Qube before building (HTTP 200 on `qwen3-asr` with a correct transcription).
- New config field `stt_hq_api`: `""`/`"whisper"` = the existing whisper protocol; `"openai"` =
  the transcriptions endpoint. Selection only affects the `hq=true` path; the default STT tier is
  untouched.
- Adapter honesty: mtmd ASR models emit no timestamps, so the result carries ONE synthesized
  full-span segment (duration derived from the 16 kHz WAV size); Qwen3-ASR's
  `language X<asr_text>` prefix is parsed into the result's language field (free detection);
  whisper decode knobs (VAD/beam/language forcing) do not apply on this path. The single-slot
  serialization mutex covers both protocols.
- Matrix updated: the v0.22.12 "cannot bind qwen3-asr yet" non-binding is now a HOW-TO (set both
  `stt_model_hq` and `stt_hq_api`); the MCP transcribe tool's `hq` description no longer
  hardcodes "large-v3".

## [0.22.14] - 2026-07-22

### Changed — running on built-in defaults is now LOUD (`config:` disclosure in doctor)
- When the effective config is built-in defaults, every config-loading command prints one stderr
  warning naming the state and the escape hatches. All THREE defaults shapes warn (the last two
  were review findings on the first draft of this change): nothing resolves anywhere; an explicit
  `--config`/`$LOCAL_OFFLOAD_CONFIG` path that does NOT exist (`Load` maps IsNotExist to defaults
  with a nil error by design — previously totally silent); and a file that exists but fails to
  parse. Silent fallback was the trap (live incident 2026-07-20): a box whose real config lived at
  a non-conventional path served every bare CLI call from built-in defaults — empty `vision_model`
  and all — and the only symptom was a misleading "no vision model configured" defer inside a
  consumer's pipeline.
- `doctor` prints a `config:` first line that is TRUTHFUL: it names the file only when that file
  was actually read; not-found and failed-to-load both disclose `BUILT-IN DEFAULTS` with the
  reason (crediting an unread file would be worse than no disclosure).
- Resolution + disclosure moved to `internal/config` (`ResolvePath`/`LoadWithSource`/
  `WarnOnDefaults`/`SourceLine`) and **`local-agent` now shares it** — review found that binary
  silently ran on defaults AND never discovered the conventional `~/.local-offload/config.json`
  at all.

## [0.22.13] - 2026-07-22

### Fixed — a spawn failure is not a venv problem (`SATISFIER_SPAWN_FAILED`)
- A satisfier subprocess (git / uv / pip) that failed to **start** — Node spawn errors like
  `spawn UNKNOWN` / `ENOENT` — was caught and relabeled `VENV_INCOHERENT`, in the worst case as
  "pip check reported conflicting installed dependencies: spawn UNKNOWN": a wrong diagnosis
  pointing the operator at healthy torch pins (live creative-marketing-pipelines report,
  2026-07-20, transient after a long render batch). Spawn failures are now classified apart
  (`isSpawnFailure`), **retried once** after 500 ms (the observed failure was transient), and then
  defer with the new typed code **`SATISFIER_SPAWN_FAILED`** naming the stage (`pack name` /
  `uv-resolve` / `pip-check`). `VENV_INCOHERENT` now always means the tool RAN and found a real
  problem.

### Changed — proven-satisfied re-runs skip the resolve; one deliberate fail-open
- The common re-run stops paying the expensive `uv` resolve no-op — but the skip is authorized by
  a **persisted marker** (`<venv>/.offload-deps-satisfied` — it attests venv state so it lives
  inside the venv and a recreated venv takes it down; the sorted `name@commit` pin-set key), written ONLY after a fully successful resolve+check. "Git didn't move this run" alone is
  NOT proof (adversarial-review finding: a run that checks packs out and fails before installing
  would otherwise skip the install forever and report ok on an unprovisioned env). A stale or
  missing marker, or any moved checkout, still runs the full resolve. The cheap `pip check`
  coherence gate always runs.
- Fail-open, narrowly: only a marker-proven, unchanged pin-set survives a coherence check whose
  subprocess fails to spawn (after the retry) — satisfy then succeeds with a `warning` (stderr
  `SATISFY WARN`). Anything unproven fails closed with `SATISFIER_SPAWN_FAILED`.
- The git checkout stage is deliberately NOT retried: a whole-step retry recomputes HEAD-before
  against its own surviving side effects (fresh clone / moved checkout) and misreports
  `changed=false` (adversarial-review repro). A git spawn failure defers typed; retrying the whole
  satisfy is the caller's idempotent recovery. The lazily-captured host-pins snapshot also no
  longer memoizes a REJECTED promise, so the 500 ms retry genuinely re-runs `pip freeze`.
- Docs de-staled in the same change: the flow doc and ADR 0007 still described the pre-v0.22.5
  "drift diagnostic only on stderr" gap as open; both now record the staged fixes. The v0.22.12
  changelog's "old shape / tier-agnostic" phrasing is also clarified below (the shipped default
  binds `gemma4-26b-a4b` to both escalation and reasoning and is unchanged; the 4-rung ladder is
  a ≥16GB matrix recommendation on top of it).

## [0.22.12] - 2026-07-21

### Changed — model matrix: validated 4-rung cascade ladder for ≥16GB tiers
- `setup/SETUP-AGENT.md` gains a **Text-cascade matrix** section recording the roster validation
  run on `ampere-16` (operator benchmark archive `2026-07-20_qube-roster-validation/`): the
  recommended ≥16GB binding is the 4-rung ladder — e2b triage (154.5 tok/s) → e4b workhorse
  (95.7) → **a 12B-MTP escalation rung (`offload-12b`, 82.1 — 2.5× the 26B it offloads from,
  grammar-clean, task-level A/B showed zero regressions)** → 26b terminal reasoning (32.5). The
  old shape bound escalation AND reasoning to the 26B, wasting a slot. 8GB tiers keep the 3-rung
  ladder; code defaults stay tier-agnostic (this is a matrix recommendation, not a default change).
- Two validated **non-bindings** recorded alongside: `gpt-oss-20b` must never take a
  grammar-constrained slot (harmony format vs GBNF = hard 500; reasoning phase eats small budgets
  → empty content; it stays the free-text/throughput model, 4-slot admission proven with aggregate
  throughput halving under 4-way load), and `stt_model_hq` cannot bind `qwen3-asr` yet (the HQ
  transcribe client speaks whisper-server HTTP; Qwen3-ASR is served by llama-server mtmd — binding
  defers with a whisper-endpoint 404, verified live and rolled back; needs an llama-server audio
  path for the HQ tier first).
- `docs/systems/offload-pipeline.md` and `docs/flows/cascade-escalation-and-defer.md` are
  cross-referenced with the matrix and their "reasoning runs on the same model as escalation"
  statements conditioned on the binding (they were flatly false under the new recommendation).

## [0.22.11] - 2026-07-20

### Fixed — model presence spans ComfyUI's full search path (`extra_model_paths.yaml`)
- The satisfier resolved every model as `join(comfyDir, model.path)` and knew nothing about
  `extra_model_paths.yaml`, so on any machine that keeps its model tree off the OS drive — e.g. a
  `V:` Optane tree, the documented Qube layout — a model that ComfyUI loads fine read as **MISSING**
  and was re-downloaded to `C:` on every run (tens of GB for a big GGUF). Presence now checks the
  canonical location first and then every directory registered for that model class. The YAML is
  read by a minimal, dependency-free parser (this repo ships no npm deps) handling `base_path`,
  `category: dir`, and block scalars (`unet: |` listing several dirs); it is **fail-safe** —
  anything unparseable yields no extra roots, i.e. exactly the previous behavior. Verified against
  the live Qube file: 17 categories parsed, `unet` correctly expanded to both `diffusion_models` and
  `unet`, and the real Qwen-Image-Edit GGUF resolved on `V:`.

### Fixed — a pre-provisioned model is adopted instead of re-downloaded
- The skip gate trusted the `.sha-ok` sidecar alone, so a model placed by hand or by `curl` — with a
  byte-correct hash but no sidecar — failed the gate and fell into the **download** branch, re-fetching
  a file that was already correct. A present file with a pinned sha is now hashed **once** and, on a
  match, adopted by writing the sentinel beside the file that was actually found (which may be on the
  secondary tree, not `comfyDir`). On a mismatch it is replaced when a `source_url` exists; with no
  `source_url` it defers `MODEL_SHA_MISMATCH` — naming the real problem instead of the misleading
  "missing on disk and no source_url".
- Both defects were reported by the creative-marketing-pipelines session from its live 16 GB
  scene-swap run on Qube, and both are covered by new tests (43 in the file, up from 31).

## [0.22.10] - 2026-07-20

### Fixed — model matrix: Qwen-Image-Edit-2511 must pin a `_1` GGUF quant, not a `_K_` one
- The v0.22.7 designation of Qwen-Image-Edit-2511 as the ≥16 GB image-edit primitive named no quant,
  which is actively misleading: 2511 **K-quants** (`Q5_K_S` and friends, including the common unsloth
  default) fail ComfyUI-GGUF's `UnetLoaderGGUF` with `cannot reshape array` even on a byte-perfect
  file (disk sha == upstream LFS oid, gguf 0.19.0, pack at HEAD) — see city96/ComfyUI-GGUF issue #247.
  Only `Q4_1`/`Q5_1` load. Following the old wording cost a 15 GB download that then fails at load.
  Both the setup guide and the media-generation system doc now pin the `_1` quants explicitly and
  record the live `ampere-16` measurement from the creative-marketing-pipelines session (2026-07-19):
  Q5_1 (15.4 GB) + fp8 Qwen2.5-VL encoder fits 16 GB with block-swap, composite peak **15,757 MiB**
  (HiDream for comparison: 15,688 MiB) — the first real footprint datum for the Qwen edit family.

## [0.22.9] - 2026-07-18

### Fixed — run-graph model download streams to disk (models >2GB now work)
- The manifest satisfier's `download` buffered the entire model file in memory via
  `Buffer.from(await r.arrayBuffer())`, which throws a ">2GB length" RangeError on Node's
  Buffer/ArrayBuffer cap — so any model over ~2GB (Qwen-Image-Edit GGUF ~14GB, RealVisXL 6.94GB)
  could never be satisfied and the run-graph manifest deferred. It now streams the response body
  straight to disk (`pipeline(Readable.fromWeb(r.body), createWriteStream(dest))`), which has no size
  limit. The sha256-verify path already streamed, so only the download was affected. Reported by the
  creative-marketing-pipelines session hitting it live on RealVisXL. Regression test asserts the
  streaming path (a mock exposing only `body`, no `arrayBuffer`).

## [0.22.8] - 2026-07-18

### Fixed — footprint store merges on write (no cross-process clobber)
- A fleet node's footprint store `Record` overwrote `~/.local-offload/footprints.json` with only the
  writing process's in-memory entries. When `fleet-measure` and the MCP server ran as separate
  processes sharing that file, whichever wrote last **clobbered** the other's records — the live Aorus
  symptom "only comfy-graph advertised" was the MCP's write wiping fleet-measure's freshly-measured
  entries. (The earlier "cache lock" diagnosis was a misread; footprints.json is not the cache.) The
  merge (`ReloadIfChanged`) previously ran only on the read path; `Record` now reload-merges the
  on-disk state **before** persisting, so a write folds in another process's records instead of
  overwriting them. Reproduction test added (two stores on one path → both records survive). Residual:
  two writes in the same mtime tick can still race — negligible in practice since renders are
  GPU-lock-serialized; a cross-process file lock would fully serialize if ever needed.

## [0.22.7] - 2026-07-18

### Added — Qwen-Image-Edit-2511 designated the ≥16GB image-edit primitive (model matrix)
- Documented Qwen-Image-Edit-2511 (Apache-2.0, commercial-safe) as the recommended ≥16GB image-*edit*
  primitive in `setup/SETUP-AGENT.md` and `docs/systems/media-generation.md`. It is a matrix
  *designation*, not a `config_seed` binding — edit workflows run through `run-graph` with the model
  set in the caller's node manifest, so no edit checkpoint is seeded into `config.json`. HiDream-O1
  (t2i) and Wan (video) stay the config_seed bindings; FLUX-family remains prohibited (ADR 0011).
  Aligns the harness model matrix with the creative-marketing-pipelines 16GB tier pick.

## [0.22.6] - 2026-07-18

### Changed — fleet nodes advertise the RAW footprint; the dispatcher owns all margin (CONTRACT v2.1)
- A fleet node used to pad its advertised footprint by ×1.2 (`vram_peak_gb = round(observed × 1.2)`).
  The dispatcher independently applies its own margin, so the two compounded (`observed × 1.2 × 1.2 +
  offset`) and pushed wan2.2/hidream past a 16 GB node's capacity — **unroutable** even though they
  run there. `Record` now stores the **raw** observed peak (`round(observed, 0.1)`); the dispatcher
  owns all routing margin. The `vram_peak_gb` contract field's meaning changes from padded footprint
  to raw measured peak (CONTRACT v2.1). New **ADR 0013** records the decision and supersedes the
  padding part of ADR 0008 (the PDH sampling core of 0008 is unchanged). Footprint tests updated to
  the raw values.

## [0.22.5] - 2026-07-18

### Changed — VENV_INCOHERENT defers say WHY (host-pin drift vs dependency conflict)
- The run-graph pack satisfier's `pipCheck` tripwire returned a bare boolean for both a genuine
  dependency conflict and host-pin drift (a pack moving `torch`/`torchvision`/`torchaudio`/`numpy`),
  so every `VENV_INCOHERENT` defer read `"conflicting installed dependencies"` and the actionable
  drift diagnostic — which pinned package moved, expected vs observed — was written only to stderr.
  `pipCheck` now returns `{ok, reason}` and the defer surfaces that reason, so a consuming workflow
  can tell drift from a conflict and see the exact pins. Makes a Qwen-image-edit (or any node-pack)
  manifest-satisfaction defer actionable instead of opaque.

## [0.22.4] - 2026-07-18

### Fixed — run-graph creates a caller-supplied out_dir instead of ENOENT-ing
- A caller-supplied `out_dir` that did not yet exist failed at first output write with an opaque
  `RUN_ERROR` — only the *defaulted* media dir was created, never a caller's directory. The Go side
  now resolves and `MkdirAll`s the output directory in either case (new `resolveOutDir` helper, unit
  tested); a directory that genuinely cannot be created defers typed ("cannot create out_dir") rather
  than failing later. The standalone `render/comfy-run-graph.mjs` write path `mkdirSync`s the target
  as well, so a fresh out-dir works there too.

## [0.22.3] - 2026-07-18

### Fixed — run-graph model satisfier crashed with "require is not defined"
- `render/manifest-satisfy.mjs`'s `defaultSatisfyDeps` called `require("node:fs")` /
  `require("node:path")` inside this ESM module, which throws `ReferenceError: require is not
  defined`. Two real failures resulted: a model **present on disk with a manifest `sha256` but no
  `.sha-ok` sentinel** fell through to the download branch and deferred `MODEL_DOWNLOAD_FAILED`
  ("require is not defined") even though nothing was wrong with the file, and a **fresh download**
  threw from `writeSentinel` (which sat outside any try/catch) and crashed the whole run with an
  untyped exit. Replaced the four `require()` calls (one in `writeSentinel`, three in `download`) with
  the existing ESM imports (adding `mkdirSync`).
- Hardened the post-download path in `satisfyModels`: the hash read and sentinel write are now
  guarded, so a genuine filesystem failure defers typed (`MODEL_DOWNLOAD_FAILED`) instead of escaping
  as a process crash.
- Added regression tests exercising the real `defaultSatisfyDeps.writeSentinel` / `.download`
  closures (the production glue was previously untested — the gap that hid this).

## [0.22.2] - 2026-07-18

### Changed — operator-neutral memory namespace (multitenancy)
- **The optional `--memory` recall namespace is no longer hardcoded.** The agent previously always
  recalled from a compiled-in `"dmmdea"` namespace alongside its own; it now recalls only its own
  namespace by default and appends an operator/shared namespace only when one is configured via the
  new `--mem-shared-namespace` flag or the `MEM0_SHARED_NAMESPACE` environment variable. This makes
  the public build operator-neutral (multitenant) — no personal namespace is baked into tracked code.
  New helper `agent.ReadUsers` builds the list; behavior is unchanged for an operator who sets the
  namespace. First step of the repo-model inversion (public becomes the canonical, multitenant source).

### Fixed
- **`TestDocsLint` is now line-ending agnostic.** The ADR frontmatter check anchored on `\n`, so a
  Windows checkout with `autocrlf` (CRLF working tree) failed every ADR. The regex now accepts
  `\r?\n`, so the gate passes for any contributor regardless of line-ending configuration.

## [0.22.1] - 2026-07-18

### Added — repo-local documentation system
- **`docs/` is now a navigable knowledge base** for humans and coding agents, with `AGENTS.md`
  as the routing layer: `systems/` (offload pipeline, coding agent, MCP server, media generation,
  fleet node, setup/installer), `flows/` (cascade escalation and defer, run-graph manifest
  satisfaction, fleet job lifecycle, zero-warm generation), `architecture/decisions/` (ADRs
  0001–0011, backfilled from decisions that previously lived only in `CLAUDE.md` invariants and
  session records), `glossary.md`, `templates/`, and `STYLE.md`.
- **`TestDocsLint`** — a structural gate run by `go test ./...`: scaffold files exist, relative
  links resolve, ADR frontmatter is schema-valid, and system/flow docs keep their navigational
  sections. Scoped to the durable documentation surface; `docs/templates/` and the dated
  `docs/superpowers/` archive are exempt from link resolution by design.
- **`CONTRIBUTING.md` documentation section** — read before changing, update in the same PR, and
  the three legal ways to resolve a docs/code disagreement.

### Fixed — documentation accuracy
- Corrected four `CLAUDE.md` claims that disagreed with the code, each re-verified against source:
  KV cache type is profile-driven (`q8_0` on 9 of 13 profiles) rather than `f16` everywhere, with
  an `amd-gcn` flash-attn exception, a cpu template that omits the flag, an `embeddinggemma` entry
  that bypasses the shared macro, and no STT carve-out (no whisper entry is templated); the policy
  broker gates effectful actions while the **loop** owns step and tool budgets (two mechanisms,
  previously described as one); `--profile` and `--two-tier` conflict only for a non-default
  profile; and the **cascade** never calls cloud while `offload_nim` is an explicit opt-in side
  channel.

### Known issues documented (not yet fixed)
- **run-graph model leg** — `render/manifest-satisfy.mjs` calls `require()` in an ESM module.
  A model present on disk with a declared `sha256` but no `.sha-ok` sentinel re-enters the download
  branch and defers as `MODEL_DOWNLOAD_FAILED: "require is not defined"`; a *successful* fresh
  download throws out of `writeSentinel` outside the try/catch and exits untyped. `sha256: null`
  works around both by skipping the branch entirely. `defaultSatisfyDeps` has no test coverage,
  which is why the suite stayed green. See `docs/flows/run-graph-manifest-satisfaction.md`.
- **`VENV_INCOHERENT` diagnosability** — host-pin drift and an ordinary dependency conflict share
  one defer detail; the drift diagnostic reaches stderr only.
- **`.git` mask asymmetry** — the read-only `.git` protection for the shell path is Linux-only; the
  native-Windows `run` path has no equivalent. The broker's `.git` denial still covers file tools on
  every platform. See ADR 0004.

## [0.22.0] - 2026-07-17

### Added — fleet-node server (`fleet-serve` / `fleet-measure`)
- **`fleet-serve`**: this box can now join the Fleet Dispatcher fleet (CONTRACT.md v2) —
  `GET /fleet/health` (live GiB VRAM, derived task/family lists, measured footprints, queue
  depth), `POST /fleet/dispatch` (immediate 202 ack with exact `job_id` echo; idempotent on
  duplicates; async execution through the existing pipeline + single-slot GPU lock), and
  `GET /fleet/jobs/{id}` (accepted→running→done|error; terminal results retained ~1h).
  New `internal/fleetnode` package: contract-exact HTTP server, drain-safe ack-then-poll
  job store, task-type mapping with strict raw-JSON run-graph payload validation at ack
  time, and a two-source VRAM sampler (nvidia-smi global snapshot every 2s; Windows PDH
  `\GPU Process Memory` per-process-tree source for footprints). Startup GPU probe refuses
  to stand up a zero-VRAM node; SIGINT drains for 30s and marks survivors
  `error:"interrupted"`. Loopback by default via the shared `internal/netguard` guard
  (extracted from local-agent, behavior identical); production binds the Tailscale address
  behind `--listen-trusted-network` on port 18811.
- **Passive measured footprints**: every image/video/audio/run-graph render through the
  pipeline now records its observed VRAM peak into `~/.local-offload/footprints.json`
  (max-keep; advertised `vram_peak_gb` = observed × 1.2), keyed by this machine's actual
  bindings — so footprints accumulate during normal harness use and stay current when
  bindings change. Implemented as a nil-gated `gpugen.Spec` sampling hook: the non-fleet
  render path is byte-identical when unset. **`fleet-measure`** primes an empty store (one
  minimal render per configured task) and prints the recorded entries.
- Config: `fleet_listen` (default `127.0.0.1:18811`), `fleet_node_id` (default hostname at
  serve time), `fleet_sampler` (`auto|pdh|global`).
- Docs: new `docs/FLEET-NODE.md` (config, Tailscale binding guidance, sampler modes, the
  PDH-vs-Afterburner validation procedure, and the recommended — never required — MSI
  Afterburner companion setup) + README/OPERATOR-GUIDE/SETUP-AGENT fleet sections.

## [0.21.1] - 2026-07-17

### Added — auto-text inpaint chain enabled (grounding eval passed)
- `inpaint-image --auto-text` now runs by default: the Task-9 grounding eval passed 3/3
  (text-stamped renders; qwen3vl found, boxed, and erased the text with zero wrong-region
  repaints; oversized images defer cleanly on the vqa load limit). The always-defer gate
  was removed per its recorded unlock condition.

### Fixed — run-graph pack satisfier drives uv directly
- The installed ComfyUI-Manager cm-cli has no `--uv` flag (live scene-swap satisfy run
  deferred VENV_INCOHERENT, typed defer + host torch untouched — exactly as designed).
  The satisfier now checks out packs first (git), then runs ONE `uv pip compile` across
  all packs on-disk requirements under the host-torch constraints and installs the lock.
  `uv` in the ComfyUI venv is the required satisfier tool (install.ps1 provisions it);
  cm-cli is no longer load-bearing for run-graph.

## [0.21.0] - 2026-07-17

### Added — edit-image op pack (deterministic post-production suite)
- **`grade`** `{levels{black,white,gamma}?, curve{points}?, wb{gray_world|scale}?, luminance_only?}`:
  tone/color grading with compose-once LUT discipline — every transform composes into ONE
  256-entry float LUT per channel and quantizes in a SINGLE `Image.point()` call (chained
  8-bit passes band visibly); the alpha band is never remapped.
- **`lut_cube`** `{path, strength?}`: `.cube` 3D LUT looks via Pillow's built-in `Color3DLUT`
  (vendored pure-python parser; 1D cubes and non-standard domains rejected); `strength` 0–1
  blends graded over original.
- **`perspective_composite`** `{overlay, quad:[[x,y]×4]}`: mockup placement — pure-python
  homography (partial-pivot Gauss, no numpy) warps the overlay into the destination quad
  (UL,UR,LR,LL winding), BICUBIC, alpha-composited.
- **`finish`** `{sharpen{radius,percent,threshold}?, median 3|5?}`: delivery sharpening with
  post-AI-upscale web defaults (1.2/80/3 — Pillow's 150% default over-crisps upscaler
  output). MUST run as the LAST op, after any resize (resampling undoes earlier
  sharpening). Real NLM/BM3D-class denoise is documented as out of PIL's scope, not faked.
- **`renditions`** (Go-side export matrix): optional `renditions[]`/`--renditions`
  `[{width/height, format png|jpg|webp, suffix}]` — after the master `out`, each entry
  re-runs the worker (resize+convert) writing `<out-stem><suffix>.<format>`; result gains
  `renditions[]`.
- **`instantiate_design`** `{set_text{Layer: copy}, replace_image{Layer: path}}` (FIRST op
  only, like `flatten_design`): GIMP layered-template factory — generated Script-Fu sets
  named text layers' copy, swaps named pixel layers at the old offsets, flattens, and feeds
  the raster to the PIL pipeline (one-call brand-variant factory). PDB calls verified
  against the installed gimp-console-3.2 (`gimp-drawable-get-offsets` 3.x naming); headless
  GIMP invocations are now serialized process-wide (profile-lock contention), and a
  no-raster script failure surfaces GIMP's stderr (layer-name mismatch is THE common case).
- Docs: README op table/CLI examples, OPERATOR-GUIDE "Deterministic post-production"
  section, `render/gimp-instantiate.scm.tmpl` (reviewable batch contract), MCP
  `offload_edit_image` description enumerates the pack + both ordering rules
  (instantiate_design first; finish last).
- Existing five ops, mask_boxes, flatten_design, and all generate/batch/run-graph/inpaint
  paths are unchanged (locked by the full suites).

## [0.20.0] - 2026-07-17

### Added — generative inpainting route (`offload_inpaint_image` / `inpaint-image`)
- SDXL-family **masked re-denoise** on the local ComfyUI (core nodes only:
  `LoadImage → ImageToMask(red) → VAEEncodeForInpaint → KSampler → VAEDecode`): re-renders
  ONLY the white region of a same-size white-on-black mask from a prompt, leaving every
  other pixel untouched. New `render/wf-sdxl-inpaint.mjs` graph builder +
  `render/comfy-inpaint.mjs` runner (staged inputs, single-slot GPU lock, zero-always-warm
  teardown), `imagegen.Inpaint` exec wrapper, `inpaint_image` pipeline task, MCP tool
  `offload_inpaint_image` `{image,mask,prompt,negative?,denoise?,grow_mask?,steps?,seed?,out?}`
  → `{image_path, seed}`, and the `inpaint-image` CLI.
- Per-machine binding via new config `inpaint_script` / `inpaint_ckpt` / `inpaint_vae` /
  `inpaint_steps` / `inpaint_cfg` / `inpaint_sampler` / `inpaint_scheduler` /
  `inpaint_timeout_sec` (default 900). The binding must be **SDXL-class** (e.g. RealVisXL):
  `VAEEncodeForInpaint` is latent-space — a pixel-space DiT (HiDream) cannot drive it, so a
  HiDream box keeps HiDream for generation and binds inpaint separately. Unbound = clean defer.
- New deterministic `mask_boxes` edit op (`edit_image` pipeline):
  `{op:"mask_boxes",boxes:[{x,y,width,height}],pad?,feather?,invert?}` replaces the working
  image with a white-on-black inpaint mask at its size — the manual mask workflow.
- EXPERIMENTAL `inpaint-image --auto-text`: vision-detected text boxes chained through
  `mask_boxes` into the inpaint; hard validation (unparseable/empty/absurd >60%-coverage
  boxes) defers with the manual `mask_boxes` workflow named. Grounding acceptance on real
  gibberish renders is still pending live eval — treat as opt-in sugar.
- Note: diffusion cannot WRITE specific legible text — inpaint-to-clean, then add real
  type with the `edit_image` `text` op.

## [0.19.0] - 2026-07-17

### Added — warm batch mode (`generate-image --batch`)
- N prompts through ONE warm ComfyUI session (checkpoint loads once): `generate-image
  --batch <jobs.jsonl>` with per-job `{prompt, negative?, width?, height?, steps?, seed?,
  out?}` overrides and a per-job result report `{count,succeeded,failed,items[]}`.
  Measured on the 16GB box: 32.6s first job (absorbs the checkpoint load) → **22.4s warm
  floor**; the old zero-warm cadence re-paid the load on every render. **Zero-warm stays
  the single-render default** — warmth exists only inside an explicit batch, and the full
  teardown (freeComfy + kill + lock release) is restored at the batch boundary, verified
  live (ComfyUI down, VRAM idle, memory stack intact, lock released). GPU lock held across
  the whole batch; per-job ledger records with ErrClass parity.
- Operational: ComfyUI-Manager on a satisfier box should run `network_mode = offline`
  (its first-start registry fetch regressed cold-start ~40s→>150s; offline verified 19s).

## [0.18.0] - 2026-07-17

### Added — run-graph primitive (`offload_run_graph` / `run-graph`)
- Generic execution of an **arbitrary ComfyUI API-format graph** in the proven GPU-lock /
  zero-always-warm lifecycle, with a per-workflow **node manifest** (custom node packs @
  pinned commits + model files with optional sha256) satisfied as part of the contract:
  unified `uv` dependency resolve via cm-cli (never sequential per-pack pip), `pip check`
  coherence gate, models downloaded + sha-verified (null-sha → reported in
  `unverified_models[]`), packs **provisioned to disk BEFORE ComfyUI starts** so they load
  on first start. Node-addressed outputs `{node_id:[{path,type,kind,width,height}]}` +
  `image_path` alias; every failure a **typed DEFER** `{deferred,code,ref,detail}` — never
  cloud. New config: `run_graph_script`. Spec:
  `docs/superpowers/specs/2026-07-17-run-graph-primitive-design.md`.
- Setup: `Ensure-RunGraphDeps` provisions ComfyUI-Manager (cm-cli, required) + GitPython
  (required by cm-cli itself) + comfy-cli (optional, best-effort).

### Fixed / hardened
- **Host-constraints (v1 protection):** every pip/uv the satisfier spawns runs under a
  constraints file pinning the host's `torch/torchvision/torchaudio/numpy`
  (`PIP_CONSTRAINT`/`UV_CONSTRAINT`), plus a post-install drift tripwire → a pack set that
  cannot install additively around the CUDA stack defers `VENV_INCOHERENT` instead of
  silently replacing ComfyUI's torch (live finding: the scene-swap lock resolved
  torch 2.11.0+cu128 → 2.13.0, which would have broken the Wan video path).
- Empty/models-only manifests skip the pack satisfier entirely (no cm-cli invocation).
- git arg-injection hardening in pack provisioning (`--` clone guard + commit-ref charset).

## [0.17.0] - 2026-07-16

### Added
- **`offload_edit_image`** — deterministic image-edit PIPELINE in one call (ops applied
  in order: crop / resize / convert / composite / text via a PIL worker on the ComfyUI
  venv python, auto-derived; `flatten_design` as the first op opens `.xcf`/`.psd` via
  GIMP, flattens, and returns the layer list — script-fu template live-verified on
  gimp-console 3.2, no raw script pass-through ever). New config: `edit_python`,
  `gimp_console_path`, `edit_timeout_sec`.
- **`offload_media`** — one ffmpeg av operation per call: `trim` (keyframe-snapped
  stream-copy default, `reencode` for exact cuts), `concat`, `extract_frames` (fps or
  count-via-probe), `convert`, `mux_audio`, `probe` (parses `ffmpeg -i` stderr —
  imageio_ffmpeg ships no ffprobe; fixture-tested against the real 7.1 banner).
- Both tools are **CPU-only and never take the GPU lock** — they run in parallel with
  renders and never evict llama-swap. Engines surface in `offload_status.media`
  (`edit_pil` / `edit_gimp` / `media_ffmpeg`); every failure class defers cleanly.
  MCP + CLI (`edit-image`, `media`); NOT registered in the read-only agent loop.
  Spec: `docs/superpowers/specs/2026-07-16-edit-media-tools-design.md`.

## [0.16.0] - 2026-07-16

### Added — quality-first generation (root-cause fix, all hardware tiers)
_Directive (operator, 2026-07-16): highest-quality deliverables always, on all hardware; speed variants
opt-in only. Spec + measured evidence:
`docs/superpowers/specs/2026-07-16-quality-first-generation-design.md`._
- **HiDream-O1 official graph** (`render/wf-hidream-o1.mjs`, selected via new
  `imagegen_family` config): ModelNoiseScale, patch-seam smoothing (kills the measured
  3× 32px patch blocking of the generic-graph path), the SamplerCustom stack, native
  2048 resolution, base (40/5/SDE) + dev (28/1/LCM) variants. Generic-graph machines
  byte-for-byte unchanged when unset.
- **Per-machine Wan weight binding** (`videogen_unet_high/low`, `videogen_text_encoder`):
  extension-keyed GGUF/safetensors DisTorch2 loaders, never down-cast — unblocks Q8_0/
  fp16 weights over the Q4 defaults.
- **Video native recipe is now the DEFAULT** (no distill LoRA, 20 steps, cfg 3.5, the
  official Wan training negative); `fast:true` opts into the improved 8-step asymmetric
  lightx2v distill (HIGH LoRA 0.7 + cfg 3, LOW 1.0 + cfg 1). `hero` = deprecated no-op.
- **Quality-first `config_seed` on every ≥16GB CUDA tier**: bf16 O1 Base + family graph +
  Q8_0 Wan experts + umt5 fp16 + 720p×81 (proven on the 16GB tier: 3.9 min/2048 render).
- `comfy-render.mjs` poll ceiling now `COMFY_WAIT_SEC`-driven (the hardcoded ~6-min
  ceiling would kill legitimate quality renders); Go aligns it to `imagegen_timeout_sec`.

### Fixed
- **LO-19: `offload_generate_video` advertised `hero`/`upscale` but never mapped them** —
  MCP callers requesting the quality pass silently got the 4-step draft. `fast`/`hero`/
  `upscale` now flow through.

## [0.15.0] - 2026-07-16

### Added
- **Blackwell profile tiers above 16GB (configs #13–15).**
  `detect.ps1` now classifies RTX PRO Blackwell workstation cards (their names — e.g.
  "NVIDIA RTX PRO 5000 Blackwell" — matched NO arch rule and fell into the ampere bands)
  and bands Blackwell VRAM above 16GB: `blackwell-32` (≥24GB), `blackwell-48` (≥40GB),
  `blackwell-72` (≥64GB; 96GB cards share it until measured). The new tiers render a new
  `cuda-resident` template: every model standalone, **no swap group, no ttl** — the whole
  roster stays hot concurrently (zero swap latency). 48/72 serve 128K ctx with
  **full-precision f16 KV**; 32 serves 64K q8_0. All values PROJECTED until an H3-style
  selftest runs on real ≥24GB hardware. Spec:
  `docs/superpowers/specs/2026-07-16-blackwell-profile-tiers-design.md`.
- **Profile `config_seed` (seed-on-create media defaults).** A profile may carry
  `config_seed` in profiles.json; install Step 8 overlays it onto the template config
  ONLY when creating `~/.local-offload/config.json` fresh (an existing per-machine
  config is never touched). The big tiers seed 720p-class video defaults.
- **`offload_status` MCP capability-discovery tool (LO-18).** Fixes the NIM-vs-local
  asymmetry: `offload_nim` was the only tool that named or listed models, so agents
  inspecting the harness concluded the text/LLM capability was the cloud NIM catalog and
  never discovered the LOCAL cascade. `offload_status` (registered first) reports the
  configured local roster, live-probes the endpoint's `/v1/models`, lists the machine's
  media engines, and names NIM as the only remote surface; local tool descriptions
  de-anonymized ("the LOCAL model cascade" instead of "a free local model").

## [0.14.0] - 2026-07-15

### Added
- **H4: flexible CUDA-keyed llama.cpp build selection (workstation/Blackwell).**
  `setup/install.ps1` now picks the CUDA build from the *detected* CUDA (never a fixed
  assumption): Blackwell (sm_120) profiles on a CUDA-13 driver install a new pinned,
  SHA-verified **cuda-13.3** prebuilt (`llama-cuda13`/`llama-cudart13`) — the SERVE tier
  (MMQ→cuBLAS fallback, ~5.6× slower prefill; peak = documented source-build vs a
  12.8/12.9 toolkit, noted when one is detected). Blackwell on a 12.x/undetected driver
  refuses loudly with driver-upgrade-or-source-build guidance; `dual-gpu` refuses with the
  multi-arch recipe (`-DCMAKE_CUDA_ARCHITECTURES="70;120"`, 12.8/12.9 toolkit — CUDA 13
  cannot compile sm_70); all other CUDA profiles keep the verified 12.4 prebuilt.
  `installed.json` records `cuda_build` under the selected component key so a driver
  upgrade or the V100 arriving forces the correct re-install on re-run. New synthetic-box
  overrides: `OFFLOAD_CUDA_DRIVER` / `OFFLOAD_CUDA_TOOLKIT`.
- **Blackwell runtime env auto-injection.** `blackwell-*` renders now carry
  `CUDA_VISIBLE_DEVICES=0` + `CUDA_MODULE_LOADING=LAZY` on every model block of the
  rendered `llama-swap.yaml` (idempotent injection; the 26B's `GGML_CUDA_DISABLE_GRAPHS`
  list is extended in place).
- **Tests:** `setup/tests/install-cuda-build.test.ps1` (dot-source seam
  `OFFLOAD_INSTALL_DOT_SOURCE=1`; pwsh 7 + PS 5.1) + Blackwell env assertions in
  `setup/render.tests.ps1`.

### Fixed
- **detect.ps1 missed the driver CUDA on newer drivers.** Drivers like 610.62 print
  `CUDA UMD Version: 13.3` instead of the classic `CUDA Version:` banner; the parse is now
  a self-tested pure function covering both formats. (Found live on the workstation —
  `cuda_driver` reported null on the exact box H4 keys its selection off.)

## [0.13.0] - 2026-07-15

### Added
- **Config-selectable voice paths (wiring).** `generate_audio kind=voice` now takes a
  `voice` selector (`generalist` | `finetuned`). The generalist path is the stock
  Chatterbox multilingual worker (a new `voicegen_ref` supplies a default es-MX reference
  clip). The `finetuned` path is a per-machine voice located entirely by config
  (`voicegen_ft_model` / `voicegen_ft_base_dir` / `voicegen_ft_ref` / `voicegen_ft_lang` +
  the `voicegen_ft_{temperature,cfg_weight,exaggeration,repetition_penalty}` recipe knobs);
  empty config → clean defer, so shared code carries no model name or personal path.
  `render/tts.mjs` branches on `--engine` to dispatch to the stock `tts_chatterbox.py` or the
  new fine-tuned worker `tts_chatterbox_ft.py`, exposed on the MCP tool + CLI as `voice`.
- **Fine-tuned worker skeleton** `render/tts_chatterbox_ft.py` — arg contract + path
  validation; defers (exit 3) until its vendored-engine load path is built + tuned in a
  later session. See `docs/superpowers/specs/2026-07-15-config-selectable-voice-wiring-design.md`.

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
  Keeps the harness hardware-agnostic — resolution is config, not a constant. The 16GB box is set to 1280×720.

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
