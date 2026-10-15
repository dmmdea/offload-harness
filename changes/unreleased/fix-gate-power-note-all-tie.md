### Fixed - the gate's power note projects a pass/fail record with the interval the gate reads it on

- **73 ties, not 240.** The power note projected every record's half-width as `hw * sqrt(n / n')`. A pass/fail record
  (every pair a tie, a win or a loss of the whole range) is read on Tango's interval, whose half-width for an all-tie
  record is `z^2 / (n + z^2)` and shrinks about as `1 / n`. So 16 tied tasks read +-0.194, the gate resolves a 0.05 margin
  at 73 ties, and the note asked for 240, more than three times what a saturated pair needs. For a pass/fail record the
  note now projects with Tango's interval at the projected size, holding the observed shares of wins and losses, and
  `tasks_needed` is the smallest count whose half-width is below the margin (or the observed effect in superiority). Every
  other record keeps the `1 / sqrt(n)` form. No verdict changes: the note adds no statistic.
- `TestGatePowerNoteAllTiePassFailUsesTheGatesOwnInterval` (16 ties, margin 0.05) failed on the parent with
  `tasks_needed = {N:240}, want 73`; `TestGatePowerNote` now pins the Tango projection for its pass/fail superiority record
  and the `1 / sqrt(n)` projection for its non-pass/fail record. A mutant that turns the Tango branch off fails both.
