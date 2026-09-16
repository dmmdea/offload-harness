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
seat, and the two Qube tiers kept it green. ADR 0047 had even predicted both would go red and wrote that they
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
