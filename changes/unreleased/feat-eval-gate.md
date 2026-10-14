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
  are 64 lower-case hex. The primary is the mean of the paired task differences over the usable non-held-out pairs in
  sorted id order, with a 95% interval in the score form of the Student-t interval (`delta -+ c * sn / sqrt(n - c^2)`,
  `c = t(n-1, .975)` from a table, `sn` the divisor-n standard deviation; see Calibration). When every difference is equal
  there is no spread to read, and `lo` is the true mean below which the sample would be 2.5% likely for scores in the
  primary's range (`q (w + c) - w`, `q = 0.025^(1/n)`, `w` the range width, `c` the one value; exact for that bound), while
  `delta` and `hi` stay the value: a margin of 0.05 of the range is certified by 80 ties and not by 40, a superiority tie is
  INCONCLUSIVE, and a constant win still ADOPTs. `eval.BootstrapDeltaMean` is unchanged (its results are pinned by a test)
  and the paired bootstrap reads the efficiency metrics.
- **Scoring.** Every candidate attempt enters its task's mean: a `pass` or `task_fail` with its score; a `cheat`, a
  `timeout`, an `infra_error`, a `judge_error` and a `not_run` at the worst score of the primary range (the bad end given
  the direction: 0 or 1, 0 or 10, or an end of the declared custom range); a declared task the candidate lacks, or holds
  with no attempt, as one `not_run`. A candidate cannot raise its mean by not finishing what it would fail. The incumbent
  is not imputed: its cheat enters at the worst score, its unfinished attempts are skipped, and a task with no usable
  incumbent attempt is an excluded pair. The excluded share is the largest of the excluded-pair, incumbent-attempt and
  candidate-attempt ratios; above 0.20 the verdict is INCONCLUSIVE, as is fewer than `min_pairs` usable pairs. Held-out
  pairs are scored the same way and can veto an ADOPT; with none usable an ADOPT becomes INCONCLUSIVE ("held-out evidence
  missing"), not BLOCK. Cost follows whether the attempt ran: a candidate's `cheat`, `timeout`, `infra_error` or
  `judge_error` that recorded a metric contributes that cost to its task's mean (it ran and spent it), a `not_run` carries
  nothing, the incumbent's unfinished attempts are skipped cost included, and an unresolved cost is JSON `null` end to
  end and never 0. A candidate's `timeout`, `infra_error` or `judge_error` that recorded no cost leaves its whole task
  unresolved on that metric, so a candidate cannot keep the savings of the attempts around it by leaving the cost of what
  timed out unrecorded (over the unresolved cap that blocks a non-inferiority ADOPT). A candidate that passed 24 of 25 attempts at 900 tokens and timed out on the 25th after burning 20000
  (1664 tokens a task, 1.66 times the incumbent's) is no longer an efficiency win. The incumbent exclusion ("the candidate
  does not run those") holds for a frozen or trusted incumbent; one supplied in the input can use it as a lever, up to the
  cap.
- **Verdicts.** Superiority adopts iff `lo > 0` and blocks iff `hi < 0`. Non-inferiority blocks iff `hi < -m` and adopts iff
  `lo > -m` with a declared efficiency metric (tokens, money or memory in GiB, finite and not negative) improving, none
  got significantly worse, and none was left unmeasured. With k declared metrics each is read on the `1 - 0.05/k`
  interval (Bonferroni); a metric regresses when its plain 95% interval lies wholly on the worse side, read from two
  resolved pairs (only an improvement needs `min_pairs`; memory ten times worse on 15 of 18 pairs and null on 3 vetoes
  now); a metric whose unresolved pairs exceed `max_excluded_share` cannot improve and blocks the ADOPT. Superiority
  reports efficiency and ignores it for the verdict. The efficiency reads stay on the percentile bootstrap; their false
  improvement rate on cost nulls is 3.5% to 5.9% at 16 tasks (a nominal 2.5%), documented, and a false improvement
  adopts nothing alone. The gate has no effect-size floor: a caller's own bar stays that caller's rule. A model
  judge needs a passed receipt for the same scorer.
- **Calibration of the primary.** The percentile interval of the paired bootstrap, which the gate read first, was measured
  anti-conservative: over 20000 simulated datasets per cell it adopted in 3.8% (uniform scores), 3.8% (scores in tenths)
  and 4.1% (x^3-skewed scores) of superiority runs at 16 tasks and in 6.1% of non-inferiority runs at exactly -margin with
  20 tasks and pass rates .90 and .85 (10.0% with 16 tasks), against a nominal 2.5%. Five candidate intervals (studentized
  bootstrap, BCa, the percentile widened by t/z, the paired t, the lower of two) were run on the same datasets at
  `B = 2000` and `B = 10000`: none stays at or under 4% in every null cell (the best, the paired t, 4.6% at 20 tasks). The
  score form of the t interval is the best single interval: of the 63 null cells of the study, 61 are at most 3.6%. A
  fourth review found that a sample whose pairs are all equal, read as one point, ADOPTed a non-inferiority claim that
  carried an efficiency win: pass rates .95 and .90 at 16 tasks 9% of the time, .99 and .94 32%, a candidate that only
  loses 5% of the incumbent's passes 37% at 20 tasks. With the lower end held back those cells are 0.0% to 3.3% at 16 to 40
  tasks (the 80-task cell of the .99/.94 incumbent, 4.8%, and of the nested-loss candidate, 8.2%, are not fixed: a strong
  incumbent and a handful of disagreeing tasks at 60 tasks and more is still anti-conservative, up to 9% in the stress
  cells, and the lower of the score form and BCa, 0 of 63 cells over 4% and 3.4% at worst, is left to the lead because it
  couples the primary to `bootstrap_b` again). The price is power (at 24 tasks a true +0.1 gain on a pass/fail score is
  adopted in 8.6% of runs against 10.7% for the plain t; 16 ties no longer certify a margin of 0.05, so a candidate truly
  equal to a .90 incumbent is adopted in 1.6% of runs at 16 tasks and 7.9% at 24). Not covered: the sparse regime above
  and the efficiency reads. `TestCalibrationStudy` prints the grid and is off unless `OFFLOAD_GATE_STUDY=1`; its output is
  identical at 24 and at 30 workers.
- **Pre-registration floors and the frozen judge.** Freeze and check refuse `bootstrap_b` below 2000, `min_pairs` below 16
  or above the non-held-out tasks (the calibration was measured from 16 tasks up; on cost nulls at 16 tasks a metric
  falsely improved in 8% to 13% of runs at `B = 10` and 42% to 50% at `B = 1`, against 3.5% to 5.9% at 2000), a
  non-inferiority margin at or above the width of the primary range, and a `custom` primary without a valid range. The
  judge (kind and identity) is frozen in the baseline and the input must match it; `corpus_hash` is an echo, and the
  adapter that turns runs into arms is the trust boundary for corpus content. The gate version is 2 and stays 2 (the gate
  has never been released and no version 2 baseline exists outside tests): a baseline of another version is refused first,
  for its version, with the advice to re-freeze it. A baseline frozen by the version 1 gate used to get "edited by hand?".
- **Exit codes and the rest.** 0 a verdict was produced (and `-h`, as the other verbs do: read the verdict from stdout,
  not the exit code alone), 1 a non-ADOPT under `--require-adopt`, 2 a refusal, a result that could not be written, or a
  usage error; a global `--config <path>` before `gate` is accepted and ignored. Freeze refuses any efficiency class but `tokens`, `money` and `memory_gib`, and any
  metric or primary name that carries a wall-time word, citing INV-5. The attempt enum is `eval.AttemptOutcome` (the name
  `eval.Outcome` is already the cascade eval's per-case struct), with `AllOutcomes` and `OutcomeFromRecord`, which maps an
  external record's outcome word (`not_checked` is `not_run`). `main.go` gains one case pair and one help line; the rest
  is `gate_cmd.go` and `internal/eval`.
- **Why.** The program's earlier proposals used three different pass rules, and this repository holds the pieces of two
  (`BootstrapDeltaMean` with a `B` and seed taken from each call site and its interval read by each caller, and ADR 0080's
  0.5 effect-size bar with no interval), so the same evidence could adopt under one and block under another, and a rule
  chosen after the numbers are seen is not a rule. The bootstrap already existed; the rules around it (scoring,
  margins, small n, binding the rules and the judge to the result) did not, and its percentile interval turned out not to be
  the right one for the primary. `agent-trajectory-gate` and the compaction A/B
  (`compeval.RunAB`) keep `B = 2000`, `seed = 42` and are listed as legacy, not migrated. Docs:
  `docs/systems/measurement-gate.md` (new, linked from `docs/README.md`) and ADR 0088 (Proposed).
- **Tests.** Red first, three times: the gate's types and a stub were committed with the rule tests failing, then the gate;
  after an adversarial review, a second red commit of attack tests (unfinished attempts, few resamples, a swapped judge, an
  unmeasured or costlier metric, a wide margin) before the fixes; and after a third review, red tests for an unfinished
  candidate attempt's cost, a regression on few resolved pairs, the false-ADOPT cells and the refusal a version 1 baseline
  gets, each before its fix. `internal/eval` tests: a verdict table, boundary rows built from exactly representable
  numbers (16 pairs, scores 8.0, 7.5, 7.0), the refusal rows, imputation on every class and both arms, the cost of each
  attempt class on each arm, the three-ratio excluded share, held-out veto and missing evidence, the floors, the frozen
  judge and receipt shapes, the efficiency vetoes and the Bonferroni read, a byte-for-byte pin of `BootstrapDeltaMean`,
  determinism, the era marker, the power note, freeze then check byte-identical, every byte of a frozen baseline edited, the
  schema's enum and properties pinned to the Go types, and a baseline as the version 1 gate wrote it (raw bytes, hash
  pinned, through `CheckBaseline` and through the verb). Calibration: `Gate` run end to end at the floors on eleven null cells
  (at most 4% over 4000 to 10000 trials, fixed seeds, skipped under `-short`; 0.0% to 3.0% measured), the interval itself
  over 50000 datasets on fifteen cells (0.0% to 3.1% measured), the interval against numpy and scipy computations and against
  its own definition (the statistic equals the critical value at both ends), the t table against scipy, and the study
  harness pinned to `BootstrapDeltaMean` bit for bit, to scipy's quantiles and to the moments of its generators. The
  `internal/eval` tests take about 6 s with the calibration cells (about 2 s with `-short`) on a 36-core machine; the cells
  split their trials over `GOMAXPROCS`, so a 4-core runner takes a few times that. Root tests drive the verb
  in-process (freeze, check, verdict round trip, exit codes 0, 1 and 2, a failed write of the result on each verb, `-h`,
  write-once freeze, the INV-5 refusals, `schema`, the version 1 refusal). Mutation checks, each proved applied (`git diff
  --quiet` exit 1 after the edit, exit 0 after the restore): the imputation switch, the cheat score, the held-out zero-pair
  rule, both floors, the `min_pairs` bound, each judge mismatch and each receipt hash check, the negative-metric refusal,
  the unresolved cap (flag, verdict and the forced no-improve), the regression veto in both directions, the Bonferroni
  level, the margin bound and each of the four write-error wraps: all 26 turned a test red; and in the third round a
  candidate's unfinished cost dropped, a `not_run`'s cost counted, the incumbent's unfinished cost counted, a null cost
  counted as a zero in the divisor, a regression read only from `min_pairs`, the primary back on the percentile bootstrap, the zero-variance value read from a sum, the
  plain t interval, a nudged critical value, the version probe disabled, the version probe blind to trailing bytes, and
  three in the harness (percentile index, generator bias, quantile bisection): all 14 turned a test red; and in the fourth
  round the zero-spread lower end off, compared by exact equality, at the wrong tail, with the wrong sign, with the range
  width forgotten, and the unrecorded-cost rule off and its flag never set: all 7 turned a test red. Red first again: the
  failing cells and tests came in their own commit.
