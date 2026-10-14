### Added - one measurement gate (local-offload gate)

- **What it does.** `local-offload gate <freeze|check|verdict|schema>` and `eval.Gate` put every adoption (a seat bake, a
  compaction change, a cascade cut, a skill proposal) behind one statistic and one set of rules. `freeze` writes a
  pre-registration (mode, primary metric with its margin and, for a `custom` class, its score range, the judge, efficiency
  metrics, `min_pairs`, bootstrap size and seed, held-out ids, declared tasks, an optional frozen incumbent) as canonical
  bytes, hashes the corpus into it and prints the baseline hash; the output is created exclusively, so a changed
  pre-registration is a new file with a new hash. `check` refuses a baseline that is not byte for byte what `freeze`
  writes: a one-byte edit of a frozen baseline either fails `check` or changes its hash, and `check --expect-hash` and the
  input's `baseline_hash` refuse it. `verdict` compares a candidate arm with the incumbent arm under that baseline and
  prints ADOPT, BLOCK or INCONCLUSIVE with `delta`, `lo`, `hi`, `n_used`, the excluded pairs and share, per-arm outcome
  counts, the cheat counts, `imputed_attempts` and per-arm `unusable_attempts`, one paired interval per efficiency metric
  (`delta`, `lo` and `hi` are null, never 0, below two resolved pairs, plus `over_unresolved_cap`, `regresses` and the
  `level` it was read at), the held-out read (`missing` when no pair is usable), a power note, both runs' stamps, the judge,
  an era marker, the baseline and corpus hashes and the verb's own build hash, with no timestamp (the same input twice is
  byte-identical). `schema` prints the gate-input JSON Schema, whose `outcome` enum is the Go list and whose receipt hashes
  are 64 lower-case hex. The statistic is `eval.BootstrapDeltaMean`, called unchanged (its results are pinned by a test)
  over the usable non-held-out task pairs in sorted id order.
- **Scoring.** Every candidate attempt enters its task's mean: a `pass` or `task_fail` with its score; a `cheat`, a
  `timeout`, an `infra_error`, a `judge_error` and a `not_run` at the worst score of the primary range (the bad end given
  the direction: 0 or 1, 0 or 10, or an end of the declared custom range); a declared task the candidate lacks, or holds
  with no attempt, as one `not_run`. A candidate cannot raise its mean by not finishing what it would fail. The incumbent
  is not imputed: its cheat enters at the worst score, its unfinished attempts are skipped, and a task with no usable
  incumbent attempt is an excluded pair. The excluded share is the largest of the excluded-pair, incumbent-attempt and
  candidate-attempt ratios; above 0.20 the verdict is INCONCLUSIVE, as is fewer than `min_pairs` usable pairs. Held-out
  pairs are scored the same way and can veto an ADOPT; with none usable an ADOPT becomes INCONCLUSIVE ("held-out evidence
  missing"), not BLOCK. An unresolved cost is JSON `null` end to end and never 0, and an imputed attempt carries no cost.
- **Verdicts.** Superiority adopts iff `lo > 0` and blocks iff `hi < 0`. Non-inferiority blocks iff `hi < -m` and adopts iff
  `lo > -m` with a declared efficiency metric (tokens, money or memory in GiB, finite and not negative) improving, none
  got significantly worse, and none was left unmeasured. With k declared metrics each is read on the `1 - 0.05/k`
  interval (Bonferroni); a metric regresses when its plain 95% interval lies wholly on the worse side; a metric whose
  unresolved pairs exceed `max_excluded_share` cannot improve and blocks the ADOPT. Superiority reports efficiency and
  ignores it for the verdict. The gate has no effect-size floor: a caller's own bar stays that caller's rule. A model
  judge needs a passed receipt for the same scorer.
- **Pre-registration floors and the frozen judge.** Freeze and check refuse `bootstrap_b` below 2000, `min_pairs` below 16
  or above the non-held-out tasks (on null data the earlier settings adopted in 5.4% of runs at 8 pairs and 3.1% at 16
  pairs with `B = 2000`, against a nominal 2.5%, and in 45% at `B = 1`), a non-inferiority margin at or above the width of
  the primary range, and a `custom` primary without a valid range. The judge (kind and identity) is frozen in the baseline
  and the input must match it; `corpus_hash` is an echo, and the adapter that turns runs into arms is the trust boundary
  for corpus content. The gate version is 2, so a baseline written under version 1 is refused and re-frozen.
- **Exit codes and the rest.** 0 a verdict was produced (and `-h`, as the other verbs do), 1 a non-ADOPT under
  `--require-adopt`, 2 a refusal, a result that could not be written, or a usage error; a global `--config <path>` before
  `gate` is accepted and ignored. Freeze refuses any efficiency class but `tokens`, `money` and `memory_gib`, and any
  metric or primary name that carries a wall-time word, citing INV-5. The attempt enum is `eval.AttemptOutcome` (the name
  `eval.Outcome` is already the cascade eval's per-case struct), with `AllOutcomes` and `OutcomeFromRecord`, which maps an
  external record's outcome word (`not_checked` is `not_run`). `main.go` gains one case pair and one help line; the rest
  is `gate_cmd.go` and `internal/eval`.
- **Why.** The program's earlier proposals used three different pass rules, and this repository holds the pieces of two
  (`BootstrapDeltaMean` with a `B` and seed taken from each call site and its interval read by each caller, and ADR 0080's
  0.5 effect-size bar with no interval), so the same evidence could adopt under one and block under another, and a rule
  chosen after the numbers are seen is not a rule. The statistic already existed; the rules around it (scoring,
  margins, small n, binding the rules and the judge to the result) did not. `agent-trajectory-gate` and the compaction A/B
  (`compeval.RunAB`) keep `B = 2000`, `seed = 42` and are listed as legacy, not migrated. Docs:
  `docs/systems/measurement-gate.md` (new, linked from `docs/README.md`) and ADR 0088 (Proposed).
- **Tests.** Red first, twice: the gate's types and a stub were committed with the rule tests failing, then the gate; and
  after an adversarial review, a second red commit of attack tests (unfinished attempts, few resamples, a swapped judge, an
  unmeasured or costlier metric, a wide margin) before the fixes. `internal/eval` tests: a verdict table, boundary rows
  built from exactly representable numbers (16 pairs, scores 8.0, 7.5, 7.0), the refusal rows, imputation on every class
  and both arms, the three-ratio excluded share, held-out veto and missing evidence, the floors, the frozen judge and
  receipt shapes, the efficiency vetoes and the Bonferroni read, a byte-for-byte pin of `BootstrapDeltaMean`, a null
  ADOPT-rate simulation (3000 trials at 16 pairs and `B = 2000`, at most 4%), determinism, the era marker, the power note,
  freeze then check byte-identical, every byte of a frozen baseline edited, and the schema's enum and properties pinned to
  the Go types. Root tests drive the verb in-process (freeze, check, verdict round trip, exit codes 0, 1 and 2, a failed
  write of the result on each verb, `-h`, write-once freeze, the INV-5 refusals, `schema`). Mutation checks, each proved
  applied (`git diff --quiet` exit 1 after the edit, exit 0 after the restore): the imputation switch, the cheat score, the
  held-out zero-pair rule, both floors, the `min_pairs` bound, each judge mismatch and each receipt hash check, the
  negative-metric refusal, the unresolved cap (flag, verdict and the forced no-improve), the regression veto in both
  directions, the Bonferroni level, the margin bound and each of the four write-error wraps: all 26 turned a test red.
