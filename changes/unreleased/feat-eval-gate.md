### Added - one measurement gate (local-offload gate)

- **What it does.** `local-offload gate <freeze|check|verdict|schema>` and `eval.Gate` put every adoption (a seat bake, a
  compaction change, a cascade cut, a skill proposal) behind one statistic and one set of rules. `freeze` writes a
  pre-registration (mode, primary metric and margin, efficiency metrics, `min_pairs`, bootstrap size and seed, held-out
  ids, declared tasks, an optional frozen incumbent) as canonical bytes, hashes the corpus into it and prints the baseline
  hash; the output is created exclusively, so a changed pre-registration is a new file with a new hash. `check` refuses a
  baseline that is not byte for byte what `freeze` writes: a one-byte edit of a frozen baseline either fails `check` or
  changes its hash, and `check --expect-hash` and the input's `baseline_hash` refuse it. `verdict` compares a
  candidate arm with the incumbent arm under that baseline and prints ADOPT, BLOCK or INCONCLUSIVE with `delta`, `lo`,
  `hi`, `n_used`, the excluded pairs and share, per-arm outcome counts, the cheat counts, one paired interval per
  efficiency metric (`delta`, `lo` and `hi` are null, never 0, below two resolved pairs), the held-out read, a power note, both runs' stamps, the judge, an era marker, the baseline and corpus
  hashes and the verb's own build hash, with no timestamp (the same input twice is byte-identical). `schema` prints the
  gate-input JSON Schema, whose `outcome` enum is the Go list. The statistic is `eval.BootstrapDeltaMean`, called
  unchanged over the usable non-held-out task pairs in sorted id order. Superiority adopts iff `lo > 0` and blocks iff
  `hi < 0`; non-inferiority blocks iff `hi < -m` and adopts iff `lo > -m` with a declared efficiency metric (tokens,
  money or memory in GiB) improving; fewer than `min_pairs` usable pairs, or an excluded share above 0.20, is
  INCONCLUSIVE; the incumbent keeps every non-ADOPT; a held-out slice can veto an ADOPT; a model judge needs a passed
  receipt for the same scorer. Only `pass`, `task_fail` and `cheat` attempts score (a cheat as 0); `infra_error`,
  `timeout`, `judge_error` and `not_run` are excluded and counted; an unresolved cost is JSON `null` end to end and never
  0. Freeze refuses any efficiency class but `tokens`, `money` and `memory_gib`, and any metric or primary name that
  carries a wall-time word, citing INV-5. Exit codes: 0 a verdict was produced, 1 a non-ADOPT under `--require-adopt`,
  2 a refusal or a usage error; a global `--config <path>` before `gate` is accepted and ignored. The attempt enum is `eval.AttemptOutcome` (the name `eval.Outcome` is already the cascade
  eval's per-case struct), with `AllOutcomes` and `OutcomeFromCaliper` (`not_checked` is `not_run`). `main.go` gains one
  case pair and one help line; the rest is `gate_cmd.go` and `internal/eval`.
- **Why.** The program's earlier proposals used three different pass rules, and this repository holds the pieces of two
  (`BootstrapDeltaMean` with a `B` and seed taken from each call site and its interval read by each caller, and ADR 0080's
  0.5 effect-size bar with no interval), so the same evidence could adopt under one and block under another, and a rule
  chosen after the numbers are seen is not a rule. The statistic already existed;
  the rules around it (usable denominator, margins, small n, binding the rules to the result) did not. `agent-trajectory-gate`
  and the compaction A/B (`compeval.RunAB`) keep `B = 2000`, `seed = 42` and are listed as legacy, not migrated;
  `BootstrapDeltaMean` is unchanged. Docs: `docs/systems/measurement-gate.md` (new, linked from `docs/README.md`) and ADR
  0088 (Proposed).
- **Tests.** Red first: the gate's types and a stub that refuses everything were committed with the rule tests failing,
  then the gate. `internal/eval` tests: a verdict table (superiority and non-inferiority ADOPT, BLOCK and INCONCLUSIVE; n
  below `min_pairs`; excluded share above the threshold and exactly at it; a missing, an equal, a worse and an
  interval-spanning-zero efficiency metric; a tie), boundary rows built from exactly representable numbers (16 pairs,
  scores 8.0, 7.5, 7.0: every difference exactly `-m` is INCONCLUSIVE, every difference -1.0 is BLOCK), the refusal rows
  (hash mismatch, model judge without or with a failing or foreign receipt, missing or unequal incumbent, era mismatch,
  undeclared or repeated task, scored attempt without a score, bad outcome word, missing stamps, out-of-range score),
  held-out veto, cheat scored 0, infra errors excluded and counted, unresolved cost never 0, determinism, the era marker,
  the power note, the exact bootstrap numbers against a direct `BootstrapDeltaMean` call, freeze then check
  byte-identical, a one-byte edit of a frozen baseline either failing `check` or changing its hash with the input's
  `baseline_hash` refusing it (every byte of the frozen file is tried), and the schema's enum and properties pinned to the
  Go types. Root tests drive the verb in-process (freeze, check, verdict round trip, exit codes 0, 1 and 2, write-once
  freeze, the INV-5 refusals, `schema`). Mutations, each proved applied (`git ls-files --eol` index column `lf`,
  `git diff --quiet` exit 1 after the edit, exit 0 after the restore): `lo > 0` to `lo >= 0` turned red the tie rows and
  the held-out test; `lo > -m` to `lo >= -m` and `hi < -m` to `hi <= -m` turned red the exact-boundary row; dropping the
  efficiency-improves requirement turned red the equal, worse and missing-metric rows. Ten further mutations (share and
  `min_pairs` comparisons, `hi < 0`, the held-out comparison and veto, the cheat score, the per-metric `min_pairs`,
  `keep_incumbent`, and the direction of an efficiency read) were run: nine were caught at once, and the survivor
  (reading `lo` instead of `hi` for a lower-is-better metric) led to the interval-spanning-zero row, after which it and
  its mirror were caught.
  A review round added tests and re-ran the mutation check for each (applied: `git diff --quiet` exit 1; restored: exit
  0): the efficiency bootstrap reading another seed or another `B` than the primary (the per-task differences now vary, so
  both are caught), "every declared metric must improve" in place of "at least one" (caught), the per-task cost mean (a
  nil value in the divisor, an unusable attempt's cost in the mean: both caught), the non-inferiority power note below two
  usable pairs (caught), the null interval below two resolved pairs (always computing it, or needing three pairs: both
  caught, the tests committed red first), the leading `--config` (caught, red first), and the input's `baseline_hash`
  binding and `--expect-hash` (each caught by the one-byte-edit tests).
