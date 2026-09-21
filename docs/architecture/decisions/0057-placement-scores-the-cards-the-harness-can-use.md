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

**Evidence, not configuration.** The Qube declares no `display_device`. On Windows/WDDM
`nvidia-smi --query-compute-apps` reports GRAPHICS processes with `[N/A]` memory; those are the
desktop. Measured on the Qube: all 28 such rows sat on card 1 while the harness's own resident
seat on card 0 produced no row at all. On Linux the query lists only CUDA processes with real
memory, so nothing is flagged and every card stays eligible — behaviour there is unchanged.

**The guard.** A display card is excluded only when at least one non-display card exists. A
single-GPU laptop or an iGPU node runs its seats on its display card by necessity; excluding it
would leave that box with nothing to score.

**Cadence.** The display set is refreshed every 15 ticks of the 2 s health sampler (30 s), not
every tick. Which card drives the desktop is hardware plus a login session, not something that
changes between health polls, and a second `nvidia-smi` on every tick of every node would double
the driver calls for no new information. A failed probe keeps the previous set, so an
`nvidia-smi` hiccup cannot suddenly re-count the desktop as harness work.

**Mixed fleets.** The tie-break uses `work_util_pct` only when BOTH nodes publish it; otherwise it
stays on `gpu_util_pct` for both. Comparing one node's desktop-free number with another's
desktop-inclusive one during a rollout would be meaningless.

## Consequences

- A Windows fleet node stops losing placements to its operator's desktop, and the Qube's lease
  verdict stops calling a game the holder's work — through the same function.
- `gpu_util_pct` is unchanged, so the fleet overview and every existing reader see what they saw.
- A pre-0.132.2 delegator ignores `work_util_pct` and behaves as before; a 0.132.2 delegator talking
  to an older node falls back to `gpu_util_pct`. Both directions of a rollout are safe.
- The health golden-shape test now pins the two new fields.
- Numbering note: this record's predecessor, the llama.cpp prompt-cache tiers, was briefly filed as
  a second ADR 0055 while ADR 0055 (liveness walls) already existed on main; it is now ADR 0056.
