### Fixed - a single-model cascade is now visible in doctor and on the ledger

- **What changed.** `doctor` prints an `escalation:` line that reads `UNAVAILABLE` when `escalation_model` is empty or equals `model` (the confidence gate is inert), `BROKEN` when the alias is not in the live roster, and `OK` otherwise. The pipeline's confhead gate, when skipped for that reason, sets `feat.escalation_unavailable = 1` on the call's ledger row. The profile seeds already blank `escalation_model` for every tier that drops the 26B and serves no other escalation model, so `setup/templates/profiles.json` is unchanged.
- **Why.** With no distinct escalation model the cascade could only "escalate" to the same weights, and nothing on the box or in the ledger said so (issue #8).
- **Check.** `doctor` run against a config whose `escalation_model` equals its `model` prints the `UNAVAILABLE` line. Closes #8.
