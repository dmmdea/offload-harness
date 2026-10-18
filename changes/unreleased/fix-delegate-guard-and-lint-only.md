### Fixed - a structured re-pack that counts upward is read as a loop, and the retry is told not to repeat

- **What was wrong.** A seat writing a grammar-constrained re-pack can run out of real items and count: `"349", "350",
  "351", ...`. Every item differs from the one before, so neither the byte-level repeat test nor the line-block test in
  `degenerateTail` had a block to find, and the cut was filed as "the budget was too small". On a delegated run of
  2026-10-15 (a 9B seat, a 4,729-character answer) that sent the second request to the 8,192-token cap with the same
  sampling; the seat counted on to 1482 and that request spent 145 s of a 197 s re-pack, after which the attempt told
  not to repeat finished the object in 14 s.
- **What it does now.** `degenerateTail` reads one more shape, last, so every output the earlier tests named keeps its
  words: a run of numbers that rise by exactly one, with the same text between every pair, that spans at least
  128 bytes, has at least 12 numbers, and reaches the end of the output (only the start of the next item follows the
  last number). It is filed as `degenerate (counting up one at a time, N numbers from A to B)`, so the retry is the
  conditioned one (frequency penalty and the do-not-repeat sentence) at the budget the first request had, not an
  escalation to the cap. The 12 and the 128 are chosen, not measured. The limit is stated in the code: it counts by one,
  so a loop that counts by two, counts down, or carries other digits in each item is read as before.
- **Tests, red first.** `TestDegenerateTailReadsAMonotoneCountingLoop` (seven loop shapes: quoted numbers on one line cut
  inside an item, inside the separator and exactly after a number, across a power of ten, a label and a number on each
  line, bare numbers on each line, unquoted numbers; and seven spared shapes: three years closing a list, nine
  consecutive numbers, twenty-five years that close the list, a count followed by prose, a numbered list of distinct
  items, a count that restarts, numbers that jump) and `TestRunAgentTaskRepackAfterACountingLoopIsNotEscalatedToTheCap`
  (end to end against a fake seat, with a fixture that is first shown to be escalated when the loop is not recognised).
  On the unmodified code both failed: the seven loop shapes read `degenerateTail = ""`, and the second attempt was a
  plain request to the cap (`the attempt after a counting loop is not conditioned against repeating`).

### Changed - every tier that binds an agent model states its agent completion budget (D-24)

- **What changed.** `TestAReasoningSeatSeedsItsCompletionBudget` walked only the tiers whose vLLM seat declares a
  `reasoning_parser`; four more tiers that bind an agent model (`blackwell-32`, `blackwell-48`, `blackwell-72`,
  `ampere-6`) seeded no `agent_max_tokens` and inherited the loop's 1,024 without saying so. The new
  `TestEveryAgentSeatingTierStatesItsCompletionBudget` walks every tier that binds an agent model, in its seed or in a
  RAM overlay, and requires one of three statements: a seeded `agent_max_tokens`, `agent_thinking: "off"` (the
  exemption), or the new tier-level marker `agent_budget_policy: "loop-default-chosen"`. A tier whose vLLM seat declares
  a `reasoning_parser` may use neither the marker nor the exemption; the marker is refused beside a number, beside the
  exemption, on a tier with no agent model, and with any other value. `TestBudgetGuardRules` pins each rule on a
  synthetic tier.
- **The marker is chosen, not measured** (operator decision D8). The four tiers above carry it, their pages in
  `docs/tiers/` say so in a "Completion budget" paragraph (rendered by `internal/tierdocs`), and the field is
  documented in `profiles.json` `_fields` and in `docs/systems/setup-installer.md`. It is not a `config_seed` key and
  never reaches a node's `config.json`. A seat window that measures a budget replaces it with a seeded
  `agent_max_tokens`.
- **Rollout note.** The marker changes the four tiers' `profiles.json` entries, so a serving config rendered from the old
  entry reads `STALE(profiles_entry_sha256)` in `audit-yaml --against-render` until it is re-rendered.
- **Tests, red first.** On the unmodified code the widened guard named exactly the four tiers
  (`tier "blackwell-32" binds an agent model and states no completion budget`, and `-48`, `-72`, `ampere-6`), and
  `TestAMarkedTierPageSaysItsBudgetIsChosenNotMeasured` failed for each marked tier before the page rendered the note.

### Changed - `ledger` says a total that reaches back before 0.117.7 undercounts (D-96)

- Before 0.117.7 the ledger priced the tokens a local seat generated at zero (the estimated Opus value was the input
  rate only), so a dollar figure summed over older rows is an undercount. The history is not restated, by design, which
  makes the line under the figure the place to say it. `local-offload ledger` now prints, under `tokens kept local
  (est.)`, `note: rows written before 0.117.7 (2026-09-14) priced generated tokens at zero, so a total that reaches back
  before then undercounts; those rows are not restated` whenever the window can reach back before the fix: a lifetime
  read, or a `--since` long enough to cross 2026-09-16 (the cut-off is one UTC day after the release, chosen, because a
  ledger row does not record the build that wrote it). A `--since 1` read prints nothing extra.
- **Test, red first.** `TestLedgerSaysLifetimeTotalsUndercountRowsBeforeOutputWasPriced` failed on the unmodified code
  (`a lifetime ledger read does not carry the pricing caveat`).

### Changed - the docs say which instrument answers which question (D-73)

- `docs/systems/opencode-integration.md` (the instrument row) now says that the dispatch-log scoreboard measures the
  lead's effort and has the lead's own turns as its denominator, and that the share gate and every savings figure read
  the ledger (`origin_session`, `cards_tokens`) with tokens as the denominator, so the two are never compared as one
  number.
