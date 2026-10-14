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

## Decision

One gate, `eval.Gate`, a pure function over a frozen baseline and one comparison, with a verb (`local-offload gate`).

1. **R1, superiority.** A claim of "better" is ADOPT iff the interval's lower end is above 0, BLOCK iff its upper end is
   below 0, else INCONCLUSIVE. The statistic is `BootstrapDeltaMean` called unchanged over the usable task pairs in sorted
   id order; the bootstrap resamples tasks.
2. **R2, non-inferiority.** A cut or an efficiency change is BLOCK iff the upper end is below `-m`, and ADOPT iff the lower
   end is above `-m` **and** at least one pre-declared efficiency metric improves (its own paired interval wholly on the good
   side of 0); else INCONCLUSIVE. Efficiency is tokens, money or resident memory in GiB. The default margins are 0.05 on a
   score in [0, 1] and 0.5 on a 0 to 10 judge scale; any other class declares its own. A metric that no task carries on both
   arms cannot improve, so a cut cannot be adopted on a missing metric. Every comparison is strict.
3. **R3, the usable denominator.** Only `pass`, `task_fail` and `cheat` attempts score, a cheat as 0, and a cheat is
   counted apart. `infra_error`, `timeout`, `judge_error` and `not_run` are excluded and reported. A task's score is the mean
   of its usable attempts; a pair is usable only if both arms have it; an excluded share above 0.20 of the declared tasks is
   INCONCLUSIVE, as is a count of usable pairs below `min_pairs` (16 by default). An unresolved cost stays unresolved and is
   never written or read as 0.
4. **R4, the incumbent wins.** The incumbent keeps every tie and every INCONCLUSIVE. The remedy for an INCONCLUSIVE at the
   corpus size is more tasks, not more attempts, because attempts of one task are not independent; the result carries a
   power note (the half-width of the interval, projected to 16, 24 and 120 tasks, and the tasks to add) so the step is a
   number.
5. **R5, held-out.** A held-out slice is declared at freeze and scored once, as a point estimate. It can turn an ADOPT into a
   BLOCK ("reject without re-tuning") and can rescue nothing.
6. **R6, pre-registration.** The mode, the primary metric and margin, the efficiency metrics, `min_pairs`, the bootstrap
   size and seed, the held-out ids and the declared tasks are frozen into a baseline file in canonical bytes and hashed
   before the first candidate run; the corpus is hashed into it, never stored. The gate refuses an input that names another
   hash, and `gate check` refuses a baseline that is not the frozen bytes. A one-byte edit of a frozen baseline either
   fails `check` or changes its hash, and `gate check --expect-hash` and the input's `baseline_hash` refuse it.
7. **R7, judge integrity.** A model judge needs a passed self-test receipt for the same scorer, or the gate refuses.
   Receipts are plain data in `internal/eval` (which must not import `internal/compeval`); the verb that produces them is a
   separate change.
8. **R8, eras.** The result carries both runs' stamps (harness build, seat configuration, engine build), the judge identity
   and an era marker, the SHA-256 of the compact JSON of those and the gate version. Results with different era markers
   are never compared; a comparison re-runs the incumbent in the new era.
9. **No wall-time term anywhere.** The efficiency class list is `tokens`, `money`, `memory_gib`; freeze refuses any other
   class, and any metric or primary name that carries a wall-time word, with a message that cites INV-5. The gate output
   holds no timestamp, so the same input twice gives byte-identical output.
10. **ADR 0080's three gates stay as the resource gates for a spill seat.** Its 0.5 bar is the effect-size minimum and
    the interval is the significance: both must hold.

## Consequences

- **The legacy callers are not migrated.** `agent-trajectory-gate` (`runAgentTrajectoryGate` in `main.go`) and the
  compaction A/B (`compeval.RunAB`) keep `B = 2000` and `seed = 42` and their own reading of the interval; the docs page
  lists them as legacy by name. `BootstrapDeltaMean` is not changed, so neither moves. New callers use the gate.
- **INCONCLUSIVE at small n is the expected answer, not a failure.** With 16 declared tasks one excluded pair already puts
  `n_used` below `min_pairs`, and the power note will usually ask for more tasks. A report that says "blocked: inconclusive
  at n = 15" with the number of tasks to add is a complete result. An operator's wish to adopt does not move a boundary.
- **A pre-registration is write-once.** `gate freeze` creates its output exclusively; a changed pre-registration is a new
  file with a new hash, and a result computed under the old hash cannot be presented under the new one.
- **The type for the attempt enum is `eval.AttemptOutcome`.** `eval.Outcome` is already the cascade eval's per-case struct,
  used by the confidence-head A/B; renaming it was not part of this decision. The taxonomy's names (`AllOutcomes`,
  `OutcomeFromCaliper`) are as specified.
- **Stricter than the minimum in four places.** Both arms must carry all three stamps and the judge must name a scorer,
  because an empty stamp would make two unrelated runs share an era marker; a task id may appear once per arm; a `pass` or
  `task_fail` score must lie in its primary class range; and a changed baseline is detected by hash even when an edit
  leaves a valid document (`gate check` alone prints that document's own hash, so a verdict adapter takes the baseline hash
  from `freeze`'s output and passes `--expect-hash`; the docs page says so).
- **No wall-time term enters through the gate.** Speed may be reported beside a verdict as a side column by a caller; the
  gate has no input for it and no rule that reads it.
- **Reading the gate's inputs needs adapters.** A Caliper-shaped record becomes a gate input outside this repository
  (`OutcomeFromCaliper` and the schema are what that adapter reads), and the self-test receipt for a model judge is a later
  verb.
