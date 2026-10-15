### Added - ADR 0090, capability review 2026-10: dispositions and reopen triggers (Proposed)

- **What it records.** One decision record for the eight outside links named for mandatory installation or adoption
  (OpenSpec, REA, the Comfy-Org workflow templates, Headroom, the CLM paper, the CLM repository, multi-harness-rl,
  GitSwarm). Each row ends in one terminal state (INSTALLED, PORTED, REPLICATED, DECLINED or PARKED, or a conditional
  pair) with its reason and its reopen trigger. Three rows read pending, naming what decides them: the Headroom rungs
  (per-rung gate verdicts), the CLM paper (the CLM measurement and the edit-tool decision) and GitSwarm (the swarm pilot).
  The clean-room provenance rules for licences this repository cannot take are written down once.
- **The Parked table.** Fifteen items the review looked at and did not take (DeepGEMM, the Headroom resident proxy for
  Claude Code, the CLM repository and its serving engine, OpenEnv and Harbor, the SoL-Pi loop, Dream-RSI, Odysseus,
  Ouroboros, the GitSwarm code release, Agent-Reach, a local Claude Code launcher with cache-prefix pinning,
  Ternary-Bonsai-2-27B, NVFP4 beyond the DeepGEMM trigger, GLM-OCR and LightOnOCR-3, Cohere Transcribe), each with a
  reason and a reopen trigger. A short list of items skipped outright follows it.
- **DeepGEMM section.** Why it is parked (its kernels cover FP8-block, FP8xFP4 and BF16 only; the vLLM seats are
  weight-only 3-bit, 2-bit and INT4 with `fp8` as the KV dtype; Ampere is excluded; sm_120 exists only on an upstream
  dev branch and a vLLM fork with open correctness issues; Mega MoE is SM100-only), the reopen trigger (a Blackwell
  sm_120 tier adopts an FP8-block or FP8xFP4/NVFP4 vLLM checkpoint), the checklist questions that watch for it (ADR
  0089's NVFP4 arm, and the cascade bake checklist once it is committed), and the procedure if it fires (CUDA 13.2
  toolchain, wheel built in a scratch copy of the seat environment, a one-card A/B with output equality first and
  throughput as a side column only, explicit operator approval).
- **Scope.** Documentation only: the ADR and its index row. No code, no install, no config change.
- **Tests.** `TestDocsLint` (frontmatter, relative links), `TestADRNumbersAreUnique` (0090 is unique and linked from
  the index), `go run ./cmd/changelog check`, and the speed-term scan of the ADR and this fragment.
