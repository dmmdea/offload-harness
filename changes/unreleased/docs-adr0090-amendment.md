### Changed - ADR 0090's pending rows are settled

- **What.** Section 4 of ADR 0090 (capability review 2026-10) gives a terminal state to every row that was waiting on a gate verdict:
  - the Headroom offload rungs are PARKED (merged, default off, not measured);
  - the CLM paper is DECLINED as measured with no need (the ladder fired on 0 of 5 recorded agent runs in the registered window; no run evicted anything);
  - GitSwarm is DECLINED as replicated and measured with no gain;
  - the acceptance-hygiene ADR is DECLINED.

  ADR numbers 0092 and 0093 are not used.
- **Why.** Since 2026-10-18 a model or loop is measured only on the operator's order, with one run, so no further pre-registered A/B will settle these rows.
