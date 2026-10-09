---
status: Proposed
date: "2026-10-09"
---

# ADR 0080 — A RAM-spill MoE is the agent seat when it earns it

## Context

The house rule on where inference runs is that the cards do it and host RAM is overflow only: no model runs on the CPU, and
RAM may hold a spill only while it truly adds capability, never becomes a RAM hog, and never makes the box unstable (the
operator's ruling of 2026-10-07, now in the global rules). Until this ADR the tier table applied that as a flat ban on the
ampere-6 tier (RTX 3050 6 GB, 32 GB of host RAM): its notes read "26B STAYS DROPPED ... any MoE tier puts experts in CPU+RAM,
and boxes in this class are SERVERS whose CPU and RAM are reserved for their services". The 2026-08-17 bake-off had also
turned away Qwen3.6-35B-A3B on that boundary (21.7 GB resident for the MTP build), not on quality, recording it as "the
ceiling this tier could reach".

The 2026-10-07/08 lean bake on the reference 6 GB node (llama.cpp b10964) re-asked the question with a build of the same
family that fits the ruling, and against the ruling's three gates rather than against a blanket ban. The model is
unsloth/Qwen3.6-35B-A3B-GGUF `Qwen3.6-35B-A3B-UD-IQ3_XXS.gguf` (12.30 GiB; 35B total, about 3B active; Apache-2.0), the routed
experts in host RAM through `--n-cpu-moe 40`, attention, the dense layers and the KV cache on the card.

| gate (the ruling) | what was measured |
|---|---|
| **It truly adds capability** | Blind pack G4, judge claude-opus-5, 24 judgements: overall **8.11 vs 7.26** for the incumbent Qwen3.5-4B (gap 0.85 against a 0.5 bar), 17 of 24 head to head. Accuracy 8.34 vs 8.01, specificity 8.32 vs 7.58, coverage 8.12 vs 6.93; degradations 4 vs 23; minor fabrications 9 vs 6, serious 3 vs 3. Hard-8 set 1,399 s vs 4,818 s (decode 16.5 vs 7.9 tok/s heat-soaked); grounded digest-8 8 of 8 in 654 s vs the incumbent's 8 of 8 in 1,066 s. |
| **It is not a RAM hog** | About 11.6 GB host RSS while loaded (the weights plus 1 GiB), released on unload; MemAvailable never under 5,446 MiB on a 32 GB box beside a game server. `--cache-ram 0` is part of this: the host prompt cache grew about 1 GiB per digest run. About 2.1 GB on the card at a 32768 window with q8_0 KV. |
| **It does not make the system unstable** | No abort and zero swap-in across the runs with the shipped flags. |

The other arms of the bake did not clear the same gates: gpt-oss-20b was void (a swap-in abort) and Gemma 4 26B-A4B failed
spill stability.

The operator's order of 2026-10-09 ("ok wire it") is the decision this ADR records.

## Decision

1. **A RAM-spill MoE may be a tier's agent seat when it passes the ruling's three gates on that tier's reference box.** The
   gates are the entry criteria, in the order above: a blind pack that separates it from the incumbent by more than the bar;
   host RSS within the weights plus 1 GiB, released on unload, with a measured MemAvailable floor; no abort and no swap-in.
   Capability is judged against the seat it replaces, not against nothing.
2. **The seat is `qwen3.6-35b-a3b-agent`, on ampere-6 only.** The tier sets `include_qwen36_35b`. No other tier does: the
   measurement is of one card and one RAM size, and another tier needs its own bake.
3. **The gate is one predicate, used twice.** The entry renders only where the box's `ram_tier` is `low`, `mid` or `high` (28 GB
   and up, the class a 32 GB box reports), and the new seed overlay `config_seed_ram_low_up` binds `agent_model` to the seat on
   the same three names. `tierseed.RAMLowUp` is that predicate; the serving render (`spillSeatIncluded`) and the seed call
   it, so a config never names a seat the roster dropped. The predicate reads the tier name case-blind and trimmed, as `install
   render` reads its flag, and an unspecified tier (a caller that does not know its RAM) is below the floor on both sides: it is
   not the wildcard it is for the 26B placement. The base `config_seed.agent_model` stays `qwen3.5-4b-agent`, which is what a
   `min` box keeps; that box renders no spill seat (it still gets the `embeddinggemma2` stack entry, which has no RAM gate). The
   floor is the repo's existing `n_cpu_moe` rule (the partial spill "survives `low`, not `min`"), not a new tier name.
4. **The spill is sanctioned by the tier's measured number, and the write gate still stands.** ampere-6 declares
   `n_cpu_moe_max: 40`; `install render` refuses any render whose `--n-cpu-moe` exceeds it (INV-1, H-01), so the literal in
   the template cannot drift above what was measured without an edit that names the tier. `moe_26b` stays `drop`: the 26B is
   not part of this decision.
5. **The entry is the measured configuration, as literals.** `--n-gpu-layers 99 --n-cpu-moe 40 --flash-attn on --cache-type-k q8_0
   --cache-type-v q8_0 --ctx-size 32768 --jinja --threads N --batch-size 2048 --ubatch-size 512 --parallel 1 --cache-ram 0
   --load-mode none`, `ttl: 300`, one heavy alternative in the interactive set (never a resident). It is a thinking model, so
   it carries no `--reasoning` flag and is not folded into the `${common}` macro (the same trap the 4B entry documents).
   `--threads` is the one flag that is not a measured literal: the bake's thread count is not recorded, so the entry takes
   the render's thread input (physical cores from `install.ps1`, half the logical CPUs from `install render` without
   `--threads`), and the spilled experts run on those threads. Recording the measured count is open.
   `--parallel 1` keeps `fleet_max_concurrent_jobs: 1` correct.
6. **The alias moves, the 4B stays.** The seat claims `agent-seat`; `qwen3.5-4b-agent` stays rendered as an un-aliased opt-in
   rollback (`include_qwen35_4b` stays true), the same hand-off the mimo and Qwen3.5-9B entries use. Pairing the seat with
   `include_mimo_9b` is refused by name.
7. **The installer follows the same gate.** On Windows the 12.3 GiB GGUF joins the download set only when the tier carries the
   seat and the box's `ram_tier` clears the floor; the entry needs llama.cpp b10964 or newer (`--load-mode`), which the same
   release's Windows pin provides.

## Consequences

- An ampere-6 box with 28 GB or more of RAM serves a measurably better agent seat (the blind pack above), at the price of
  about 11.6 GB of host RAM while the seat is loaded and a 12.3 GiB download. The RAM is released after the 5-minute idle
  unload that every entry has.
- The seed gains a second RAM-gated layer. `audit-config` compares a `low` box against the base seed plus `config_seed_ram_low_up`
  (it used to compare `low` against the base seed alone), and the tier pages show the unconditional and the gated
  `agent_model` rows side by side.
- The `low` tier starts at 28 GB, four GB under the box the bake ran on. A 28 to 31 GB box has that much less headroom than the
  5.4 GiB floor that was measured; the floor was kept at the existing rule on purpose, and tightening it is a one-predicate
  change (`RAMLowUp` and the PowerShell twin) if a smaller box shows the pressure.
- `include_qwen36_35b` is omitted from the provenance basis when false, so it adds nothing to the Params bytes of a tier that does
  not carry the seat. That is all it promises: `template_sha256` and `profiles_entry_sha256` are in the basis too, and the
  templates changed, so every node rendered from a changed template reads STALE until re-rendered, as for any template change.
- ampere-6's notes no longer say that no expert may sit in RAM; they say which spill earned its place and why the 26B did not.

## Alternatives considered

- **Bind the seat in `config_seed_ram_mid_high`.** That layer applies on 56 GB and up; the reference node is a 32 GB box that
  classifies as `low`, so the measured box would render the seat and keep the 4B in its config. Rejected.
- **A per-tier RAM floor field instead of a shared predicate.** More general, and a second place a floor can disagree with the
  overlay. Rejected until a second tier needs a different floor.
- **Bind the seat in the base seed and blank it on `min`.** Needs an overlay for the smaller box instead of the larger one and
  makes the unsafe case the default. Rejected.
- **Render the seat on every box and let the lane fail where RAM is short.** The roster would advertise a capability the box
  cannot serve; the closure gates exist to prevent exactly that. Rejected.
- **Keep the flat ban.** It was written before a seat could be measured against the three gates and it left the tier on the
  weaker seat for a reason the operator has since narrowed. Rejected by the ruling.

## Related code

- `install_render.go` (`spillSeatIncluded`, `servingProfile.IncludeQwen3635B`, `deriveRender`, `warnMissingGatedModels`)
- `internal/servingtmpl/servingtmpl.go` (`Params.IncludeQ3635B`, `modelQ3635B`, `__Q3635B_ALT__`, the alias hand-off), `internal/servingtmpl/provenance.go`
- `internal/tierseed/tierseed.go` (`RAMLowUp`, `Profile.ConfigSeedLowUp`), `internal/tierdocs/tierdocs.go`, `audit_config.go`
- `setup/templates/profiles.json` (ampere-6), `setup/templates/llama-swap.linux-cuda.yaml`, `setup/templates/llama-swap.win-cuda.yaml`
- `setup/install.ps1` (`model-qwen36-35b`, `Test-RamLowUp`, `Get-GatedModelKeys`)
- Tests: `ampere6_spill_seat_test.go`, `internal/servingtmpl/q3635b_invariant_test.go`, `internal/tierseed/ram_lowup_test.go`

## Related docs

- [Setup and installer](../../systems/setup-installer.md), [ampere-6 tier](../../tiers/ampere-6.md)
- [ADR 0047](0047-ampere-16-agent-seat-reaudit.md) (the 16 GB-class agent seat, the same blind-pack method)
- [ADR 0054](0054-dual-route-node-renders-a-cpu-seat-family-beside-its-gpu-seats.md) (the dual-route CPU seat family and its amendment that no model runs on the CPU; this decision changes the reading of RAM spill only and leaves that amendment as it was)
- [ADR 0056](0056-llamacpp-prompt-cache-tiers.md) (the per-RAM-tier `--cache-ram` this seat deliberately does not follow)
