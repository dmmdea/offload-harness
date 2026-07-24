---
status: Accepted
date: "2026-07-24"
---

# Phase C: dedupe rung, re-request pinning, FORCE_PRESERVE guards, fit telemetry

## Context

The OmniRoute-harvest design's Phase C refits the compaction driver with the harvested escalation
loop and adds the content-addressed dedupe rung. Two of its items landed early with ADR 0015 (the
budget targets the SERVED window; headroom-first was already the ladder's happy path). The
remaining items are this decision. Evidence feeding it: the Phase-B real-corpus measurement
(v0.22.24) — the marker rung's per-kind entity destruction is measured, and the mini corpus pinned
the buried-error-survival property the bare marker lacked.

## Decision

1. **Content-addressed dedupe is the ladder's cheapest rung** (always on, before GCF): an OLDER
   tool body byte-identical to a LATER tool result collapses to a reference marker naming the
   later call id. The newest copy stays authoritative; pairing is intact; the information remains
   reachable via the reference. Bodies under 64 chars are not worth a reference. If the later copy
   is itself compacted afterwards the reference degrades to pointing at a marker — no worse than
   the marker the older copy would have become.
2. **Re-requested results are pinned (the H8 ramp).** The circuit breaker's exact-repeat refusal
   is proof the model WANTED a result back; that result's call id becomes Pinned for the rest of
   the run: exempt from dedupe/skeleton/elide, its unit from drop. Pins resolve TRANSITIVELY
   through dedupe references (pinning a reference alone would protect a 50-char marker while the
   referenced bytes are destroyed). When the original result NO LONGER EXISTS as raw content
   (already a compaction artifact, or its unit dropped), the refusal would point the model at
   destroyed bytes with no recovery — so the call is re-executed ONCE per exact (name,args) pair
   and the FRESH result pinned; later repeats are refused as usual. The LOSSLESS GCF rung still
   applies to pinned bodies; `emergencyShrink` stays pin-blind (at that point the alternative is
   a dead run).
3. **FORCE_PRESERVE lines survive every rung short of the run's own death.** The elision rung
   keeps a bounded residue (≤5 lines / ≤400 chars including separators, rune-safe truncation) of
   a body's signal lines (the skeleton rung's own `signalLine` class: errors, failures, warnings,
   test summaries) UNDER the bare marker; the drop rung refuses to drop a unit whose bodies still
   carry signal (or are pinned). "Short of the run's own death" is literal: `emergencyShrink` —
   the reactive-overflow last resort — CAN strip residue back to the bare marker line, exactly as
   it is pin-blind, so a residue-heavy transcript can always be made to fit before the run dies.
   A ladder that (short of that last resort) cannot fit the budget exhausts HONESTLY:
4. **fit=false telemetry.** `Result.CompactionsExhausted` counts steps whose ladder ran and still
   could not fit the input budget; the standalone runner and `agent_run` surface it. A best-effort
   over-budget request is never silent.
5. **Floor-mode monotonicity is a tested invariant**: compaction is idempotent at a fixed budget,
   and a harder-compacted transcript re-compacted at a gentler budget stays exactly as compacted —
   a turn only ever moves DOWN the ladder (no-op → dedupe/GCF → skeleton → marker → drop), never
   back up. This is the llama.cpp KV-prefix stability doctrine made explicit.

**Deliberate deviation from the harvested design**: the projected-fit driver (static per-rung
expected-reduction factors, stop at first PROJECTED fit) is NOT ported. OmniRoute needed
projection because its rungs were expensive; this ladder's rungs cost microseconds and are
applied-and-MEASURED per step, which is strictly more accurate than projection and cannot drift
from reality as calibration constants would. The factors' remaining value — knowing the ladder
cannot fit before sending — is delivered by the fit telemetry instead.

## Consequences

- The base ladder's bytes change for signal-bearing bodies (marker + residue): the compaction-eval
  ratchet correctly BREACHES against pre-Phase-C baselines — re-freeze baselines after adopting.
  The mini-corpus pinned property evolved with it: the buried error now survives BOTH ladders
  (pinned in both directions by the renamed eval test).
- Duplicate-heavy transcripts (a model re-reading files) compact losslessly-by-reference before
  anything destructive runs.
- A pathological transcript that is all signal can exhaust the ladder over budget; the reactive
  retry and `emergencyShrink` (ADR 0015, extended here to reclaim residue) remain a REAL backstop,
  and the exhaustion is now counted on both the proactive and reactive paths.
- Dedupe never grows a body (a reference larger than the duplicate is skipped), and a dedupe
  reference can dangle if the referenced unit is later dropped — disclosed in the rung's comment.
