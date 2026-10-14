### Added - `ledger --failures`: a failure-class register that groups the ledger's failed jobs and joins each class to a human-maintained root cause and fix

- **The gap.** The ledger already says why every deferred row failed (the closed reason-code set) and `ledger` already prints the
  five most frequent reasons of the last week, but nothing counted failures by class across door, seat and model, and nothing kept
  what is known about a class next to its count. A failure that recurs was a log line read again each time. Of the register's three
  columns, two (count, example) are facts the ledger holds; the third pair (root cause, structural fix) is a person's judgement, and
  code must not invent it.
- **The change.** `local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE]` prints one JSON object
  and nothing else on stdout. A failed job row (deferred, a failing reason code, or a failed acceptance check; cache hits, dispatch
  markers and inner rows with a parent never) is classed by its reason code, which already holds the seven wire defer classes. A row
  that predates the stamped code is classed at read time by the ledger's own `DeferCode`, and counted in `class_derived_rows`. Each
  class carries its count and share, the most frequent short reason, and its rows counted per door, per seat, per model and per
  (door, seat, model). The ledger has no seat or model column, so the seat is `model_tier` as recorded and the model its part after
  the last colon (on a delegation row that part is the seat alias, a seat and not a model); a missing door reads `(unknown)` and a missing `model_tier` reads `(unrecorded)`.
- **The human half.** `docs/systems/failure-register.md` holds the table (class, meaning, root cause, structural fix). It starts
  from the code: one row for every failure member of the closed reason-code set, the meaning and root cause restating the
  constants' own comments, and the fix column `open` wherever none is recorded. The page is embedded in the binary; `--table`
  reads another copy. Classes are joined to it by name: a class with no row says `undocumented` in every human column, an empty
  root-cause cell reads `not recorded`, an empty fix cell reads `open`, and a table row for a class the ledger does not hold adds
  nothing. A test fails when the closed set gains a member the table has no row for, and when a wire defer class is filed under a
  different code than the ledger files it under.
- **Honest numbers.** Classes come out by count descending, then class name, and the output holds no clock reading: the same ledger
  and table give the same bytes. A class that has no rows in the window is not listed. A token total no row measured is `null`,
  never `0`: a row measured its card work when the ledger stores a figure above 0 for it, when its older token columns hold work,
  or when its stored 0 is the row's own (its reason code says no node ran the job, it names no node, or it is the node's own row).
  A delegator's stored 0 for a job a node owned (`poll_deadline`, `node_unreachable`, a failed verification that came back from a
  node) is not a measurement, because the node's work is not on that row: it counts as unmeasured and reads `null`.
  `rows_with_tokens` says how many rows the sum covers, so a total over fewer rows than `count` reads as a floor. Test, bake and measurement traffic is left out and counted, as in every
  production view of the ledger.
- **Surface.** `main.go` changes by two lines: the `ledger` case calls `runLedgerMode`, which sends `--failures` to the register and
  everything else to the verb as it was, and the `ledger` usage line names the flag. The operator guide and the README list the mode. The reader lives in `internal/ledger/failures.go`. It scans the file itself (a 16 MiB line
  buffer, where `ReadAll` takes 1 MiB) because it has to see whether a row carried the `cards_tokens` key, which `Entry` cannot
  hold. `ledger_readers_lint_test.go` only inspects `ledger.ReadAll` calls from outside the package, so it does not see this reader:
  the test-traffic rule here is held by this feature's own tests.
- **Tests, red first.** The acceptance tests and a synthetic fixture ledger and table were committed before the reader existed
  and failed to build. They pin: deterministic bytes on the fixture (a committed golden file, flag order irrelevant), the class
  order including count ties, an unresolved total serialized as `null` and never `0` (the fixture lines are hand-written, because
  `Record` always writes the key), a zero the row owns kept as `0` and a delegator's zero for a job a node owned read as `null`, classes with no rows absent (test, bake, marker, inner, cache-hit
  and passing rows make none), the join by class name with `undocumented` for a class without a row and `not recorded` / `open`
  for empty cells, the table parser (CRLF, an escaped pipe, a second table ignored, malformed tables refused), the embedded page
  against the closed set, the verb's `--since` window (rows stamped relative to the clock), the default path with no `--table`
  (joined to the page the binary embeds), the cut of `model_tier` at its last colon, and that `ledger` without the flag is unchanged.
  The delegator-zero tests were written first and failed on an assertion, against a golden file written by hand from the rule.
- **Tests, green by design.** A table test that sends a stored-0 row naming a node and a fleet job through each of the eleven never-ran classes was added after the reader and passed when written; it was added because dropping a class from the set left the suite green, and the mutation runs below are what show it pins them.
- **Mutations.** Eighteen one-line mutants (the token pointer rule, the measured-zero rule, both sort keys, the run-class filter,
  the job rule, the acceptance rule, the example-reason tie-break, the orphan inner row, the window, the group order, the cache-hit
  rule, the three join defaults, the flag parser, the duplicate-row check and the dispatch itself), each restored byte for byte
  with `git diff --quiet` exit 0 afterwards. One mutant survived: a marker guard that the job rule makes unreachable, which was
  removed rather than tested. The cache-hit rule got its own test row and its mutant is red. A later round ran twenty-one more, on the
  rules added after review: the window's sign and unit, the default table and the last-colon cut (four); each of the eleven
  classes the never-ran set holds, dropped from the set one at a time (eleven); and each term of the zero-ownership rule, which
  is the never-ran lookup, the served-door term, the requester term and the three conjuncts of the no-node term (six). Seven of the eleven set
  members survived at first, because no test pinned them and the suite stayed green when one was dropped; a table test now sends
  a stored-0 row that names a node and a fleet job through all eleven, with a run class as a control, and holds the set to
  its written list in both directions. After that test every one of the twenty-one mutants is red, and each was restored byte for
  byte with `git diff --quiet` exit 0. No mutation log is committed; the table test is what keeps the set pinned.
