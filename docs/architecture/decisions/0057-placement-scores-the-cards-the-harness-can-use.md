---
status: Accepted
date: "2026-09-21"
---

# ADR 0057 — Placement scores the cards the harness can use, not the operator's screen

## Context

Two surfaces asked "how busy are the cards?" and both answered with the busiest card on the
whole box. On a machine someone was using, that card is the display card, and its load is the
desktop or whatever the operator is running.

1. **The lease verdict** (`gpuactivity.Assess`). `held-working` outranks `held-idle`, so the
   desktop pinned the verdict at "the holder's own job" and `held-idle` could never fire.
   Measured on the Qube 2026-09-20 while the operator played a game: the verdict read
   `held-working — 33% on card 1 (RTX 5070 Ti)` while the lease holder had spent **4 seconds of
   CPU in 141 minutes** and both cards it fenced sat at 0%. Three jobs queued behind it. Fixed in
   0.132.1.
2. **Placement.** The fleet node publishes `gpu_util_pct`, and `delegate/gate.go` breaks
   placement ties on it. So a Windows node whose operator was gaming advertised that load and
   lost ties to a node it should have beaten. 0.132.1 named this and left it; this record fixes it.

## Decision

**Two questions get two figures.** `gpu_util_pct` keeps its documented meaning — the busiest
card on the box — because the fleet dashboard and the PAIR multi-GPU rule want exactly that ("the
shared card is the one that matters"), and changing a field's meaning under its existing readers
would break a stated contract. A new additive field, **`work_util_pct`** (+ `work_util_known`),
is the busiest card the harness can actually run a seat on. The placement tie-break compares
`work_util_pct` when both nodes publish it.

**One rule, in the leaf.** Which card is a display card is decided once, by
`gpuprobe.DisplayCardUUIDs`, and both the lease verdict and the node's placement figure call it.
It lives in `gpuprobe` because that leaf already exists so the placement guards and the health
sampler read every card through one parser. A second copy of the rule would have been a second
chance to get it wrong — which is precisely how 0.132.1 left the placement half standing.

**The card's own property, not an inference.** `nvidia-smi --query-gpu=display_active` says
whether a card is driving a display. It rides the ONE per-device query every reader in the harness
already runs, so it costs no extra call, and it is a fact about the card rather than a guess about
what someone happens to be running.

The first cut of this record inferred it from the process list instead — on Windows/WDDM
`--query-compute-apps` reports processes with `[N/A]` memory, and those were taken to be the
desktop. That was wrong twice over, and the correction is the reason this section was rewritten:
`[N/A]` is a WDDM property, not a graphics-process property (nvidia-smi cannot size ANY process
there, and it types all 24 of the Qube's desktop rows `C+G` — compute AND graphics), and a
native-Windows CUDA seat produces exactly the same shape. ComfyUI is one, and the 3-card law pins
it to card 0 or 2 — so the heuristic would have flagged the card the harness was WORKING on,
dropped it from `work_util_pct`, and made a saturated node advertise itself as idle. It would have
inverted the very defect it was written to cure.

Measured 2026-09-21 across all three node shapes:

| node | cards | `display_active` |
|---|---|---|
| Qube, operator gaming | 3 | `Disabled` / **`Enabled`** / `Disabled` (card 1, exactly) |
| Lenovo, headless Linux | A2 | `Disabled` — nothing flagged |
| Aorus, laptop, screen on the iGPU | RTX 3070 | `Disabled` — the RTX is scored |

Only an exact `Enabled` counts as a yes. A driver that does not report the field answers
`[Not Supported]` and older ones `[N/A]`; both mean "we do not know", which must never read as
"this is the operator's screen" — the whole point of excluding a card is that we are sure.

**The guard.** A display card is excluded only when at least one non-display card exists. A
single-GPU laptop or an iGPU node runs its seats on its display card by necessity; excluding it
would leave that box with nothing to score.

**No cadence, because there is nothing to refresh.** The display set is derived from the same
device sample the health tick already takes, so there is no second probe, no separate interval and
nothing carried forward between ticks. The first cut needed all three (a 30 s probe, a carry-forward
on failure) because its evidence came from a DIFFERENT query than the one the sampler ran; reading
the card property removes that machinery instead of tuning it. The set follows the hardware on the
very next tick, with no stale window to expire.

**Mixed fleets: one figure per NODE, never per pair.** This record's first cut said the tie-break
compares `work_util_pct` only when BOTH nodes publish it and otherwise stays on `gpu_util_pct` for
both. That is right pairwise and wrong as an ordering. `bestRemote` FOLDS the comparison over a
roster, and Go requires a strict weak ordering from any comparison used to order a set
(`slices.SortFunc` states it, transitive incomparability included). Choosing the metric per
comparison ranked the same three nodes on two different metrics depending on who was being
compared, and during a rollout — the only time a mixed fleet exists — that produced a strict cycle:

    gaming (work 0, gpu 80) > light (work 5, gpu 5)   on work_util
    light  (work 5, gpu 5)  > older  (gpu 10)         on gpu_util
    older  (gpu 10)         > gaming (work 0, gpu 80) on gpu_util

The winner was then whichever node the roster happened to start from. `placementUtil` picks the
figure per NODE — `work_util_pct` when that node publishes it, `gpu_util_pct` when it does not —
which restores a total order. The cost is that a rollout compares an upgraded node's desktop-free
number against an old node's desktop-inclusive one, a bias toward the node whose number is true.

**The same defect had two more instances, and they are fixed here too.** Both made placement depend
on roster order:

- The P2C near-tie draw hashed the SEED ALONE, so it returned the same bit whichever way round it
  was asked: `betterRemote(P, Q)` and `betterRemote(Q, P)` were both true for a near-tie. The draw
  is now a bounded jitter applied to the seat's OWN eta, derived from the seed and the node id, so
  it is part of the key rather than a branch on the pair. Bucketing the eta would also have been
  total, but a hard edge inside the near-tie band stops nearby seats spreading — the herding the
  clause exists to prevent. Jitter has no edges, and spreads across any number of tied seats.
- A seat publishing no `seat_rate` made its comparisons fall back to window for that PAIR only.
  It is now ranked as if it ran at the fleet's MEDIAN published rate, keeping its own backlog and
  cold load — the same shape as `agent.assumedPrefillTokS`, which assumes a rate for a stall wall
  until one is measured. A constant would have been either "an unmeasured seat wins everything"
  (taking placements from seats known to be fast) or "it never gets work, so it never earns the
  samples that would rank it". The median is neither, and it moves as the fleet does. With no node
  publishing a rate there is no prior, every seat is unranked, and the order falls to window for
  all of them — the original rule, applied uniformly, which is still total.

## Consequences

- A Windows fleet node stops losing placements to its operator's desktop, and the Qube's lease
  verdict stops calling a game the holder's work — through the same function.
- `gpu_util_pct` is unchanged, so the fleet overview and every existing reader see what they saw.
- A pre-0.132.2 delegator ignores `work_util_pct` and behaves as before; a 0.132.2 delegator talking
  to an older node falls back to `gpu_util_pct`. Both directions of a rollout are safe.
- The health golden-shape test now pins the two new fields.
- Placement is a strict weak ordering, pinned by a brute-force check over a 16-node roster (measured
  and unmeasured seats, four window sizes, backlogs, cold loads) across 40 seeds. Three separate
  cycles had to be removed to get there; all three were the same mistake — choosing the comparison
  metric from the PAIR instead of from the node.
- A node that publishes no `seat_rate` is placed on an assumption that a single real run replaces,
  so a fresh or re-imaged node neither starves nor captures the queue.
- Numbering note: this record's predecessor, the llama.cpp prompt-cache tiers, was briefly filed as
  a second ADR 0055 while ADR 0055 (liveness walls) already existed on main; it is now ADR 0056.
