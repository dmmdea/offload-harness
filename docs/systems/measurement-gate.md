# Measurement gate

## Purpose

One gate decides every adoption in the program: a seat bake, a compaction change, a cascade cut, a skill proposal. A
candidate is compared with the incumbent on the same declared tasks: the mean of the paired task differences with a 95%
interval, under rules that were written down and hashed before the first candidate ran. The interval is Tango's score
interval for pass/fail pairs and a score form of the Student-t interval over an augmented sample for every other score
([the statistic](#the-statistic)), measured to keep its false-ADOPT rate at or under 4% in every null cell of the grids below,
the strong-incumbent cells included; the paired bootstrap the repository already had (`eval.BootstrapDeltaMean`) reads the
efficiency metrics.
The gate returns **ADOPT**, **BLOCK** or **INCONCLUSIVE**, and the incumbent keeps its place unless the verdict is
ADOPT.

It replaces the pass rules the program's earlier proposals each wrote for themselves: three different ones were in use.
This repository holds two of the pieces. `BootstrapDeltaMean` takes its `B` and seed from each call site and each caller
reads its interval its own way (`agent-trajectory-gate` turns it into a verdict, the compaction A/B only reports it; both
pass `B = 2000` and `seed = 42`). [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md)
sets a 0.5 effect-size bar on a 0 to 10 judge scale with no interval at all. [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md)
records the decision.

The gate is a pure function plus a verb. It reads scores that someone else produced, so it measures nothing itself and
needs no model, no GPU and no network. The one subcommand that can reach a model is `gate selftest`, which runs a model
scorer on a few control pairs to write the receipt a model judge needs; `freeze`, `check`, `verdict` and `schema` never do.

## The rules

| rule | in plain words |
|---|---|
| **R1 superiority** | A claim of "better": ADOPT iff the interval's lower end `lo` is above 0; BLOCK iff the upper end `hi` is below 0; otherwise INCONCLUSIVE. |
| **R2 non-inferiority** | A cut or an efficiency change ("not worse by more than the margin `m`, and cheaper"): BLOCK iff `hi < -m`; ADOPT iff `lo > -m` **and** at least one declared efficiency metric improves **and** none got significantly worse or went unmeasured; otherwise INCONCLUSIVE. See [the efficiency metrics](#the-efficiency-metrics). In superiority mode the efficiency metrics are reported and ignored for the verdict. |
| **R3 scoring and the usable denominator** | Every candidate attempt enters its task's mean. A `pass` or `task_fail` enters with its score. A `cheat`, a `timeout`, an `infra_error`, a `judge_error` and a `not_run` enter at the **worst score** of the primary range (the bad end of it, given the direction). A declared task the candidate lacks, or holds with no attempt, counts as one `not_run` attempt. So every declared task is a usable pair, and a candidate cannot raise its mean by not finishing what it would fail. **Cost follows whether the attempt ran.** A candidate's `cheat`, `timeout`, `infra_error` or `judge_error` that recorded a metric contributes that cost to its task's mean, because the attempt ran and spent it (the worst score bounds how many of them a non-inferior candidate can carry, and the `cheats` count is reported apart). A metric the adapter did not record is `null` and stays unresolved, never 0: a `pass`, `task_fail` or `cheat` without it is left out of its task's mean cost, and a candidate's `timeout`, `infra_error` or `judge_error` without it leaves its task's cost unresolved altogether, because the attempt that did not finish is the one likely to have spent the most and the attempts around it must not stand for the task (over the unresolved cap that blocks a non-inferiority ADOPT). So an adapter must record the cost of every attempt that ran. A `not_run`, explicit or the placeholder for a missing task, never ran and carries nothing. The **incumbent is not imputed**: its cheat enters at the worst score, but its `timeout`, `infra_error`, `judge_error` and `not_run` attempts are skipped, cost included, and a task with no usable incumbent attempt is an excluded pair. The asymmetry is the point: the incumbent keeps every tie, so only the candidate pays for the attempts it did not finish. The unit is the task: a task's score is the mean of its attempts as above, and the interval is over tasks. The excluded share is the largest of three ratios: excluded pairs over declared main tasks, the incumbent's unusable attempts over its attempts, and the candidate's unusable attempts over its attempts (attempt totals count the `not_run` placeholders). Above `max_excluded_share` (0.20) the verdict is INCONCLUSIVE. That cap may sit above the margin because the candidate's unusable attempts already count at the worst score: the cap no longer bounds a free exclusion, and the only exclusions left are incumbent attempts, which the candidate does not run. That holds for a frozen or trusted incumbent. An incumbent supplied in the input can use the exclusion as a lever: marking some of its own best attempts unfinished (or whole tasks it wins) lowers its mean and removes tasks from the comparison, up to the cap. Freeze the incumbent (`freeze --incumbent`) when the comparison must not be steerable from the input. |
| **R4 the incumbent wins** | The incumbent keeps every tie and every INCONCLUSIVE (`keep_incumbent` is true unless ADOPT). Fewer than `min_pairs` usable pairs is INCONCLUSIVE, and `min_pairs` cannot be frozen below 16. The remedy for an INCONCLUSIVE at the corpus size is **more tasks, not more attempts**, because attempts of one task are not independent; the power note says how many. |
| **R5 held-out** | A held-out slice is declared at freeze and scored once, with the same scoring as the main slice. It cannot rescue a verdict. It can veto one: an ADOPT whose held-out point estimate fails becomes BLOCK, "reject without re-tuning". With no usable held-out pair (every held-out incumbent task unusable) an ADOPT becomes INCONCLUSIVE with the reason "held-out evidence missing", never BLOCK: no evidence is not a failed check. |
| **R6 pre-registration** | The mode, the primary metric, its margin and (for `custom`) its range, the judge, the efficiency metrics, `min_pairs`, the bootstrap size and seed, the held-out ids and the declared tasks are written into a baseline file and hashed before the first candidate run. A verdict names that hash and the gate refuses an input that names another one. |
| **R7 judge integrity** | The judge (kind and identity) is part of the baseline, and the input must name the same one: a scorer swapped after the freeze is refused, and a model judge cannot be declared deterministic to skip its receipt. A model judge is allowed only with a passed self-test receipt for the same scorer: `scorer_id` equal to the identity, `passed` true, and `pairs_hash` and `build_sha256` each 64 lower-case hex characters. Until a scorer has a receipt, only deterministic checks (the acceptance DSL, exact match) judge. The gate only reads the receipt and checks its shape, not what its two hashes are hashes of; `gate selftest` produces it (see Verbs and exit codes). |
| **R8 eras** | Every result carries the stamps of the two runs, the judge identity and an **era marker**. Results with different era markers are never compared; a comparison re-runs the incumbent in the new era. |

The gate has **no effect-size floor**: the verdict is the interval alone. A caller that also wants a minimum effect keeps
that bar as its own rule beside the gate; an ADOPT does not say any such bar was met.

**What the gate does not check.** `corpus_hash` is an echo of the corpus file given to freeze; the gate does not
compare it with the arms. The adapter that turns runs into arms is the trust boundary for corpus content: the gate
checks the task ids of each arm against the declared list, not the text of a task.

No rule, verdict, ranking or acceptance line carries a wall-time term (INV-5, [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)).
A `timeout` is a budget failure counted as an unfinished attempt, not a duration the gate reads. Efficiency is measured
in tokens, money or resident memory in GiB, and the baseline refuses any other class.

## The statistic

For the usable, non-held-out pairs in sorted task-id order, with `inc[i]` and `cand[i]` the task scores, the primary is the
mean of the paired differences `d[i] = cand[i] - inc[i]`, reported as `delta`, with a two-sided 95% interval. `w` is the
width of the primary's score range (1 for `unit_interval`, 10 for `judge_0_10`, `hi - lo` for `custom`), and every `d[i]` lies
in `[-w, w]`. Two constructions read the interval, and the data alone decides which.

**Pass/fail pairs: Tango's score interval.** When every difference is a tie (0), a win of the whole range (`+w`) or a loss of
it (`-w`), to within 1e-9 of the range, the dataset is three counts, and the interval is the set of null differences `mu` (in
units of the range) whose score statistic stays within +-1.96 of zero (Tango 1998):

```
z(mu) = (wins - losses - n*mu) / sqrt(n * (u + v - mu^2))        u - v = mu, u and v at their maximum-likelihood values
```

`u` and `v`, the probabilities of a win and of a loss, are fitted to the counts under the constraint `u - v = mu` (the root
of a quadratic, `tangoVar0`), so the variance is the one the hypothesised difference would give and not the one the sample
happened to show. That is what holds the interval where the arms disagree on a handful of tasks, where a variance read from
the sample is small and noisy. The ends are found by bisection (80 halvings of `[-1, 1]`), so the result depends on nothing
but IEEE arithmetic and a square root, never on the platform's math library. With no discordant pair the ends are
`+-w * z^2 / (n + z^2)`, `z` = 1.96: 0.194 w at 16 tasks, 0.138 at 24, 0.088 at 40, 0.060 at 60, 0.050 at 73 and 0.046 at 80.
A margin of 0.05 of the range is certified by 73 ties and not by 72 (the chance of 73 ties when the candidate truly loses 5% of
the incumbent's passes is 2.4%, and of 61 ties 4.4%). A superiority tie is INCONCLUSIVE at any size, and a constant win of the
whole range still ADOPTs (`[0.613 w, w]` at 16 tasks).

**Every other pair of scores: the score form on an augmented sample.** Partial credit, a mean of several attempts, a judge's
integer scale: there is no lattice to put a null variance on. The interval is the score form of the Student-t interval of the
`n` differences with four pseudo-pairs added, two at `+w` and two at `-w`:

```
N      = n + 4                       the n differences and the four pseudo-pairs
m      = mean of the N values        sn = sqrt(sum((x - m)^2) / N) over the N values
c      = t(N-1, .975)                half = c * sn / sqrt(N - c^2)
lo, hi = delta - half, delta + half  (clipped to [-w, w], the range the mean cannot leave)
```

The score form takes its variance at the boundary `mu` instead of at the estimate (the augmented sample's interval is the
confidence set of `(m - mu) / sqrt(mean((x - mu)^2) / N)`); `c` comes from a table (exact to float64 up to 200 degrees of
freedom, then the normal-quantile expansion). The pseudo-pairs are the idea of Agresti and Caffo (2000, two successes and two failures) and
Agresti and Min (2005, half an observation in each cell of the 2x2 table of a matched pair) of adding pseudo-observations to a
small count, generalised to bounded scores: they inflate the variance when the differences are few, vanish as `n` grows, and give
a sample whose differences are all equal a standard error of its own, so no special case remains. Their weight is not a
published form, it is the smallest of the variants measured that stays at or under 4% in every stress null below: half a pair
at each of the four cells (Agresti-Min's weights) reads a candidate that loses 5% of the incumbent's passes at 8.6% over 80
tasks, and one pair at each end leaves a mostly pass/fail score set with a few half-credit tasks at 4.4% over 160. The
centre is the observed mean `delta` and not `m`, which would shrink it toward zero by `n/N`: a real gain keeps its size, a loss at
the margin is not flattered, and `delta` always lies inside the interval.

The two meet at an edge. A dataset is pass/fail only if every pair is, so one half-credit task among a thousand sends the whole
dataset to the second construction, whose interval is wider: sixteen ties read `+-0.194 w`, fifteen ties and one difference of
0.01 read `+-0.237 w`. That discontinuity is the price of not guessing a lattice for the scores. A scorer that is meant to be
pass/fail should be exactly so.

With fewer than two pairs there is no interval and `delta`, `lo` and `hi` read 0; the verdict is INCONCLUSIVE through
`min_pairs`. Scores or costs so large that a sum overflows to a number that is not finite are refused ("overflow"), never
reported as a verdict about infinity. `bootstrap_b` and `seed` do not touch the primary: the paired bootstrap reads the
efficiency metrics (below). `eval.BootstrapDeltaMean` is unchanged, to the bit, for its legacy callers, and a test pins it.

### Why this interval

**What was replaced.** The percentile interval of the paired bootstrap, which the gate read first, is anti-conservative on the
data the gate sees most. Under a null (no real difference, or a true difference of exactly `-margin`) a one-sided 95% interval
adopts in 2.5% of runs; over 20000 simulated datasets per cell, `B = 2000`, the percentile interval adopted in 3.8% (uniform
scores), 3.7% (scores in tenths) and 4.1% (x^3-skewed scores) of superiority runs at 16 tasks and in 6.1% of non-inferiority
runs at exactly `-margin` with 20 tasks and pass rates .90 and .85. Five candidate bootstrap and t intervals (the studentized
bootstrap, BCa, the percentile widened about the estimate by `t/z`, the paired t, the lower of two of them) were run on the same
datasets, and none stayed at or under 4% in every null cell. The score form of the Student-t interval, which gate version 2
read with the rule of three for a sample whose differences are all equal, was the best of them: 61 of the 63 null cells of the
study at most 3.6%. Its two failures and a fifth review's cells were the same regime, **a strong incumbent and a handful of
disagreeing tasks**, where an interval built on the observed variance cannot see the tasks that did not disagree: one loss in
62 tasks cleared a margin of 0.05, so a candidate that loses 5% of the incumbent's passes and wins none was adopted in 0% of
runs up to 60 tasks, 4.4% at 61, 17.7% at 62, 16.4% at 64, 8.6% at 80, 11.8% at 100, 9.4% at 160, 6.5% at 250, 5.6% at 500 and
4.3% at 1000 (exact binomial sums), pass rates .99 and .94 in 6.6% of runs at 61 tasks and 11.1% at 62, and the same shapes
at a margin of 0.10 in 15.6% at 32 tasks. Version 3 closes it.

**What was compared for version 3.** Pseudo-pair augmentation, for every pair of scores and in three weights (half a pair at each
of the four cells; one pair at each end; two pairs at each end), and Tango's score interval, which applies to pass/fail pairs and
falls back to the augmented form on every other score. Each was measured on three null grids and on the power rows, on the same
datasets. Pass/fail cells are exact multinomial sums (a dataset of pass/fail pairs is three counts; there is no sampling
error, and the sum skips the datasets under 1e-14 probability, under 1e-9 in total); the other cells are 20000 simulated
datasets with fixed seeds, a standard error of 0.11 to 0.14 points near 2.5% to 4%. The grids: the 63 null cells of the earlier
study (superiority nulls at 16, 24 and 40 tasks, non-inferiority nulls at exactly `-0.05` at 16, 20, 24, 40 and 80); a sparse grid
of 54 cells (a candidate that only loses tasks and loses 5% of them, pass rates .99 and .94, and .97 and .92, each at 16, 24,
40, 60, 61, 62, 64, 80, 100, 160, 250, 500 and 1000 tasks, and the same shapes moved to sit at a margin of 0.10 at 16, 31, 32, 40
and 80); and 49 stress cells for scores that are not pass/fail with few and coarse disagreements (a few half-credit tasks, losses to
half credit, losses of mixed size, means of 3 and 5 attempts). False-ADOPT rate, per cent, worst cell and cells over 4%:

| interval | 63 cells | 54 sparse cells | 49 stress cells | worst over every size 16 to 500 and every share of disagreeing tasks (margin 0.05 / 0.10 / superiority) |
|---|---|---|---|---|
| version 2: score form, rule of three | 8.61, 2 over | 17.73, 29 over | 15.48, 25 over | 17.73 / 15.64 / 2.48 |
| half a pair at each of four cells (Agresti-Min) | 8.61, 2 over | 8.61, 12 over | 7.20, 18 over | 12.21 / 9.85 / 2.47 |
| one pair at each end | 2.88, none | 4.61, 3 over | 4.42, 2 over | 5.97 / 5.30 / 2.50 |
| two pairs at each end, for every pair | 2.25, none | 3.43, none | 3.22, none | 4.01 / 3.83 / 2.61 |
| **Tango for pass/fail pairs, two pairs at each end otherwise (version 3)** | **3.04, none** | **2.44, none** | **3.21, none** | **3.01 / 3.11 / 2.71** |

Two pairs at each end for every pair clears the three grids but not the envelope (4.0% for a candidate that loses 5% of the
incumbent's passes at 444 tasks), and it costs the most power on pass/fail data. Tango's interval is the best calibrated
and, on pass/fail data, the most powerful of the ones that clear the grids, so pass/fail pairs go to it. The other scores keep
the augmented form because the alternatives that do not augment fail on them: the version 2 score form reads a few half-credit
tasks at up to 15.5% (`stress partial-credit nested loss 5%`, 62 tasks). No minimum-disagreement floor was needed, because a
candidate clears every cell without one.

False-ADOPT rate, per cent, version 2 against version 3, for the shapes the review named (exact sums; nominal 2.5%):

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

**The price: power.** ADOPT rate, per cent, at a true effect (exact for pass/fail rows; simulated for the rest). The gate asks
more of a sample than version 2 did where version 2 over-adopted, and less where its interval was needlessly wide:

| row | tasks | version 2 | version 3 |
|---|---|---|---|
| superiority, pass rates .50 to .60 | 16 / 24 / 40 | 4.21 / 8.46 / 12.64 | 6.92 / 8.93 / 14.69 |
| non-inferiority, pass rates .90 and .90 | 16 / 24 / 40 / 100 / 250 | 1.47 / 7.85 / 10.81 / 20.46 / 45.44 | 4.63 / 6.31 / 10.66 / 19.20 / 45.29 |
| non-inferiority, pass rates .95 and .95 | 16 / 40 / 100 / 250 | 0.32 / 16.39 / 36.47 / 71.56 | 1.96 / 12.51 / 29.72 / 68.45 |
| non-inferiority, identical outputs (every pair a tie) | 16 / 40 / 73 / 100 | 0 / 0 / 100 / 100 | 0 / 0 / 100 / 100 |
| superiority, uniform scores, a true +0.10 | 24 | 19.45 | 6.73 |
| non-inferiority, uniform scores, equal arms | 24 | 6.19 | 2.04 |
| non-inferiority, 0..10 judge integers, equal arms (margin 0.5) | 40 / 61 / 100 / 250 | 27.78 / 42.05 / 62.87 / 95.99 | 2.67 / 12.07 / 37.72 / 92.67 |
| superiority, 0..10 judge integers, a true +1 point | 40 / 61 / 100 | 79.03 / 93.69 / 99.52 | 27.02 / 69.75 / 97.23 |
| non-inferiority, means of 5 attempts, mixed difficulty, equal arms | 40 / 61 / 100 / 250 | 60.22 / 76.48 / 92.11 / 99.94 | 0.52 / 10.20 / 58.44 / 99.74 |

Identical outputs now need 73 tasks and not 61 (a sample of 61 to 72 ties was the version 2 ADOPT that carried 4.4% to 5% of
false ones). On pass/fail scores version 3 has more power at 16 tasks, where version 2 was nearly blind, and a little less from
24 to 250 tasks for a strong incumbent (up to 7 points at pass rates .95 and .95). On every other score it has **much** less below
100 tasks, because two pseudo-pairs at each end of a 0..10 range weigh against differences of a point or two: at 40 tasks a judge
scale that is truly non-inferior is adopted in 2.7% of runs against 27.8%, and a mean of 5 attempts in 0.5% against 60.2%. That
cost is real, and the ADR states it as an open follow-up (an interval for coarse lattices that does not pay it). Until then a
primary that can be scored pass/fail should be.

**What this covers, and what it does not.** The cells above: both arms win and lose tasks, or most pairs tie, 16 to 1000 tasks,
margins 0.05 and 0.10, pass/fail scores, scores in tenths, uniform, right-skewed, judge integers, attempt means and a few
half-credit tasks. For pass/fail data the figure is over every size from 16 to 500 and every share of disagreeing tasks, not only
the cells: at most 3.0% at margin 0.05 (at 20 tasks, with 35% of the tasks disagreeing), 3.1% at margin 0.10 and 2.7% for
superiority. For other scores nothing is claimed beyond the cells: a score set that is nearly pass/fail with a different coarse
shape, or a size between the cells, is not covered. Below 16 tasks nothing is claimed (`min_pairs` cannot be frozen lower), and
the efficiency reads keep the calibration given under them.

`TestPassFailFalseAdoptRateIsExactlyAtMostFourPercentWhereTheArmsBarelyDisagree` runs the exact sums on 30 sparse rows (61 to 250
tasks and the margin-0.10 sizes) with the figures computed outside the package (scipy: the restricted likelihood maximised
numerically, not by the closed form), and requires at most 4% and agreement to 2e-6; `TestNestedLossFalseAdoptRateIsTheBinomialSum`
pins the nested-loss sawtooth at 20 sizes from 16 to 1000 tasks; `TestPrimaryIntervalOnPassFailPairsIsTangosScoreInterval` and
`TestPrimaryIntervalOnOtherScoresIsTheAugmentedScoreForm` pin the interval to scipy and numpy references;
`TestTangoIntervalEndsAreWhereTheStatisticIsPlusOrMinusZ` and `TestTangoVarianceIsTheRestrictedMaximumLikelihood` check the
inversion and the variance against their definitions over thousands of draws; `TestNullAdoptRateAtTheFloorsIsCalibrated` runs `Gate` end
to end on eleven null cells at the floors (0.0% to 2.5%; skipped under `-short`) and `TestPrimaryIntervalFalseAdoptRateOnTheStatedNulls`
the interval alone over 50000 datasets on fifteen cells (0.0% to 2.5%). The package's tests take about 10 s (about 6 s with `-short`) on
a 36-core machine; the simulated cells split their trials over `GOMAXPROCS`, so a 4-core runner takes a few times that. The grids, the power
rows, the stress cells and the envelope are `TestCalibrationStudy` and `TestSparseCalibrationStudy` in `internal/eval`, off unless
`OFFLOAD_GATE_STUDY=1` (1442 s, about 24 minutes, on 36 workers; the closed-form part alone, `TestSparseCalibrationStudy`, about 5):

```
OFFLOAD_GATE_STUDY=1 go test ./internal/eval/ -run 'TestCalibrationStudy|TestSparseCalibrationStudy' -v -timeout 2h
```

**Floors.** `bootstrap_b` cannot be frozen below 2000 and `min_pairs` below 16 (the default), and `min_pairs` cannot exceed
the number of non-held-out tasks. The calibration above was measured from 16 tasks up, so 16 is the smallest sample the
gate makes a claim about. The bootstrap reads the efficiency metrics, and 2000 is the `B` the existing callers of
`BootstrapDeltaMean` use: on the cost nulls of the study at 16 tasks a metric falsely improved in 8% to 13% of runs at
`B = 10` and in 42% to 50% at `B = 1`, against 3.5% to 5.9% at `B = 2000`
(`TestEfficiencyCalibrationStudy`, same switch).

Boundaries are strict, to within a tolerance of 1e-9 of the score range: `lo > 0`, `hi < 0`, `lo > -m`, `hi < -m` each hold
only if the end clears the line by more than that. An interval that touches the line is INCONCLUSIVE, whichever way the
last bit of a float fell, and pass/fail pairs that are off by rounding (a tie that is `0.1 + 0.2` against `0.3`, 5.6e-17 apart)
are still pass/fail pairs. The test sets the margin from the interval the gate itself computed, to the bit and 1e-12 of the
range either side of it, on both ends and both score ranges, and 1e-6 past it for the contrast. The other tests build their
boundary rows from exactly representable numbers (16 pairs, scores 8.0, 7.5, 7.0) so a mutation of a comparison flips a row on
every platform.

## The efficiency metrics

Each declared efficiency metric gets its own paired bootstrap over the pairs that resolved that metric on both arms,
with the same `bootstrap_b` and seed. The values are finite and not negative (a negative value is refused at input). A
candidate's `cheat`, `timeout`, `infra_error` or `judge_error` that recorded a value contributes it (the attempt ran and
spent it, [R3](#the-rules)); a `null` value is unresolved and a `not_run` carries nothing; the incumbent's unfinished
attempts are skipped, cost included.

- **Improves.** With `k` declared metrics each one is read on the two-sided `1 - 0.05/k` interval (a Bonferroni
  correction; `k = 1` is the plain 95% interval, equal to `BootstrapDeltaMean`), so that declaring `k` metrics does not
  multiply each metric's chance of a false improvement by `k`. The result carries the `level` used. A metric improves
  when `hi < 0` (lower is better) or `lo > 0` (higher is better) on that interval and at least `min_pairs` pairs resolved
  it. The `delta`, `lo` and `hi` reported for the metric are the ones at that level.
- **Regresses.** A metric regresses when its plain 95% interval lies wholly on the worse side (`lo > 0` for
  lower-is-better, `hi < 0` for higher-is-better), read wherever the metric has an interval, that is from two resolved
  pairs: only an improvement needs `min_pairs`. In non-inferiority mode any regressing metric turns an ADOPT into
  INCONCLUSIVE, "declared efficiency metric X got significantly worse": a cut that saves tokens and costs ten times the
  memory is not adopted, even when the memory was resolved on only 15 of 18 pairs.
- **Over the unresolved cap.** A metric whose `unresolved_pairs` over the usable pairs exceeds `max_excluded_share` has
  `over_unresolved_cap` true and cannot improve. In non-inferiority mode it also blocks the ADOPT (INCONCLUSIVE, the
  reason names the metric): a declared metric that was not measured cannot be traded away. A metric that no task
  carries on both arms is the extreme case. The cap is also the one lever left on the candidate's side: a candidate whose
  `timeout`, `infra_error` and `judge_error` attempts record no cost leaves those tasks unresolved on that metric and keeps
  its efficiency win on the rest, for up to `max_excluded_share` of the tasks, so the adapter must record the cost of every
  attempt that ran.
- **Superiority mode** reports every metric's interval, `improves`, `regresses` and the cap, and ignores all of them for
  the verdict.

A metric's `delta`, `lo` and `hi` are **`null`, never 0, when fewer than two pairs resolved it** (`n < 2`): with one pair
or none there is no interval, and a 0 would read as "no change" where the truth is "no data". `n` and `unresolved_pairs`
say how much data there was. A metric with two or more resolved pairs always has numbers, and a measured 0 is a 0. The
primary `delta`, `lo` and `hi`, and the held-out `delta`, are plain numbers and read 0 when there is no data behind them
(`n_used` below 2; `held_out.n` of 0); read `n_used` and `held_out.n` first, as the verdict's reason does.

**These reads stay on the percentile bootstrap, not on the primary's interval, and they are not as well calibrated.** On
cost nulls with no real difference (independent lognormal and exponential costs, and a paired multiplicative cost; 16, 24
and 40 tasks; `TestEfficiencyCalibrationStudy`) the plain 95% read says "improves" in 3.5% to 5.9% of runs at 16 tasks, 2.1%
to 4.9% at 24 and 1.5% to 4.8% at 40 (a nominal 2.5%); read for three metrics at `1 - 0.05/3` it is 0.6% to 3.0% per
metric, not measured jointly. The score form of the t interval (the primary's form in gate version 2) on the same cost data is
0.3% to 1.3% at 16 tasks. Moving the efficiency reads to a closed form was considered and not done, for three reasons. A false improvement adopts nothing alone: the
primary has to clear the margin as well, and the false-ADOPT cells above are measured with an efficiency metric that truly
improves, so they bound what a false improvement adds, which is a non-inferior candidate that is not cheaper. The veto side
is the conservative one: a regression is read on the narrower plain interval, so a wrong veto costs a rerun and never an
adoption. And the bootstrap stays meaningful: a closed form would leave `bootstrap_b` and `seed`, two pre-registered
fields, reading nothing. The Bonferroni read is the one that was there, each of `k` metrics at `1 - 0.05/k`, so the
change leaves it exactly as conservative as it was.

## Outcomes

Each attempt carries one of seven classes (`eval.AttemptOutcome`, in `internal/eval/outcome.go`; the type is not named
`Outcome` because that name is the cascade eval's per-case struct):

| class | enters the task's score | its recorded cost | meaning |
|---|---|---|---|
| `pass` | with its score | counts | finished; the score is required |
| `task_fail` | with its score | counts | finished; the score is required |
| `cheat` | at the worst score, on both arms | counts | tried to game the scorer; counted apart |
| `infra_error` | candidate: at the worst score. Incumbent: skipped | candidate: counts. Incumbent: skipped | the run broke |
| `timeout` | candidate: at the worst score. Incumbent: skipped | candidate: counts. Incumbent: skipped | the attempt hit its budget |
| `judge_error` | candidate: at the worst score. Incumbent: skipped | candidate: counts. Incumbent: skipped | the judge could not score it |
| `not_run` | candidate: at the worst score. Incumbent: skipped | nothing, on either arm | never ran. A declared task missing from an arm, or present with no attempt, counts as one `not_run` |

A cost that was not recorded is `null`, never 0: it is left out of the task's mean on a `pass`, `task_fail` or `cheat`, and on a candidate's `timeout`, `infra_error` or `judge_error` it leaves the task's cost unresolved.

The **worst score** is the bad end of the primary's range, given `higher_is_better`: 0 for `unit_interval` and
`judge_0_10` (1 and 10 for a lower-is-better primary), and the matching end of the declared `primary.range` for
`custom`. A custom primary must declare `range` as `[lo, hi]` with both ends finite, `hi` above `lo` and `hi - lo`
finite; freeze and check refuse a custom baseline without one, and an input `pass` or `task_fail` score outside the range
is refused as the bounded classes' are. The counts of timeouts, infra errors, judge errors and `not_run` attempts of each
arm are `unusable_attempts`; the candidate attempts among them that entered at the worst score are `imputed_attempts`
(cheats stay in `cheats`).

`eval.OutcomeFromRecord` maps the outcome word of an external evaluation record to this list. The record shape it reads is
one string, the word the record's own check wrote: the six words it shares with the gate (`pass`, `task_fail`, `cheat`,
`infra_error`, `timeout`, `judge_error`) map to themselves and `not_checked` is `not_run`; anything else is unknown and the
caller must refuse it. The embedded JSON Schema's `outcome` enum is the same list and a test compares them.

## Formats

**Pre-registration** (what the operator writes; `gate freeze --prereg`). Left-out fields take defaults; unknown fields
are refused. The `judge` is required.

```json
{
  "mode": "non_inferiority",
  "primary": {"name": "answer_quality", "class": "judge_0_10"},
  "judge": {"kind": "deterministic", "identity": "acceptance-dsl"},
  "efficiency": [{"name": "forwarded_tokens", "class": "tokens"}],
  "min_pairs": 16,
  "bootstrap_b": 10000,
  "seed": 1,
  "max_excluded_share": 0.2,
  "held_out": ["task-017", "task-018"],
  "tasks": ["task-001", "task-002"]
}
```

**Baseline** (what freeze writes; `internal/eval/baseline.go`). The canonical bytes are `json.MarshalIndent(b, "", "  ")`
plus one newline, LF only, and the baseline hash is the SHA-256 of those bytes. Fields: `gate_version` ("3"), `mode`,
`primary {name, class, margin, higher_is_better, range}`, `judge {kind, identity}`,
`efficiency [{name, class, lower_is_better}]`, `min_pairs`, `bootstrap_b`, `seed`, `max_excluded_share`, `held_out`,
`corpus_hash`, `tasks` and an optional frozen `incumbent` arm. `tasks`, `held_out` and the incumbent's tasks are sorted.
`corpus_hash` is the SHA-256 of the corpus file given to freeze. The gate version is 3: a baseline of another version is
refused, first and for its version, with the advice to re-freeze it (a version 1 file has no `judge`, so its bytes are not
this gate's canonical form; a version 2 file has this gate's fields but its verdicts were read with another primary
interval; neither was edited by hand). The version moved from 2 to 3 because the primary interval changed and verdicts moved on
data that is not sparse too: a judge-scale comparison of 16 to 40 tasks that cleared a margin of 0.5 under version 2 by a
narrow primary no longer does. `internal/eval/testdata` holds a baseline as the version 1 gate wrote it and one as the version 2
gate wrote it, for the refusal tests.

| field | default | notes |
|---|---|---|
| `mode` | none | `superiority` or `non_inferiority` |
| `primary.class` and margin | | `unit_interval`: margin 0.05. `judge_0_10`: margin 0.5. `custom`: non-inferiority must carry its own margin or freeze refuses, and must declare `range`. In superiority mode the margin is ignored and recorded as 0 |
| `primary.range` | none | `[lo, hi]`, required for `custom` and refused for the other classes |
| non-inferiority margin bound | | the margin must be below the width of the primary's range: under 1 for `unit_interval`, under 10 for `judge_0_10`, under `hi - lo` for `custom`. A margin as wide as the range would let a candidate that fails every task through on cost |
| `judge` | none | required: `kind` is `deterministic` or `model`, `identity` is the scorer's name (a model id for a model judge) |
| `primary.higher_is_better` | true | `false` is refused: only higher-is-better primary scores are supported |
| `efficiency[].class` | none | `tokens`, `money` or `memory_gib`. Any other class, in particular one naming seconds, latency, wall, duration or time, is refused with a message that cites INV-5; so is a metric or primary **name** that carries such a word. Non-inferiority with no efficiency metric is refused: a cut must name what it saves |
| `efficiency[].lower_is_better` | true | |
| `min_pairs` | 16 | at least 16, and at most the number of tasks that are not held out |
| `bootstrap_b` | 10000 | at least 2000; the resamples that read the efficiency metrics |
| `seed` | 1 | |
| `max_excluded_share` | 0.20 | within 0 to 1 |

Freeze also refuses an empty corpus, a task list that is empty, unsorted, repeated or holds an empty id, a held-out id that
is not a declared task, a pre-registration that holds out every task, and a frozen incumbent that does not fit the
declared tasks. `gate check` runs the same rules on a baseline file, so a hand-built canonical file below a floor is
refused.

Corpus conventions (docs only, nothing is committed): a corpus is harvested redacted at harvest time, hash-stamped,
machine-local and never committed; `freeze` and `check` reproduce from the hash, as `compaction-eval` does.

**Gate input** (what `gate verdict --input` reads; `gate schema` prints its JSON Schema, draft 2020-12). `baseline_hash`;
`incumbent` (an arm; required unless the baseline froze one; when both exist they must be equal); `candidate` (an arm);
`judge {kind, identity}`, which must be the judge the baseline froze; `receipt {scorer_id, pairs_hash, passed, build_sha256}`
for a model judge (`gate selftest` prints it; the two hashes are 64 lower-case hex characters). An arm is
`{stamps {harness_build_sha256, seat_config_sha256, engine_build}, tasks [{id, attempts [{outcome, score, metrics}]}]}`.
Stamps come from the runs that produced the scores, never from the gate binary. A metric value is finite and not
negative. A metric value of `null` (or an absent key) is **unresolved**: it stays unresolved end to end, the pair is
skipped for that metric and counted in `unresolved_pairs`, and it is never written or read as 0. On a candidate's
`timeout`, `infra_error` or `judge_error` it leaves the whole task unresolved for that metric, not just that attempt, so
record the cost of what timed out.

**Result** (`eval.GateResult`, snake_case, struct order, no timestamp): `verdict`, `reason`, `keep_incumbent`, `mode`,
`margin`, `delta`, `lo`, `hi` (the observed mean paired difference and its 95% interval), `n_used`, `n_declared`, `excluded_pairs`, `excluded_share`, `attempt_counts` (all seven
classes per arm, zeros included), `cheats`, `imputed_attempts` (candidate attempts that entered at the worst score
because they were `timeout`, `infra_error`, `judge_error` or `not_run`), `unusable_attempts` (per arm, those four classes
together, the `not_run` placeholders for missing tasks included), `efficiency` (per metric: `delta`, `lo` and `hi`, each
`null` below two resolved pairs, then `n`, `unresolved_pairs`, `improves`, `over_unresolved_cap`, `regresses` and `level`,
the two-sided level of `lo` and `hi`), `held_out` (`{n, delta, failed, missing}` or `null` when none is declared;
`missing` is true when `n` is 0 and `failed` is true only when `n` is above 0 and the delta is below the threshold),
`power`, `stamps`, `judge`, `era_marker`, `gate_version`, `corpus_hash`, `baseline_hash`, `bootstrap_b`, `seed`, and the
verb's `gate_build_sha256`. `n_declared` is the number of declared tasks that are not held out. The same input twice gives
byte-identical output.

## Refusals

The gate returns an error and no verdict for: an input `baseline_hash` that is not the hash of the baseline handed in (or
a baseline whose bytes no longer hash to it); a judge kind other than `deterministic` or `model`; an empty judge
identity; a judge whose kind or identity is not the one the baseline froze; a `model` judge with no receipt, a receipt
with `passed` false, a receipt for another scorer, or a receipt whose `pairs_hash` or `build_sha256` is not 64
lower-case hex characters; an incumbent absent from both the input and the baseline; an incumbent in both that is not
equal (tasks compared in id order); an input incumbent whose stamps differ from the frozen incumbent's (another era); a
task id in either arm that the baseline never declared, or one that appears twice in an arm; a `pass` or `task_fail`
attempt with no score, or with a score outside the primary range (`unit_interval` 0 to 1, `judge_0_10` 0 to 10, `custom`
the frozen range); an efficiency value that is not finite or is negative; scores or costs whose sums overflow;
an outcome word outside the seven; incomplete stamps on either arm. A refusal exits 2 from the verb.

## Verbs and exit codes

```
local-offload gate freeze   --prereg P --corpus C [--incumbent ARM] --out BASELINE
local-offload gate check    --baseline BASELINE [--expect-hash SHA256]
local-offload gate verdict  --baseline BASELINE --input INPUT [--require-adopt]
local-offload gate selftest --pairs PAIRS --scorer ID [--out RECEIPT]
local-offload gate schema
```

- `freeze` validates, hashes the corpus, writes the canonical baseline and prints `{"baseline": <path>, "baseline_hash": <sha>}`.
  The output file is created exclusively: freezing the same inputs again to the same path is a no-op, and a changed
  pre-registration goes to a new path (and has a new hash).
- `check` reads the baseline's `gate_version` first (another version is refused for it, with the advice to re-freeze),
  then parses the baseline, re-marshals it canonically and refuses unless the bytes are identical ("not in canonical
  form: edited by hand?"), then prints the hash. **A one-byte edit of a frozen baseline either fails `check` or changes
  its hash, and `gate check --expect-hash` and the input's `baseline_hash` refuse it.** What `check` alone cannot do is
  refuse an edit that leaves a valid, canonical document (a letter inside a name, the value of `seed`): that is
  another pre-registration, `check` prints its hash, and only the hash tells it from the frozen one. So a verdict
  adapter takes `baseline_hash` from `freeze`'s output (or a hash it recorded then), never from a later `gate check` of
  the file, and passes `--expect-hash` when it wants `check` itself to refuse.
- `verdict` runs the gate and prints the result as indented JSON.
- `selftest` ranks a file of control pairs with a named scorer and prints the receipt a model judge needs (rule R7), in
  the shape of the gate input's `receipt`: `{"scorer_id", "pairs_hash", "passed", "build_sha256"}`.
  - `--pairs` is a JSON array of `{name, good, degraded}`, the control pairs of the compaction eval. Unknown fields and
    trailing data are refused, and so is an empty array: a self-test with nothing to rank proves nothing.
  - `pairs_hash` is the SHA-256 of the pairs file's bytes, so the same pairs in another file are another hash.
    `build_sha256` is the SHA-256 of the executable that ran the self-test (the hash `verdict` prints as
    `gate_build_sha256`). `scorer_id` is the `--scorer` id, and the judge identity the baseline freezes is that id.
  - The scorer must score every pair's good side above its degraded side. A tie is a failure, and the first
    mis-ranking ends the run. A failed self-test still prints the receipt, with `passed` false, writes the mis-ranking
    to stderr and exits 1; the gate refuses that receipt. A run that never reached a finding (an unreadable, malformed or
    empty pairs file, an unknown scorer, a scorer that returned an error, a build that cannot be hashed) prints no
    receipt and exits 2.
  - `--out` also writes the receipt to a file and replaces one that is there, so that a failing run does not leave an
    older passing receipt behind.
  - Scorers: `compaction-outcome`, the outcome scorer of `compaction-eval ab` (accept, then entity recall over a summary
    of the transcript, through the configured seat). It reads `--config`. A seat that is down scores every side 0, which
    is a tie, so it fails the self-test rather than erroring. The receipt records the scorer id, the pairs and the
    harness build that ran the self-test; it does not say which model answered, which the stamps of the runs carry
    (`seat_config_sha256`).
  - What the gate checks of a receipt: that its `scorer_id` is the judge identity, that `passed` is true, and that both
    hashes are 64 lower-case hex characters. It does not compare `pairs_hash` with any pairs file or `build_sha256`
    with any build, so a receipt written by an older build still certifies a scorer that kept its id. A scorer whose
    behaviour changes is given a new receipt, or a new id, by convention; the gate does not enforce it. Comparing
    `build_sha256` with the runs' `harness_build_sha256` would need the build that scores to be the build that ran the
    record, which it is not today (a model scorer can run from another build).
- `schema` prints the embedded input schema.
- A global `--config <path>` (or `--config=<path>`) before the subcommand, as in `local-offload --config x.json gate
  schema`, is accepted by every subcommand. `selftest` reads it, to reach the seat of a scorer that calls one; the
  others ignore it: the gate reads no config.
- `-h` and `--help` print the usage and exit 0, as the other verbs do whose flags are parsed with `flag.ExitOnError`.

| exit | meaning |
|---|---|
| 0 | the command ran; for `verdict`, a verdict was produced (ADOPT, BLOCK or INCONCLUSIVE); for `selftest`, the scorer ranked every pair; also `-h` |
| 1 | `verdict --require-adopt` and the verdict is not ADOPT; `selftest` and the scorer mis-ranked a pair (the result, the receipt with `passed` false, is still printed) |
| 2 | a refusal (bad hash, bad receipt, malformed or unreadable input, a baseline that is not the frozen bytes, a freeze the gate cannot honour; for `selftest`, no pairs, an unknown scorer, a scorer that returned an error, or a build that cannot be hashed), a result that could not be written, or a usage error |

Every failure of `verdict`, and every `selftest` run that did not reach a finding, is 2, so 1 can only mean a finding.
Read the verdict from stdout, not the exit code alone (`-h` exits 0, and a refusal prints no verdict): 0 from `verdict`
means a verdict was produced, whichever it is; use `--require-adopt` when the exit code has to mean ADOPT. Read a
receipt's `passed`, not the exit code alone, before it goes into a gate input. The verb reads the files it is named
and writes `freeze --out` and `selftest --out`. With its real scorer, `selftest` also reads the config and runs the
scorer through the production pipeline, so it writes what any pipeline call writes: ledger rows and the result cache
among them.

## The power note

`power` is the half-width `hw = (hi - lo) / 2` of the main interval read three ways, with no extra statistic and with
the interval the gate itself reads. For a pass/fail record (every pair a tie, a win or a loss of the whole range, read on
Tango's interval) `projected` is that interval's half-width at `n' = 16, 24, 120` usable tasks with the observed shares
of wins and losses held, and `tasks_needed` is the smallest count whose half-width is below `m`: an all-tie record's
half-width is `z^2 / (n + z^2)`, so a 0.05 margin is resolved at 73 ties, where a `1 / sqrt(n)` curve asked for 240. For
any other record `projected` is `hw * sqrt(n / n')` (the sizes of the digest contract set, a blind pack and the
confidence-calibration probe) and `tasks_needed` is `ceil(n * (hw / m)^2)`. `m` is the margin (non-inferiority) or the
observed `|delta|` (superiority); `tasks_to_add` is `max(0, tasks_needed - n)`. `tasks_needed` and
`tasks_to_add` are numbers, or the word `"unbounded"` when no finite answer follows (superiority with `delta` of 0, fewer
than two usable pairs, or a projection past a billion tasks). It is the corpus-growth step rule R4 prescribes: add tasks,
not attempts. It does not make `n_used` reach `min_pairs`; the verdict's reason says that separately.

## The era marker

`era_marker` is the hex SHA-256 of the compact JSON of

```
{"gate_version": ..., "incumbent_stamps": {...}, "candidate_stamps": {...}, "judge_identity": ...}
```

in that field order (each stamps object in the order of the input format above), and of nothing else: not the scores,
not the judge kind, not the gate binary's own hash (reported separately as `gate_build_sha256`). Two results with
different era markers were produced under different harness builds, seat configurations, engine builds or judges and are
**never compared**. To compare, re-run the incumbent in the new era. Scores from a model judge are never compared with
scores from another judge for the same reason, whatever scale they share.

## Legacy callers

These two call `eval.BootstrapDeltaMean` directly with `B = 2000` and `seed = 42`, read their own interval and are **not
migrated** by the gate; they stay as they are:

- the `agent-trajectory-gate` verb (`runAgentTrajectoryGate` in `learn_cmd.go`);
- the compaction A/B (`compeval.RunAB` in `internal/compeval/abtest.go`).

New callers use the gate. `BootstrapDeltaMean` itself is unchanged, to the bit, and a test pins it.

## ADR 0080 and the resource gates

[ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md) sets three gates for a
RAM-spill seat: a blind pack that beats the incumbent by more than a 0.5 bar, host RSS within the weights plus 1 GiB and
released on unload, and no abort and no swap-in. They stay **resource gates**. The measurement gate has no effect-size
floor, so it does not apply the 0.5 bar: its verdict is the interval alone. A caller that wants the bar keeps it as its
own rule beside the gate's verdict, and an ADOPT does not say it was met. In gate terms, a seat claim on a 0 to 10 judge
scale is a superiority or non-inferiority run on class `judge_0_10`; the gate decides the interval, and the caller's bar
and the lease and ledger measurements decide the rest.

## Source map

- [`internal/eval/gate.go`](../../internal/eval/gate.go) — `Gate`, `GateInput`, `GateResult`, the scoring, the verdict rules, the power note, the era marker
- [`internal/eval/baseline.go`](../../internal/eval/baseline.go) — `PreReg`, `Baseline`, `FreezeBaseline`, `CheckBaseline`, `BaselineHash`, the freeze-time validation
- [`internal/eval/outcome.go`](../../internal/eval/outcome.go) — `AttemptOutcome`, `AllOutcomes`, `OutcomeFromRecord`, `Refusal`
- [`internal/eval/gate_input.go`](../../internal/eval/gate_input.go) — `Attempt`, `Task`, `Arm`, `Stamps`, arm validation, the INV-5 name check, the embedded schema
- [`internal/eval/gate-input.schema.json`](../../internal/eval/gate-input.schema.json) — the gate input JSON Schema
- [`internal/eval/interval.go`](../../internal/eval/interval.go) — `primaryInterval`, Tango's score interval for pass/fail pairs and the augmented score form for every other score, and the Student-t critical values it reads
- [`internal/eval/paireddelta.go`](../../internal/eval/paireddelta.go) — `BootstrapDeltaMean` (the legacy callers' statistic, unchanged) and the replicate and percentile helpers the efficiency read shares
- [`internal/eval/gate_test.go`](../../internal/eval/gate_test.go), [`baseline_test.go`](../../internal/eval/baseline_test.go), [`outcome_test.go`](../../internal/eval/outcome_test.go), [`interval_test.go`](../../internal/eval/interval_test.go) — the verdict table, boundary rows, refusals, determinism, the interval against independent computations
- [`internal/eval/gate_attack_test.go`](../../internal/eval/gate_attack_test.go) — the attacks a candidate that wants to be adopted would try: unfinished attempts and the cost they burned, a metric resolved on few pairs, a swapped judge, a costlier metric; and the end-to-end false-ADOPT cells
- [`internal/eval/gate_exact_test.go`](../../internal/eval/gate_exact_test.go) — the exact multinomial sums of the false-ADOPT rate on pass/fail pairs, and the interval pinned to scipy and numpy references
- [`internal/eval/gate_sim_test.go`](../../internal/eval/gate_sim_test.go) — the bootstrap calibration harness: the nulls, the candidate intervals, the study that prints the grid
- [`internal/eval/gate_sparse_sim_test.go`](../../internal/eval/gate_sparse_sim_test.go) — the closed-form study: the sparse-regime grid, the stress nulls, the power rows and the envelope
- [`internal/eval/testdata/baseline-v1-frozen.json`](../../internal/eval/testdata/baseline-v1-frozen.json), [`baseline-v2-frozen.json`](../../internal/eval/testdata/baseline-v2-frozen.json) — baselines as the version 1 and version 2 gates wrote them, for the refusal tests
- [`gate_cmd.go`](../../gate_cmd.go), [`gate_cmd_test.go`](../../gate_cmd_test.go), [`gate_cmd_attack_test.go`](../../gate_cmd_attack_test.go) — the `gate` verb and its exit codes
- [`main.go`](../../main.go) — the `gate` case and its help line
- [`compaction_eval.go`](../../compaction_eval.go), [`internal/compeval/ratchet.go`](../../internal/compeval/ratchet.go) — the freeze/check shape the baseline copies
- [`internal/compeval/abtest.go`](../../internal/compeval/abtest.go) — `RunAB`, a legacy caller
- [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md), [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md), [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)
