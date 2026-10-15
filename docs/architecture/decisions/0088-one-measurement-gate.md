---
status: Proposed
date: "2026-10-14"
---

# 0088 — One measurement gate for every adoption

Release: unreleased (rules R1 to R8 of the measurement gate; `docs/systems/measurement-gate.md` has the plain-words version)

## Context

Every change that swaps a seat, cuts a rung of the cascade, changes a compaction ladder or adopts a proposed skill needs
a pass rule, and the program's earlier proposals used three different ones. This repository holds the pieces of two:

- `eval.BootstrapDeltaMean` (a paired bootstrap of the mean difference with a 95% interval), whose `B` and seed each call
  site chooses and whose interval each caller reads its own way: `agent-trajectory-gate` turns it into a verdict and the
  compaction A/B only reports it (both pass `B = 2000`, `seed = 42`);
- a 0.5 effect-size bar on a 0 to 10 judge scale, with no interval, from
  [ADR 0080](0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md).

Different pass rules mean the same evidence can adopt under one and block under another, and a rule chosen after the
numbers are seen is not a rule. Meanwhile the statistic that should have been the common ground already existed and was tested:
`BootstrapDeltaMean` resamples aligned per-item scores at the same index for both arms and returns the mean difference with
its interval. What was missing was everything around it: what counts as a usable attempt, what a margin is, what happens
at small n, and a way to bind the rules to the result so that they cannot move afterwards. Whether its percentile interval
was the right interval for the primary was measured later, and it was not (decision 12).

[ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) already fixes one constraint on any such gate:
acceptance criteria and pass rules carry no wall-time term (INV-5).

A first version of the rules below was attacked the way a candidate that wants to be adopted would attack it, and the
attacks worked: a candidate that timed out on 4 of 20 tasks was adopted at the 0.20 excluded-share cap, four times the
0.05 margin; a candidate whose other attempts did not finish, against an incumbent that finished and failed them, was
adopted; a scorer other than the frozen one was accepted; a margin of 2 on a [0, 1] score let a candidate that failed
every task through on cost. Rules 2, 3 and 6 to 8 below, and the floors, close those holes; each says why.

A later review measured the primary interval itself and attacked the efficiency read once more. On null data the percentile
interval of the paired bootstrap said ADOPT in 3.6% to 4.1% of simulated superiority runs at 16 tasks and in 6.35% of
non-inferiority runs at exactly the margin (20 tasks, pass rates .90 and .85, tokens ten times cheaper), against a nominal
2.5%. A candidate that passed 24 of 25 attempts at 900 tokens and timed out on the 25th after burning 20000 (1664 tokens a
task, 1.66 times the incumbent's 1000) was an ADOPT on "efficiency improved", because a timeout's cost never reached the
mean. And a metric resolved on 2 to `min_pairs - 1` pairs could neither improve nor regress, so memory ten times worse on
15 of 18 pairs (null on the other 3) was neutral. Decisions 2, 3 and 12 close them.

A fifth review measured the interval exactly where the arms barely disagree: a strong incumbent, a candidate at exactly the
margin, a handful of tasks that differ. The score form read a candidate that loses 5% of the incumbent's passes as an ADOPT in
4.4% of samples at 61 tasks, 17.7% at 62, 16.4% at 64, 11.8% at 100, 6.5% at 250 and over 4% to about 1500 tasks (exact binomial
sums), because one loss in 62 tasks leaves too little spread to read; pass rates .99/.94 were adopted in 11.1% at 62 tasks. Decision
12 replaces the interval, and the gate version moves to 3.

## Decision

One gate, `eval.Gate`, a pure function over a frozen baseline and one comparison, with a verb (`local-offload gate`).
The gate version is 3: a baseline written under another version (1 or 2) is refused, first and for its version.

1. **R1, superiority.** A claim of "better" is ADOPT iff the interval's lower end is above 0, BLOCK iff its upper end is
   below 0, else INCONCLUSIVE. The statistic is the mean of the paired task differences over the usable pairs in sorted id
   order, read on the 95% interval of decision 12; the unit is the task. `BootstrapDeltaMean` is
   unchanged for its legacy callers and a test pins it; the paired bootstrap reads the efficiency metrics.
2. **R2, non-inferiority.** A cut or an efficiency change is BLOCK iff the upper end is below `-m`, and ADOPT iff the lower
   end is above `-m` **and** at least one pre-declared efficiency metric improves **and** none of them got significantly
   worse or went unmeasured; else INCONCLUSIVE. Efficiency is tokens, money or resident memory in GiB, finite and not
   negative. The default margins are 0.05 on a score in [0, 1] and 0.5 on a 0 to 10 judge scale; a `custom` class declares
   its own. Every comparison is strict. The efficiency read has five parts, each with its reason:
   - *Several metrics are read at the Bonferroni level.* "Any of k metrics improves" is a best-of-k, so false improvements
     would run at about k times the single-metric rate. With k declared metrics each is read on the two-sided `1 - 0.05/k`
     interval, computed from one bootstrap distribution by reading other percentiles; with one metric it is the plain 95%
     interval, identical to `BootstrapDeltaMean`. The level is in the result.
   - *A regression vetoes.* A metric regresses when its plain 95% interval lies wholly on the worse side, read wherever the
     metric has an interval (two or more resolved pairs); in non-inferiority mode any regression turns an ADOPT into
     INCONCLUSIVE. Without this a cut that saves tokens and costs ten times the memory was adopted. Read only from
     `min_pairs` resolved pairs, a metric resolved on 2 to `min_pairs - 1` pairs was neutral: memory ten times worse on 15
     of 18 pairs, null on 3 (an unresolved share of 0.167, under the cap), was an ADOPT. Only an improvement needs
     `min_pairs`. The plain interval is used for the veto because more vetoes is the conservative side.
   - *An unmeasured metric cannot be traded away.* A metric whose unresolved pairs over the usable pairs exceed
     `max_excluded_share` cannot improve and, in non-inferiority mode, blocks the ADOPT (INCONCLUSIVE, naming it). Without
     this a candidate could report null cost on the tasks where it is dearer.
   - *Superiority mode reports efficiency and ignores it for the verdict.*
   - *The efficiency reads stay on the percentile bootstrap.* They are not as well calibrated as the primary: on cost nulls
     with no real difference (independent lognormal and exponential costs and a paired multiplicative cost; 16, 24 and 40
     tasks) the plain 95% read says "improves" in 3.5% to 5.9% of runs at 16 tasks, 2.1% to 4.9% at 24 and 1.5% to 4.8% at
     40, and 0.6% to 3.0% per metric at the three-metric level, against 0.3% to 1.3% for the score form of the t interval (the
     primary's form at version 2) on the same data at 16 tasks (`TestEfficiencyCalibrationStudy`). Moving them was considered and not done. A false improvement
     adopts nothing alone, since the primary has to clear the margin as well, and the false-ADOPT cells of decision 12 are
     measured with an efficiency metric that truly improves, so they bound what a false improvement adds: a non-inferior
     candidate that is not cheaper. The veto side is the conservative one. And a closed form would leave `bootstrap_b` and
     `seed`, two pre-registered fields, reading nothing. The Bonferroni read is the one that was there, so it is exactly as
     conservative as it was.
   A metric that no task carries on both arms cannot improve, so a cut cannot be adopted on a missing metric.
3. **R3, the scoring rule.** Every candidate attempt enters its task's mean. A `pass` or `task_fail` enters with its score;
   a `cheat` enters at the worst score of the primary range; a `timeout`, `infra_error`, `judge_error` and `not_run` enter
   at the worst score too, and a declared task the candidate lacks, or holds with no attempt, counts as one `not_run`
   attempt. Every declared candidate task is therefore usable. The worst score is the bad end of the range given
   `higher_is_better`: 0 or 1 for `unit_interval`, 0 or 10 for `judge_0_10`, and the matching end of a required
   `primary.range = [lo, hi]` for `custom` (both finite, `hi` above `lo`, a finite width). A `pass` or `task_fail` score
   outside the range is refused at input.
   *Why the candidate is imputed.* A first version skipped unfinished attempts one by one, so a candidate whose other
   attempts timed out was judged on the one that passed. A timeout is a budget failure and is not a wall-time term, so
   INV-5 holds.
   *Why the incumbent is not.* The incumbent's `timeout`, `infra_error`, `judge_error` and `not_run` attempts are skipped
   (its cheat is not: it enters at the worst score), and a task with no usable incumbent attempt is an excluded pair. The
   asymmetry is the point: the incumbent keeps every tie, so only the candidate pays for the timeouts, infra errors, judge
   errors and missing runs it causes. Imputing the incumbent would charge the status quo for noise it did not make.
   The excluded share is the largest of three ratios: excluded pairs over declared main tasks, the incumbent's unusable
   attempts over its attempts, and the candidate's unusable attempts over its attempts (totals include the `not_run`
   placeholders). Above `max_excluded_share` the verdict is INCONCLUSIVE, as is a count of usable pairs below `min_pairs`.
   The default cap stays 0.20 and may sit above the margin: the candidate's unusable attempts already enter at the worst
   score, so the cap no longer bounds a free exclusion; the only exclusions left are incumbent attempts, which the
   candidate does not run. That holds for a frozen or trusted incumbent. An incumbent supplied in the input can use the
   exclusion as a lever (marking some of its own best attempts unfinished lowers its mean and drops tasks it wins), up to the
   cap, so a comparison that must not be steerable from the input freezes its incumbent. The result counts
   `imputed_attempts` and, per arm, `unusable_attempts`.
   *Cost follows whether the attempt ran.* A candidate's `cheat`, `timeout`, `infra_error` or `judge_error` that recorded a
   metric contributes that cost to its task's mean: it ran and spent it, and the worst score bounds how many of them a
   non-inferior candidate can carry. Without this a timeout carried a score and no cost, and a candidate that passed 24
   attempts at 900 tokens and burned 20000 on a timed-out 25th was adopted as an efficiency win at 1.66 times the
   incumbent's spend. A cost that was not recorded is `null` and stays unresolved, never 0 (so an adapter must record the
   cost of every attempt that ran). On a `pass`, `task_fail` or `cheat` it is left out of the task's mean. On a candidate's
   `timeout`, `infra_error` or `judge_error` it leaves the task's cost unresolved altogether: the same candidate with 24
   passes at 900 tokens and a timeout that recorded nothing kept an efficiency win of -100 tokens a task on 20 tasks, and
   recorded 30000 tokens on the timeout it regressed (+1064). The attempt that did not finish is the one likely to have
   spent the most, so the attempts around it must not stand for the task; over the unresolved cap that blocks a
   non-inferiority ADOPT, and an honest harness is told to record the cost of what timed out. The cap is the one lever
   left: a candidate that records no cost on its unfinished attempts can leave up to `max_excluded_share` of its tasks
   unresolved on that metric and keep its efficiency win on the rest. A `not_run`, explicit or the
   placeholder for a missing task, never ran and carries nothing; the incumbent's unfinished attempts are skipped, cost
   included.
4. **R4, the incumbent wins.** The incumbent keeps every tie and every INCONCLUSIVE. The remedy for an INCONCLUSIVE at the
   corpus size is more tasks, not more attempts, because attempts of one task are not independent; the result carries a
   power note (the half-width of the interval, projected to 16, 24 and 120 tasks, and the tasks to add) so the step is a
   number.
5. **R5, held-out.** A held-out slice is declared at freeze and scored once, as a point estimate, with the scoring of R3. It
   can turn an ADOPT into a BLOCK ("reject without re-tuning") and can rescue nothing. With no usable held-out pair an
   ADOPT becomes INCONCLUSIVE ("held-out evidence missing"), not BLOCK: no evidence is not a failed check
   (`held_out.missing`, and `held_out.failed` only when pairs exist).
6. **R6, pre-registration.** The mode, the primary metric, margin and range, the judge, the efficiency metrics,
   `min_pairs`, the bootstrap size and seed, the held-out ids and the declared tasks are frozen into a baseline file in
   canonical bytes and hashed before the first candidate run; the corpus is hashed into it, never stored. The gate refuses an
   input that names another hash, and `gate check` refuses a baseline that is not the frozen bytes. A one-byte edit of a
   frozen baseline either fails `check` or changes its hash, and `gate check --expect-hash` and the input's `baseline_hash`
   refuse it. A baseline of another gate version is refused first and for its version, with the advice to re-freeze it:
   a version 1 file has no `judge`, so its bytes are not this gate's canonical form, and it used to be refused as "edited
   by hand". Two floors are part of validation: `bootstrap_b` at least 2000 (the existing callers use 2000; the default
   stays 10000) and `min_pairs` at least 16 (the default), and `min_pairs` at most the number of non-held-out tasks.
   `min_pairs`: the primary's calibration was measured from 16 tasks up (decision 12), so 16 is the smallest sample the
   gate makes a claim about. `bootstrap_b`: the bootstrap reads the efficiency metrics, and a read from a handful of
   resamples is mostly noise: on the cost nulls of the study at 16 tasks a metric falsely improved in 8% to 13% of runs at
   `B = 10` and in 42% to 50% at `B = 1`, against 3.5% to 5.9% at 2000. `TestNullAdoptRateAtTheFloorsIsCalibrated` runs the
   gate end to end at the floors on six of the cells of decision 12 and requires at most 4%, and
   `TestPrimaryIntervalFalseAdoptRateOnTheStatedNulls` requires it of the interval alone over 50000 datasets per cell.
   A non-inferiority margin must be below the width of the primary's range (1, 10 or `hi - lo`): a margin as wide as the
   range lets a candidate that fails every task through on cost.
7. **R7, judge integrity.** The judge (kind and identity) is frozen in the baseline and the input must name the same
   one. Without that, a frozen incumbent could be re-scored by another scorer, and a model id could be declared
   `deterministic` to skip the receipt. A model judge needs a passed self-test receipt for the same scorer, with
   `pairs_hash` and `build_sha256` each 64 lower-case hex characters, or the gate refuses. Receipts are plain data in
   `internal/eval` (which must not import `internal/compeval`); the verb that produces them is a separate change.
   `corpus_hash` stays an echo of the corpus file given to freeze: the adapter that turns runs into arms is the trust
   boundary for corpus content, and the gate checks task ids against the declared list, not the text of a task.
8. **R8, eras.** The result carries both runs' stamps (harness build, seat configuration, engine build), the judge identity
   and an era marker, the SHA-256 of the compact JSON of those and the gate version. Results with different era markers
   are never compared; a comparison re-runs the incumbent in the new era.
9. **No wall-time term anywhere.** The efficiency class list is `tokens`, `money`, `memory_gib`; freeze refuses any other
   class, and any metric or primary name that carries a wall-time word, with a message that cites INV-5. The gate output
   holds no timestamp, so the same input twice gives byte-identical output.
10. **The gate has no effect-size floor, and ADR 0080's three gates stay as the resource gates for a spill seat.** The
    gate's verdict is the interval alone; it does not apply ADR 0080's 0.5 bar. A caller that wants that bar keeps it as
    its own rule beside the gate, and an ADOPT does not say it was met.
11. **Exit codes.** A result that cannot be written exits 2 with a `gate:` line, like every other gate failure, so that 1
    stays "the verdict is not ADOPT". `-h` prints the usage and exits 0, matching the verbs whose flags are parsed with
    `flag.ExitOnError`. So the verdict is read from stdout, not from the exit code alone: `-h` exits 0 without running
    anything, a refusal prints no verdict, and 0 from `verdict` means a verdict was produced, whichever it is
    (`--require-adopt` makes the exit code mean ADOPT).
12. **The primary is read on Tango's score interval for pass/fail pairs and on an augmented score form for every other
    score.** The primary is the mean of the paired task differences, `delta`, each difference in `[-w, w]` with `w` the
    width of the primary's range. Two constructions read its 95% interval, and the data decide which.
    *Pass/fail pairs.* When every difference is a tie, a win of the whole range or a loss of it (to within 1e-9 of the
    range), the dataset is three counts and the interval is Tango's score interval for a paired difference (Tango 1998,
    Statistics in Medicine 17:891): the set of null differences `mu` whose statistic
    `(wins - losses - n*mu) / sqrt(n * (u + v - mu^2))` stays within +-1.96, with the probabilities of a win and a loss
    `u` and `v` fitted under `u - v = mu` (the larger root of a quadratic, `tangoVar0`), inverted by 80
    bisections. Its variance is the one the hypothesised difference would give, not the one the sample showed, which is what
    holds it where the arms disagree on a handful of tasks. With no discordant pair the ends are `+-w z^2/(n + z^2)`:
    a margin of 0.05 of the range is certified by 73 ties and not by 72 (the chance of 73 ties at a true loss rate of 5% is
    2.4%; of 61 ties, which version 2 certified, 4.4%). This is the principled form of the fourth review's "rule of three" argument
    (`3/n`, the one-sided 95% bound on an event that did not occur in `n` trials), at the interval's own 2.5% level.
    *Every other pair of scores* (partial credit, a mean of several attempts, a judge's integer scale) has no lattice to put a
    null variance on. Its interval is the score form of the Student-t interval of the `n` differences and four pseudo-pairs,
    two at `+w` and two at `-w`: `delta -+ c * sn / sqrt(N - c^2)` with `N = n + 4`, `c = t(N-1, .975)` and `sn` the divisor-`N`
    standard deviation of the augmented sample about its own mean, clipped to `[-w, w]`. The score form takes its variance at
    the boundary instead of at the estimate. The pseudo-pairs are the idea of Agresti and Caffo (2000) and Agresti and Min
    (2005) of adding pseudo-observations to a small count, generalised to bounded scores: they inflate the variance when the
    differences are few, vanish as `n` grows, and give a sample of equal differences a standard error of its own, so the version 2
    rule of three, a special case, is gone. The centre is the observed mean, not the augmented one (which would shrink `delta`
    toward zero by `n/N`). `delta` is the observed mean difference, `bootstrap_b` and `seed` do not touch the primary, and
    with fewer than two pairs there is no interval and the numbers read 0. `c` is a table to 200 degrees of freedom and a
    three-term expansion beyond, so the gate needs no special function.
    *The weights are a choice, and say so.* The literal Agresti-Min weights (half a pair at each of the four cells of the 2x2
    table) read a candidate that loses 5% of the incumbent's passes at 8.6% over 80 tasks, one pair at each end (the weight the
    review's brief named first) leaves a mostly pass/fail score set with a few half-credit tasks at 4.4% over 160 tasks
    and a candidate that loses 5% of the incumbent's passes at 5.97% over 119 tasks, and two pairs at each end clears every
    null cell of the three grids. The fallback uses two. They are the smallest weights measured that do, not a derived
    quantity, and this is not a published form.
    A difference exactly on the margin, or an interval end that lands on it by float rounding, is neither ADOPT nor BLOCK: the
    verdict compares an end with its boundary to within 1e-9 of the score range, and pairs that are equal or whole-range on
    paper but not to the last bit (0.95 - 0.90 against 0.70 - 0.65; `0.1 + 0.2` against `0.3`) are read as such.
    *What was compared.* Two candidates, as the fifth review named them: pseudo-pair augmentation (for every pair of scores,
    in the three weights above) and Tango's interval, which for scores that are not pass/fail falls back on the augmented form.
    The study (`TestCalibrationStudy` and `TestSparseCalibrationStudy`, `internal/eval`, off unless `OFFLOAD_GATE_STUDY=1`)
    ran each on three grids and on the power rows, on the same datasets. Pass/fail cells are exact multinomial sums
    (a dataset of pass/fail pairs is three counts; there is no sampling error, the sum skips the datasets under 1e-14
    probability, under 1e-9 in total, and `TestExactSumAgreesWithSimulation` pins that the exact and simulated columns measure the
    same thing); the other cells are 20000 simulated datasets with seeds from the cell's name and size, a standard error of 0.11
    to 0.14 points near 2.5% to 4%. The grids are the 63 null cells of the earlier study, a sparse grid of 54 cells (a
    candidate that only loses tasks and loses 5% of them, pass rates .99/.94 and .97/.92, at 16, 24, 40, 60, 61, 62, 64, 80,
    100, 160, 250, 500 and 1000 tasks; the same shapes at a margin of 0.10, which are a candidate that loses 10%, pass rates
    .99/.89 and .97/.87, at 16, 31, 32, 40 and 80 tasks) and 49 stress cells for scores that are not pass/fail with few and
    coarse disagreements (partial credit, losses to half credit, losses of mixed size, means of 3 and 5 attempts), plus the
    envelope: the largest exact rate over every size from 16 to 500 and eleven shares of disagreeing tasks (nine for
    superiority), at both margins. False-ADOPT rate, per cent, worst cell and cells over 4%:

    | interval | 63 cells | 54 sparse cells | 49 stress cells | envelope, margin 0.05 / 0.10 / superiority |
    |---|---|---|---|---|
    | version 2: score form, rule of three | 8.61, 2 over | 17.73, 29 over | 15.48, 25 over | 17.73 / 15.64 / 2.48 |
    | half a pair at each of four cells (Agresti-Min) | 8.61, 2 over | 8.61, 12 over | 7.20, 18 over | 12.21 / 9.85 / 2.47 |
    | one pair at each end | 2.88, none | 4.61, 3 over | 4.42, 2 over | 5.97 / 5.30 / 2.50 |
    | two pairs at each end, for every pair | 2.25, none | 3.43, none | 3.22, none | 4.01 / 3.83 / 2.61 |
    | **Tango for pass/fail pairs, two pairs at each end otherwise (version 3)** | **3.04, none** | **2.44, none** | **3.21, none** | **3.01 / 3.11 / 2.71** |

    The decision rule was: the candidate with no cell over 4% and the best power. Two pairs at each end for every pair
    clears the three grids but reaches 4.01%, just over the bar, for a candidate that loses 5% of the incumbent's passes at 444 tasks, off the grids,
    and has the least power on pass/fail data (non-inferiority at pass rates .95/.95, 40 tasks: 3.3% against 12.5%). Tango's
    interval has no cell over 4% and the best power among those that clear. So pass/fail pairs go to it, and the augmented
    form with two pairs reads the rest, because the alternatives that do not augment fail on them: the version 2 score form
    adopts a few half-credit tasks at up to 15.5% (`stress partial-credit nested loss 5%`, 62 tasks). No candidate failed
    every cell, so no minimum-disagreement floor was added. The version 2 and version 3 figures of the review's shapes:

    | null at exactly the margin | tasks | version 2 | version 3 |
    |---|---|---|---|
    | a candidate that only loses 5% of the incumbent's passes | 61 / 62 / 64 | 4.38 / 17.73 / 16.39 | 0.00 / 0.00 / 0.00 |
    | the same | 80 / 100 / 160 | 8.61 / 11.83 / 9.39 | 1.65 / 0.59 / 1.22 |
    | the same | 250 / 500 / 1000 | 6.50 / 5.59 / 4.33 | 1.31 / 1.99 / 2.12 |
    | pass rates .99 and .94 | 61 / 62 / 64 | 6.60 / 11.09 / 10.07 | 1.30 / 1.25 / 1.15 |
    | pass rates .97 and .92 | 61 / 62 / 64 | 4.81 / 5.13 / 4.71 | 1.93 / 1.86 / 1.73 |
    | margin 0.10, a candidate that only loses 10% | 31 / 32 / 40 / 80 | 3.82 / 15.64 / 8.05 / 8.80 | 0.00 / 0.00 / 1.48 / 1.07 |
    | margin 0.10, pass rates .99 and .89 | 31 / 32 / 40 / 80 | 3.19 / 12.27 / 5.92 / 5.80 | 0.79 / 0.72 / 1.28 / 1.56 |
    | margin 0.10, pass rates .97 and .87 | 31 / 32 / 40 / 80 | 3.10 / 7.55 / 3.86 / 3.77 | 1.36 / 1.26 / 1.52 / 1.96 |

    The full grids are under "Measured grid at gate version 3".
    *The price.* ADOPT rate, per cent, at a true effect (exact for pass/fail rows, simulated for the rest), version 2 against
    version 3 (the eight rows of the earlier study first, then the rows added for this decision):

    | row | tasks | version 2 | version 3 |
    |---|---|---|---|
    | superiority, pass rates .50 to .60 | 16 / 24 / 40 | 4.21 / 8.46 / 12.64 | 6.92 / 8.93 / 14.69 |
    | non-inferiority, pass rates .90 and .90 | 16 / 24 / 40 | 1.47 / 7.85 / 10.81 | 4.63 / 6.31 / 10.66 |
    | superiority, uniform scores, a true +0.10 | 24 | 19.45 | 6.73 |
    | non-inferiority, uniform scores, equal arms | 24 | 6.19 | 2.04 |
    | non-inferiority, pass rates .90 and .90 | 100 / 250 | 20.46 / 45.44 | 19.20 / 45.29 |
    | non-inferiority, pass rates .95 and .95 | 16 / 40 / 100 / 250 | 0.32 / 16.39 / 36.47 / 71.56 | 1.96 / 12.51 / 29.72 / 68.45 |
    | non-inferiority, identical outputs (every pair a tie) | 16 / 40 / 73 / 100 | 0 / 0 / 100 / 100 | 0 / 0 / 100 / 100 |
    | non-inferiority, 0..10 judge integers, equal arms (margin 0.5) | 40 / 61 / 100 / 250 | 27.78 / 42.05 / 62.87 / 95.99 | 2.67 / 12.07 / 37.72 / 92.67 |
    | superiority, 0..10 judge integers, a true +1 point | 40 / 61 / 100 | 79.03 / 93.69 / 99.52 | 27.02 / 69.75 / 97.23 |
    | non-inferiority, means of 5 attempts, mixed difficulty, equal arms | 40 / 61 / 100 / 250 | 60.22 / 76.48 / 92.11 / 99.94 | 0.52 / 10.20 / 58.44 / 99.74 |

    Identical outputs now need 73 tasks, not 61. On pass/fail scores version 3 has more power at 16 tasks, where version 2 was
    nearly blind, and a little less from 24 to 250 tasks against a strong incumbent (up to 7 points at .95/.95). On every other
    score it has much less below 100 tasks, and that is the cost of the fallback: two pseudo-pairs at each end of a 0..10 range
    weigh against differences of a point or two. It is stated here because the decision's purpose is a gate that never adopts on
    sparse disagreement, and a fail-closed gate takes that price first; the follow-up below is how it might be lowered.
    *Where it is not calibrated, and what is open.* For pass/fail data the envelope above is the whole claim: at most 3.0%
    (margin 0.05, at 20 tasks with 35% of the tasks disagreeing), 3.1% (margin 0.10) and 2.7% (superiority), over every size from 16 to
    500. For other scores nothing is claimed beyond the 49 stress cells and the 63-cell grid (all at most 3.2%): a score set that is
    nearly pass/fail with a coarse shape no cell holds, a size between the cells, a size below 16 tasks. The two constructions meet
    at an edge: a dataset is pass/fail only if every pair is, so one half-credit task among a thousand sends the whole dataset to
    the fallback, whose interval is wider (sixteen ties read `+-0.194 w`, fifteen ties and a difference of 0.01 read `+-0.237 w`).
    The efficiency reads are unchanged and keep the calibration of decision 2.

## Consequences

- **The legacy callers are not migrated.** `agent-trajectory-gate` (`runAgentTrajectoryGate` in `main.go`) and the
  compaction A/B (`compeval.RunAB`) keep `B = 2000` and `seed = 42` and their own reading of the interval; the docs page
  lists them as legacy by name. `BootstrapDeltaMean` is not changed, so neither moves. New callers use the gate.
- **The primary does not depend on `bootstrap_b` or `seed`.** They pre-register the bootstrap that reads the efficiency
  metrics. A verdict from the gate and the interval a legacy caller prints for the same scores are therefore different
  intervals by design.
- **INCONCLUSIVE at small n is the expected answer, not a failure.** With 16 declared tasks and the floor at 16, one
  unusable incumbent task already puts `n_used` below `min_pairs`, and the power note will usually ask for more tasks. A
  report that says "blocked: inconclusive at n = 15" with the number of tasks to add is a complete result. An operator's
  wish to adopt does not move a boundary.
- **A pre-registration is write-once.** `gate freeze` creates its output exclusively; a changed pre-registration is a new
  file with a new hash, and a result computed under the old hash cannot be presented under the new one. Moving to gate
  version 2 meant a baseline written under version 1 is re-frozen, with a judge and, for `custom`, a range; moving to version 3
  means a baseline written under version 1 or 2 is re-frozen (a version 2 file has the same fields, and its verdicts were read
  with another primary interval).
- **The gate version is 3, not 2.** Version 2 had never been released, and its earlier rule changes (the cost rule, the
  regression rule, the rule of three, the verdict tolerance) landed in it. The primary interval is a different matter: it
  moves verdicts on data that is not sparse. The tests that drove it show it: on a 0..10 judge scale the augmented interval
  of 16 to 20 pairs is about 2.4 points wide, so the verdict-table rows whose primary sat just inside a margin of 0.5 (rows 4,
  5, 6c, 6d and 9, and the efficiency tests built on them) went from ADOPT or BLOCK to INCONCLUSIVE and had to be given a
  primary clear of the margin; 61 to 72 ties went from ADOPT to INCONCLUSIVE; pass/fail superiority at pass rates .50 to .60
  and 16 tasks went from 4.2% to 6.9% ADOPT. A baseline frozen under version 2 is refused for its version, as one frozen under
  version 1 is, and `internal/eval/testdata/baseline-v2-frozen.json` is a real version 2 file, written by the version 2
  binary and pinned by hash, for the test.
- **The type for the attempt enum is `eval.AttemptOutcome`.** `eval.Outcome` is already the cascade eval's per-case struct,
  used by the confidence-head A/B; renaming it was not part of this decision. The taxonomy's helpers are `AllOutcomes` and
  `OutcomeFromRecord`, which maps the outcome word of an external evaluation record to the taxonomy: the six shared words
  and `not_checked`, which is `not_run`.
- **Stricter than the minimum in several places.** Both arms must carry all three stamps and the judge must name a scorer,
  because an empty stamp would make two unrelated runs share an era marker; a task id may appear once per arm; a `pass` or
  `task_fail` score must lie in its primary class range; an efficiency value must be finite and not negative; scores or
  costs whose sums overflow are refused; and a changed baseline is detected by hash even when an edit leaves a
  valid document (`gate check` alone prints that document's own hash, so a verdict adapter takes the baseline hash from
  `freeze`'s output and passes `--expect-hash`; the docs page says so).
- **The calibration claim is scoped to what the tests measure.** For pass/fail pairs: the exact sums of
  `TestPassFailFalseAdoptRateIsExactlyAtMostFourPercentWhereTheArmsBarelyDisagree` (30 rows, 61 to 250 tasks and the margin-0.10
  sizes, each at most 2.3%, pinned to scipy), the nested-loss sawtooth of `TestNestedLossFalseAdoptRateIsTheBinomialSum` (20
  sizes, at most 2.4%) and the study's envelope (at most 3.1% over every size from 16 to 500 and every share of disagreeing
  tasks, at both margins and for superiority). For every other score: the 63 null cells, the 54 sparse cells and the 49 stress
  cells of the study (at most 3.2% with the pass/fail rows included), the eleven end-to-end cells of
  `TestNullAdoptRateAtTheFloorsIsCalibrated` (16 to 24 tasks, 0.0% to 2.5%) and the fifteen interval cells of
  `TestPrimaryIntervalFalseAdoptRateOnTheStatedNulls` (0.0% to 2.5%). Nothing is claimed for scores that are not pass/fail
  with a coarse shape no cell holds, for sizes below 16 tasks, or for the efficiency reads beyond the figures of decision 2.
- **The primary pays for pass/fail and for nothing else.** On a pass/fail primary the gate has about version 2's power (more at 16
  tasks, a little less from 24 to 250 against a strong incumbent). On any other primary, and a mean of several attempts per task
  is one, it has much less below 100 tasks (decision 12 has the rows). A corpus whose scorer can be pass/fail should be scored that
  way; the follow-up below is the open question.
- **No wall-time term enters through the gate.** Speed may be reported beside a verdict as a side column by a caller; the
  gate has no input for it and no rule that reads it.
- **Reading the gate's inputs needs adapters.** A record from an external evaluation tool becomes a gate input outside this
  repository (`OutcomeFromRecord` and the schema are what that adapter reads), and the self-test receipt for a model judge
  is a later verb.

## Follow-ups

Dated 2026-10-14, from the review that made the gate version 3. Open statistical questions, not work in this change.

- **A fallback that does not pay for coarse lattices it cannot see.** The augmented form costs most where the differences are
  many and small relative to the range (a 0..10 judge, a mean of attempts): 27.8% to 2.7% power at 40 tasks, 60.2% to 0.5% for a mean of
  5 attempts. Tango's interval gets its variance from a lattice of three points; the same restricted-variance idea on the
  lattice a score set actually has (steps of 1/k for k attempts, half credit) would put the pseudo-mass where the lattice
  forces it and leave a dense score set alone. The stress cells show the shape of the target: the version 2 score form is
  calibrated on means of 3 and 5 attempts (at most 3.4% at 24 to 250 tasks) and fails only on coarse sparse sets (a few half-credit
  tasks, up to 15.5%). One variant was tried informally and is not a result: pseudo-pairs weighed by the share of tied pairs held 3.2%
  at worst on the stress shapes up to 1000 tasks and kept 57.7% of the 0..10 judge power at 100 tasks (against 37.7% now, 62.9% at version 2),
  but its weight was fitted to those cells and it has no derivation. It needs one before it is a candidate.
- **The edge between the two constructions.** One half-credit task among a thousand sends a pass/fail dataset to the fallback and
  widens its interval (sixteen ties `+-0.194 w`, fifteen ties and a difference of 0.01 `+-0.237 w`). A mixed read, Tango's variance for the
  tied and whole-range pairs and the sample's for the rest, is the open version of the same question.
- **Weights chosen on a grid.** Two pairs at each end is the smallest weight measured that clears the stress nulls; it is not
  derived. The stress nulls are shapes the review could name (half credit, mixed-size losses, 3 and 5 attempts), and a coarse
  set of another shape is not covered.
- **Sampling error of the simulated cells.** They are 20000 datasets: 0.11 to 0.14 points near 2.5% to 4%. The largest version 3
  figure in a simulated cell is 3.21% (`stress partial-credit nested loss 5%`, 1000 tasks): six standard errors under the 4% bar
  and five over the nominal 2.5%, so these cells are bounds and not estimates of the exact rate.

## History: the primary interval at gate version 2

Gate version 1 read the primary on the percentile bootstrap and version 2 on the score form of the Student-t interval with the rule of
three for a sample whose differences are all equal. The text of version 2's decision and the figures it rested on are kept here,
because the bootstrap candidates were not run again for version 3 and the version 3 grids are measured against these. Tenses
and the labels "the gate" and "the one chosen" refer to version 2.

**Version 2's primary: the score form of the Student-t interval.** The primary is the mean of the paired task
differences, `delta`, with `delta -+ c * sn / sqrt(n - c^2)`, where `c = t(n-1, .975)` and `sn` is the standard
deviation with divisor `n`. It is the confidence set of `(delta - mu) / sqrt(mean((d - mu)^2) / n)`, whose variance is
taken at the boundary `mu` instead of at the estimate. Against the plain t interval it multiplies the critical value
by `sqrt((n-1) / (n - c^2))`: 1.14 at 16 tasks, 1.10 at 20, 1.08 at 24, 1.04 at 40, 1.02 at 80. With every difference
equal (to within a rounding error of the scores) there is no spread to read, and the interval was that one value, so
16 ties ADOPTed a non-inferiority claim that carried an efficiency win: pass rates .95 and .90 falsely ADOPTed 9% of
the time at 16 tasks, .99 and .94 32%, and a candidate that only loses 5% of the incumbent's passes 37% at 20 tasks.
`delta` is now the one value `v`, taken from the data and not from a sum, and the interval is the **rule of three**
about it, `[v - 3w/n, v + 3w/n]` clipped to `[-w, w]`, `w` the width of the primary's range and `n` the usable pairs.
For pass/fail scores with no discordant pair among `n`, `3/n` is the one-sided 95% upper bound on the discordance
rate (`1 - 0.05^(1/n)`), so the mean difference can be as low as `-3/n`; on other scores it is the same allowance,
scaled by the range. A tie sits `0.1875 w` below 0 at 16 tasks, `0.125 w` at 24, `0.075 w` at 40, `0.05 w` at 60 and
`0.0375 w` at 80: a margin of 0.05 of the range is certified by 61 ties and not by 60, whose end sits exactly on it.
*Why both ends.* An exact one-sided bound on the lower end alone (`1 - 0.025^(1/n)`, the interval's own 2.5% tail,
with the upper end left at `c`) was tried first. It gives the same figure in every cell of the grid below, since it
differs from `3/n` only for ties at 61 to 71 pairs, and it needs 72 ties against 61. The decision is the symmetric
rule of three, the standard allowance for an event that did not occur in `n` trials, which makes a superiority tie
INCONCLUSIVE at any size without a special case and keeps a constant difference far from zero decisive: every pair a
win of the whole range is `[w - 3w/n, w]` and adopts, every pair a loss blocks. The cost of moving the upper end is
that a sample with no spread cannot BLOCK until its upper end clears the margin: every task lost by 1.0 on a 0..10
scale (margin 0.5) is INCONCLUSIVE up to 60 tasks and a BLOCK from 61. The incumbent keeps its place either way (R4).
*What it does not close.* `3/n` is a little looser than the interval's own tail: at 61 pairs a candidate that loses
5% of the incumbent's passes ties them all 4.4% of the time, so the nested-loss null reads 4.4% at 61 tasks (4.2%
measured), where it reads 0% up to 60. That is the first step of the sparse regime described under "Where it is not
calibrated", which is larger than this step. A difference exactly on the margin, or an interval end that lands on it
by float rounding (`0.1 + 0.2` against `0.3` at 60 pairs, or `0.55 - 0.5` at 30), is neither ADOPT nor BLOCK: the
verdict compares an end with its boundary to within 1e-9 of the score range, and differences that are equal on paper
but not to the last bit (0.95 - 0.90 against 0.70 - 0.65) are treated as equal. The tolerance changes a verdict only for an end within 1e-9 of the range of its boundary.
With fewer than two pairs there is no interval and the numbers read 0. `c` is a table to 200 degrees of freedom and a three-term expansion beyond, so the gate needs no special function.
`delta` is the observed mean difference, and `bootstrap_b` and `seed` no longer touch the primary.
*What was measured.* `TestCalibrationStudy` (`internal/eval/gate_sim_test.go`, off unless `OFFLOAD_GATE_STUDY=1`)
draws 20000 datasets per cell: 63 null cells, in which the true difference is 0 (superiority) or exactly `-0.05`
(non-inferiority on a unit interval, the worst case the rule must still refuse). Superiority: pass/fail at p = .50 and
.85, uniform scores, scores in tenths (k/10, k = 0 to 10), x^3-skewed scores (u^3 for u uniform) and a two-point 0/10
judge, at 16, 24 and 40 tasks. Non-inferiority: pass rates .90/.85, .70/.65 and .55/.50, uniform scores (the candidate's
on [0, .9]) and x^3-skewed scores (the candidate's times .8), and, added after the fourth review, the strong
incumbents (pass rates .95/.90, .97/.92 and .99/.94) and a candidate that only loses 5% of the incumbent's passes
(it never wins a task the incumbent loses), at 16, 20, 24, 40 and 80 tasks, with an efficiency metric
that clearly improves, so only the primary decides. Every method reads the same datasets and the same resamples; a
sample whose pairs are all equal is a point to every method but (vi), so the columns of the other methods are what they
were before the fourth review and (vi) is the gate. Data
seeds come from the cell and the dataset index, bootstrap seeds from the dataset index: the tables are identical at 24
and at 30 workers. The percentile interval reproduces the review's figures within simulation error (3.77, 3.69 and 4.12
against its 3.86, 3.58 and 4.10 for uniform scores, tenths and x^3-skewed scores at 16 tasks, and 6.12 against 6.35 for
the non-inferiority cell at 20 tasks; the standard error of a rate near 4% over 20000 datasets is 0.14 points).
*Why not one of the five.* The review named five candidates: the studentized bootstrap, BCa, the percentile widened
about the estimate by `t/z` (and by `t/z * sqrt(n/(n-1))`), the paired t, and the lower of two. Not one stays at or
under 4% in every cell, at `B = 2000` or `B = 10000`. A sixth, the score form of the t interval ((vi), the one version 2 chose),
was added when none of the five cleared the bar. On the 43 cells of the first study it has none over 4% (the worst is
2.7%) and the paired t has one (4.6%); the 20 cells added after the fourth review are where the others, and the score
form at 80 tasks, fail:

| interval | cells over 4% (of 63), B = 2000 | worst cell, B = 2000 | cells over 4%, B = 10000 | worst cell, B = 10000 |
|---|---|---|---|---|
| percentile (current) | 25 | 44.09 | 25 | 44.09 |
| (i) studentized | 23 | 44.09 | 23 | 44.09 |
| (ii) BCa | 23 | 44.09 | 24 | 44.09 |
| (iii) pctl x t/z | 19 | 44.09 | 19 | 44.09 |
| (iii') pctl x t/z x sqrt(n/(n-1)) | 19 | 44.09 | 19 | 44.09 |
| (iv) paired t | 18 | 44.09 | 18 | 44.09 |
| (vi) score-form t | 2 | 8.23 | 2 | 8.23 |

The bootstrap intervals resample the sample's own lattice: on pass/fail scores where the arms differ on a handful of
tasks they inherit its spread, which is both small and noisy. The plain t interval does the same through its standard
error. Take 20 tasks with pass rates .90 and .85, so the true loss rate is 13.5%: two wins and no loss gives a mean
of 0.1 and a lower end of -0.044 on the t interval, inside the margin of 0.05 (3.3% of all such samples have no loss
and two or more wins, and the t interval adopts every one of them), while the score form gives -0.059. The lower of two
of the five inherits the failure of its worse member:

| lower of two | cells over 4% (B = 2000) | worst cell (B = 2000) | cells over 4% (B = 10000) | worst cell (B = 10000) |
|---|---|---|---|---|
| min(studentized, t) | 16 | 44.09 | 16 | 44.09 |
| min(studentized, BCa) | 20 | 44.09 | 20 | 44.09 |
| min(BCa, t) | 16 | 44.09 | 16 | 44.09 |
| min(iii', BCa) | 17 | 44.09 | 17 | 44.09 |
| min(iii', t) | 18 | 44.09 | 18 | 44.09 |
| min(studentized, iii') | 17 | 44.09 | 17 | 44.09 |
| min(percentile, vi) | 2 | 8.13 | 2 | 8.23 |
| min(BCa, vi) | 0 | 3.40 | 0 | 3.45 |
| min(iii', vi) | 2 | 8.13 | 2 | 8.23 |

The score form is the best single interval and does not clear the bar on the 63 cells: 61 are at most 3.6%, and two,
both at 80 tasks, are not: pass rates .99/.94 (4.8%) and the nested-loss candidate (8.2%). Those two are the regime of
a strong incumbent and a handful of disagreeing tasks from 61 tasks, where the score form is still anti-conservative
and the rule of three certifies a sample of ties (the stress table below runs it from 60 to 200 tasks, with the sizes
61 and 64 where that starts: the nested-loss candidate 4.2% at 61 tasks, 16.8% at 64, 8.9% at 80, 5.8% at 120 and 6.3%
at 200, pass rates .99/.94 6.5% at 61 and 10.1% at 64, .97/.92 4.9% and 4.7%). The nested-loss rate is an exact
binomial sum, pinned by `TestNestedLossFalseAdoptRateIsTheBinomialSum`: 0% to 60 tasks, 4.4% at 61 (all ties), 17.7% at
62 (one loss clears the margin), 12.9% at 70, 8.6% at 80, 5.7% at 90, 11.8% at 100 (two losses), 9.4% at 160, 6.5% at
250, 5.6% at 500 and 4.3% at 1000, and the peaks of that sawtooth stay over 4% to about 1500 tasks. The lower of the
score form and BCa is the only combination that clears all 63 null cells (0 over 4%, the worst 3.4% at `B = 2000` and
3.5% at `B = 10000`) and the 18 stress cells at 16 to 60 and 80 to 200 tasks (at most 3.4%), and it keeps the score
form's power to within 0.7 points on every power cell. It does not clear the stress cells at 61 and 64 tasks: five of
those six are over 4% (the nested-loss candidate 4.2% at 61 and 3.7% at 64, .97/.92 4.9% and 4.2%, .99/.94 6.5% and
5.9%). It
was **not adopted here**: BCa is a bootstrap, so it would couple the primary back to `bootstrap_b` and `seed`, which
this decision made no longer touch it, and it changes the primary's algorithm in a repair that was asked to fix the
all-tied case. Decision 12 is the interval that closes 61 to 64 tasks without the bootstrap. The other lower-of-two rows with (vi) keep the score
form's failure. At `B = 10000` the bootstrap intervals move by at most 0.6 points and the closed forms not at all.
*The price.* The interval is wider than the plain t by the factors above, so power falls. At 24 tasks a true gain of
0.1 on a pass/fail score is adopted in 8.6% of superiority runs (the paired t 10.7%, the percentile 9.7%, which
over-adopts) and a candidate truly equal to the incumbent in 7.9% of non-inferiority runs at margin 0.05 (the paired t
12.3%, the percentile 15.7%; the score form read 8.8% when a sample with no spread was a point, and at 16 tasks it
read 5.8% and now reads 1.6%, because 16 ties cannot certify a margin of 0.05; the exact one-sided bound tried
before the rule of three gave these figures to the digit, and every cell below, since the two differ only for ties at
61 to 71 pairs). The power cells:

| cell | tasks | percentile (current) | (i) studentized | (ii) BCa | (iii) pctl x t/z | (iii') | (iv) paired t | (vi) score form |
|---|---|---|---|---|---|---|---|---|
| power sup binary .50->.60 | 16 | 7.99 | 6.98 | 6.93 | 7.99 | 7.99 | 6.88 | 4.06 |
| power ni binary .90->.90 | 16 | 22.55 | 22.54 | 22.50 | 22.55 | 22.55 | 8.77 | 1.61 |
| power sup binary .50->.60 | 24 | 9.74 | 8.21 | 8.80 | 9.74 | 9.74 | 10.68 | 8.61 |
| power ni binary .90->.90 | 24 | 15.73 | 15.17 | 19.20 | 12.38 | 12.06 | 12.27 | 7.85 |
| power sup binary .50->.60 | 40 | 13.54 | 13.11 | 12.67 | 13.54 | 13.54 | 14.88 | 12.81 |
| power ni binary .90->.90 | 40 | 10.64 | 14.92 | 12.41 | 10.64 | 10.64 | 10.61 | 10.55 |
| power sup uniform +.10 | 24 | 28.66 | 22.48 | 27.94 | 25.26 | 23.97 | 24.03 | 19.45 |
| power ni uniform 0 | 24 | 10.46 | 7.38 | 9.96 | 8.75 | 8.16 | 8.18 | 6.19 |

*Where it is not calibrated.* The stress cells of the same study, outside the bar: scores with a spike at zero, the
heavy-tailed rare-large score, and a strong incumbent at the margin where the two arms disagree on a handful of tasks,
from 60 to 200 tasks, with the sizes 61 and 64 added after the rule of three because they are the first at which a
sample of ties certifies a margin of 0.05 (the last column is the lower of (vi) and BCa, for the reason above):

| stress null | tasks | percentile (current) | (i) studentized | (ii) BCa | (iii) pctl x t/z | (iii') | (iv) paired t | (vi) score form | min(BCa, vi) |
|---|---|---|---|---|---|---|---|---|---|
| stress sup zero-inflated | 16 | 4.76 | 5.82 | 7.90 | 3.39 | 3.02 | 1.69 | 0.70 | 0.70 |
| stress sup rare-large | 16 | 6.16 | 6.19 | 10.10 | 3.03 | 2.33 | 1.44 | 0.52 | 0.52 |
| stress ni zero-inflated | 16 | 10.30 | 9.50 | 11.64 | 9.25 | 8.90 | 6.49 | 3.74 | 3.41 |
| stress sup zero-inflated | 24 | 4.26 | 4.75 | 5.90 | 3.52 | 3.28 | 2.32 | 1.47 | 1.47 |
| stress sup rare-large | 24 | 5.00 | 5.89 | 7.52 | 3.69 | 3.34 | 2.05 | 1.29 | 1.29 |
| stress ni zero-inflated | 24 | 6.54 | 6.77 | 7.74 | 5.80 | 5.54 | 4.45 | 3.41 | 3.23 |
| stress ni binary .97/.92 | 60 | 3.44 | 5.19 | 3.82 | 3.44 | 3.44 | 5.06 | 4.41 | 3.23 |
| stress ni binary .99/.94 | 60 | 3.69 | 6.96 | 3.73 | 3.69 | 3.69 | 6.96 | 5.47 | 2.26 |
| stress ni nested loss 5% | 60 | 4.89 | 4.89 | 4.89 | 4.89 | 4.89 | 4.89 | 0.00 | 0.00 |
| stress ni binary .97/.92 | 61 | 7.14 | 4.99 | 6.66 | 3.83 | 3.24 | 5.55 | 4.87 | 4.87 |
| stress ni binary .99/.94 | 61 | 11.65 | 6.54 | 6.77 | 8.40 | 3.52 | 11.45 | 6.53 | 6.53 |
| stress ni nested loss 5% | 61 | 18.11 | 4.23 | 4.23 | 18.11 | 4.23 | 18.30 | 4.23 | 4.23 |
| stress ni binary .97/.92 | 64 | 6.20 | 5.58 | 5.79 | 6.20 | 6.20 | 4.66 | 4.66 | 4.17 |
| stress ni binary .99/.94 | 64 | 10.41 | 6.21 | 6.21 | 10.41 | 10.41 | 10.12 | 10.12 | 5.85 |
| stress ni nested loss 5% | 64 | 16.62 | 3.74 | 3.74 | 16.62 | 16.62 | 16.78 | 16.78 | 3.74 |
| stress ni binary .97/.92 | 80 | 3.37 | 5.02 | 3.38 | 3.37 | 3.37 | 3.52 | 3.52 | 3.37 |
| stress ni binary .99/.94 | 80 | 4.72 | 4.39 | 3.03 | 4.72 | 4.72 | 4.75 | 4.75 | 3.03 |
| stress ni nested loss 5% | 80 | 8.79 | 1.94 | 1.94 | 8.79 | 8.79 | 8.87 | 8.87 | 1.94 |
| stress ni binary .97/.92 | 120 | 2.62 | 3.22 | 2.59 | 2.62 | 2.62 | 3.31 | 3.16 | 2.56 |
| stress ni binary .99/.94 | 120 | 3.85 | 2.40 | 2.69 | 3.85 | 3.85 | 6.00 | 4.33 | 2.69 |
| stress ni nested loss 5% | 120 | 5.75 | 0.25 | 0.86 | 5.75 | 5.75 | 5.75 | 5.75 | 0.86 |
| stress ni binary .97/.92 | 200 | 2.83 | 3.35 | 2.73 | 2.83 | 2.83 | 3.52 | 3.52 | 2.70 |
| stress ni binary .99/.94 | 200 | 3.07 | 2.76 | 2.42 | 3.07 | 3.07 | 4.06 | 4.06 | 2.42 |
| stress ni nested loss 5% | 200 | 3.13 | 1.80 | 1.33 | 3.13 | 3.13 | 6.33 | 6.33 | 1.33 |

A candidate that only ever loses tasks (the "nested loss" rows) and the strong incumbents were adopted in up to 44% of
runs at 16 tasks, 36% at 20, 29% at 24 and 13% at 40 under every interval, because a sample with no spread had an
interval of one value. With the rule of three (decision 12) the nested-loss candidate is at 0% from 16 to 60 tasks, and
the strong incumbents are at most 3.3% at 16 to 40 tasks. From 61 tasks the sample of ties is certified, and from 62 a
sample with one loss in it is too, because the score form reads little spread in it: 4.2% at 61 tasks, 16.8% at 64 (one
loss in 64), 8.9% at 80 (one loss in 80), 5.8% at 120 (two) and 6.3% at 200 (five) for the nested-loss candidate, and up
to 10.1% for pass rates .99/.94 and 4.9% for .97/.92 at 60 to 200 tasks. The nested-loss figure falls and rises as the
number of losses the interval accepts steps up, and stays over 4% at its peaks to about 1500 tasks. A zero-inflated
score that is non-inferior at exactly the margin is adopted in 3.7% of runs at 16 tasks with the score form and 3.4% at
24. What closes it is an interval whose variance comes from a parametric null (for pass/fail scores the
Bernoulli discordance at the boundary, as the Wilson interval does for a rate) in place of the sample's second moment about
the boundary, which is what the score form reads and is small when the sample holds one loss: decision 12 is that interval.

## Measured grid at gate version 3

False ADOPT rate in per cent, from the closed-form study (`TestSparseCalibrationStudy`; the same tables print at the end of
`TestCalibrationStudy`). Pass/fail rows (every row whose cell name says binary or nested loss, and the two-point 0/10 rows)
are exact multinomial sums; the others are 20000 datasets per cell with seeds from the cell's name and size. A star marks a
cell over 4%. The columns: **v2**, the version 2 interval (score form, rule of three); **half pair x4**, the Agresti-Min
weights applied to every pair; **1 pair** and **2 pairs, all**, one and two pseudo-pairs at each end applied to every pair,
centred on the observed mean; **version 3**, Tango's interval for pass/fail pairs and two pairs at each end otherwise. The v2
column is re-simulated here with new draws, so its figures differ from the version 2 grid below within sampling error.

### The 63 null cells of the earlier study

| cell | n | v2 | half pair x4 | 1 pair | 2 pairs, all | version 3 |
| --- | --- | --- | --- | --- | --- | --- |
| sup binary p=.50 | 16 | 1.19 | 0.98 | 1.16 | 1.16 | 2.16 |
| sup binary p=.85 | 16 | 0.83 | 0.25 | 0.25 | 0.25 | 2.30 |
| sup uniform | 16 | 1.31 | 0.40 | 0.33 | 0.18 | 0.18 |
| sup deciles 0..1 | 16 | 1.53 | 0.53 | 0.48 | 0.34 | 0.34 |
| sup x^3 skewed | 16 | 1.39 | 0.34 | 0.24 | 0.10 | 0.10 |
| sup two-point 0/10 | 16 | 1.19 | 0.98 | 1.16 | 1.16 | 2.16 |
| sup binary p=.50 | 24 | 2.02 | 1.47 | 1.88 | 1.88 | 2.15 |
| sup binary p=.85 | 24 | 1.61 | 0.67 | 0.71 | 0.71 | 2.39 |
| sup uniform | 24 | 1.76 | 0.92 | 0.76 | 0.43 | 0.43 |
| sup deciles 0..1 | 24 | 1.98 | 1.05 | 0.97 | 0.66 | 0.66 |
| sup x^3 skewed | 24 | 1.67 | 0.70 | 0.57 | 0.32 | 0.32 |
| sup two-point 0/10 | 24 | 2.02 | 1.47 | 1.88 | 1.88 | 2.15 |
| sup binary p=.50 | 40 | 2.12 | 1.73 | 2.12 | 2.12 | 2.64 |
| sup binary p=.85 | 40 | 2.03 | 1.68 | 1.61 | 1.25 | 2.14 |
| sup uniform | 40 | 1.93 | 1.35 | 1.23 | 0.80 | 0.80 |
| sup deciles 0..1 | 40 | 2.19 | 1.56 | 1.50 | 1.05 | 1.05 |
| sup x^3 skewed | 40 | 2.10 | 1.33 | 1.18 | 0.82 | 0.82 |
| sup two-point 0/10 | 40 | 2.12 | 1.73 | 2.12 | 2.12 | 2.64 |
| ni binary .90/.85 | 16 | 0.64 | 0.16 | 0.16 | 0.16 | 2.05 |
| ni binary .70/.65 | 16 | 1.12 | 0.87 | 0.87 | 1.01 | 2.07 |
| ni binary .55/.50 | 16 | 1.10 | 0.99 | 1.00 | 1.28 | 2.03 |
| ni continuous uniform | 16 | 1.31 | 0.46 | 0.29 | 0.14 | 0.14 |
| ni continuous x^3 skewed | 16 | 1.96 | 0.46 | 0.26 | 0.09 | 0.09 |
| ni binary .95/.90 | 16 | 0.14 | 0.02 | 0.02 | 0.02 | 0.86 |
| ni binary .97/.92 | 16 | 0.03 | 0.00 | 0.00 | 0.00 | 0.31 |
| ni binary .99/.94 | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.02 |
| ni nested loss 5% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| ni binary .90/.85 | 20 | 1.86 | 0.84 | 0.84 | 0.25 | 2.31 |
| ni binary .70/.65 | 20 | 1.74 | 1.69 | 1.69 | 1.24 | 3.04 |
| ni binary .55/.50 | 20 | 1.74 | 1.73 | 1.73 | 1.50 | 2.98 |
| ni continuous uniform | 20 | 1.73 | 0.77 | 0.55 | 0.30 | 0.30 |
| ni continuous x^3 skewed | 20 | 2.01 | 0.73 | 0.36 | 0.17 | 0.17 |
| ni binary .95/.90 | 20 | 1.02 | 0.22 | 0.22 | 0.03 | 1.07 |
| ni binary .97/.92 | 20 | 0.42 | 0.05 | 0.05 | 0.01 | 0.42 |
| ni binary .99/.94 | 20 | 0.03 | 0.00 | 0.00 | 0.00 | 0.03 |
| ni nested loss 5% | 20 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| ni binary .90/.85 | 24 | 2.41 | 0.89 | 0.89 | 0.50 | 2.18 |
| ni binary .70/.65 | 24 | 1.50 | 1.49 | 1.54 | 1.53 | 2.57 |
| ni binary .55/.50 | 24 | 1.49 | 1.49 | 1.64 | 1.64 | 2.50 |
| ni continuous uniform | 24 | 1.79 | 0.84 | 0.60 | 0.35 | 0.35 |
| ni continuous x^3 skewed | 24 | 2.09 | 1.04 | 0.63 | 0.22 | 0.22 |
| ni binary .95/.90 | 24 | 3.08 | 0.28 | 0.28 | 0.07 | 1.16 |
| ni binary .97/.92 | 24 | 2.31 | 0.08 | 0.08 | 0.01 | 0.50 |
| ni binary .99/.94 | 24 | 0.55 | 0.00 | 0.00 | 0.00 | 0.04 |
| ni nested loss 5% | 24 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| ni binary .90/.85 | 40 | 2.59 | 1.49 | 1.47 | 1.36 | 2.57 |
| ni binary .70/.65 | 40 | 1.94 | 1.87 | 1.87 | 1.90 | 2.53 |
| ni binary .55/.50 | 40 | 1.86 | 1.85 | 1.85 | 1.95 | 2.43 |
| ni continuous uniform | 40 | 2.32 | 1.70 | 1.40 | 0.91 | 0.91 |
| ni continuous x^3 skewed | 40 | 2.42 | 1.74 | 1.20 | 0.69 | 0.69 |
| ni binary .95/.90 | 40 | 2.77 | 1.98 | 1.48 | 0.62 | 2.26 |
| ni binary .97/.92 | 40 | 3.18 | 2.86 | 1.41 | 0.20 | 1.73 |
| ni binary .99/.94 | 40 | 2.87 | 2.86 | 0.52 | 0.01 | 0.54 |
| ni nested loss 5% | 40 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| ni binary .90/.85 | 80 | 2.42 | 2.36 | 2.05 | 1.81 | 2.39 |
| ni binary .70/.65 | 80 | 2.26 | 2.25 | 2.25 | 2.16 | 2.44 |
| ni binary .55/.50 | 80 | 2.26 | 2.26 | 2.26 | 2.25 | 2.50 |
| ni continuous uniform | 80 | 2.49 | 2.12 | 1.80 | 1.41 | 1.41 |
| ni continuous x^3 skewed | 80 | 2.51 | 2.17 | 1.77 | 1.35 | 1.35 |
| ni binary .95/.90 | 80 | 2.99 | 2.50 | 1.93 | 1.54 | 2.23 |
| ni binary .97/.92 | 80 | 3.38 | 2.97 | 2.07 | 1.14 | 1.91 |
| ni binary .99/.94 | 80 | 4.94* | 4.92* | 2.88 | 0.93 | 1.53 |
| ni nested loss 5% | 80 | 8.61* | 8.61* | 1.65 | 1.65 | 1.65 |
| **worst cell** |  | 8.61 | 8.61 | 2.88 | 2.25 | 3.04 |
| **cells over 4%** (of 63) |  | 2 | 2 | 0 | 0 | 0 |

### The 54 sparse-regime cells

| cell | n | v2 | half pair x4 | 1 pair | 2 pairs, all | version 3 |
| --- | --- | --- | --- | --- | --- | --- |
| sparse nested loss 5% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| sparse binary .99/.94 | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.02 |
| sparse binary .97/.92 | 16 | 0.03 | 0.00 | 0.00 | 0.00 | 0.31 |
| sparse nested loss 5% | 24 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| sparse binary .99/.94 | 24 | 0.55 | 0.00 | 0.00 | 0.00 | 0.04 |
| sparse binary .97/.92 | 24 | 2.31 | 0.08 | 0.08 | 0.01 | 0.50 |
| sparse nested loss 5% | 40 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| sparse binary .99/.94 | 40 | 2.87 | 2.86 | 0.52 | 0.01 | 0.54 |
| sparse binary .97/.92 | 40 | 3.18 | 2.86 | 1.41 | 0.20 | 1.73 |
| sparse nested loss 5% | 60 | 0.00 | 4.61* | 4.61* | 0.00 | 0.00 |
| sparse binary .99/.94 | 60 | 5.45* | 2.75 | 2.75 | 0.34 | 1.36 |
| sparse binary .97/.92 | 60 | 4.28* | 2.13 | 2.13 | 0.90 | 2.00 |
| sparse nested loss 5% | 61 | 4.38* | 4.38* | 4.38* | 0.00 | 0.00 |
| sparse binary .99/.94 | 61 | 6.60* | 3.51 | 2.60 | 0.33 | 1.30 |
| sparse binary .97/.92 | 61 | 4.81* | 3.05 | 2.04 | 0.87 | 1.93 |
| sparse nested loss 5% | 62 | 17.73* | 4.16* | 4.16* | 0.00 | 0.00 |
| sparse binary .99/.94 | 62 | 11.09* | 3.34 | 2.46 | 0.32 | 1.25 |
| sparse binary .97/.92 | 62 | 5.13* | 2.91 | 1.96 | 0.84 | 1.86 |
| sparse nested loss 5% | 64 | 16.39* | 3.75 | 3.75 | 0.00 | 0.00 |
| sparse binary .99/.94 | 64 | 10.07* | 3.04 | 2.20 | 0.30 | 1.15 |
| sparse binary .97/.92 | 64 | 4.71* | 2.64 | 1.80 | 0.81 | 1.73 |
| sparse nested loss 5% | 80 | 8.61* | 8.61* | 1.65 | 1.65 | 1.65 |
| sparse binary .99/.94 | 80 | 4.94* | 4.92* | 2.88 | 0.93 | 1.53 |
| sparse binary .97/.92 | 80 | 3.38 | 2.97 | 2.07 | 1.14 | 1.91 |
| sparse nested loss 5% | 100 | 11.83* | 3.71 | 3.71 | 0.59 | 0.59 |
| sparse binary .99/.94 | 100 | 6.59* | 4.46* | 2.87 | 1.45 | 1.45 |
| sparse binary .97/.92 | 100 | 3.57 | 2.84 | 2.65 | 1.63 | 1.89 |
| sparse nested loss 5% | 160 | 9.39* | 3.88 | 3.88 | 1.22 | 1.22 |
| sparse binary .99/.94 | 160 | 4.69* | 4.21* | 2.73 | 1.70 | 1.93 |
| sparse binary .97/.92 | 160 | 3.28 | 3.02 | 2.47 | 1.98 | 2.14 |
| sparse nested loss 5% | 250 | 6.50* | 6.50* | 3.14 | 3.14 | 1.31 |
| sparse binary .99/.94 | 250 | 4.02* | 3.97 | 2.97 | 2.23 | 2.19 |
| sparse binary .97/.92 | 250 | 2.97 | 2.78 | 2.75 | 2.29 | 2.31 |
| sparse nested loss 5% | 500 | 5.59* | 3.43 | 3.43 | 3.43 | 1.99 |
| sparse binary .99/.94 | 500 | 3.44 | 3.43 | 2.98 | 2.59 | 2.23 |
| sparse binary .97/.92 | 500 | 2.85 | 2.79 | 2.79 | 2.55 | 2.37 |
| sparse nested loss 5% | 1000 | 4.33* | 4.33* | 3.07 | 3.07 | 2.12 |
| sparse binary .99/.94 | 1000 | 3.18 | 3.17 | 3.02 | 2.80 | 2.26 |
| sparse binary .97/.92 | 1000 | 2.78 | 2.78 | 2.66 | 2.60 | 2.44 |
| sparse m=.10 nested loss 10% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| sparse m=.10 binary .99/.89 | 16 | 2.35 | 0.01 | 0.01 | 0.00 | 0.17 |
| sparse m=.10 binary .97/.87 | 16 | 4.45* | 0.13 | 0.13 | 0.01 | 0.94 |
| sparse m=.10 nested loss 10% | 31 | 3.82 | 3.82 | 3.82 | 0.00 | 0.00 |
| sparse m=.10 binary .99/.89 | 31 | 3.19 | 2.84 | 2.84 | 0.11 | 0.79 |
| sparse m=.10 binary .97/.87 | 31 | 3.10 | 1.96 | 1.96 | 0.45 | 1.36 |
| sparse m=.10 nested loss 10% | 32 | 15.64* | 3.43 | 3.43 | 0.00 | 0.00 |
| sparse m=.10 binary .99/.89 | 32 | 12.27* | 2.87 | 2.53 | 0.10 | 0.72 |
| sparse m=.10 binary .97/.87 | 32 | 7.55* | 2.83 | 1.76 | 0.42 | 1.26 |
| sparse m=.10 nested loss 10% | 40 | 8.05* | 8.05* | 1.48 | 1.48 | 1.48 |
| sparse m=.10 binary .99/.89 | 40 | 5.92* | 5.92* | 1.28 | 1.03 | 1.28 |
| sparse m=.10 binary .97/.87 | 40 | 3.86 | 3.86 | 1.52 | 0.79 | 1.52 |
| sparse m=.10 nested loss 10% | 80 | 8.80* | 3.53 | 3.53 | 1.07 | 1.07 |
| sparse m=.10 binary .99/.89 | 80 | 5.80* | 4.17* | 2.71 | 1.56 | 1.56 |
| sparse m=.10 binary .97/.87 | 80 | 3.77 | 3.62 | 2.39 | 1.92 | 1.96 |
| **worst cell** |  | 17.73 | 8.61 | 4.61 | 3.43 | 2.44 |
| **cells over 4%** (of 54) |  | 29 | 12 | 3 | 0 | 0 |

### The 49 stress cells (scores that are not pass/fail, few and coarse disagreements)

| cell | n | v2 | half pair x4 | 1 pair | 2 pairs, all | version 3 |
| --- | --- | --- | --- | --- | --- | --- |
| stress partial-credit nested loss 5% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 24 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 24 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 24 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 40 | 2.67 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 40 | 6.67* | 0.00 | 0.00 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 40 | 9.12* | 0.00 | 0.00 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 60 | 1.42 | 5.18* | 4.00* | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 60 | 4.76* | 1.23 | 0.21 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 60 | 7.38* | 5.12* | 1.54 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 61 | 5.24* | 4.98* | 3.65 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 61 | 4.93* | 1.25 | 0.15 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 61 | 8.34* | 4.83* | 1.54 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 62 | 15.48* | 4.55* | 3.28 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 62 | 4.45* | 1.16 | 0.17 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 62 | 10.70* | 4.22* | 1.32 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 64 | 14.35* | 4.24* | 2.98 | 0.00 | 0.00 |
| stress half-credit-only nested loss 5% | 64 | 3.96 | 0.88 | 0.15 | 0.00 | 0.00 |
| stress mixed-size nested loss 5% | 64 | 9.85* | 4.07* | 1.29 | 0.00 | 0.00 |
| stress partial-credit nested loss 5% | 80 | 9.78* | 7.20* | 1.88 | 1.26 | 1.26 |
| stress half-credit-only nested loss 5% | 80 | 8.77* | 0.98 | 0.18 | 0.03 | 0.03 |
| stress mixed-size nested loss 5% | 80 | 10.12* | 4.31* | 1.67 | 0.46 | 0.46 |
| stress partial-credit nested loss 5% | 100 | 10.71* | 4.55* | 3.19 | 0.73 | 0.73 |
| stress half-credit-only nested loss 5% | 100 | 5.56* | 2.30 | 0.73 | 0.01 | 0.01 |
| stress mixed-size nested loss 5% | 100 | 7.42* | 4.18* | 2.21 | 0.54 | 0.54 |
| stress partial-credit nested loss 5% | 160 | 7.66* | 4.45* | 4.42* | 1.68 | 1.68 |
| stress half-credit-only nested loss 5% | 160 | 3.69 | 3.69 | 1.74 | 0.29 | 0.29 |
| stress mixed-size nested loss 5% | 160 | 6.18* | 5.08* | 3.48 | 1.82 | 1.82 |
| stress partial-credit nested loss 5% | 250 | 5.54* | 5.53* | 3.67 | 2.54 | 1.90 |
| stress half-credit-only nested loss 5% | 250 | 5.07* | 3.10 | 1.69 | 0.83 | 0.83 |
| stress mixed-size nested loss 5% | 250 | 5.23* | 4.61* | 3.35 | 2.24 | 2.24 |
| stress partial-credit nested loss 5% | 500 | 4.87* | 4.15* | 3.90 | 3.12 | 2.92 |
| stress half-credit-only nested loss 5% | 500 | 3.91 | 2.71 | 2.71 | 1.76 | 1.76 |
| stress mixed-size nested loss 5% | 500 | 4.48* | 4.30* | 3.58 | 2.79 | 2.79 |
| stress partial-credit nested loss 5% | 1000 | 3.94 | 3.94 | 3.52 | 3.22 | 3.21 |
| stress half-credit-only nested loss 5% | 1000 | 2.72 | 2.72 | 2.72 | 2.08 | 2.08 |
| stress mixed-size nested loss 5% | 1000 | 3.89 | 3.74 | 3.43 | 3.14 | 3.14 |
| stress 5 attempts .98->.93 | 24 | 2.94 | 0.00 | 0.00 | 0.00 | 0.00 |
| stress 3 attempts .97->.92 | 24 | 3.44 | 0.07 | 0.00 | 0.00 | 0.00 |
| stress 5 attempts .98->.93 | 40 | 3.02 | 0.07 | 0.00 | 0.00 | 0.00 |
| stress 3 attempts .97->.92 | 40 | 2.80 | 0.46 | 0.07 | 0.00 | 0.00 |
| stress 5 attempts .98->.93 | 61 | 2.93 | 0.35 | 0.02 | 0.00 | 0.00 |
| stress 3 attempts .97->.92 | 61 | 2.92 | 0.87 | 0.26 | 0.03 | 0.03 |
| stress 5 attempts .98->.93 | 100 | 2.83 | 0.88 | 0.16 | 0.01 | 0.01 |
| stress 3 attempts .97->.92 | 100 | 2.89 | 1.70 | 0.76 | 0.22 | 0.22 |
| stress 5 attempts .98->.93 | 250 | 2.97 | 1.89 | 1.10 | 0.43 | 0.43 |
| stress 3 attempts .97->.92 | 250 | 2.77 | 2.34 | 1.77 | 1.09 | 1.09 |
| **worst cell** |  | 15.48 | 7.20 | 4.42 | 3.22 | 3.21 |
| **cells over 4%** (of 49) |  | 25 | 18 | 2 | 0 | 0 |

### Power, ADOPT rate in per cent at a true effect

| cell | n | v2 | half pair x4 | 1 pair | 2 pairs, all | version 3 |
| --- | --- | --- | --- | --- | --- | --- |
| power sup binary .50->.60 | 16 | 4.21 | 3.68 | 4.29 | 4.29 | 6.92 |
| power sup binary .50->.60 | 24 | 8.46 | 6.72 | 8.07 | 8.07 | 8.93 |
| power sup binary .50->.60 | 40 | 12.64 | 10.96 | 12.64 | 12.63 | 14.69 |
| power ni binary .90->.90 | 16 | 1.47 | 0.37 | 0.37 | 0.37 | 4.63 |
| power ni binary .90->.90 | 24 | 7.85 | 2.72 | 2.72 | 1.41 | 6.31 |
| power ni binary .90->.90 | 40 | 10.81 | 6.94 | 6.79 | 5.98 | 10.66 |
| power ni binary .90->.90 | 100 | 20.46 | 20.45 | 19.22 | 17.30 | 19.20 |
| power ni binary .90->.90 | 250 | 45.44 | 45.42 | 45.29 | 43.67 | 45.29 |
| power ni binary .95->.95 | 16 | 0.32 | 0.04 | 0.04 | 0.04 | 1.96 |
| power ni binary .95->.95 | 40 | 16.39 | 13.73 | 9.86 | 3.26 | 12.51 |
| power ni binary .95->.95 | 100 | 36.47 | 34.10 | 33.41 | 27.01 | 29.72 |
| power ni binary .95->.95 | 250 | 71.56 | 71.05 | 70.84 | 68.09 | 68.45 |
| power ni identical outputs | 16 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| power ni identical outputs | 40 | 0.00 | 0.00 | 0.00 | 0.00 | 0.00 |
| power ni identical outputs | 73 | 100.00 | 100.00 | 100.00 | 0.00 | 100.00 |
| power ni identical outputs | 100 | 100.00 | 100.00 | 100.00 | 100.00 | 100.00 |
| power ni identical outputs | 250 | 100.00 | 100.00 | 100.00 | 100.00 | 100.00 |
| power sup uniform +.10 | 24 | 19.45 | 11.94 | 10.40 | 6.73 | 6.73 |
| power ni uniform 0 | 24 | 6.19 | 3.91 | 3.10 | 2.04 | 2.04 |
| power ni judge 0..10, equal arms | 24 | 15.57 | 3.77 | 1.01 | 0.14 | 0.14 |
| power sup judge 0..10, +1 point | 24 | 51.81 | 19.18 | 10.04 | 2.53 | 2.53 |
| power ni 5 attempts, mixed difficulty, equal arms | 24 | 43.36 | 1.03 | 0.07 | 0.01 | 0.01 |
| power ni 5 attempts .95->.95 | 24 | 36.80 | 1.79 | 0.10 | 0.01 | 0.01 |
| power ni judge 0..10, equal arms | 40 | 27.78 | 15.05 | 7.68 | 2.67 | 2.67 |
| power sup judge 0..10, +1 point | 40 | 79.03 | 60.02 | 47.58 | 27.02 | 27.02 |
| power ni 5 attempts, mixed difficulty, equal arms | 40 | 60.22 | 23.50 | 5.56 | 0.52 | 0.52 |
| power ni 5 attempts .95->.95 | 40 | 58.98 | 21.70 | 6.06 | 0.64 | 0.64 |
| power ni judge 0..10, equal arms | 61 | 42.05 | 31.47 | 22.34 | 12.07 | 12.07 |
| power sup judge 0..10, +1 point | 61 | 93.69 | 87.88 | 82.53 | 69.75 | 69.75 |
| power ni 5 attempts, mixed difficulty, equal arms | 61 | 76.48 | 56.19 | 34.29 | 10.20 | 10.20 |
| power ni 5 attempts .95->.95 | 61 | 77.39 | 54.76 | 33.20 | 10.69 | 10.69 |
| power ni judge 0..10, equal arms | 100 | 62.87 | 56.89 | 49.05 | 37.72 | 37.72 |
| power sup judge 0..10, +1 point | 100 | 99.52 | 99.20 | 98.72 | 97.23 | 97.23 |
| power ni 5 attempts, mixed difficulty, equal arms | 100 | 92.11 | 86.44 | 77.53 | 58.44 | 58.44 |
| power ni 5 attempts .95->.95 | 100 | 94.31 | 88.27 | 78.27 | 58.09 | 58.09 |
| power ni judge 0..10, equal arms | 250 | 95.99 | 95.43 | 94.39 | 92.67 | 92.67 |
| power sup judge 0..10, +1 point | 250 | 100.00 | 100.00 | 100.00 | 100.00 | 100.00 |
| power ni 5 attempts, mixed difficulty, equal arms | 250 | 99.94 | 99.93 | 99.88 | 99.74 | 99.74 |
| power ni 5 attempts .95->.95 | 250 | 99.98 | 99.98 | 99.96 | 99.91 | 99.91 |

The worst exact false-ADOPT rate over every size from 16 to 500 and eleven shares of disagreeing tasks (nine for
superiority): version 2 17.73% / 15.64% / 2.48% (margin 0.05 / margin 0.10 / superiority), half pair x4 12.21% / 9.85% / 2.47%,
1 pair 5.97% / 5.30% / 2.50%, 2 pairs for every pair 4.01% / 3.83% / 2.61%, version 3 3.01% / 3.11% / 2.71%.

## Measured grid at gate version 2

The bootstrap study at version 2, kept for the candidates it compared. False ADOPT rate in per cent, 20000 datasets per cell,
`B = 2000` (a nominal 2.5%; a star marks a cell over 4%). The
closed forms, (iv) and (vi), do not depend on `B`. The last column is the version 2 gate (the rule of three for a sample with no
spread); the column before it is the score form with a sample of ties read as a point, as the gate did before the fourth
review. The comparison with the margin is the gate's, to within 1e-9 of the score range, which moves a few bootstrap cells
by 0.2 points or less where a replicate's end lands on the margin by float rounding.

| null | tasks | all pairs equal | percentile | (i) studentized | (ii) BCa | (iii) pctl x t/z | (iii') x sqrt(n/(n-1)) | (iv) paired t | (vi) score form, ties as a point | (vi) score form, the gate |
|---|---|---|---|---|---|---|---|---|---|---|
| sup binary p=.50 | 16 | 0.00 | 2.48 | 2.34 | 2.16 | 2.48 | 2.48 | 2.04 | 1.07 | 1.07 |
| sup binary p=.85 | 16 | 0.97 | 2.44 | 4.98* | 4.87* | 2.44 | 2.44 | 2.35 | 0.81 | 0.81 |
| sup uniform | 16 | 0.00 | 3.77 | 2.06 | 3.48 | 2.71 | 2.42 | 2.42 | 1.31 | 1.31 |
| sup deciles 0..1 | 16 | 0.00 | 3.69 | 2.27 | 3.52 | 3.04 | 2.69 | 2.73 | 1.53 | 1.53 |
| sup x^3 skewed | 16 | 0.00 | 4.12* | 3.06 | 4.33* | 3.16 | 2.79 | 2.56 | 1.39 | 1.39 |
| sup two-point 0/10 | 16 | 0.00 | 2.56 | 2.57 | 2.33 | 2.56 | 2.56 | 2.10 | 1.10 | 1.10 |
| sup binary p=.50 | 24 | 0.00 | 2.30 | 1.90 | 1.98 | 2.30 | 2.30 | 2.54 | 1.89 | 1.88 |
| sup binary p=.85 | 24 | 0.04 | 2.51 | 3.45 | 3.71 | 2.51 | 2.51 | 2.38 | 1.56 | 1.56 |
| sup uniform | 24 | 0.00 | 3.21 | 2.21 | 3.08 | 2.63 | 2.44 | 2.48 | 1.76 | 1.76 |
| sup deciles 0..1 | 24 | 0.00 | 3.36 | 2.38 | 3.14 | 2.90 | 2.67 | 2.69 | 1.98 | 1.98 |
| sup x^3 skewed | 24 | 0.00 | 3.50 | 2.70 | 3.65 | 2.86 | 2.56 | 2.41 | 1.67 | 1.67 |
| sup two-point 0/10 | 24 | 0.00 | 2.19 | 1.86 | 2.00 | 2.19 | 2.19 | 2.43 | 1.87 | 1.87 |
| sup binary p=.50 | 40 | 0.00 | 2.38 | 2.24 | 2.14 | 2.38 | 2.38 | 2.62 | 2.10 | 2.10 |
| sup binary p=.85 | 40 | 0.00 | 2.30 | 3.44 | 2.80 | 2.30 | 2.30 | 2.19 | 2.07 | 2.07 |
| sup uniform | 40 | 0.00 | 2.77 | 2.16 | 2.62 | 2.48 | 2.33 | 2.30 | 1.94 | 1.93 |
| sup deciles 0..1 | 40 | 0.00 | 2.94 | 2.36 | 2.80 | 2.73 | 2.54 | 2.56 | 2.19 | 2.19 |
| sup x^3 skewed | 40 | 0.00 | 3.14 | 2.62 | 3.16 | 2.79 | 2.62 | 2.56 | 2.10 | 2.10 |
| sup two-point 0/10 | 40 | 0.00 | 2.35 | 2.27 | 2.15 | 2.35 | 2.35 | 2.59 | 2.15 | 2.15 |
| ni binary .90/.85 | 16 | 1.80 | 9.96* | 9.95* | 9.93* | 9.96* | 9.96* | 3.74 | 2.48 | 0.68 |
| ni binary .70/.65 | 16 | 0.01 | 3.65 | 3.04 | 3.27 | 3.65 | 3.65 | 2.04 | 1.09 | 1.08 |
| ni binary .55/.50 | 16 | 0.00 | 3.40 | 2.35 | 2.79 | 3.40 | 3.40 | 2.11 | 1.20 | 1.20 |
| ni continuous uniform | 16 | 0.00 | 3.66 | 1.97 | 3.42 | 2.64 | 2.31 | 2.36 | 1.31 | 1.31 |
| ni continuous x^3 skewed | 16 | 0.00 | 4.71* | 3.48 | 4.72* | 3.73 | 3.31 | 3.19 | 1.96 | 1.96 |
| ni binary .95/.90 | 16 | 8.87 | 20.33* | 20.33* | 20.33* | 20.33* | 20.33* | 9.73* | 9.05* | 0.18 |
| ni binary .97/.92 | 16 | 17.42 | 27.93* | 27.93* | 27.93* | 27.93* | 27.93* | 17.79* | 17.45* | 0.03 |
| ni binary .99/.94 | 16 | 31.90 | 37.43* | 37.43* | 37.43* | 37.43* | 37.43* | 31.91* | 31.90* | 0.00 |
| ni nested loss 5% | 16 | 44.09 | 44.09* | 44.09* | 44.09* | 44.09* | 44.09* | 44.09* | 44.09* | 0.00 |
| ni binary .90/.85 | 20 | 0.71 | 6.12* | 6.08* | 6.15* | 6.12* | 6.12* | 4.58* | 2.55 | 1.84 |
| ni binary .70/.65 | 20 | 0.00 | 2.62 | 2.08 | 2.35 | 2.62 | 2.62 | 2.98 | 1.73 | 1.73 |
| ni binary .55/.50 | 20 | 0.00 | 2.56 | 1.81 | 2.08 | 2.56 | 2.56 | 2.90 | 1.71 | 1.71 |
| ni continuous uniform | 20 | 0.00 | 3.69 | 2.37 | 3.41 | 2.91 | 2.62 | 2.66 | 1.73 | 1.73 |
| ni continuous x^3 skewed | 20 | 0.00 | 4.11* | 3.08 | 4.01* | 3.33 | 3.00 | 2.94 | 2.01 | 2.01 |
| ni binary .95/.90 | 20 | 4.63 | 13.44* | 13.45* | 13.45* | 13.44* | 13.44* | 8.21* | 5.61* | 0.97 |
| ni binary .97/.92 | 20 | 10.95 | 19.50* | 19.50* | 19.50* | 19.50* | 19.50* | 13.18* | 11.35* | 0.40 |
| ni binary .99/.94 | 20 | 24.55 | 29.98* | 29.98* | 29.98* | 29.98* | 29.98* | 25.00* | 24.58* | 0.03 |
| ni nested loss 5% | 20 | 35.77 | 35.77* | 35.77* | 35.77* | 35.77* | 35.77* | 35.77* | 35.77* | 0.00 |
| ni binary .90/.85 | 24 | 0.30 | 5.50* | 5.15* | 7.05* | 3.84 | 3.69 | 3.81 | 2.71 | 2.41 |
| ni binary .70/.65 | 24 | 0.00 | 4.15* | 2.46 | 3.94 | 2.03 | 2.03 | 2.41 | 1.38 | 1.38 |
| ni binary .55/.50 | 24 | 0.00 | 4.08* | 2.33 | 3.72 | 2.30 | 2.30 | 2.60 | 1.55 | 1.55 |
| ni continuous uniform | 24 | 0.00 | 3.24 | 2.20 | 3.06 | 2.70 | 2.53 | 2.54 | 1.78 | 1.79 |
| ni continuous x^3 skewed | 24 | 0.00 | 3.86 | 2.86 | 3.69 | 3.17 | 2.92 | 2.85 | 2.09 | 2.09 |
| ni binary .95/.90 | 24 | 2.69 | 9.99* | 9.89* | 11.19* | 9.52* | 9.38* | 9.40* | 5.92* | 3.23 |
| ni binary .97/.92 | 24 | 6.40 | 14.19* | 14.16* | 14.70* | 14.07* | 14.04* | 14.04* | 8.74* | 2.35 |
| ni binary .99/.94 | 24 | 18.37 | 23.39* | 23.38* | 23.41* | 23.38* | 23.37* | 23.37* | 18.91* | 0.53 |
| ni nested loss 5% | 24 | 29.20 | 29.20* | 29.20* | 29.20* | 29.20* | 29.20* | 29.20* | 29.20* | 0.00 |
| ni binary .90/.85 | 40 | 0.01 | 2.70 | 3.83 | 3.21 | 2.70 | 2.70 | 2.71 | 2.70 | 2.70 |
| ni binary .70/.65 | 40 | 0.00 | 2.37 | 2.33 | 2.29 | 2.37 | 2.37 | 2.61 | 2.01 | 2.01 |
| ni binary .55/.50 | 40 | 0.00 | 2.23 | 1.91 | 2.02 | 2.23 | 2.23 | 2.35 | 1.74 | 1.74 |
| ni continuous uniform | 40 | 0.00 | 3.22 | 2.51 | 3.07 | 2.84 | 2.73 | 2.79 | 2.32 | 2.32 |
| ni continuous x^3 skewed | 40 | 0.00 | 3.42 | 2.65 | 3.17 | 3.00 | 2.85 | 2.84 | 2.42 | 2.42 |
| ni binary .95/.90 | 40 | 0.24 | 3.08 | 6.49* | 4.10* | 3.08 | 3.08 | 2.96 | 2.96 | 2.73 |
| ni binary .97/.92 | 40 | 1.29 | 4.62* | 8.52* | 5.64* | 4.62* | 4.62* | 4.55* | 4.55* | 3.26 |
| ni binary .99/.94 | 40 | 5.89 | 8.77* | 9.90* | 8.89* | 8.77* | 8.77* | 8.76* | 8.76* | 2.87 |
| ni nested loss 5% | 40 | 12.61 | 12.61* | 12.61* | 12.61* | 12.61* | 12.61* | 12.61* | 12.61* | 0.00 |
| ni binary .90/.85 | 80 | 0.00 | 2.37 | 2.94 | 2.50 | 2.37 | 2.37 | 2.71 | 2.45 | 2.45 |
| ni binary .70/.65 | 80 | 0.00 | 2.23 | 2.33 | 2.14 | 2.23 | 2.23 | 2.40 | 2.20 | 2.20 |
| ni binary .55/.50 | 80 | 0.00 | 2.23 | 2.32 | 2.14 | 2.23 | 2.23 | 2.46 | 2.18 | 2.18 |
| ni continuous uniform | 80 | 0.00 | 3.01 | 2.60 | 2.94 | 2.81 | 2.74 | 2.76 | 2.49 | 2.49 |
| ni continuous x^3 skewed | 80 | 0.00 | 3.04 | 2.56 | 2.88 | 2.83 | 2.77 | 2.76 | 2.51 | 2.51 |
| ni binary .95/.90 | 80 | 0.00 | 2.79 | 4.02* | 2.92 | 2.79 | 2.79 | 3.03 | 2.98 | 2.99 |
| ni binary .97/.92 | 80 | 0.01 | 3.38 | 5.17* | 3.42 | 3.38 | 3.38 | 3.55 | 3.56 | 3.55 |
| ni binary .99/.94 | 80 | 0.29 | 4.78* | 4.35* | 3.17 | 4.78* | 4.78* | 4.83* | 4.83* | 4.83* |
| ni nested loss 5% | 80 | 1.71 | 8.13* | 1.71 | 1.71 | 8.13* | 8.13* | 8.23* | 8.23* | 8.23* |
