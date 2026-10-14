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
at small n, and a way to bind the rules to the result so that they cannot move afterwards.

[ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) already fixes one constraint on any such gate:
acceptance criteria and pass rules carry no wall-time term (INV-5).

A first version of the rules below was attacked the way a candidate that wants to be adopted would attack it, and the
attacks worked: a candidate that timed out on 4 of 20 tasks was adopted at the 0.20 excluded-share cap, four times the
0.05 margin; a candidate whose other attempts did not finish, against an incumbent that finished and failed them, was
adopted; a scorer other than the frozen one was accepted; a margin of 2 on a [0, 1] score let a candidate that failed
every task through on cost. Rules 2, 3 and 6 to 8 below, and the floors, close those holes; each says why.

## Decision

One gate, `eval.Gate`, a pure function over a frozen baseline and one comparison, with a verb (`local-offload gate`).
The gate version is 2: a baseline written under another version is refused.

1. **R1, superiority.** A claim of "better" is ADOPT iff the interval's lower end is above 0, BLOCK iff its upper end is
   below 0, else INCONCLUSIVE. The statistic is `BootstrapDeltaMean` called unchanged over the usable task pairs in sorted
   id order; the bootstrap resamples tasks. A test pins the function's results on a fixed input, so the legacy callers
   cannot move.
2. **R2, non-inferiority.** A cut or an efficiency change is BLOCK iff the upper end is below `-m`, and ADOPT iff the lower
   end is above `-m` **and** at least one pre-declared efficiency metric improves **and** none of them got significantly
   worse or went unmeasured; else INCONCLUSIVE. Efficiency is tokens, money or resident memory in GiB, finite and not
   negative. The default margins are 0.05 on a score in [0, 1] and 0.5 on a 0 to 10 judge scale; a `custom` class declares
   its own. Every comparison is strict. The efficiency read has four parts, each with its reason:
   - *Several metrics are read at the Bonferroni level.* "Any of k metrics improves" is a best-of-k, so false improvements
     would run at about k times 2.5%. With k declared metrics each is read on the two-sided `1 - 0.05/k` interval, computed
     from one bootstrap distribution by reading other percentiles; with one metric it is the plain 95% interval,
     identical to `BootstrapDeltaMean`. The level is in the result.
   - *A regression vetoes.* A metric regresses when its plain 95% interval lies wholly on the worse side with at least
     `min_pairs` resolved pairs; in non-inferiority mode any regression turns an ADOPT into INCONCLUSIVE. Without this a cut
     that saves tokens and costs ten times the memory was adopted. The plain interval is used for the veto because more
     vetoes is the conservative side.
   - *An unmeasured metric cannot be traded away.* A metric whose unresolved pairs over the usable pairs exceed
     `max_excluded_share` cannot improve and, in non-inferiority mode, blocks the ADOPT (INCONCLUSIVE, naming it). Without
     this a candidate could report null cost on the tasks where it is dearer.
   - *Superiority mode reports efficiency and ignores it for the verdict.*
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
   candidate does not run. The result counts `imputed_attempts` and, per arm, `unusable_attempts`. An unresolved cost stays
   unresolved and is never written or read as 0; an imputed attempt carries a score and no cost.
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
   refuse it. Two floors are part of validation, with their measurement: `bootstrap_b` at least 2000 (the existing
   callers use 2000; the default stays 10000) and `min_pairs` at least 16 (the default), and `min_pairs` at most the number
   of non-held-out tasks. On null data the first version adopted in 5.4% of simulated runs at 8 pairs and 3.1% at 16 pairs
   with `B = 2000`, against a nominal 2.5%; at `B = 10` it adopted in 9% and at `B = 1` in 45%, and two pairs at +0.01 each
   adopted. A test repeats the 16-pair, `B = 2000` run over 3000 trials and requires at most 4%.
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
    `flag.ExitOnError`.

## Consequences

- **The legacy callers are not migrated.** `agent-trajectory-gate` (`runAgentTrajectoryGate` in `main.go`) and the
  compaction A/B (`compeval.RunAB`) keep `B = 2000` and `seed = 42` and their own reading of the interval; the docs page
  lists them as legacy by name. `BootstrapDeltaMean` is not changed, so neither moves. New callers use the gate.
- **INCONCLUSIVE at small n is the expected answer, not a failure.** With 16 declared tasks and the floor at 16, one
  unusable incumbent task already puts `n_used` below `min_pairs`, and the power note will usually ask for more tasks. A
  report that says "blocked: inconclusive at n = 15" with the number of tasks to add is a complete result. An operator's
  wish to adopt does not move a boundary.
- **A pre-registration is write-once.** `gate freeze` creates its output exclusively; a changed pre-registration is a new
  file with a new hash, and a result computed under the old hash cannot be presented under the new one. Moving to gate
  version 2 means a baseline written under version 1 is re-frozen, with a judge and, for `custom`, a range.
- **The type for the attempt enum is `eval.AttemptOutcome`.** `eval.Outcome` is already the cascade eval's per-case struct,
  used by the confidence-head A/B; renaming it was not part of this decision. The taxonomy's helpers are `AllOutcomes` and
  `OutcomeFromRecord`, which maps the outcome word of an external evaluation record to the taxonomy: the six shared words
  and `not_checked`, which is `not_run`.
- **Stricter than the minimum in several places.** Both arms must carry all three stamps and the judge must name a scorer,
  because an empty stamp would make two unrelated runs share an era marker; a task id may appear once per arm; a `pass` or
  `task_fail` score must lie in its primary class range; an efficiency value must be finite and not negative; scores or
  costs whose bootstrap sums overflow are refused; and a changed baseline is detected by hash even when an edit leaves a
  valid document (`gate check` alone prints that document's own hash, so a verdict adapter takes the baseline hash from
  `freeze`'s output and passes `--expect-hash`; the docs page says so).
- **No wall-time term enters through the gate.** Speed may be reported beside a verdict as a side column by a caller; the
  gate has no input for it and no rule that reads it.
- **Reading the gate's inputs needs adapters.** A record from an external evaluation tool becomes a gate input outside this
  repository (`OutcomeFromRecord` and the schema are what that adapter reads), and the self-test receipt for a model judge
  is a later verb.
