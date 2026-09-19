---
status: Accepted
date: "2026-09-18"
---

# 0051 — The decision margin's full denominator ships behind a flag, and every row names its scale

## Context

`internal/confidence.classMassMargin` folds each alternative at the decision position to a legal
class, sums its raw probability mass, and returns `(first - second) / total`. The defect (register
D-130) is where `total` is accumulated: inside the `ci >= 0` branch. The denominator is therefore
the MATCHED class mass only — mass on unmatched tokens, and on legal labels that never made the
`top_logprobs` window, is excluded from it entirely.

Research measured the declared (matched) mass at **~0.097** on llama.cpp. The margin is thus
divided by about a tenth of the distribution, and the live classify margin p50 of **0.985** is
inflated by roughly an order of magnitude. The gate under-escalates precisely where it is supposed
to earn its keep: hard contracts with many labels, where the mass is spread and little of it lands
on a matched token.

Fixing the denominator in place was rejected. Three consumers compare a margin against **stored
history**, and a margin has no meaning without its scale:

- `internal/health` baselines a tier's margin against its own early history and route-skips on a
  collapse — a flag flip mid-ledger looks exactly like a quality collapse.
- the exemplar harvest gate compares against a matched-scale constant (0.6).
- conformal `calibrate` derives the per-task production cutoffs from stored margins.

A silent re-scaling would have handed all three a mixture of two distributions ~10x apart. Worse,
the calibrated matched-scale threshold **0.65** applied to a ~0.097-mass scale turns a gate that
never fires into one that fires on everything.

## Decision

1. `internal/confidence` computes both scales in one pass. `Margin` keeps its exact behaviour (the
   matched scale). `MarginDetail` / `MarginFull` add the full-denominator margin, `DeclaredMass`
   (matched / all, 0..1) and `Ambiguous` — the count of alternatives credited to NO class because
   they prefix more than one, which is how taxonomy-shaped label sets like `{billing,
   billing_dispute}` silently lose real mass.
2. `confidence_margin_full_denominator` (default **false**) selects the scale.
   `confidence_margin_threshold_full` (default **0**) is the full scale's OWN threshold. **0 means
   the margin gate never fires on the full scale** — announced once per process, never silently.
   The matched-scale constant and the per-task conformal thresholds are never consulted on the full
   scale.
3. Every row (`core.Meta`, `ledger.Entry`) carries `margin_scale` (`matched` | `full`; **empty on a
   pre-flag row reads as matched**), `margin_declared_mass` and `margin_ambiguous`. The last two are
   recorded in BOTH modes, so the data needed to derive a full-scale threshold accumulates BEFORE
   anyone flips the flag.
4. The three history-reading consumers filter to one scale and report the rows they excluded.
5. The flag is flipped only with a threshold re-derived from rows carrying `margin_scale: full` and
   passed through the openjev / gold-set gate.

## Consequences

- With the flag off nothing changes but three new, omitted-when-zero row fields. Existing ledgers,
  thresholds and baselines stay valid.
- The re-derivation data accrues immediately: `margin_declared_mass` on live rows is the measurement
  of how much mass the gate has been ignoring, per task and per label set.
- `margin_ambiguous` makes the `classOf` zeroing rate visible for the first time. It is a fact, not
  yet a fix: an ambiguous token is still credited to no class on both scales.
- Turning the flag on without a threshold DISABLES the margin gate. That is the intended failure
  mode — a disabled gate that says so beats a gate silently comparing across scales — but it means
  a flip with no threshold loses the escalation signal until one is set.
- The meta-router's `mr-verifier` reads `meta.margin` as a graded confidence. Its CC1 ceilings were
  measured on matched-scale numbers and must be re-measured after any flip. No change is made in
  that repo here.
- health and calibration each analyse fewer rows after a flip, until the full-scale rows accumulate.
  Both now print the excluded count rather than quietly shrinking their n.

## Alternatives considered

- **Fix the denominator in place.** Cheapest to write, and it silently invalidates every stored
  margin, both baselines and all per-task conformal thresholds at once. Rejected.
- **Keep the matched scale and widen `top_logprobs`.** More of the legal mass would be seen, but the
  denominator would still exclude unmatched tokens; it treats a symptom and costs serving work.
- **Reuse `confidence_margin_threshold` for both scales.** The 0.65 constant on a ~0.097-mass scale
  fires on nearly every call. Rejected outright — the full scale gets its own key or no gate.
- **Report both margins and let each consumer choose.** Two live numbers with no single answer to
  "did this call escalate?"; the scale marker gives the same auditability with one gating number.

## Related code

- [`../../../internal/confidence/confidence.go`](../../../internal/confidence/confidence.go)
- [`../../../internal/pipeline/pipeline.go`](../../../internal/pipeline/pipeline.go)
- [`../../../internal/config/config.go`](../../../internal/config/config.go)
- [`../../../internal/core/types.go`](../../../internal/core/types.go)
- [`../../../internal/ledger/ledger.go`](../../../internal/ledger/ledger.go)
- [`../../../internal/health/health.go`](../../../internal/health/health.go)
- [`../../../internal/calibration/calibration.go`](../../../internal/calibration/calibration.go)

## Related docs

- [../../systems/offload-pipeline.md](../../systems/offload-pipeline.md)
- [../../flows/cascade-escalation-and-defer.md](../../flows/cascade-escalation-and-defer.md)
- [../../glossary.md](../../glossary.md)
