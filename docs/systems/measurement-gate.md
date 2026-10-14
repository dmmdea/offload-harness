# Measurement gate

## Purpose

One gate decides every adoption in the program: a seat bake, a compaction change, a cascade cut, a skill proposal. A
candidate is compared with the incumbent on the same declared tasks, with the statistic the repository already had
(`eval.BootstrapDeltaMean`, a paired bootstrap of the mean difference with a 95% interval), under rules that were
written down and hashed before the first candidate ran. The gate returns **ADOPT**, **BLOCK** or **INCONCLUSIVE**, and
the incumbent keeps its place unless the verdict is ADOPT.

It replaces three pass rules that were in use side by side: a paired-standard-error rule, `BootstrapDeltaMean` with an
ad hoc reading of its interval, and "not worse" with no statistic. [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md)
records the decision.

The gate is a pure function plus a verb. It reads scores that someone else produced, so it measures nothing itself and
needs no model, no GPU and no network.

## The rules

| rule | in plain words |
|---|---|
| **R1 superiority** | A claim of "better": ADOPT iff the interval's lower end `lo` is above 0; BLOCK iff the upper end `hi` is below 0; otherwise INCONCLUSIVE. |
| **R2 non-inferiority** | A cut or an efficiency change ("not worse by more than the margin `m`, and cheaper"): BLOCK iff `hi < -m`; ADOPT iff `lo > -m` **and** at least one declared efficiency metric improves; otherwise INCONCLUSIVE. A metric improves when its own paired interval lies wholly on the good side of 0. The metric list is frozen first, so choosing the best of several afterwards is not possible. |
| **R3 usable denominator** | Only `pass`, `task_fail` and `cheat` attempts score. A cheat scores 0 whatever score it carries and is counted apart. `infra_error`, `timeout`, `judge_error` and `not_run` are excluded and reported with counts. The unit is the task: a task's score is the mean of its usable attempts, a pair is usable only if both arms have the task usable, and the bootstrap resamples tasks. If the excluded share of declared tasks is above `max_excluded_share` (0.20), the verdict is INCONCLUSIVE. An unresolved cost never becomes 0. |
| **R4 the incumbent wins** | The incumbent keeps every tie and every INCONCLUSIVE (`keep_incumbent` is true unless ADOPT). Fewer than `min_pairs` usable pairs is INCONCLUSIVE. The remedy for an INCONCLUSIVE at the corpus size is **more tasks, not more attempts**, because attempts of one task are not independent; the power note says how many. |
| **R5 held-out** | A held-out slice is declared at freeze and scored once. It cannot rescue a verdict. It can veto one: an ADOPT whose held-out point estimate fails becomes BLOCK, "reject without re-tuning". |
| **R6 pre-registration** | The mode, the primary metric and margin, the efficiency metrics, `min_pairs`, the bootstrap size and seed, the held-out ids and the declared tasks are written into a baseline file and hashed before the first candidate run. A verdict names that hash and the gate refuses an input that names another one. |
| **R7 judge integrity** | A model judge is allowed only with a passed self-test receipt for the same scorer. Until a scorer has one, only deterministic checks (the acceptance DSL, exact match) judge. The gate only reads the receipt; the verb that produces it (`gate selftest`) is a later change. |
| **R8 eras** | Every result carries the stamps of the two runs, the judge identity and an **era marker**. Results with different era markers are never compared; a comparison re-runs the incumbent in the new era. |

No rule, verdict, ranking or acceptance line carries a wall-time term (INV-5, [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)).
Efficiency is measured in tokens, money or resident memory in GiB, and the baseline refuses any other class.

## The statistic

For the usable, non-held-out pairs in sorted task-id order, with `inc[i]` and `cand[i]` the task scores, the gate calls

```
delta, lo, hi = eval.BootstrapDeltaMean(inc, cand, bootstrap_b, seed)
```

unchanged: `delta` is the mean of the bootstrap replicates of `cand - inc`, `lo` and `hi` are their 2.5% and 97.5%
points. With fewer than two pairs the function returns zeros and the verdict is INCONCLUSIVE through `min_pairs`. Each
declared efficiency metric gets its own call over the pairs that resolved that metric on both arms, with the same
`bootstrap_b` and `seed`, and improves when `hi < 0` (lower is better) or `lo > 0` (higher is better). A metric needs at
least `min_pairs` resolved pairs to improve. A metric that no task carries on both arms resolves nowhere and cannot
improve, so **a non-inferiority cut cannot ADOPT on a missing metric**.

Boundaries are strict: `lo > 0`, `hi < 0`, `lo > -m`, `hi < -m`. An interval that touches the line is INCONCLUSIVE. The
tests build their boundary rows from exactly representable numbers (16 pairs, scores 8.0, 7.5, 7.0) so a mutation of a
comparison flips a row on every platform.

## Outcomes

Each attempt carries one of seven classes (`eval.AttemptOutcome`, in `internal/eval/outcome.go`; the type is not named
`Outcome` because that name is the cascade eval's per-case struct):

| class | scored | meaning |
|---|---|---|
| `pass` | yes | finished; the score is required |
| `task_fail` | yes | finished; the score is required |
| `cheat` | yes, as 0 | tried to game the scorer; counted apart |
| `infra_error` | no | the run broke |
| `timeout` | no | the attempt hit its budget |
| `judge_error` | no | the judge could not score it |
| `not_run` | no | never ran. A declared task missing from an arm, or present with no attempt, counts as one `not_run` |

`eval.OutcomeFromCaliper` maps a Caliper record's word to this list: the six shared words map to themselves and
`not_checked` is `not_run`; anything else is unknown and the caller must refuse it. The embedded JSON Schema's `outcome`
enum is the same list and a test compares them.

## Formats

**Pre-registration** (what the operator writes; `gate freeze --prereg`). Left-out fields take defaults; unknown fields
are refused.

```json
{
  "mode": "non_inferiority",
  "primary": {"name": "answer_quality", "class": "judge_0_10"},
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
plus one newline, LF only, and the baseline hash is the SHA-256 of those bytes. Fields: `gate_version` ("1"), `mode`,
`primary {name, class, margin, higher_is_better}`, `efficiency [{name, class, lower_is_better}]`, `min_pairs`,
`bootstrap_b`, `seed`, `max_excluded_share`, `held_out`, `corpus_hash`, `tasks` and an optional frozen `incumbent`
arm. `tasks`, `held_out` and the incumbent's tasks are sorted. `corpus_hash` is the SHA-256 of the corpus file given to
freeze.

| field | default | notes |
|---|---|---|
| `mode` | none | `superiority` or `non_inferiority` |
| `primary.class` and margin | | `unit_interval`: margin 0.05. `judge_0_10`: margin 0.5. `custom`: non-inferiority must carry its own margin or freeze refuses. In superiority mode the margin is ignored and recorded as 0 |
| `primary.higher_is_better` | true | `false` is refused: only higher-is-better primary scores are supported |
| `efficiency[].class` | none | `tokens`, `money` or `memory_gib`. Any other class, in particular one naming seconds, latency, wall, duration or time, is refused with a message that cites INV-5; so is a metric or primary **name** that carries such a word. Non-inferiority with no efficiency metric is refused: a cut must name what it saves |
| `efficiency[].lower_is_better` | true | |
| `min_pairs` | 16 | at least 2 |
| `bootstrap_b` | 10000 | at least 1 |
| `seed` | 1 | |
| `max_excluded_share` | 0.20 | within 0 to 1 |

Freeze also refuses an empty corpus, a task list that is empty, unsorted, repeated or holds an empty id, a held-out id that
is not a declared task, a pre-registration that holds out every task, and a frozen incumbent that does not fit the
declared tasks.

Corpus conventions (docs only, nothing is committed): a corpus is harvested redacted at harvest time, hash-stamped,
machine-local and never committed; `freeze` and `check` reproduce from the hash, as `compaction-eval` does.

**Gate input** (what `gate verdict --input` reads; `gate schema` prints its JSON Schema, draft 2020-12). `baseline_hash`;
`incumbent` (an arm; required unless the baseline froze one; when both exist they must be equal); `candidate` (an arm);
`judge {kind, identity}`; `receipt {scorer_id, pairs_hash, passed, build_sha256}` for a model judge. An arm is
`{stamps {harness_build_sha256, seat_config_sha256, engine_build}, tasks [{id, attempts [{outcome, score, metrics}]}]}`.
Stamps come from the runs that produced the scores, never from the gate binary. A metric value of `null` (or an absent
key) is **unresolved**: it stays unresolved end to end, the pair is skipped for that metric and counted in
`unresolved_pairs`, and it is never written or read as 0.

**Result** (`eval.GateResult`, snake_case, struct order, no timestamp): `verdict`, `reason`, `keep_incumbent`, `mode`,
`margin`, `delta`, `lo`, `hi`, `n_used`, `n_declared`, `excluded_pairs`, `excluded_share`, `attempt_counts` (all seven
classes per arm, zeros included), `cheats`, `efficiency` (per metric: `delta`, `lo`, `hi`, `n`, `unresolved_pairs`,
`improves`), `held_out` (`{n, delta, failed}` or `null` when none is declared), `power`, `stamps`, `judge`, `era_marker`,
`gate_version`, `corpus_hash`, `baseline_hash`, `bootstrap_b`, `seed`, and the verb's `gate_build_sha256`. `n_declared` is
the number of declared tasks that are not held out. The same input twice gives byte-identical output.

## Refusals

The gate returns an error and no verdict for: an input `baseline_hash` that is not the hash of the baseline handed in (or
a baseline whose bytes no longer hash to it); a judge kind other than `deterministic` or `model`; an empty judge
identity; a `model` judge with no receipt, a receipt with `passed` false, or a receipt for another scorer; an incumbent
absent from both the input and the baseline; an incumbent in both that is not equal (tasks compared in id order); an input
incumbent whose stamps differ from the frozen incumbent's (another era); a task id in either arm that the baseline never
declared, or one that appears twice in an arm; a `pass` or `task_fail` attempt with no score, or with a score outside the
primary class range (`unit_interval` 0 to 1, `judge_0_10` 0 to 10); an outcome word outside the seven; incomplete stamps on
either arm. A refusal exits 2 from the verb.

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
  form: edited by hand?"), then prints the hash. An edit that leaves a valid, canonical document is another
  pre-registration with another hash; `--expect-hash` (the hash `freeze` printed) refuses it, and so does any verdict
  input that names the old hash.
- `verdict` runs the gate and prints the result as indented JSON.
- `schema` prints the embedded input schema.

| exit | meaning |
|---|---|
| 0 | the command ran; for `verdict`, a verdict was produced (ADOPT, BLOCK or INCONCLUSIVE) |
| 1 | `verdict --require-adopt` and the verdict is not ADOPT (the result is still printed) |
| 2 | a refusal (bad hash, bad receipt, malformed or unreadable input, a baseline that is not the frozen bytes, a freeze the gate cannot honour) or a usage error |

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

New callers use the gate. `BootstrapDeltaMean` itself is unchanged.

## ADR 0080 and the resource gates

[ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md) sets three gates for a
RAM-spill seat: a blind pack that beats the incumbent by more than a 0.5 bar, host RSS within the weights plus 1 GiB and
released on unload, and no abort and no swap-in. They stay **resource gates**. What changes is how the first is read: the
0.5 bar is the effect-size minimum and the interval is the significance, and **both** must hold. In gate terms, a seat
claim on a 0 to 10 judge scale is a superiority or non-inferiority run on class `judge_0_10` whose ADOPT also clears the
resource gates; the gate decides the first, the lease and ledger measurements decide the others.

## Source map

- [`internal/eval/gate.go`](../../internal/eval/gate.go) — `Gate`, `GateInput`, `GateResult`, the scoring, the verdict rules, the power note, the era marker
- [`internal/eval/baseline.go`](../../internal/eval/baseline.go) — `PreReg`, `Baseline`, `FreezeBaseline`, `CheckBaseline`, `BaselineHash`, the freeze-time validation
- [`internal/eval/outcome.go`](../../internal/eval/outcome.go) — `AttemptOutcome`, `AllOutcomes`, `OutcomeFromCaliper`, `Refusal`
- [`internal/eval/gate_input.go`](../../internal/eval/gate_input.go) — `Attempt`, `Task`, `Arm`, `Stamps`, arm validation, the INV-5 name check, the embedded schema
- [`internal/eval/gate-input.schema.json`](../../internal/eval/gate-input.schema.json) — the gate input JSON Schema
- [`internal/eval/paireddelta.go`](../../internal/eval/paireddelta.go) — `BootstrapDeltaMean`, the one statistic
- [`internal/eval/gate_test.go`](../../internal/eval/gate_test.go), [`baseline_test.go`](../../internal/eval/baseline_test.go), [`outcome_test.go`](../../internal/eval/outcome_test.go) — the verdict table, boundary rows, refusals, determinism
- [`gate_cmd.go`](../../gate_cmd.go), [`gate_cmd_test.go`](../../gate_cmd_test.go) — the `gate` verb and its exit codes
- [`main.go`](../../main.go) — the `gate` case and its help line
- [`compaction_eval.go`](../../compaction_eval.go), [`internal/compeval/ratchet.go`](../../internal/compeval/ratchet.go) — the freeze/check shape the baseline copies
- [`internal/compeval/abtest.go`](../../internal/compeval/abtest.go) — `RunAB`, a legacy caller
- [ADR 0088](../architecture/decisions/0088-one-measurement-gate.md), [ADR 0080](../architecture/decisions/0080-a-ram-spill-moe-is-the-agent-seat-when-it-earns-it.md), [ADR 0050](../architecture/decisions/0050-placement-ranks-adequate-seats-by-expected-completion.md)
