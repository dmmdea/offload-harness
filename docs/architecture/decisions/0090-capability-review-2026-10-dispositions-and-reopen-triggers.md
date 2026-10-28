---
status: Proposed
date: "2026-10-14"
---

# 0090 — Capability review 2026-10: dispositions and reopen triggers

> Status: Superseded by ADR 0099 for the pinning mechanism (2026-10-28): an INSTALLED disposition is no longer "pinned to an exact version": software is installed from its publisher's current release with the integrity the publisher gives, checked at install time; the version and hash named for OpenSpec are a record of the admission, not a pin. The rest of this decision stands, and the text below is kept as written.

## Context

In October 2026 the operator named eight outside links for "mandatory installation or adoption", and a research pass
read each of them together with the neighbours they pulled in (a kernel library, a web-reach CLI, an inference engine,
a set of model candidates). Only some of them can be installed. Several are papers with no code, several carry a
licence an Apache-2.0 repository cannot take, and several assume a daemon, a container runtime or a class of
accelerator this harness does not have. Without a written record every one of them is researched again the next time
someone meets the link, and "mandatory" reads as "unfinished".

This ADR is the record. Each of the eight links ends in exactly one terminal state, and everything else the review
looked at and did not take is listed with its reason and the event that would make it worth reading again. It adds no
code and installs nothing. Two decisions it leans on were written alongside it: [ADR 0088](0088-one-measurement-gate.md)
(the one measurement gate: superiority and non-inferiority modes over a pre-registered, frozen baseline) and
[ADR 0089](0089-strata-is-an-opt-in-agent-seat-backend-that-earns-its-place-through-the-gate.md) (the Strata engine ruling). A third dependency, the cascade
re-evaluation's bake checklist, is not yet a repository artifact: its line is carried in the program's bake plan and
will be committed with the bake work.

**Terminal states.** Only these five exist.

| State | Meaning |
|---|---|
| INSTALLED | pinned to an exact version, admitted under the SEC-L8 checklist in [security](../../systems/security.md), telemetry off, state kept off the system drive, uninstall rehearsed |
| PORTED | the concept is built in our own code (clean-room where the licence requires it) |
| REPLICATED | rebuilt from a paper |
| DECLINED | not taken; the reason and the reopen trigger are recorded |
| PARKED | not pursued now; the reason and the reopen trigger are recorded |

A row may carry a conditional pair ("REPLICATED if X, else DECLINED"); the row then names the test that decides it and
the reopen trigger of the losing branch. A row that depends on a measurement not yet made reads **pending** and names
what decides it. A terminal state is the state a row is held to: INSTALLED or PORTED here means the work is scheduled
to reach it, and a row counts as reached only when its change has merged. A reopen trigger makes a row eligible for a
fresh look; it adopts nothing by itself.

Facts about outside projects were read from public pages on 2026-10-13 and not re-checked afterwards. Where the review
could not settle a fact, it is marked unverified, in the form the [style guide](../../STYLE.md) prescribes.

## Decision

### 1. The closure ledger: the operator's eight links

| Link | Terminal state | Reason | Reopen trigger |
|---|---|---|---|
| OpenSpec — https://github.com/Fission-AI/OpenSpec | **INSTALLED**: the CLI (`@fission-ai/openspec`) is admitted and installed, pinned to release 1.14.1 (tarball sha256 `4a88e334938316db6916fd4f3aaf2213e6fc23b4fe327efd2afe7754c2d3b0bf`), with telemetry off and its npm prefix and global configuration on a data drive. The admitted-software table in [security](../../systems/security.md) carries its row and the leak gate's pinning of its generated files is merged; the `openspec/` tree itself lands in a sibling change. | MIT, no daemon, project-scoped, runs on the Node runtime already present. Telemetry is on by default (an anonymous POST). The documented opt-outs include `OPENSPEC_TELEMETRY=0` and `DO_NOT_TRACK=1`, and a separate switch, `OPENSPEC_NO_UPDATE_CHECK=1`, skips the registry version check; the install and the registration environment set all three. Admission rests on verified registry hashes and signed build provenance (SEC-L8 checks 1 to 10, no finding); no malware-scanner record exists for it. | Not applicable while installed. A scanner detection on a later lookup, or a release that changes what it sends or where it writes, reopens the admission (each new pin is its own admission). |
| REA (npm package `rea-agents`) — https://github.com/morluto/rea | **INSTALLED** on the Claude Code side only: exact pin, local scope, one scratch project, capture tools denied by a hook unless a disposable-environment marker is set. Its concepts are **PORTED** here (a compact tool-schema profile; authority-graded acceptance). **DECLINED** if its home directory cannot be relocated off the system drive, telemetry cannot be shown off, or admission fails. | MIT, MCP over stdio spawned on demand, no model. Four breaking releases in two and a half days, a lookalike organisation reported distributing a trojan executable, and the repository lists an optional dependency the published npm manifest does not: so the install is the exact npm pin whose repository field is confirmed, never the vendor's own setup command. | If declined: a documented way to relocate its home directory, or a documented telemetry switch. |
| Comfy-Org workflow templates — https://github.com/Comfy-Org/workflow_templates | **INSTALLED** (the data layer, shipped in 0.150.0: [catalog](../../../render/templates-catalog.mjs), [licence map](../../../render/templates-license-map.json) and run gate) plus **PORTED** (a converter from UI-format templates to API graphs, the run route, a builder drift guard). The route half is **DECLINED** if the approval that adds a third, conditional licence class under [ADR 0058](0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md) is refused. | MIT, and ComfyUI pins the template wheels on every node. Running a template needs a UI-to-API converter that upstream does not ship, and a template whose model licence is unlisted must Defer (unknown is not permissive). | The conditional-licence approval is given, or upstream ships a converter. |
| Headroom — https://github.com/headroomlabs-ai/headroom | **pending per-rung gate verdicts.** Offload side: each compaction rung ported from its documented behaviour is **PORTED** if it passes ADR 0088's non-inferiority mode (accuracy on the frozen replay corpus, forwarded tokens as the efficiency metric) and **DECLINED** (measured, no gain) if it does not. Claude Code side: **DECLINED**, see the Parked table. | Apache-2.0 and free (only an Enterprise tier is paid). The product is a Python/torch proxy with a CPU model and a default-on beacon, so only its ideas travel: deterministic rungs, default off, idempotent and monotonic ([ADR 0016](0016-phase-c-dedupe-pinning-force-preserve.md)), prefix-stable ([ADR 0017](0017-kv-reuse-is-binary-and-how-we-measure-it.md)), built from documented behaviour with a NOTICE entry. Its compressor weights are never used or ported: about 4.78% of the training data is non-commercial. | For a declined rung: a different corpus or a new compressor idea, run through the same gate. |
| The CLM paper (arXiv 2609.37725) — https://arxiv.org/abs/2609.37725 | **pending the CLM measurement and the edit-tool decision.** **PORTED** means: the measurement first (pager re-fetch rate, compaction-fire rate and prefill on recorded 27B-seat runs, against the pager's own 10% re-fetch gate in [contextpager.go](../../../internal/agent/contextpager.go)), a benchmark adapter, and a typed `edit_context` tool only if those numbers and ADR 0088's non-inferiority mode justify it. Otherwise **DECLINED**, with the measured rates recorded. | CC BY 4.0 paper, so the ideas are free to rebuild. The gains were measured at 32K-token budgets, long horizons and a 27B model; our seats have 49K to 163K windows and short runs, so measure before building. | If declined: a later corpus on which the re-fetch rate clears 10%, or a 27B-class seat on which the ladder fires often. |
| The CLM repository — https://github.com/facebookresearch/context-language-models | **DECLINED**: licence, engine, hardware. Clean-room provenance rules are recorded below. | CC BY-NC 4.0 cannot enter an Apache-2.0 repository. Its serving engine (suffix cache reuse) runs only on SGLang 0.5.16 with one Qwen 27B hybrid and a side buffer of about 22 GiB; its RL recipe needs 64 H200. | See the Parked table (CLM repository and its engine). |
| multi-harness-rl — https://huggingface.co/spaces/FineEnvs/multi-harness-rl | **PORTED**: a seat-by-loop matrix, an acceptance-hygiene ADR, shared vocabulary. **DECLINED**: installing OpenEnv and Harbor. | The Space is a CC BY 4.0 article. The stack it documents needs Python 3.12 exactly, Docker and H100-class accelerators, and trainable capture only on vLLM or SGLang. | See the Parked table (OpenEnv and Harbor). |
| GitSwarm (arXiv 2610.04862) — https://arxiv.org/abs/2610.04862 | **pending the swarm pilot.** **REPLICATED** if the contract-mode swarm pilot beats independent best-of-N at equal episode budget on a Go-test objective through ADR 0088's superiority mode and the Claude Code skill pilot reports; otherwise **DECLINED** (replicated, measured, no gain). | Paper only: no code, no release. The numbers rest on frontier closed models at 100 to 500 episodes. The mechanism is small and cheap to rebuild: a commit graph with a mandatory `informed_by` trailer, worktree-isolated episodes, contribute / nominate / abstain endings and an announce log. | A code release by the paper's authors. |

The three pending rows (Headroom rungs, the CLM paper, GitSwarm) are settled by one amendment after the measurements
and the gate verdicts exist. Section 4 is that amendment (2026-10-18); none of the three reads as adopted.

**Clean-room provenance rules.** They apply to every row above or below whose upstream licence this repository cannot
take, and to the CLM edit tool in particular.

1. Code under a licence Apache-2.0 cannot take (CC BY-NC, AGPL) is not copied, adapted or vendored. The change that
   implements a port does not consult that code either: behaviour is taken from the paper, the documentation and this
   repository's own design. This is a rule for the implementing change; the review itself summarised three Odysseus
   source files as design reference, as that row states.
2. A change that follows such a project records what it was built from. The CLM edit tool also records a normalised
   n-gram overlap check of the new code against the non-commercial repository.
3. MIT community ports of non-commercial work are references to read, never code to vendor.
4. Where the upstream licence is permissive (Headroom), the behaviour is still rebuilt from its documentation and gets
   a NOTICE entry.
5. Weights from a project whose training data includes a non-commercial slice are never used or ported.

### 2. The Parked table

Every row has a reason and a reopen trigger. A source link is the project's public page.

| Item | Why it is not taken | Reopen trigger |
|---|---|---|
| DeepGEMM — https://github.com/deepseek-ai/DeepGEMM | See section 3. | A Blackwell sm_120 tier adopts an FP8-block or FP8xFP4/NVFP4 vLLM checkpoint as a seat. |
| Headroom as a resident Claude Code proxy (**DECLINED**) — https://docs.headroomlabs.ai/docs/security-model | A resident man-in-the-middle process, so a listener that never exits; it handles every request and credential in plaintext; subscription login through a base-URL proxy is unverified, and an API-key-only route would add metered spend; its `wrap` command edits the client's settings file; its beacon and update check are on by default. | A subscription login is verified through the proxy in a disposable profile, and the operator then approves a foreground, non-resident trial. |
| The CLM repository and its SCR engine — https://github.com/facebookresearch/context-language-models | CC BY-NC 4.0 cannot enter an Apache-2.0 repository. SCR (suffix cache reuse) is SGLang-only, one model family, tensor-parallel 1, with a side buffer that does not fit a 16 GB card. The RL recipe needs 64 H200 and a paid judge. It had nine commits on `main` when read. | The code is released under a licence Apache-2.0 can take, or an engine this harness already serves gains the same partial-prefix reuse. The RL recipe stays out until a node class of that scale exists. |
| OpenEnv and Harbor — https://github.com/huggingface/OpenEnv , https://github.com/harbor-framework/harbor | They need Python 3.12 exactly (3.10 and 3.11 do not install the Harbor dependencies), a container runtime (a Docker daemon is a new daemon) and H100-class accelerators for the training they document. Trainable capture works only on vLLM or SGLang, not on llama.cpp seats. Both move fast (OpenEnv shipped four releases in three weeks). | A training or trainable-capture workload is wanted, a node class meets the Python, container and accelerator requirements, and the container runtime is approved. A plain evaluation run does not need either project. |
| The SoL-Pi loop — https://github.com/NVlabs/SoL-Pi | The auto-research loop and its skill template are not in the repository (the whole tree was checked); what ships is a set of TypeScript extensions pinned to one release of another agent. The repository's own agent instruction files direct a global package install, so it is read by fetch only and never cloned into an agent working tree. Ported instead: the evidence-preserving reducer rung and the gate method (pre-declared tolerance, an efficiency metric, a held-out slice that never feeds back). A Claude Code advisory reducer hook is also parked: it needs a post-tool-use timeout above 10 s and a hook that starts a seat load. | The loop itself is published in the repository (then it is re-read for replication). For the hook: Claude Code allows the longer timeout and a hook may start a seat load under the lease. |
| Dream-RSI (arXiv 2609.14858) — https://github.com/zhengkid/Dream-RSI | No code and no licence file, so all rights are reserved: ideas only. The code is described as "being prepared". Its finding that history works better as an evaluator than as prompt text rests on one task (ConvDiv), so it is a hypothesis. Ported: the incumbent is always a candidate (ADR 0088), replay-scored candidates, history used as the evaluator rather than as prompt text. | Code and a licence ship; the parts of the paper the review could not read (the policy prompts in appendix D.2, the tail of the HTML version and the repository's PDF) are then read before anything leans on it, and the row is re-evaluated for adoption or replication. |
| Odysseus — https://github.com/odysseus-dev/odysseus | AGPL-3.0-or-later against our Apache-2.0, and a general self-hosted AI workspace with an always-on container stack and an unsandboxed shell agent. Clean-room concepts only: the research pass summarised three of its source files (skill extraction, skill lifecycle and the skill-audit route) to decide this disposition, nothing was copied, and any port is built clean-room from the documented idea. Ported: the skill audit with a without-skill baseline arm, demote not delete. | The project moves to a licence Apache-2.0 can take. |
| Ouroboros (arXiv 2608.08311) — https://github.com/razzant/ouroboros | MIT, but a Python daemon stack with a self-modifying core and a background loop; its local lane defaults to CPU layers (no model runs on the CPU here) and it needs paid provider keys. Its benchmark figures are self-reported. Ported: a failure-class register, review evidence bound to a SHA with an explicit NOT_RUN outcome, deterministic gates before any model review, and "an advisory finding always leaves a durable trace". | A stable OpenAI-compatible route from it to our seats is confirmed and a sandboxed trial is approved. |
| The GitSwarm code release — https://arxiv.org/abs/2610.04862 | No code exists. The replication in the ledger stands in for it. | The authors release code under a licence Apache-2.0 can take; it is then compared with our replication before anything is adopted. |
| Agent-Reach, concepts only — https://github.com/Panniantong/Agent-Reach | Agent-Reach itself is not installed. It hardcodes third-party backends for web reading, search and transcription, so URLs, queries and audio leave the machine (against [ADR 0001](0001-defer-never-cloud-fallback.md)); it installs from a mutable `main` archive plus global npm, pipx, apt and brew artifacts (against SEC-L8: a tag and a sha256, never `curl \| sh`); its `watch` and `check-update` verbs are schedulers and phone-home calls; its URL check is lexical only, with no DNS or redirect validation; and 325 of about 380 commits are one author's. Kept as our own work: a per-lane `active_backend` report in the doctor command, and text-only source adapters for YouTube subtitles and RSS. For the subtitle adapter, yt-dlp 2026.08.19 (the pure-Python zipapp, sha256 `1fa6733c37ea6fb51c99ad8fe785e7b7e5f3246c9b980230329d4fb72ed8d4d6`, scanner result 0 of 62) was admitted. | Upstream makes the backends configurable, pins its installs by tag and hash, drops the scheduler verbs and validates URLs after DNS resolution. All four, not one. |
| A local Claude Code launcher and cache-prefix pinning (parked low) — https://github.com/Niko1221/Strata | A `claude-local` launcher profile that points Claude Code at a local seat's Anthropic-compatible endpoint, and the Strata engine's pinning of Claude Code's per-request billing-header stamps (`cch` and `cc_version`) so the prompt prefix stays identical each turn, an idea that would be adapted to llama-swap seats. Never for the main session, and the behaviour of `count_tokens` and `cache_control` through it is unverified. | The operator asks for an offline worker profile. |
| Ternary-Bonsai-2-27B — https://huggingface.co/prism-ml/Ternary-Bonsai-2-27B-gguf | Its ternary types need a second llama.cpp fork: stock llama.cpp rejects the files or loads one type as garbage. [ADR 0081](0081-the-embeddinggemma2-entry-may-run-from-a-second-llama-build.md) is the precedent for a second build, but each build is its own admission. The vendor's figures put it at 98.2% of the full-precision score on average, with the gap in the knowledge and reasoning categories (79.86 against 85.55), reasoning defaults to `xhigh` and `low` is unsupported, and the fork builds for CUDA, Metal and CPU only. Unmeasured on this fleet. | The ternary types land in the llama.cpp build the fleet already runs, or a tier's bake needs a 27B-class checkpoint under about 7 GB and a second-build slot is admitted for the fork. |
| NVFP4 beyond the DeepGEMM trigger — https://huggingface.co/nvidia/Qwen3.8-27B-NVFP4 | The vendor reports GPQA 88.01 against 88.92 for BF16 and IFBench 78.93 against 80.07, tested on GB300 parts. No NVFP4 vLLM seat is built or baked from this ADR; the question rides two checklists (section 3). | A bake checklist answers that the checkpoint runs on sm_120 and fits the tier's VRAM by arithmetic; it is then baked through ADR 0088's gate, and the DeepGEMM trigger fires with it. |
| GLM-OCR and LightOnOCR-3 — https://huggingface.co/zai-org/GLM-OCR , https://huggingface.co/lightonai/LightOnOCR-3-4B | Neither states a llama.cpp path. GLM-OCR is MIT, about 1.3B parameters, 94.62 on one vendor document benchmark but transcription only (its layout score on another is 0). LightOnOCR-3-4B is Apache-2.0, 86.3 on one vendor benchmark, and 9.1 GB in BF16, so it needs a quant. The incumbent OCR seat, Qwen3-VL-8B, has a measured DocVQA ANLS of 0.965 (the 32B scored 0.928 and lost the OCR role on it; see the measured notes in [profiles.json](../../../setup/templates/profiles.json)). | A GGUF with its multimodal projector (or another path a tier already serves) is published and verified to load; each is then baked against the incumbent through ADR 0088's gate. |
| Cohere Transcribe — https://huggingface.co/CohereLabs/cohere-transcribe-03-2026 | Apache-2.0, 2B parameters, a vendor mean WER of 5.42, but no timestamps, no diarization and no language detection, and only transformers and vLLM serve it. whisper-large-v3-turbo (MIT, WER 7.83, with timestamps) stays the transcription seat. A candidate for `stt_model_hq` only. | It gains timestamps, or a serving path a node already runs, or a lane that wants a no-timestamp high-quality transcript is baked on `stt_model_hq`. |

> **Unverified:** the research read the second version of the Dream-RSI paper (sections 3 to 5 and appendices A and B.2)
> but not its policy prompts (appendix D.2 was truncated), the last 8,319 characters of the HTML, or the repository's PDF
> (unreadable through the fetch tool). Whether OpenEnv and Harbor run on Windows or WSL2 was not established. Whether the
> NVFP4 checkpoint runs on sm_120 at all is stated nowhere on its card. Every vendor benchmark figure above is the
> vendor's, not ours.

**Skipped, not parked.** These were decided against outright and carry no reopen trigger: Action Fusion; ObservationPack
and compaction as Claude Code hooks (a hook fires once, the output-rewrite field is lossy, and no hook can trigger
compaction); any install of SoL-Pi, Odysseus or Ouroboros; the proposed `optimize` command of Caliper (an upstream issue, not shipped; Caliper is a skill-evaluation CLI used on the
Claude Code side); the Ouroboros constitution and identity layer; the Ouroboros import-graph atlas.

### 3. DeepGEMM: parked, with a decision hook

DeepGEMM is a CUDA kernel library. It has no kernel for anything this fleet runs today, so it is PARKED, not a work
item. The only thing this section adds to the repository is a question that two other checklists carry: ADR 0089's NVFP4 arm,
and the cascade bake checklist once it is committed.

**Why it is parked** (as read on 2026-10-13):

- It has GEMM kernels for FP8-block, FP8xFP4 and BF16 operands only (plus grouped GEMMs for mixture-of-experts layers
  and attention-indexer kernels). Our vLLM seats run 3-bit, 2-bit and INT4 weight-only checkpoints; `fp8` in a seat's
  configuration is the KV-cache dtype, not a GEMM operand. Every other text, vision and speech-to-text seat runs on
  ggml's own kernels (llama.cpp for text and vision, whisper.cpp for speech-to-text), and a Strata seat (ADR 0089) is
  ggml too. The ComfyUI media lanes do load FP8 and MXFP8 checkpoints, but they run them through PyTorch and cuBLAS, not
  DeepGEMM, so they are outside the trigger. Before this ADR nothing in the repository mentioned DeepGEMM or installed
  it.
- Ampere is excluded: the minimum is SM90. That removes every ampere tier and volta-16. It is CUDA-only, so the AMD and
  RK3588 tiers are out as well.
- sm_120 (consumer Blackwell) is not on upstream `main`, which supports SM90 and SM100; the maintainers declined it
  there in an issue that is still open. It exists on an upstream dev branch (merged 2026-09-23) and in the vLLM
  project's own fork, which is where vLLM's install script builds from. Both are young. Open issues on 2026-10-13 named
  nondeterministic wrong output in one grouped FP8 kernel, wrong attention-indexer logits, and an illegal memory
  access at CUDA-graph replay; the fork's merge note says its full suite is not green. The qualified parts were an
  RTX 5090 and an RTX PRO 6000, not the 16 GB and 8 GB parts this fleet has.
- Mega MoE, the fused mixture-of-experts kernel, is SM100-only.

> **Unverified:** whether the vLLM builds the seats run would import a DeepGEMM that happened to be present, and
> whether a wheel built for another architecture fails loudly on an sm_120 card.

**Reopen trigger.** A Blackwell sm_120 tier adopts an FP8-block or FP8xFP4/NVFP4 vLLM checkpoint as a seat. In practice
that needs more than one 16 GB card (a 27B FP8 checkpoint is about 27 GB) or a mixture-of-experts model with few active
parameters. A ComfyUI media lane that loads an FP8 checkpoint does not fire it: those lanes are not vLLM seats.

**Cross-links.** The trigger is asked, not watched:

- The cascade re-evaluation's bake checklist will carry the question "does the chosen checkpoint use FP8-block or NVFP4
  on sm_120?". That line is carried in the program's bake plan, which is not yet a repository artifact, and will be
  committed with the bake work; until then this cross-link is pending. The candidate that raises it is
  `nvidia/Qwen3.8-27B-NVFP4`, whose sm_120 support its card does not state.
- ADR 0089's NVFP4 arm carries the same question.
- Strata's ggml NVFP4 path does not use DeepGEMM. A "yes" from the Strata arm therefore does not fire the trigger;
  only a vLLM seat does.

**If the trigger fires,** the whole integration is an experiment in the seat environment, in this order. The Go code
never imports DeepGEMM, vLLM does, so there is nothing to build in this repository first.

1. Preconditions. A CUDA 13.x `nvcc` is present for the seat environment (DeepGEMM compiles its kernels at run time):
   13.2 is the validated toolchain, and 13.3 crashes `ptxas` on one MXFP4 kernel. The vLLM version in use must pin a
   DeepGEMM revision that carries the sm_120 kernels; that is known for vLLM's main branch and unknown for older pins.
2. Build the vllm-project DeepGEMM wheel in a scratch copy of the seat environment on a data drive, never in the
   production one. The kernel cache must live where it survives the 5-minute idle unload, or every load pays the
   compile again.
3. A one-card A/B on a `blackwell-16` card, under a GPU lease, with an FP8-block (or FP8xFP4) checkpoint that fits:
   `VLLM_USE_DEEP_GEMM=0` against `=1`, same checkpoint, same prompts. Output equality comes first, and it is a real
   test because the open sm_120 issues are exactly nondeterminism and illegal access. Time to first token and
   four-stream throughput are recorded as side columns after it. No speed figure decides anything
   ([ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md)).
4. Explicit operator approval before the toolchain install or the wheel build, and again before any seat-template
   change. Only an equal-output result that the operator wants becomes an opt-in knob in the vLLM seat template, with
   the wheel pin and the toolchain requirement recorded in the tier's measured note.

### 4. Amendment 2026-10-18: the pending rows settled (ENH-15b)

On 2026-10-18 the operator made measurement opt-in: a model or loop is measured only when the operator orders it, with
one run, and the result is recorded in the benchmark documentation. No further pre-registered A/B was ordered. So
every row that was waiting on a gate verdict now ends in a terminal state, and the reopen trigger is unchanged.

| Row | Terminal state | What decided it |
|---|---|---|
| Headroom (offload rungs) | **PARKED**. The search rung and the logs rung are merged and default off. | Their non-inferiority A/B was not run. Reopen: an operator order to measure them, or a real run whose context overflow a rung would have absorbed. |
| The CLM paper | **DECLINED (measured, no need)**. No `edit_context` tool and no CLM benchmark adapter beyond the merged ContextBench port. | `trace-corpus` over the recorded agent-loop runs, read on 2026-10-18. In the window registered before any data (from 2026-10-16), the ladder fired on 0 of 5 runs (Wilson 95% upper bound 0.434). Across all rows from 2026-08-17 it fired on 4 of 9 runs that record it, and all 4 come from rows where the seat reported a bogus 8,192-token window. No run evicted anything, so the pager re-fetch rate has no data. The leased ContextBench smoke was stopped after it drove the host's commit to 168.3 of 171.7 GiB (register B-56, since bounded by the seat launcher's page-cache watcher). The reopen trigger stands: a corpus whose re-fetch rate clears 10%, or a seat on which the ladder fires often. |
| GitSwarm | **DECLINED (replicated, measured, no gain)**. | Pilot on a real harness issue, same gate for both branches. The swarm of three plus an integrator used 2.6x the output tokens and 2.6x the wall time of one agent. Its slices collapsed into a chain, it missed a fix the single agent made, and blind review scored it 7 against 8. The reopen trigger stands. |
| multi-harness-rl (acceptance-hygiene ADR) | The seat-by-loop matrix and vocabulary stay **PORTED**. The acceptance-hygiene ADR is **DECLINED**. | Its matrix measured a 9B pair of seats once (32 of 32 each, 16 ties: inconclusive, incumbent kept) and the rest were not run. A standing pre-registration procedure contradicts measurement on order only. Reopen: the operator orders standing measurement. |

The two numbers reserved for the measurement ADRs of this program (0092, acceptance hygiene; 0093, the CLM measurement)
are not used: their content is the two rows above.

## Consequences

- Each of the eight links now has one answer and a place to look it up. Someone who meets a link finds why it was
  declined or parked and what would change that, instead of researching it again.
- Three rows read pending (Headroom rungs, the CLM paper, GitSwarm). They are settled by one amendment once the
  measurements and ADR 0088's verdicts exist. If a measurement comes back inconclusive, the row keeps its declined
  branch and says "inconclusive at n" rather than being forced either way.
- Nothing here builds or installs. The only artifact beyond this file is a question that two checklists carry: ADR
  0089's NVFP4 arm, and the cascade bake checklist, which is not yet a repository artifact.
- The upstream facts are dated 2026-10-13 and will age. A correction to a fact, or an added source link, is a small
  edit to the Parked table; a changed decision is a new ADR that supersedes this one.
- The REA terminal state describes an install that lives outside this repository; this ADR records only that it is
  INSTALLED or DECLINED, not its configuration. OpenSpec's configuration is recorded in the admitted-software table in
  [security](../../systems/security.md).
- Status Proposed: the decisions are the operator's to accept, and an agent does not move this ADR to Accepted.

## Alternatives considered

- **One ADR per link.** Eight near-identical records, no single place that shows every mandatory link closed, and the
  reopen triggers (the part that stays useful) scattered.
- **Keep the dispositions in the private plan.** The refusals that matter to contributors (a licence this repository
  cannot take, a hardware floor, a daemon we do not run) belong where a contributor looks, and the plan names hosts.
- **Install everything named, to satisfy "mandatory".** Impossible for a paper with no code and for a non-commercial
  licence, and wrong for a resident credential-handling proxy.
- **A flat "no" with no trigger.** Reasons change (a licence, a release, a hardware tier); a trigger records which
  change would matter, so a row is neither forgotten nor reopened on a hunch.

## Related code

- [internal/agent/contextpager.go](../../../internal/agent/contextpager.go): the pager and its 10% re-fetch gate that
  decide the CLM paper row.
- [internal/eval/gate.go](../../../internal/eval/gate.go): ADR 0088's gate; [internal/eval/paireddelta.go](../../../internal/eval/paireddelta.go)
  holds the paired bootstrap the gate reads the efficiency metrics with.
- [compaction_eval.go](../../../compaction_eval.go): the replay corpus the Headroom rungs are scored on.
- [render/templates-catalog.mjs](../../../render/templates-catalog.mjs) and
  [render/templates-license-map.json](../../../render/templates-license-map.json): the shipped data layer of the Comfy
  templates row.
- [setup/templates/profiles.json](../../../setup/templates/profiles.json): the vLLM seat declarations (weight-only
  checkpoints, `kv_cache_dtype fp8`) that make DeepGEMM inapplicable today.

## Related docs

- [Security, layer SEC-L8 (admitting third-party software)](../../systems/security.md)
- [ADR 0001](0001-defer-never-cloud-fallback.md), [ADR 0016](0016-phase-c-dedupe-pinning-force-preserve.md),
  [ADR 0017](0017-kv-reuse-is-binary-and-how-we-measure-it.md), [ADR 0027](0027-big-moe-opt-in-engine.md),
  [ADR 0036](0036-the-agent-lane-is-a-harnessed-environment.md),
  [ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md),
  [ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md),
  [ADR 0058](0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md),
  [ADR 0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md),
  [ADR 0081](0081-the-embeddinggemma2-entry-may-run-from-a-second-llama-build.md)
- [Tier blackwell-16](../../tiers/blackwell-16.md): the tier the DeepGEMM A/B would run on.
- [ADR 0088](0088-one-measurement-gate.md) (the measurement gate) and [ADR 0089](0089-strata-is-an-opt-in-agent-seat-backend-that-earns-its-place-through-the-gate.md)
  (the Strata ruling). The cascade bake checklist is not yet a repository artifact.
