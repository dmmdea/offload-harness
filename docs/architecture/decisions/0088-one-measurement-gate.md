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

## Decision

One gate, `eval.Gate`, a pure function over a frozen baseline and one comparison, with a verb (`local-offload gate`).
The gate version is 2: a baseline written under another version is refused, first and for its version.

1. **R1, superiority.** A claim of "better" is ADOPT iff the interval's lower end is above 0, BLOCK iff its upper end is
   below 0, else INCONCLUSIVE. The statistic is the mean of the paired task differences over the usable pairs in sorted id
   order, read on the score form of the 95% Student-t interval (decision 12); the unit is the task. `BootstrapDeltaMean` is
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
     40, and 0.6% to 3.0% per metric at the three-metric level, against 0.3% to 1.3% for the primary's score form on the
     same data at 16 tasks (`TestEfficiencyCalibrationStudy`). Moving them was considered and not done. A false improvement
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
   non-inferiority ADOPT, and an honest harness is told to record the cost of what timed out. A `not_run`, explicit or the
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
12. **The primary is read on the score form of the Student-t interval.** The primary is the mean of the paired task
    differences, `delta`, with `delta -+ c * sn / sqrt(n - c^2)`, where `c = t(n-1, .975)` and `sn` is the standard
    deviation with divisor `n`. It is the confidence set of `(delta - mu) / sqrt(mean((d - mu)^2) / n)`, whose variance is
    taken at the boundary `mu` instead of at the estimate. Against the plain t interval it multiplies the critical value
    by `sqrt((n-1) / (n - c^2))`: 1.14 at 16 tasks, 1.10 at 20, 1.08 at 24, 1.04 at 40, 1.02 at 80. With every difference
    equal (to within a rounding error of the scores) there is no spread to read, and the interval was that one value, so
    16 ties ADOPTed a non-inferiority claim that carried an efficiency win: pass rates .95 and .90 falsely ADOPTed 9% of
    the time at 16 tasks, .99 and .94 32%, and a candidate that only loses 5% of the incumbent's passes 37% at 20 tasks.
    `delta` and `hi` are now the one value, taken from the data and not from a sum, and `lo` is the true mean below which
    the sample would be 2.5% likely, `q (w + c) - w` for `q = 0.025^(1/n)`, `c` the one value and `w` the width of the
    primary's range. A distribution with mean `t` puts at most `(w + t) / (w + c)` of its mass on `c`, so `n` equal values
    have at most that probability to the power `n`; the bound is exact, not a rule of thumb. A tie sits `0.206 w` below 0
    at 16 tasks, `0.142 w` at 24, `0.088 w` at 40 and `0.045 w` at 80: a margin of 0.05 of the range is certified by 80
    ties and not by 40. *Why only `lo`.* The incumbent wins every tie (R4), so only the end a candidate must clear is
    widened; a symmetric widening also stopped a candidate that loses every task by the same amount from being BLOCKed
    (every task lost by 1.0 on a 0..10 scale could not reject a mean of -0.5). *Why not INCONCLUSIVE for every tie.* A
    consistent win (every task +1) is a clear ADOPT of a superiority claim and stays one (`lo` is 0.59 at 16 tasks).
    A difference exactly on the margin is neither ADOPT nor BLOCK, and differences that are equal on paper but not to the
    last bit (0.95 - 0.90 against 0.70 - 0.65) are treated as equal, which closes the float edge where
    `-0.04999999999999993` cleared a margin of 0.05. With fewer than two pairs there is no interval and the numbers read
    0. `c` is a table to 200 degrees of freedom and a three-term expansion beyond, so the gate needs no special function.
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
    and at 30 workers. The percentile interval reproduces the review's figures within simulation error (3.77, 3.81 and 4.12
    against its 3.86, 3.58 and 4.10 for uniform scores, tenths and x^3-skewed scores at 16 tasks, and 6.12 against 6.35 for
    the non-inferiority cell at 20 tasks; the standard error of a rate near 4% over 20000 datasets is 0.14 points).
    *Why not one of the five.* The review named five candidates: the studentized bootstrap, BCa, the percentile widened
    about the estimate by `t/z` (and by `t/z * sqrt(n/(n-1))`), the paired t, and the lower of two. Not one stays at or
    under 4% in every cell, at `B = 2000` or `B = 10000`. A sixth, the score form of the t interval ((vi), the one chosen),
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
    a strong incumbent and a handful of disagreeing tasks at 60 tasks and more, where the score form is still
    anti-conservative (the stress table below runs it to 200 tasks: 4.1% to 5.5% at .97/.92 and .99/.94 from 60 tasks
    in the cells where it exceeds 4%, and 5.8% to 8.9% at nested loss from 80 tasks). The lower of the score form and BCa
    is the only combination that clears all 63 cells (0 over 4%, the worst 3.4% at `B = 2000` and 3.5% at `B = 10000`) and
    all 18 stress cells (at most 3.4%), and it keeps the score form's power to within 0.7 points on every power cell. It
    was **not adopted here**: BCa is a bootstrap, so it would couple the primary back to `bootstrap_b` and `seed`, which
    this decision made no longer touch it, and it changes the primary's algorithm in a repair that was asked to fix the
    all-tied case. That is the lead's decision, with these figures. The other lower-of-two rows with (vi) keep the score
    form's failure. At `B = 10000` the bootstrap intervals move by at most 0.6 points and the closed forms not at all.
    *The price.* The interval is wider than the plain t by the factors above, so power falls. At 24 tasks a true gain of
    0.1 on a pass/fail score is adopted in 8.6% of superiority runs (the paired t 10.7%, the percentile 9.7%, which
    over-adopts) and a candidate truly equal to the incumbent in 7.9% of non-inferiority runs at margin 0.05 (the paired t
    12.3%, the percentile 15.7%; the score form read 8.8% before a sample with no spread had its lower end held back, and
    at 16 tasks it read 5.8% and now reads 1.6%, because 16 ties cannot certify a margin of 0.05). The power cells:

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
    from 60 to 200 tasks (the last column is the lower of (vi) and BCa, for the reason above):

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
    interval of one value. With the lower end held back (decision 12) the nested-loss candidate is at 0% from 16 to 60
    tasks, and the strong incumbents are at most 3.3% at 16 to 40 tasks. At 80 tasks and more the sample has a loss or two
    in it and the score form accepts it: 8.2% at 80 tasks (it accepts one loss in 80), 5.8% at 120 and 6.3% at 200 for the
    nested-loss candidate, and up to 5.5% for pass rates .97/.92 and .99/.94 at 60 to 200 tasks. A zero-inflated score that
    is non-inferior at exactly the margin is adopted in 3.7% of runs at 16 tasks with the score form and 3.4% at 24. The
    remedy is more tasks, which the power note counts, a margin the corpus size can certify, or the lead's decision on the
    lower of two above.

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
  version 2 means a baseline written under version 1 is re-frozen, with a judge and, for `custom`, a range.
- **Version 2 absorbs these rules.** The gate is new in the change that carries it, version 2 has never been released,
  and no version 2 baseline exists outside tests, so the cost rule, the regression rule and the primary interval landed in
  it rather than in a version 3 that nobody ran. The fourth review's two rules landed in it too: the lower end of a sample
  with no spread, which changes the verdict of every all-tied non-inferiority comparison from ADOPT to INCONCLUSIVE below
  80 tasks, and a candidate's unfinished attempt of unknown cost, which leaves its task unresolved on that metric. A
  baseline frozen at an earlier head of this branch has the same bytes and hash under this gate, because the format did not
  change, but its verdicts can: re-run any comparison made with it.
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
- **The calibration claim is scoped to what the tests measure.** The 63 cells of decision 12 (61 at most 3.6%, two over
  4%: pass rates .99/.94 and the nested-loss candidate, both at 80 tasks), the eleven end-to-end cells of
  `TestNullAdoptRateAtTheFloorsIsCalibrated` (16 to 24 tasks, 0% to 3.0%) and the fifteen interval cells of
  `TestPrimaryIntervalFalseAdoptRateOnTheStatedNulls` (0% to 3.1%); nothing is claimed for a strong incumbent with a
  handful of disagreeing tasks from 60 tasks up (stress cells, up to 9%), below 16 tasks, or for the efficiency reads
  beyond the figures of decision 2.
- **No wall-time term enters through the gate.** Speed may be reported beside a verdict as a side column by a caller; the
  gate has no input for it and no rule that reads it.
- **Reading the gate's inputs needs adapters.** A record from an external evaluation tool becomes a gate input outside this
  repository (`OutcomeFromRecord` and the schema are what that adapter reads), and the self-test receipt for a model judge
  is a later verb.

## Measured grid

False ADOPT rate in per cent, 20000 datasets per cell, `B = 2000` (a nominal 2.5%; a star marks a cell over 4%). The
closed forms, (iv) and (vi), do not depend on `B`.

| null | tasks | all pairs equal | percentile | (i) studentized | (ii) BCa | (iii) pctl x t/z | (iii') x sqrt(n/(n-1)) | (iv) paired t | (vi) score form, ties as a point | (vi) score form, this change |
|---|---|---|---|---|---|---|---|---|---|---|
| sup binary p=.50 | 16 | 0.00 | 2.48 | 2.34 | 2.16 | 2.48 | 2.48 | 2.04 | 1.07 | 1.07 |
| sup binary p=.85 | 16 | 0.97 | 2.44 | 4.98* | 4.87* | 2.44 | 2.44 | 2.35 | 0.81 | 0.81 |
| sup uniform | 16 | 0.00 | 3.77 | 2.06 | 3.48 | 2.71 | 2.42 | 2.42 | 1.31 | 1.31 |
| sup deciles 0..1 | 16 | 0.00 | 3.81 | 2.27 | 3.64 | 3.04 | 2.69 | 2.73 | 1.53 | 1.53 |
| sup x^3 skewed | 16 | 0.00 | 4.12* | 3.06 | 4.33* | 3.16 | 2.79 | 2.56 | 1.39 | 1.39 |
| sup two-point 0/10 | 16 | 0.00 | 2.56 | 2.57 | 2.33 | 2.56 | 2.56 | 2.10 | 1.10 | 1.10 |
| sup binary p=.50 | 24 | 0.00 | 2.30 | 1.90 | 1.98 | 2.30 | 2.30 | 2.54 | 1.89 | 1.88 |
| sup binary p=.85 | 24 | 0.04 | 2.51 | 3.45 | 3.71 | 2.51 | 2.51 | 2.38 | 1.56 | 1.56 |
| sup uniform | 24 | 0.00 | 3.21 | 2.21 | 3.08 | 2.63 | 2.44 | 2.48 | 1.76 | 1.76 |
| sup deciles 0..1 | 24 | 0.00 | 3.45 | 2.38 | 3.24 | 2.90 | 2.67 | 2.69 | 1.98 | 1.98 |
| sup x^3 skewed | 24 | 0.00 | 3.50 | 2.70 | 3.65 | 2.86 | 2.56 | 2.41 | 1.67 | 1.67 |
| sup two-point 0/10 | 24 | 0.00 | 2.19 | 1.86 | 2.00 | 2.19 | 2.19 | 2.43 | 1.87 | 1.87 |
| sup binary p=.50 | 40 | 0.00 | 2.38 | 2.24 | 2.14 | 2.38 | 2.38 | 2.62 | 2.10 | 2.10 |
| sup binary p=.85 | 40 | 0.00 | 2.30 | 3.44 | 2.80 | 2.30 | 2.30 | 2.19 | 2.07 | 2.07 |
| sup uniform | 40 | 0.00 | 2.77 | 2.16 | 2.62 | 2.48 | 2.33 | 2.30 | 1.94 | 1.93 |
| sup deciles 0..1 | 40 | 0.00 | 3.02 | 2.36 | 2.90 | 2.73 | 2.54 | 2.56 | 2.19 | 2.19 |
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
| ni binary .90/.85 | 20 | 0.71 | 6.12* | 6.25* | 6.15* | 6.12* | 6.12* | 4.58* | 2.55 | 1.84 |
| ni binary .70/.65 | 20 | 0.00 | 2.62 | 2.15 | 2.35 | 2.62 | 2.62 | 2.98 | 1.73 | 1.73 |
| ni binary .55/.50 | 20 | 0.00 | 2.56 | 1.86 | 2.08 | 2.56 | 2.56 | 2.90 | 1.71 | 1.71 |
| ni continuous uniform | 20 | 0.00 | 3.69 | 2.37 | 3.41 | 2.91 | 2.62 | 2.66 | 1.73 | 1.73 |
| ni continuous x^3 skewed | 20 | 0.00 | 4.11* | 3.08 | 4.01* | 3.33 | 3.00 | 2.94 | 2.01 | 2.01 |
| ni binary .95/.90 | 20 | 4.63 | 13.44* | 13.48* | 13.45* | 13.44* | 13.44* | 8.21* | 5.61* | 0.97 |
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
| ni binary .70/.65 | 40 | 0.00 | 2.37 | 2.47 | 2.29 | 2.37 | 2.37 | 2.61 | 2.01 | 2.01 |
| ni binary .55/.50 | 40 | 0.00 | 2.23 | 1.97 | 2.02 | 2.23 | 2.23 | 2.35 | 1.74 | 1.74 |
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
