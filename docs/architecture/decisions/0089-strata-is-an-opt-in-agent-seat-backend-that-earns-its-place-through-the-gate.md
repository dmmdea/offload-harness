---
status: Proposed
date: "2026-10-14"
---

# 0089 — Strata is an opt-in agent-seat backend that earns its place through the gate

## Context

On 2026-10-13 the operator asked for a Strata backend and a Strata seat slot in every harness tier, and said it "needs to be
very well thought because this might become the main agent seat in the future". The model family behind it was measured here
in August 2026, parked, and its seats were removed in 0.115.4. This ADR is the ruling that decides what that history and the
standing rules since then allow. It builds nothing and changes no shipped configuration. The work that follows is tracked as
six items, which this ADR names and refers to below by name:

| item | what it does |
|---|---|
| the spike | preflight and an engine-only spike on `<node-b>` (private bench notes, no repo code) |
| the A/B | production-path A/B against the incumbent agent seat |
| the predicate change | the eligibility predicate, the profile slot and its coverage test |
| the seat package | `internal/strataseat`, `install_strataseat.go` and the seat-guard roster |
| the re-pack lane change | the structured re-pack lane for a seat that cannot constrain decoding |
| the promotion record | the promotion decision record |

**What Strata is.** The following is as read from the project's public documentation on 2026-10-13. A re-read of the
install guide, the model guide, the details page and the releases page on 2026-10-14 confirmed the hardware floors and the
KV-cache placement stated below; the rest is not yet re-verified, and the spike re-reads it before anything is
installed. Strata is an MIT-licensed local inference engine (a C++/CUDA/HIP core and a Python
server), not a model and not an agent runtime. It serves one large MoE family, Qwen3.8-Flash-Next (125B parameters, 6B active)
in several quantizations and derivatives, and since its 0.1.44 release Qwen3.6-35B-A3B. Attention and the dense layers live on
the cards. The KV cache lives in VRAM below 64K of context; at 64K and more the default setup keeps the context's KV cache in
host RAM and only the part the attention reads in VRAM (`--kv-resident 32768`), and `--kv-streaming on|off` overrides that. The
experts are pinned in host RAM, and an adaptive cache keeps the hot ones in the remaining VRAM; an expert that misses the card
cache is computed by the host CPU pool concurrently with the GPU. It serves OpenAI chat, Anthropic Messages and OpenAI Responses
endpoints, with `idle_unload_s`, `min_free_vram_mib` and `POST /unload?wait=` as its GPU-sharing controls. It has no grammar field, no logprobs and no embeddings, and it runs one
request at a time by default. Its only structured-output option is `response_format` with a JSON schema, implemented as schema
prompting plus validation, which answers HTTP 502 `structured_output_failed` on a violation. It is pre-1.0: ten releases in nine
days (0.1.40.1 to 0.1.44.1), defaults that change answers moved in 0.1.41 to 0.1.43, a rewritten repository history, an
anonymous maintainer, unsigned Windows binaries, and an updater that pulls an unpinned branch.

**The record on this model family.** These figures are history. They say what the new measurement has to be taken on and
against; none of them is a gate, a threshold or a target anywhere below.

- The August 2026 differential suite scored the Flash-Next IQ4_XS arm on two cards 18/18 at a total of 1828 s, against 18/18 at
  271 s for the then-incumbent llama.cpp Qwen3.8-27B ([the frozen roadmap](../../ROADMAP-2026-08-26-frozen.md)).
- The same roadmap records a later, separate completion on the UD-IQ4_XS quant with `--n-cpu-moe 40`: parity with the incumbent of that
  date at every measured difficulty (across the base, hard and frontier tiers, 46/46 on each side), at about twice the
  incumbent's wall and about 70 GB of host RAM.
- On the production path (20 document-grounded contracts per arm through `delegate --contract`) the 27B scored 20/20 at a
  median of 28 s and Flash-Next 15/20 at a median of 539 s. The roadmap's conclusion: "the instrument-parity story ... does not
  survive the production path".
- Release 0.115.4, shipped on 2026-09-10 ([CHANGELOG](../../../CHANGELOG.md)), removed the opt-in `qwen3.8-flash-next` and
  `qwen3.8-flash-next-262k` seats from the `blackwell-3x16` template. They parked 28 to 32 expert layers in host RAM, and the
  operator's rule of that day was that the cards do the inference and RAM is overflow only.
- The incumbent has moved: the tier's agent seat today is its vLLM Qwen3.8-27B seat on the two non-display cards
  ([tier page](../../tiers/blackwell-3x16.md)), and the 28 s is an August llama.cpp figure. The comparison is re-measured in the
  same session, not inherited.

**The standing rules since then.** Both are the operator's, and both are in his global rules.

- 2026-10-07: host-RAM spill is allowed only for a model that does not fit its card, and only with all three conditions
  measured: it adds capability, it is bounded and released on unload, and the host stays stable. Attention, the dense layers and
  the KV cache stay on the cards; only spilled MoE experts may run on the host. Weights parked in RAM and staged to the card
  step by step, with all compute on the GPU, are the other allowed form.
- 2026-10-10: that rule is about inference. A service that depends on RAM (a cache tier, a build, a test gate, weights streamed
  to a card) may use RAM whenever RAM is the more useful resource for that task, inside one limit: the host is never overloaded
  and never pages.
- Unchanged: no model runs on the CPU, and every idle model unloads after five minutes (INV-1 and INV-2).

[The roadmap](../../ROADMAP.md) lists three triggers for re-evaluating Flash-Next, one of which is a card-informed
differential tier that beats 0.46 of the incumbent's decode rate. Strata's expert placement is that kind of tier. A trigger
that fires is a reason to measure, not a verdict.

**Precedents.** [ADR 0027](0027-big-moe-opt-in-engine.md) seated a big-MoE engine as a hand-launched, lease-gated opt-in outside
llama-swap and the cascade, and keeps the grammar-constrained cascade on llama.cpp permanently.
[ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md) made a second engine first-class and added a coverage gate that
asserts the set of tiers, so a capability a table cannot state is not lost silently.
[ADR 0058](0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md) carries a licence on the binding and
publishes an unknown licence as unknown. [ADR 0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md) is the first
spill seat built on the 2026-10-07 rule: three measured gates, and one predicate used for render and seed.
[ADR 0002](0002-grammar-reliable-serving-flags.md) and
[ADR 0069](0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md) decide how a
seat that cannot take a grammar is asked for structure. ADR 0080 is still Proposed. ADR 0058 was accepted by the operator on 2026-10-14
together with a third licence state, `conditional`, and its status was flipped with the template-route change. Both are cited as the nearest precedent, not as dependencies.

## Decision

1. **Strata is a candidate for the agent class only.** It is the same slot the vLLM seat occupies, and possibly a long-context
   or code seat. It is never a text-cascade rung: it has no grammar and no logprobs, so ADR 0027's rule that the cascade stays
   on llama.cpp stands. The `strata_mcp.py` control server is not adopted (it duplicates the harness's own doors and leaves the
   model resident, which breaks the five-minute rule), and the launcher profile that points Claude Code at a local endpoint
   stays parked until an operator asks for an offline worker profile.

2. **The slot is dormant and nothing is displaced.** The fallback is the tier's current agent seat. Strata never removes or
   replaces a declared `vllm_seat`: ADR 0048 makes that a capability deletion that needs the operator's explicit instruction.
   A Strata seat is exclusive with the vLLM seat and with ComfyUI on shared cards, through the GPU lease.

3. **Two shapes, one per phase.**
   - **The spike** takes ADR 0027's shape: hand-launched, under `local-offload gpu reserve` with `--devices` and `--ram`
     declared and an honest `--for`, outside llama-swap and the cascade, on the newest release (never a pinned tag, ADR 0099), its engine archive
     checked against the publisher's digest and the release id recorded with each measurement, with `--data-dir` and every installer write (settings file, venv, caches, weights) relocated by environment to a
     data drive, or the spike closes without it (there is no junction). `idle_unload_s=300` is set on every arm, because the
     spike has no llama-swap timer and a manual unload after each run fails if the session dies. `STRATA_IQ_MT_MIN=1` and
     `--adapt-every 100000` are set on every arm for reproducibility, and `--kv-streaming off` on the arm that is measured
     against the clause in Decision item 6. The updater is never used. After each run the run script
     calls `/unload?wait=`, stops the process and checks that VRAM and host RSS are released. Each artifact (the engine and every
     weight family) gets an admission record under [the third-party admission checklist](../../systems/security.md), with
     a telemetry check before the first run: the project says only that nothing leaves the PC, so the engine is scanned for
     outbound endpoints and its connections are listed during the first launch.
   - **The production shape is seam B**, the vLLM precedent: a package `internal/strataseat` (Validate, Detect, Resolve,
     Bindings, Entry) and `install_strataseat.go`. The llama-swap entry carries `ttl: 300` and llama-swap owns idle unload;
     Strata's own `idle_unload_s` is left unset so two timers cannot race, and the stop path calls `/unload?wait=` first. The
     cascade seat guard keys on the `vllm_seats` roster by name, so the roster key is generalized or a `strata_seats` key is
     added beside it, or the guard would not protect the seat. The harness has launch sets for a WSL distro and for systemd
     only, and the engine documents that Windows and WSL2 pin a fraction of the expert arena, so native Windows is a new launch
     kind. Nothing in this paragraph is built by this ADR.

4. **The harness-seat exception.** A Strata seat launched by llama-swap is a harness seat, the established exception to the
   rule against hosting extra workloads on the operator's machines, exactly as the vLLM seats and ComfyUI are. The spike is not
   a hosted workload: it is a measurement that is hand-launched, lease-gated, outside llama-swap, run with `idle_unload_s=300`,
   and stopped afterwards. Its listener is loopback only and is written to the host's port file in the same change.

5. **One slot in every tier, rendered only where one predicate says so.** Every tier declares either a `strata_seat` or a
   `strata_debt` with its reason, and a coverage test beside `vllm_coverage_test.go` asserts the set in both directions (a tier
   in neither list fails, and a slot that names a checkpoint without a recorded verdict fails). Only `blackwell-3x16` names a
   checkpoint; every other eligible tier gets a checkpoint-less dormant slot, with no model named until a bake on that class
   exists. A `cpu` tier is ineligible by the no-CPU-inference rule, and a tier whose card or architecture the engine does not
   support carries `strata_debt`. The gate is one predicate used twice, as in ADR 0080 Decision item 3: `tierseed.StrataEligible`
   decides both what the serving render includes and what the seed binds, so a config never names a seat the roster dropped. Its
   inputs, as re-read from the project's install guide and model guide on 2026-10-14, are:

   | input | value as last read | status |
   |---|---|---|
   | GPU | NVIDIA compute capability 7.5 or newer (RTX 20 series and newer) for the ready-made CUDA 13 engine; Pascal and Volta only through an experimental CUDA 12 engine. AMD discrete RDNA2 to RDNA4 through HIP, integrated Radeon unsupported (the AMD part is from the first read, not re-read) | to be hard-coded only by the predicate change, with `source` and `verified_at` |
   | driver | 580 or newer on NVIDIA (the install guide and the details page say so; the project's front-page README gives no number, which is why one earlier read found none) | to be hard-coded only by the predicate change, with `source` and `verified_at` |
   | VRAM | 12 GB or more; 8 GB runs, slowly. Treating 8 GB as an explicit opt-in is this harness's own design rule, not upstream wording | to be hard-coded only by the predicate change, with `source` and `verified_at` |
   | CPU | x86-64 with AVX2 for the ready-made engine; a CPU without AVX2 gets only an experimental, slow, locally compiled build | to be hard-coded only by the predicate change, with `source` and `verified_at` |
   | host RAM | by arm, from the model guide's table: Coder 32 GB; Q2_0 and IQ2_XS 48 GB; IQ3_XXS and IQ3_S 64 GB, and for IQ3_S the guide calls 96 GB or more the comfortable pick | to be hard-coded only by the predicate change, with `source` and `verified_at` |

   None of these is a constant yet. The predicate change hard-codes a floor only with its `source` and `verified_at`. The spike
   pin is release v0.1.44.1, the newest tag at the 2026-10-14 re-read; pin re-read at spike time, because a project that ships
   this often may have tagged another by then, and the engine archive's hash is recorded then. An earlier fetch of the project's
   page returned a stale version, which is why a re-read comes before any constant.

6. **The default checkpoint reverses the shape 0.115.4 removed, under the later rule, and only if it is measured to fit it.**
   `blackwell-3x16` names Flash-Next GSQ-RCO IQ3_S (a 54.8 GB first shard, the arm the project reports as matching the full
   model) with its experts spilled to host RAM. That is the shape 0.115.4 took out, admitted now only because the 2026-10-07 rule
   permits a spill that passes its three measured conditions. The admission is conditional on the spike measuring that Strata's
   split matches the rule's clause: attention and the dense layers run on the cards, only expert matmuls run on the host, and
   the per-process CPU share stays within 10 percentage points of the engine-only `--n-cpu-moe` arm's on the same weights. The
   rule also keeps the KV cache on the cards, and Strata's default does not at 64K of context and above (Context, above). So the
   arm measured against the clause pins `--kv-streaming off`, and where the KV cache lives is measured and recorded on every
   arm: bytes in VRAM per card and bytes in host RAM, at each context length the A/B uses. An arm whose KV cache sits in host
   RAM is recorded as that and does not meet the clause as the rule words it; if the pinned arm cannot hold the A/B's contexts in
   VRAM, that is a recorded failure of the clause, not a reason to relax it. The engine's own statement of where expert compute
   runs is quoted in the record, the footprint of the n-gram table it reads from disk is recorded, and ADR 0080's resource
   conditions are measured in the same run. If the split does not match, or the operator flags the reversal, the IQ3_S arm is
   not named and only the Coder arm remains a candidate for the spike to run alone. The Coder is a pruned variant. The pre-spike
   estimate that puts its experts essentially in VRAM was made for all three cards (48 GB), not for the pair, and the project's
   own documents call the three-card layout unmeasured. On the pair's two non-display cards the same arithmetic leaves less room
   for the expert cache, so the Coder is expected to spill part of its experts there: the fallback is rule-clean only if the
   spike measures otherwise, and any spill is held to the same clause and the same measured conditions as the IQ3_S arm. If no
   arm clears, the closure is DECLINED with its reopen trigger recorded. The reversal is recorded as
   applied by default under the operator's 2026-10-13 instruction to go with the best recommendations, and it was confirmed on
   2026-10-14, when he answered "internal-use measurement OK" for the weights' licences; he may still flag it in use, and this ADR
   stays Proposed.

7. **Placement is by role, resolved at run time.** The pair means the two non-display cards. It is resolved with
   `local-offload gpu cards` immediately before every run and pinned by GPU UUID prefix in both `--devices` and Strata's
   `--gpus`; never by index, and no UUID goes into `profiles.json`, a public file or a commit message. Strata's multi-GPU
   auto-pick can include the display card, which may carry at most `gpu_display_cap_pct` and cannot take a load of this size, so
   `--gpus` is always pinned. Which physical card plays which role changed in October 2026, so no card index stands for a role
   in this decision, and the tier page's older device numbers are not used.

8. **Licences are carried as seat metadata.** The engine is MIT. The model files carry their own licences:

   | licence | covers | `commercial_use` |
   |---|---|---|
   | Qwen Community License 1.0 | the Qwen3.8-Flash-Next weights and their quantized derivatives | unverified |
   | Swift Open License 1.0 | the Swift fine-tune of that family | unverified |

   The operator accepted an internal-use measurement on 2026-10-14; the licence reading is pending, and until it is recorded
   `commercial_use` stays `unverified`. This follows ADR 0058's principle, an unknown licence is published as unknown and never
   as commercial-safe, but not its mechanism: 0058's overlay key is a bool today (the third licence state the operator accepted
   for it, `conditional`, is not a bool), and these are seat-metadata fields written by the seat, not an image-family key. Until the reading is recorded Strata stays an opt-in with the tag set and is not a tier-default
   agent seat (ADR 0058 Decision item 4, applied by analogy).

9. **The re-pack lane is measured both ways.** A Strata seat cannot take a grammar, and `isVLLMSeat` is the only engine-kind
   branch at the re-pack send site, so the re-pack lane change adds one. Option 1 is ADR 0069's path: the shape in the system prompt
   (`ForUnconstrained`) and a strict single-object parse (`parser.ExtractOne`); whether `ForUnconstrained` applies to the
   re-pack build is unverified and that change checks it first. Option 2 is `response_format` with a JSON schema on the tool-less call,
   with HTTP 502 `structured_output_failed` counted as a failed attempt and not as a breaker trip; only this option needs an
   amendment to ADR 0002, which says `response_format` stays unused on every engine. Both are measured; the choice follows the
   measurement. The re-pack exhaustion rate must be no worse than 12.6 %, the rate ADR 0002 measured on the seats other than vLLM;
   19.7 %, measured on vLLM seats that silently ignored the grammar, was the failure behind register row D-129.

10. **The promotion ladder.** It is a proposal and the operator may change it. Quality is the only gate (INV-5). Every comparison
    runs through the one measurement gate, [ADR 0088](0088-one-measurement-gate.md); its margin, its usable-only
    denominator, its rule that the incumbent wins ties and its frozen baseline are not repeated here. The mode and the
    efficiency metrics are declared in the frozen baseline before the first candidate run.

    | level | what it means | needs |
    |---|---|---|
    | 0 spike | a hand-launched measurement, private notes | the spike's preflight, a lease, `idle_unload_s=300` |
    | 1 opt-in seat | rendered where eligible, named by the caller, never the agent-seat alias | the gate's non-inferiority mode against the incumbent re-measured in the same session; ADR 0080's two resource gates, each measured (host RSS within the weights plus 1 GiB, released on unload, with a MemAvailable floor; no abort and no swap-in); a feasibility floor: the contract's fitted final completes inside the contract's own budget at the seat's measured rate (tagged `speed-lint: allow feasibility floor`) |
    | 2 tier-default agent seat | the tier's agent alias moves to Strata | level 1, plus a blind gap above ADR 0080's 0.5 bar (its first gate), budget-defers no more than the incumbent's, reproduction in a second session, and the licence reading recorded |
    | 3 main agent seat across tiers | the default everywhere it is eligible | level 2 held through two weeks of ledger, and an operator-visible record |

    Wall time is a side column, and among seats that already passed the adequacy gate it may serve as an ordering key; it never
    decides a pass. The spike records, side by side with the engine-only `--n-cpu-moe` arm: VRAM per card, host RSS, the
    MemAvailable floor, per-process CPU share, where the KV cache lives (VRAM per card and host RAM, at each context length),
    the expert-coverage line the engine prints, the cold start in both memory modes
    against `ttl: 300`, and the decode and prompt rates as side columns. It also checks the kernel path: the `blackwell-16` tier
    page records that the llama.cpp 13.x asset segfaults its MMQ kernel on sm_120 into a much costlier cuBLAS fallback
    ([tier page](../../tiers/blackwell-16.md)), and Strata's CUDA 13 engine uses llama.cpp-derived kernels, so the first start is
    checked for the same crash and the prompt rate is watched for the same fallback. The seat becomes a tier default or the main
    agent seat only through the promotion decision record, written after the A/B and again after two weeks of ledger, not
    silently.

11. **The DeepGEMM cross-link.** The spike's NVFP4 arm carries the checklist question: does the checkpoint use FP8-block or NVFP4 on
    sm_120? Strata's ggml NVFP4 path does not use DeepGEMM; the reopen trigger is in [ADR 0090](0090-capability-review-2026-10-dispositions-and-reopen-triggers.md).

## Consequences

- No behaviour changes today. This ADR adds no code, and the dormant slots it describes cost one declaration per tier and a
  coverage test. The tier's agent seat stays what it is until the ladder says otherwise.
- The measurement is the deliverable. If the spike shows the split does not match the clause, or the A/B does not clear level 1, the
  seat stays unshipped, the weights of every arm that does not graduate are removed, and the promotion record is the closure,
  with its reopen trigger (a release that changes where expert compute runs, or an operator request).
- The risks are known and are measured rather than argued: the project is days old and pre-1.0; its defaults can change answers
  between releases, so every arm pins a tag; concurrency is one and there is no grammar or logprobs, so the decision-margin gate
  is inert and re-pack exhaustion is the open question; a first load that pins 35 to 55 GB can freeze the host for one to three
  minutes, so the spike runs when nobody is working at the box; and a resident reload against `ttl: 300` means an idle unload
  costs a cold start, whose price is recorded and not assumed.
- ADR 0048 stays whole: vLLM remains first-class on every tier, and a Strata promotion removes no `vllm_seat`.
- Results carry the seat's licence tag. A result that came from a Strata seat names `commercial_use: unverified` until the
  reading is recorded.

## Alternatives considered

- **Leave it parked, with no slot.** The 0.115.4 removal stands and nothing is built. Rejected as the starting position,
  because the rule changed, a re-evaluation trigger fired, and the operator asked; it is the outcome if the spike or the A/B does not
  clear.
- **Make it the default agent seat now.** Rejected: the production-path record says the opposite, and the gate decides.
- **Declare a slot on `blackwell-3x16` only.** Rejected: ADR 0048's lesson is that a capability a table cannot state is lost
  silently, and the operator asked for every tier. Other eligible tiers get a dormant, checkpoint-less slot.
- **Use it for the text cascade.** Rejected: no grammar, no logprobs, one model per process.
- **Run it as a standing service, through its own MCP control server or its start-up recipe.** Rejected: the model stays
  resident past five minutes, and an unattended launcher is the kind of scheduler the house rules forbid.
- **Take `response_format` as the only re-pack path.** Not chosen in advance: it forces an ADR 0002 amendment, and ADR 0069's
  path may be enough. Both are measured.
- **Update through the project's own updater.** Rejected: it pulls an unpinned branch.

## Related code

- [`internal/vllmseat/vllmseat.go`](../../../internal/vllmseat/vllmseat.go), [`install_vllmseat.go`](../../../install_vllmseat.go) and
  [`vllm_coverage_test.go`](../../../vllm_coverage_test.go): the seat package, installer and coverage gate that seam B copies
- [`internal/tierseed/tierseed.go`](../../../internal/tierseed/tierseed.go) (`RAMLowUp`, the predicate shape
  `StrataEligible` follows) and `setup/templates/profiles.json`
- [`internal/config/config.go`](../../../internal/config/config.go) (`VLLMSeats`, `UnconstrainedSeats`) and
  [`internal/seatguard/guard.go`](../../../internal/seatguard/guard.go) (the guard reads the `vllm_seats` roster)
- [`internal/pipeline/agenttask.go`](../../../internal/pipeline/agenttask.go) (`isVLLMSeat` at the re-pack send site),
  [`internal/tasks/unconstrained.go`](../../../internal/tasks/unconstrained.go) (`ForUnconstrained`) and
  [`internal/parser/strict.go`](../../../internal/parser/strict.go) (`ExtractOne`)
- [`internal/servingtmpl/servingtmpl.go`](../../../internal/servingtmpl/servingtmpl.go): the `ttl: 300` entry shape
- Planned and not yet present: `internal/strataseat`, `install_strataseat.go`, `strata_coverage_test.go` and
  `tierseed.StrataEligible`

## Related docs

- [ADR 0027](0027-big-moe-opt-in-engine.md), [ADR 0048](0048-vllm-is-a-first-class-engine-on-every-tier.md),
  [ADR 0058](0058-non-commercial-model-families-ship-only-as-named-license-tagged-opt-ins.md),
  [ADR 0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md), [ADR 0002](0002-grammar-reliable-serving-flags.md),
  [ADR 0069](0069-an-unconstrained-seat-runs-classify-and-extract-from-the-prompt-and-the-text-lane-ships-dark.md)
- [ADR 0088](0088-one-measurement-gate.md) (the one measurement gate) and [ADR 0090](0090-capability-review-2026-10-dispositions-and-reopen-triggers.md)
  (the DeepGEMM reopen trigger)
- [The frozen roadmap](../../ROADMAP-2026-08-26-frozen.md), [the roadmap](../../ROADMAP.md), the 0.115.4 entry in the
  [CHANGELOG](../../../CHANGELOG.md), [the `blackwell-3x16` tier](../../tiers/blackwell-3x16.md), and
  [the security doc](../../systems/security.md) for third-party admission
