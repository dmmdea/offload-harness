---
status: Accepted
date: "2026-07-18"
---

# Fix the tier binding before adding latency-based defers

## Context

The harness runs on a range of hardware, and on the slow end a task that a fast box completes in
seconds can take a minute or more. A natural-looking remedy is to predict a call's latency from
input size, compare it against a budget, and Defer early when the prediction exceeds the budget —
returning control to the caller instead of making it wait.

A well-specified proposal along these lines was contributed: a mirror of the existing triviality
check, deferring on predicted `A + B·chars` latency against a `max_latency_ms` budget, fitted from
ledger data, worst-case over the chain, text-cascade only, failing open. It was honest about its
limits, noting that cold model swaps and GPU-lock contention are invisible to the predictor.

The question it raised is not whether the design is sound. It is what a slow result actually
indicates.

## Decision

**When the harness is slow on a given machine, the first response is to fix that machine's Tier
binding — model choice, quantization, serving flags, hardware profile — not to add a gate that
defers the work back to the caller.**

Latency-based defer gates are not adopted as the remedy for slow hardware.

The reasoning: a Defer hands the task to the caller, which is a frontier model. So a latency gate
does not make the work cheaper or faster in any absolute sense — it moves the work from the local
tier to the expensive one, and it does so precisely when the local tier is least well configured.
Every task that trips the gate is a task the harness was built to absorb and did not. Worse, the gate
*masks the signal*: a badly-bound tier that should show up as a fixable configuration problem instead
shows up as a slightly higher defer rate that looks like normal operation.

Slow output on a correctly-bound tier is a hardware fact. Slow output on a badly-bound tier is a bug,
and it is the far more common case — a model at the wrong quantization, a profile that under-uses
available VRAM, or a serving flag that disables an optimization will each cost more than any gate
recovers.

This is a decision about **ordering and defaults**, not a permanent ban. A latency gate may earn its
place later, as an operator-chosen policy for a machine whose binding has already been optimized and
measured. It does not get to be the first answer.

## Consequences

- A slowness report is triaged as a configuration question first: which tier is bound, at what
  quantization, under which hardware profile, with which serving flags.
- Defer rate stays meaningful as a quality signal, because it is not inflated by a latency policy.
- Genuinely slow hardware stays slow, and is described honestly rather than papered over. Latency
  measurements from low-end machines are treated as data about the profile, not as a case for a gate.
- The contributed proposal is deprioritized rather than rejected outright; the conditions under which
  it could be revisited are the ones above.

## Alternatives considered

- **Adopt the predicted-latency defer gate now.** Rejected for the ordering reason above: it would
  ship a symptom-level remedy and hide the root cause.
- **Ban latency gates permanently.** Rejected as overreach. On a measured, well-bound machine an
  operator-set latency budget is a legitimate policy choice.
- **Make it opt-in immediately, defaulted off.** Rejected for now on the grounds that an available
  gate becomes the first thing reached for, which is exactly the ordering this decision is trying to
  establish. Revisit once tier-binding triage is routine.

## Related docs

- [0001-defer-never-cloud-fallback.md](0001-defer-never-cloud-fallback.md) — what a Defer means and
  costs
- [../../systems/offload-pipeline.md](../../systems/offload-pipeline.md) — tiers, escalation, and the
  thresholds that govern defers
- [../../systems/setup-installer.md](../../systems/setup-installer.md) — hardware profiles and how a
  machine's tier binding is chosen
