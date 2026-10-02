---
status: Accepted
date: "2026-09-16"
---

# 0048 — vLLM is a first-class engine on every tier, and a tier never loses it as a side effect

> **Operator hard rule, 2026-09-16.** vLLM is critical infrastructure for every future tier, equal in standing
> to llama.cpp. **Every tier, every box, every seat must be able to TEST and SEAT a model under vLLM exactly as
> it can under llama.cpp today.** Every vLLM candidate is measured against its llama.cpp counterpart so the
> ENGINE ROUTE is a per-tier decision backed by measurement. Removing a `vllm_seat` is a capability deletion and
> requires an explicit operator instruction — it is never a side effect of changing a model.

## Context — what went wrong

[ADR 0047](0047-ampere-16-agent-seat-reaudit.md) re-audited the ampere-16 agent seat and moved it from a 4B to
Qwen3.8-27B. The seat change was correct and is not in question here. What shipped alongside it was not:
PR #343/#344 (merged `1f8b6d9`) **deleted the tier's entire `vllm_seat` object** from
`setup/templates/profiles.json` —

```
id qwen3.5-4b-vllm · unit vllm-agent-seat · port 18797 · device 0
model_repo hub/models--RedHatAI--Qwen3.5-4B-quantized.w4a16
max_model_len 131072 · gpu_memory_utilization 0.65 · kv_cache_dtype fp8_e5m2
tool_call_parser qwen3_xml · reasoning_parser qwen3 · fallback_agent_model qwen3.5-4b-agent
```

— when only the MODEL needed changing. ADR 0047's own note records it as intended ("the `vllm_seat` declaration
is REMOVED from this tier"), which is exactly the problem: a capability deletion was written down as a
housekeeping detail. Three consequences, none of them stated at the time:

1. **The installer stopped rendering the vLLM seat for the tier** — the systemd unit, both llama-swap wrappers,
   the polkit rule and the llama-swap entry. A fresh `ampere-16` install now gets llama.cpp only. The reference
   box was unaffected only because its unit is hand-installed, which hid the regression.
2. **The tier lost its cache-server capability.** [ADR 0045](0045-a-cache-server-binding-per-vllm-seat.md) binds
   a cache server PER vLLM SEAT, and [systems/cache-server.md](../../systems/cache-server.md) defines a seat as
   "a vLLM engine with the LMCache MP connector". No vLLM seat, no LMCache — so deleting the declaration quietly
   deleted the tier's only path to the cache server.
3. **The evidence FOR vLLM on that card went with it.** The deleted block carried the measurement: on the
   reference A2 the ENGINE ALONE moved the same Qwen3.5-4B weights from 6.79 to 7.58 blind and removed every
   filler finding (ADR 0035, and the four-way re-eval of 2026-09-08).

## The gates did not catch it, and that is the second defect

Both existing vLLM guards iterate over whatever is declared and `continue` on a nil:

| gate | line | shape |
|---|---|---|
| `TestAReasoningSeatSeedsItsCompletionBudget` | `agent_budget_test.go:64` | `if p.VLLMSeat == nil \|\| p.VLLMSeat.ReasoningParser == "" { continue }` |
| `TestEveryDeclaredVLLMSeatValidates` | `vllmseat_table_test.go:35` | `if p.VLLMSeat == nil { continue }` |

So deleting a declaration does not turn a gate red — it removes the tier from the gate's iteration and the gate
passes **vacuously**. The backstop at `agent_budget_test.go:90` fires only when NO tier declares a reasoning
seat, and the two `<node-b>` tiers kept it green. ADR 0047 had even predicted both would go red and wrote that they
"must be re-pointed in the same change, not deleted"; they were neither re-pointed nor deleted — they were
un-triggered, silently.

**A guard written as `if X == nil { continue }` over a declared capability is blind by construction.** It can
only check the shape of what exists, never the fact that something stopped existing.

## Decision

1. **vLLM is a first-class engine of this harness on every tier**, equal to llama.cpp. The target state is that
   every tier can test and seat a model under either engine, and the engine route per tier is decided by
   measurement between the two — not assumed.
2. **`ampere-16`'s `vllm_seat` is restored** exactly as it was, with the model unchanged
   (`qwen3.5-4b-vllm`). The A-07 seat verdict is untouched: the llama.cpp 27B entry still renders and
   `config_seed.agent_model` still names the fallback, which is what a box without the venv reads.
3. **A tier losing vLLM capability must FAIL a gate by name.** New gate
   `TestEveryTierCanSeatAModelUnderVLLM` (`vllm_coverage_test.go`) asserts the SET of tiers rather than
   iterating the declared ones:
   - `vllmSeatTiers` is a **regression floor** — a tier in it that stops declaring a seat fails, naming the tier
     and the seat it lost;
   - `vllmSeatDebt` lists every tier that cannot yet seat a model under vLLM, each with its reason, and a tier
     that gains a seat while still listed fails with "promote it";
   - a tier in neither set fails, so a NEW tier cannot be added without declaring where it stands on vLLM.
4. **`TestEveryDeclaredVLLMSeatValidates` no longer skips when nothing is declared** — "nothing to validate" is
   the loudest failure available, not a reason to go quiet.

## Consequences

**The debt is now countable.** The gate reports coverage on every run; today it reads
**3 of 16 tiers, 13 owing a seat**. That number is the backlog this ADR opens, and the debt list is meant to
shrink to empty. It is deliberately visible rather than aspirational: before this change the true figure was 2
of 16 and nothing in the repo said so.

**Changing a model on a vLLM tier** means editing fields INSIDE the `vllm_seat` object — `model_repo`,
`max_model_len`, `gpu_memory_utilization`, the parsers. It never means deleting the object. If a tier genuinely
must give up its seat, the operator says so and the entry moves to `vllmSeatDebt` with the reason, which is a
visible, reviewable change rather than a silent one.

**Every seat measurement carries a vLLM arm.** A bake-off that measures only llama.cpp candidates does not
settle a tier's seat, because it has not tested the engine axis. The engine effect is not small: +0.38 on the 4B
and +0.29 on the 12B in the 2026-09-08 four-way, on identical weights.

**Verification.** The new gate was mutation-tested against the real regression: re-applying the A-07 deletion
turns `TestEveryTierCanSeatAModelUnderVLLM` red with the tier named, while both pre-existing gates still report
`ok` — which is the blindness this ADR exists to close.

## Amendment 1 (2026-09-17): blackwell-16 pays its seat, measured on its own silicon

The first debt entry to close is the one this ADR called "twin-arch sibling of ampere-16; owes the same seat".
It was NOT paid by copying ampere-16's declaration, and the measurement is why that rule stands:

- **The copy would have shipped a broken seat.** ampere-16's launch line carries `kv_cache_dtype fp8_e5m2`. On an
  RTX 5060 Ti (sm_120) under vLLM 0.29 that dtype through the FlashInfer backend returns NaN tokens — raw
  completions `<tool_call>!!!!…` at 8,580 prompt tokens, a hallucinated prompt at 290 — and every digest-8
  contract deferred `unparsed_tool_call` in two runs. Nothing in the harness's health, speed or smoke checks
  noticed: the seat was "healthy", 33 tok/s, and answered READY. Isolated one variable per arm (record:
  `Benchmarks and Optimizations/2026-09-16-16gb-tier-pass/blackwell-16/raw-toolcall-test/`): transformers
  version, the flashinfer sampler flag and prefix caching are irrelevant, `TRITON_ATTN` refuses the model, bf16 KV
  is coherent, and **`fp8` (e4m3fn) on the same FlashInfer backend is coherent**. blackwell-16 therefore declares
  `kv_cache_dtype: fp8`; `blackwell16_bound_lane_test.go` pins that field with the reason.
- **Operating point and lane, measured:** 49,152 @ util 0.92 (KV 76,314 tokens, 1.55×; 65,536 also fits),
  27.75 tok/s single / TTFT 0.43 s / 64.39 tok/s at 4 streams, digest-8 8/8 at the bound lane (4,096 / thinking
  off / vendor sampling / 900 s), blind quality 8.53 — level with the same checkpoint on the A2 (8.46), below the
  llama.cpp IQ3_S+MTP arm (9.30). The tier's llama.cpp lane stays the fallback.
- **WSL2 launches of a vLLM ≥ 0.29 venv pin the V1 model runner.** 0.29's default V2 runner needs UVA; `seat_fg.sh`
  now exports `VLLM_USE_V2_MODEL_RUNNER=0` when `/proc/version` says microsoft AND the venv's vLLM (read from its
  dist-info name) is ≥ 0.29, unless the env file already chose. The version gate is load-bearing: 0.28's V2 runner runs
  on WSL2 — 0.28 picks it per configuration, and the TP2 DFlash spec-decode arms logged `gpu_worker.py:396] Using V2
  Model Runner` and served — so a WSL-only pin would have silently moved such a 0.28 seat to V1 (caught in review).
  *Corrected 2026-09-23:* this line first said the production pair seat logs the V2 runner on 0.28.0; those lines are
  `gpu_worker.py:429`, which exists only in 0.29.0, and the production seats serve on 0.28's V1 runner. The gate stands
  on the DFlash evidence. vLLM's own `VLLM_WSL2_ENABLE_PIN_MEMORY=1` lets 0.29's V2 runner start on
  WSL2 and it then dies in kernel warm-up (`CUDA error: invalid device ordinal`), so the pin is the working path there.
- **The tier's llama.cpp lane gets a seeded budget too.** `TestAReasoningSeatSeedsItsCompletionBudget` asks every tier
  with a reasoning vLLM seat for `config_seed.agent_max_tokens`, because that value is what a fresh install runs when the
  vLLM prerequisites are absent and the fallback (`gemma-4-26b-agent`) is the lane. blackwell-16 seeds **4,096** — stated
  plainly: NOT a 26B measurement. It is the value the two `<node-b>` tiers seed for their 27B llama.cpp lane and the direction
  the 9B measured (+0.62 at 4,096), applied to a thinking-heavy 26B; the alternative was the loop's silent 1,024. The
  measurement that settles it (1,024 vs 4,096, blind, on a 16 GB Blackwell card) is masterplan row D-119.
- **Coverage now reads 4 of 16 tiers, 12 owing a seat.** The debt list shrank by one; the rule for the next
  eleven is the same: measure on the tier's silicon, never copy a sibling's dtype or window.

## Amendment 2 (2026-09-30): a tier never loses a LAYER or a declared second seat, and the table has a home for both

Register A-113. This is the same defect one level up. On 2026-09-18 the ampere-16 reference box gained a second
vLLM seat, the Qwen3.6-35B-A3B 2-bit GSQ (register A-100), and a second layer, `fast`, that routes digest-shaped
contracts to it. Both lived only in that box's hand-edited config and in two Go test files (`internal/placement`,
`internal/delegate`): `profiles.json` carried neither, so every fresh ampere-16 install lost them and the
generated tier page could not state them. The gates could not see it for the reason this ADR gives above: they
validate what is declared, and a layer or a seat that is not declared is not iterated.

The table could not simply be edited, because it had exactly one `vllm_seat` per tier, and the card cannot
hold two heavy seats at once (13.9 GiB and 11.7 GiB against a 15,356 MiB card, each sized to most of it). So the schema
grew, by the smallest steps that carry what the box actually runs.

**What the table can now say**

1. `extra_vllm_seats` beside `vllm_seat`: a tier's further vLLM seats, served on demand on the same card and never
   the agent lane. They are validated as non-lane seats (`Spec.ValidateExtra`): no fallback, and every lane field
   (`fallback_agent_*`, `agent_*`) is REFUSED rather than ignored, because a setting that reads as a decision and
   is never read is how a table drifts from its box. Ids, aliases and units must be unique across the tier, and an
   extra seat must share the lane seat's card.
2. `vllm_seat.storeless_reason` (and the same on an extra seat): the MEASURED reason a seat runs with no cache
   server, seeded verbatim into its `kv_cache_server` binding. Until now the derived binding carried one generic
   sentence and the measurement lived on the node. It is refused beside a `cache_server`: a seat is bound to a store
   or it is storeless, never both.
3. `layers` on a tier that composes nothing (ampere-16). A layer seat names its vLLM seat explicitly, so the values
   are the reference node's own and `audit-config` reports MATCH for the layer keys against a fixture that carries
   them (a live extract of that node redacts each layer seat's `ctx_tokens`, so the 32,768 is the value the placement
   and delegate tests pin, not a live reading); the copy of the window, the
   concurrency and the card pin that naming implies must equal the seat's own declaration, or the table is refused
   at parse (one number, one place).
4. `n_cpu_moe_max`: the tier's measured spill (H-01, below).

**How a box resolves them.** Whether a box runs a seat is decided per seat, by the check the lane seat already had
(the hand-built venv plus that seat's own weights; an extra seat also needs the wrapper scripts its entry runs, which
the operator installs, below). A running extra seat joins the `vllm_seats` roster with its own
binding; a layer that names a seat the box does not run is dropped; and `single`, the planner-default layer
(placement row 5b), is what keeps the rest placeable, so a set that lost it is not seeded at all and the box stays a
plain box. A box with no vLLM prerequisites seeds exactly what it seeded before layers existed.

**How it renders.** Every vLLM seat of a tier becomes an ALTERNATIVE of the others inside the residents set
(`emb & rer & (vagt | vagt2)`). Both shapes that look natural are wrong on one card: two co-resident members make
the matrix call the pair a valid combination and llama-swap loads the second beside the first, and a swappable
alternative beside a resident lane seat admits `lane + extra` together. As alternatives they stay resident-class (an
ordinary chat request never evicts the agent lane) while asking for the extra seat by name swaps it in. An extra
seat's entry names its wrappers after its own unit, because the lane seat's `vllm-seat-cmd.sh` has the lane unit
baked in and would start the wrong engine. `install render` now runs the composition check for any tier that
declares layers, not only for one that composes, so a layer routed to an undefined seat is refused.

**What the installer does not do for an extra seat.** It renders the llama-swap entry, the roster entry, the binding
and the layer, but not the seat's systemd unit, wrapper scripts or polkit rule: the seat's production launch line
carries `--language-model-only`, which the shared linux-systemd run script cannot express, so rendering it there
would ship an approximation of a measured configuration. Because they are the operator's step they are also the
seat's prerequisite (`Spec.DetectExtra`): llama-swap does not check that an entry's `cmd` exists when it loads its
config, so a seat advertised without them would be rostered, bound and layered and would fail only when a contract
asked for it. Until the two wrapper scripts are in the seat directory, `install seed` and `install render` leave the
seat and its layer out and name the missing file; [composite-tier.md](../../systems/composite-tier.md) lists what the
operator installs. Teaching the run script the flag is a change to the seat templates and is left to the change that
owns them.

**Gates.** `TestEveryTierKeepsItsDeclaredLayerSet` is the layer counterpart of `TestEveryTierCanSeatAModelUnderVLLM`:
`layerSetTiers` records each composite tier's layers and the seat roles each serves, and a tier that stops declaring
one fails by name (exact in both directions, so a new layer must be registered to be protected). The roles are the
capability and the models are not: swapping the model behind a seat never touches the floor. `extraSeatFloor` pins
the extra seat, because a layer naming a seat that is no longer declared is just a name nothing recognises as vLLM
and would be seeded on boxes that cannot serve it. `TestEveryDeclaredVLLMSeatValidates` now validates the extra
seats too; it, `tierseed.Resolve` and `Artifacts` all used to check `vllm_seat` only.

**H-01, the two Go-test gaps.** `install render` refuses `--n-cpu-moe` above the tier's measured spill
(`n_cpu_moe_max`, deliberately a separate number from the `n_cpu_moe` a tier ships, because one field cannot check
itself; no shipped tier declares one, and the "14 on 16 GB" figure has no measurement record in this repository, so it
is not attached to any tier). A tier that names the partial placement with no N is refused, since that renders the
every-expert `--cpu-moe`. The INV-16 gate (`TestInstallRendersOnAnyTierWithoutACacheServer`) walks the table: every
tier renders with no vLLM prerequisites, and a seat that declares no store renders its unit and wrappers with no
cache-server piece and seeds an explicit storeless binding.

**Verification.** Each new gate was made to fail against the real regression it guards, with the table restored
byte for byte afterwards: deleting the `fast` layer, deleting the extra seat while keeping its layer, breaking the extra
seat's tool parser, drifting a layer's window from its seat's, deleting every layer, and moving the fast layer's seat
to another role each turn a named gate red; a hard-coded `--n-cpu-moe 30` in a rendered command and a spill of 20
against a measured 14 are refused at the write gate.
