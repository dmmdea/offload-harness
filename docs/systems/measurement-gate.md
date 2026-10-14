# Measurement gate

## Purpose

One gate decides every adoption in the program: a seat bake, a compaction change, a cascade cut, a skill proposal. A
candidate is compared with the incumbent on the same declared tasks, with the statistic the repository already had
(`eval.BootstrapDeltaMean`, a paired bootstrap of the mean difference with a 95% interval), under rules that were
written down and hashed before the first candidate ran. The gate returns **ADOPT**, **BLOCK** or **INCONCLUSIVE**, and
the incumbent keeps its place unless the verdict is ADOPT.

It replaces the pass rules the program's earlier proposals each wrote for themselves: three different ones were in use.
This repository holds two of the pieces. `BootstrapDeltaMean` takes its `B` and seed from each call site and each caller
reads its interval its own way (`agent-trajectory-gate` turns it into a verdict, the compaction A/B only reports it; both
pass `B = 2000` and `seed = 42`). [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md)
sets a 0.5 effect-size bar on a 0 to 10 judge scale with no interval at all. [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md)
records the decision.

The gate is a pure function plus a verb. It reads scores that someone else produced, so it measures nothing itself and
needs no model, no GPU and no network.

## The rules

| rule | in plain words |
|---|---|
| **R1 superiority** | A claim of "better": ADOPT iff the interval's lower end `lo` is above 0; BLOCK iff the upper end `hi` is below 0; otherwise INCONCLUSIVE. |
| **R2 non-inferiority** | A cut or an efficiency change ("not worse by more than the margin `m`, and cheaper"): BLOCK iff `hi < -m`; ADOPT iff `lo > -m` **and** at least one declared efficiency metric improves **and** none got significantly worse or went unmeasured; otherwise INCONCLUSIVE. See [the efficiency metrics](#the-efficiency-metrics). In superiority mode the efficiency metrics are reported and ignored for the verdict. |
| **R3 scoring and the usable denominator** | Every candidate attempt enters its task's mean. A `pass` or `task_fail` enters with its score. A `cheat`, a `timeout`, an `infra_error`, a `judge_error` and a `not_run` enter at the **worst score** of the primary range (the bad end of it, given the direction). A declared task the candidate lacks, or holds with no attempt, counts as one `not_run` attempt. So every declared task is a usable pair, and a candidate cannot raise its mean by not finishing what it would fail. The **incumbent is not imputed**: its cheat enters at the worst score, but its `timeout`, `infra_error`, `judge_error` and `not_run` attempts are skipped, and a task with no usable incumbent attempt is an excluded pair. The asymmetry is the point: the incumbent keeps every tie, so only the candidate pays for the attempts it did not finish. The unit is the task: a task's score is the mean of its attempts as above, and the bootstrap resamples tasks. The excluded share is the largest of three ratios: excluded pairs over declared main tasks, the incumbent's unusable attempts over its attempts, and the candidate's unusable attempts over its attempts (attempt totals count the `not_run` placeholders). Above `max_excluded_share` (0.20) the verdict is INCONCLUSIVE. That cap may sit above the margin because the candidate's unusable attempts already count at the worst score: the cap no longer bounds a free exclusion, and the only exclusions left are incumbent attempts, which the candidate does not run. An unresolved cost never becomes 0, and an imputed attempt contributes a score and no cost. |
| **R4 the incumbent wins** | The incumbent keeps every tie and every INCONCLUSIVE (`keep_incumbent` is true unless ADOPT). Fewer than `min_pairs` usable pairs is INCONCLUSIVE, and `min_pairs` cannot be frozen below 16. The remedy for an INCONCLUSIVE at the corpus size is **more tasks, not more attempts**, because attempts of one task are not independent; the power note says how many. |
| **R5 held-out** | A held-out slice is declared at freeze and scored once, with the same scoring as the main slice. It cannot rescue a verdict. It can veto one: an ADOPT whose held-out point estimate fails becomes BLOCK, "reject without re-tuning". With no usable held-out pair (every held-out incumbent task unusable) an ADOPT becomes INCONCLUSIVE with the reason "held-out evidence missing", never BLOCK: no evidence is not a failed check. |
| **R6 pre-registration** | The mode, the primary metric, its margin and (for `custom`) its range, the judge, the efficiency metrics, `min_pairs`, the bootstrap size and seed, the held-out ids and the declared tasks are written into a baseline file and hashed before the first candidate run. A verdict names that hash and the gate refuses an input that names another one. |
| **R7 judge integrity** | The judge (kind and identity) is part of the baseline, and the input must name the same one: a scorer swapped after the freeze is refused, and a model judge cannot be declared deterministic to skip its receipt. A model judge is allowed only with a passed self-test receipt for the same scorer: `scorer_id` equal to the identity, `passed` true, and `pairs_hash` and `build_sha256` each 64 lower-case hex characters. Until a scorer has a receipt, only deterministic checks (the acceptance DSL, exact match) judge. The gate only reads the receipt; the verb that produces it (`gate selftest`) is a later change. |
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

For the usable, non-held-out pairs in sorted task-id order, with `inc[i]` and `cand[i]` the task scores, the gate calls

```
delta, lo, hi = eval.BootstrapDeltaMean(inc, cand, bootstrap_b, seed)
```

unchanged: `delta` is the mean of the bootstrap replicates of `cand - inc`, `lo` and `hi` are their 2.5% and 97.5%
points. A test pins the function's results on a fixed input, so the legacy callers cannot move. With fewer than two
pairs the function returns zeros and the verdict is INCONCLUSIVE through `min_pairs`. Scores or costs so large that a
bootstrap sum overflows to a number that is not finite are refused ("overflow"), never reported as a verdict about
infinity.

**Floors.** `bootstrap_b` cannot be frozen below 2000 and `min_pairs` below 16 (the default), and `min_pairs` cannot
exceed the number of non-held-out tasks. The reason is measured: on null data (no real difference between the arms) the
gate adopted in 5.4% of simulated runs at 8 pairs and in 3.1% at 16 pairs with `B = 2000`, against a nominal 2.5%; at
`B = 10` it adopted in 9% and at `B = 1` in 45%. A test repeats the 16-pair, `B = 2000` run over 3000 trials and
requires an ADOPT rate of at most 4%.

Boundaries are strict: `lo > 0`, `hi < 0`, `lo > -m`, `hi < -m`. An interval that touches the line is INCONCLUSIVE. The
tests build their boundary rows from exactly representable numbers (16 pairs, scores 8.0, 7.5, 7.0) so a mutation of a
comparison flips a row on every platform.

## The efficiency metrics

Each declared efficiency metric gets its own paired bootstrap over the pairs that resolved that metric on both arms,
with the same `bootstrap_b` and seed. The values are finite and not negative (a negative value is refused at input), and
a candidate's imputed attempt carries no cost, so a timed-out task leaves its cost unresolved.

- **Improves.** With `k` declared metrics each one is read on the two-sided `1 - 0.05/k` interval (a Bonferroni
  correction; `k = 1` is the plain 95% interval, equal to `BootstrapDeltaMean`), so the chance that some metric falsely
  improves stays near 2.5% rather than `k` times that. The result carries the `level` used. A metric improves when `hi < 0`
  (lower is better) or `lo > 0` (higher is better) on that interval and at least `min_pairs` pairs resolved it. The
  `delta`, `lo` and `hi` reported for the metric are the ones at that level.
- **Regresses.** A metric regresses when its plain 95% interval lies wholly on the worse side (`lo > 0` for
  lower-is-better, `hi < 0` for higher-is-better) with at least `min_pairs` resolved pairs. In non-inferiority mode any
  regressing metric turns an ADOPT into INCONCLUSIVE, "declared efficiency metric X got significantly worse": a cut that
  saves tokens and costs ten times the memory is not adopted.
- **Over the unresolved cap.** A metric whose `unresolved_pairs` over the usable pairs exceeds `max_excluded_share` has
  `over_unresolved_cap` true and cannot improve. In non-inferiority mode it also blocks the ADOPT (INCONCLUSIVE, the
  reason names the metric): a declared metric that was not measured cannot be traded away. A metric that no task
  carries on both arms is the extreme case.
- **Superiority mode** reports every metric's interval, `improves`, `regresses` and the cap, and ignores all of them for
  the verdict.

A metric's `delta`, `lo` and `hi` are **`null`, never 0, when fewer than two pairs resolved it** (`n < 2`): with one pair
or none there is no interval, and a 0 would read as "no change" where the truth is "no data". `n` and `unresolved_pairs`
say how much data there was. A metric with two or more resolved pairs always has numbers, and a measured 0 is a 0. The
primary `delta`, `lo` and `hi`, and the held-out `delta`, are plain numbers and read 0 when there is no data behind them
(`n_used` below 2; `held_out.n` of 0); read `n_used` and `held_out.n` first, as the verdict's reason does.

## Outcomes

Each attempt carries one of seven classes (`eval.AttemptOutcome`, in `internal/eval/outcome.go`; the type is not named
`Outcome` because that name is the cascade eval's per-case struct):

| class | enters the task's mean | meaning |
|---|---|---|
| `pass` | with its score | finished; the score is required |
| `task_fail` | with its score | finished; the score is required |
| `cheat` | at the worst score, on both arms | tried to game the scorer; counted apart |
| `infra_error` | candidate: at the worst score. Incumbent: skipped | the run broke |
| `timeout` | candidate: at the worst score. Incumbent: skipped | the attempt hit its budget |
| `judge_error` | candidate: at the worst score. Incumbent: skipped | the judge could not score it |
| `not_run` | candidate: at the worst score. Incumbent: skipped | never ran. A declared task missing from an arm, or present with no attempt, counts as one `not_run` |

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
plus one newline, LF only, and the baseline hash is the SHA-256 of those bytes. Fields: `gate_version` ("2"), `mode`,
`primary {name, class, margin, higher_is_better, range}`, `judge {kind, identity}`,
`efficiency [{name, class, lower_is_better}]`, `min_pairs`, `bootstrap_b`, `seed`, `max_excluded_share`, `held_out`,
`corpus_hash`, `tasks` and an optional frozen `incumbent` arm. `tasks`, `held_out` and the incumbent's tasks are sorted.
`corpus_hash` is the SHA-256 of the corpus file given to freeze. The gate version is 2: a baseline of another version is
refused.

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
| `bootstrap_b` | 10000 | at least 2000 |
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
for a model judge (the two hashes are 64 lower-case hex characters). An arm is
`{stamps {harness_build_sha256, seat_config_sha256, engine_build}, tasks [{id, attempts [{outcome, score, metrics}]}]}`.
Stamps come from the runs that produced the scores, never from the gate binary. A metric value is finite and not
negative. A metric value of `null` (or an absent key) is **unresolved**: it stays unresolved end to end, the pair is
skipped for that metric and counted in `unresolved_pairs`, and it is never written or read as 0.

**Result** (`eval.GateResult`, snake_case, struct order, no timestamp): `verdict`, `reason`, `keep_incumbent`, `mode`,
`margin`, `delta`, `lo`, `hi`, `n_used`, `n_declared`, `excluded_pairs`, `excluded_share`, `attempt_counts` (all seven
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
the frozen range); an efficiency value that is not finite or is negative; scores or costs whose bootstrap sums overflow;
an outcome word outside the seven; incomplete stamps on either arm. A refusal exits 2 from the verb.

## Verbs and exit codes

```
local-offload gate freeze  --prereg P --corpus C [--incumbent ARM] --out BASELINE
local-offload gate check   --baseline BASELINE [--expect-hash SHA256]
local-offload gate verdict --baseline BASELINE --input INPUT [--require-adopt]
local-offload gate schema
```

- `freeze` validates, hashes the corpus, writes the canonical baseline and prints `{"baseline": <path>, "baseline_hash": <sha>}`.
  The output file is created exclusively: freezing the same inputs again to the same path is a no-op, and a changed
  pre-registration goes to a new path (and has a new hash).
- `check` parses the baseline, re-marshals it canonically and refuses unless the bytes are identical ("not in canonical
  form: edited by hand?"), then prints the hash. **A one-byte edit of a frozen baseline either fails `check` or changes
  its hash, and `gate check --expect-hash` and the input's `baseline_hash` refuse it.** What `check` alone cannot do is
  refuse an edit that leaves a valid, canonical document (a letter inside a name, the value of `seed`): that is
  another pre-registration, `check` prints its hash, and only the hash tells it from the frozen one. So a verdict
  adapter takes `baseline_hash` from `freeze`'s output (or a hash it recorded then), never from a later `gate check` of
  the file, and passes `--expect-hash` when it wants `check` itself to refuse.
- `verdict` runs the gate and prints the result as indented JSON.
- `schema` prints the embedded input schema.
- A global `--config <path>` (or `--config=<path>`) before the subcommand, as in `local-offload --config x.json gate
  schema`, is accepted and ignored: the gate reads no config.
- `-h` and `--help` print the usage and exit 0, as the other verbs do whose flags are parsed with `flag.ExitOnError`.

| exit | meaning |
|---|---|
| 0 | the command ran; for `verdict`, a verdict was produced (ADOPT, BLOCK or INCONCLUSIVE); also `-h` |
| 1 | `verdict --require-adopt` and the verdict is not ADOPT (the result is still printed) |
| 2 | a refusal (bad hash, bad receipt, malformed or unreadable input, a baseline that is not the frozen bytes, a freeze the gate cannot honour), a result that could not be written, or a usage error |

Every failure of `verdict` is 2, so 1 can only mean a verdict. The verb reads only the files it is named and writes only
`freeze --out`.

## The power note

`power` is the half-width `hw = (hi - lo) / 2` of the main interval read three ways, with no extra statistic:
`projected` is `hw * sqrt(n / n')` for `n' = 16, 24, 120` usable tasks (the sizes of the digest contract set, a blind
pack and the confidence-calibration probe); `tasks_needed` is `ceil(n * (hw / m)^2)` where `m` is the margin
(non-inferiority) or the observed `|delta|` (superiority); `tasks_to_add` is `max(0, tasks_needed - n)`. `tasks_needed` and
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

- the `agent-trajectory-gate` verb (`runAgentTrajectoryGate` in `main.go`);
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
- [`internal/eval/paireddelta.go`](../../internal/eval/paireddelta.go) — `BootstrapDeltaMean`, the one statistic, and the replicate and percentile helpers the efficiency read shares
- [`internal/eval/gate_test.go`](../../internal/eval/gate_test.go), [`baseline_test.go`](../../internal/eval/baseline_test.go), [`outcome_test.go`](../../internal/eval/outcome_test.go) — the verdict table, boundary rows, refusals, determinism
- [`internal/eval/gate_attack_test.go`](../../internal/eval/gate_attack_test.go) — the attacks a candidate that wants to be adopted would try: unfinished attempts, few resamples, a swapped judge, a costlier metric
- [`gate_cmd.go`](../../gate_cmd.go), [`gate_cmd_test.go`](../../gate_cmd_test.go), [`gate_cmd_attack_test.go`](../../gate_cmd_attack_test.go) — the `gate` verb and its exit codes
- [`main.go`](../../main.go) — the `gate` case and its help line
- [`compaction_eval.go`](../../compaction_eval.go), [`internal/compeval/ratchet.go`](../../internal/compeval/ratchet.go) — the freeze/check shape the baseline copies
- [`internal/compeval/abtest.go`](../../internal/compeval/abtest.go) — `RunAB`, a legacy caller
- [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md), [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md), [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)
