### Added - `ledger --failures`: a failure-class register that groups the ledger's failed jobs by class and kind and joins each class to a human-maintained root cause and fix

- **The gap.** The ledger already says why every deferred row failed (the closed reason-code set) and `ledger` already prints the
  five most frequent reasons of the last week, but nothing counted failures by class across door, seat and model, and nothing kept
  what is known about a class next to its count. A failure that recurs was a log line read again each time. Of the register's
  columns, the counts and the example are facts the ledger holds; the root cause and the structural fix are a person's
  judgement, and code must not invent them.
- **The change.** `local-offload ledger --failures [--since DAYS] [--include-test-traffic] [--table FILE]` prints one JSON object
  and nothing else on stdout. A failed job row (deferred, a failing reason code, or a failed acceptance check; cache hits, dispatch
  markers, inner rows with a parent and refused attempts never) is classed by its reason code, which already holds the seven
  wire defer classes. A row that predates the stamped code is classed at read time by the ledger's own `DeferCode`, and counted in
  `class_derived_rows`. Each class carries its count and share, its kind, the most frequent short reason, and its rows counted per
  door, per seat, per model and per (door, seat, model). The ledger has no seat or model column, so the seat is `model_tier` as
  recorded; on a delegation row (task `agent_delegate`, `<node>:<seat alias>`) the model is the part after the last colon, which
  is the seat alias, a seat and not a model, and on every other row `model_tier` is a model alias and stays whole, a colon of its
  own included (`family:size`); a missing door reads `(unknown)` and a missing `model_tier` reads `(unrecorded)`. The report
  names its window in `since_days`. `ledger --failures=false` is the savings summary again; it used to exit 2 with a usage dump,
  because the summary's flag set did not know the flag.
- **A refused dispatch is not a failed job.** A node that answers a dispatch with a capacity refusal (a 503 or a 429) is a place in
  line: the delegator waits and places the subtask again, and the subtask's outcome is another row. The refused attempt wrote a
  deferred ledger row of its own, so the register counted a subtask that landed elsewhere as two failures and inflated
  `queue_full` and `job_rows`. The cause was in the writer: the refusal mark was set only on the delegation-log line. The
  ledger row now carries an omitempty `refusal` column from the same flag, and the register sets a marked row aside in
  `refused_attempts` (same window, same test-traffic rule, never a job and never a failure). A column was chosen over a new
  `phase` value because a phase that `CountsAsJob` skips would change what every job counter in the ledger calls a job (the
  savings summary, the share readers, the report, `loupe`), while the delegation log and the rigger already treat a refusal as
  a mark that a reader sets aside. The column is absent on old rows, so a ledger written before it counts its refusals as the
  failures they were then recorded as. `ledger_readers_lint_test.go` only governs callers of `ledger.ReadAll` and says
  nothing about columns; what a new column owes is `omitempty`, a comment on what absence means, and a row in the ledger-row
  table of `docs/FLEET-NODE.md`, all done. The other job counters set a refusal row aside as well; see "A refusal row is not a job" in this release.
- **Kinds.** Capacity, caller and policy defers sat in `failure_rows` beside stack faults, against `reasoncode.go` (congestion
  is a place in line, not a failure) and `agentwire.go` (the defer classes keep a node's capacity, the caller's contract and a
  broken stack apart). Every class now has a kind, closed and written in code (`internal/ledger/failurekinds.go`): `stack_fault`,
  `capacity`, `caller`, `ceiling`, `work`, and `unclassified` for `other` alone, a sixth kind added to the brief's five so that
  a failure nobody could classify is never counted as the stack's. A test fails for a failure class with no kind and for a
  kind written for a code the set does not hold; the kinds table on the page is held to the code by a second test. The report
  gives each class's kind, the failure rows per kind in `kinds`, and the stack-fault share on its own line
  (`stack_fault_rows`, `stack_fault_share`, null with no failure row). The classes that straddle two kinds
  (`no_eligible_node`, `poll_deadline`, `write`) are filed with a comment on their line saying which way and why.
- **No silent drops.** A row that fails to decode, a line cut off mid-file and a last line still being written are counted in
  `unparsed_lines` (a blank line is none). A ledger that is not there answers `ledger_found: false` beside its zeros, exit 0,
  and both conditions also print a note on stderr, so a missing ledger no longer reads as "no failures".
- **The human half.** `docs/systems/failure-register.md` holds the table (class, meaning, root cause, structural fix), one row
  for every failure member of the closed reason-code set. The page is embedded in the binary; `--table` reads another copy.
  Classes are joined to it by name: a class with no row says `undocumented` in every human column, an empty root-cause cell reads
  `not recorded`, an empty fix cell reads `open`, and a table row for a class the ledger does not hold adds nothing. The root
  cause and structural fix cells previously held restatements of the code's own comments for all 31 classes, so a row existed
  for every class, `undocumented_classes` was always empty and the gap detector could not fire. They now ship empty: no cell of
  the table was investigated by a person, so none is kept. A class counts as investigated only when its row holds a root cause
  or a fix a person wrote (a cell reading the defaults "not recorded" or "open" is not), `classes[].investigated` says so, and
  `uninvestigated_classes` lists the classes in the ledger that are not. A test fails for a non-empty cell on the shipped page
  until the class is named in the test with what the finding rests on. A test fails when the closed set gains a member the
  table has no row for, and when a wire defer class is filed under a different code than the ledger files it under.
- **Honest numbers.** Classes come out by count descending, then class name, and the output holds no clock reading: the same ledger
  and table give the same bytes. A class that has no rows in the window is not listed. A token total no row measured is `null`,
  never `0`: a row measured its card work when the ledger stores a figure above 0 for it, when its older token columns hold work,
  or when its stored 0 is the row's own (its reason code says no node ran the job, it names no node, or it is the node's own row).
  The classes that say no node ran the job are ten: `node_busy`, `capacity_wait`, `queue_full`, `no_eligible_node`,
  `queue_withdrawn`, `shed`, `dispatch_refused`, `config`, `contract`, `bad_input`. `write` was in that set and is not any more:
  a node refuses the write door before a run (`agent_allow_write` is false) or after one (the finished write set broke a door
  cap), and the class does not say which, so a delegator's stored 0 on a write row that names a node is a gap and reads `null`.
  A delegator's stored 0 for a job a node owned (`poll_deadline`, `node_unreachable`, a failed verification that came back from a
  node) is likewise not a measurement, because the node's work is not on that row. `rows_with_tokens` says how many rows the sum
  covers, so a total over fewer rows than `count` reads as a floor. Test, bake and measurement traffic is left out and counted,
  as in every production view of the ledger.
- **Surface.** `main.go` changes by two lines: the `ledger` case calls `runLedgerMode`, which sends `--failures` to the register and
  everything else to the verb as it was (taking a `--failures=false` out of what the verb parses), and the `ledger` usage line
  names the flag. The operator guide and the README list the mode. The reader lives in `internal/ledger/failures.go`. It scans the
  file itself (a 16 MiB line buffer, where `ReadAll` takes 1 MiB) because it has to see whether a row carried the `cards_tokens`
  key, which `Entry` cannot hold. `ledger_readers_lint_test.go` only inspects `ledger.ReadAll` calls from outside the package,
  so it does not see this reader: the test-traffic rule here is held by this feature's own tests. The one new ledger column,
  `refusal`, is written by `internal/delegate`.
- **Tests, red first.** The acceptance tests and a synthetic fixture ledger and table were committed before the reader existed
  and failed to build. They pin: deterministic bytes on the fixture (a committed golden file, flag order irrelevant), the class
  order including count ties, an unresolved total serialized as `null` and never `0` (the fixture lines are hand-written, because
  `Record` always writes the key), a zero the row owns kept as `0` and a delegator's zero for a job a node owned read as `null`,
  classes with no rows absent (test, bake, marker, inner, cache-hit and passing rows make none), the join by class name with
  `undocumented` for a class without a row and `not recorded` / `open` for empty cells, the table parser (CRLF, an escaped pipe, a
  second table ignored, malformed tables refused), the embedded page against the closed set, the verb's `--since` window (rows
  stamped relative to the clock), the default path with no `--table`, and that `ledger` without the flag is unchanged. The
  delegator-zero tests were written first and failed on an assertion, against a golden file written by hand from the rule. For the
  review round, each fix has its failing test committed before the fix: the writer stamping the refusal column (fails on an
  assertion: the ledger row of a refused attempt carries no mark), the register setting a refusal aside, kinds, the unreadable
  lines and the missing ledger (these three fail to build), `--failures=false` run end to end in a child process because the
  refusal is an `os.Exit` (exit 2 with the usage dump), the cut of `model_tier` on non-delegation rows, `since_days`, `write` as
  a class where a node's stored 0 is a gap, and the investigated rule with the shipped page's empty cells (a build failure, then
  an assertion on the page).
- **Tests, green by design.** A table test that sends a stored-0 row naming a node and a fleet job through each never-ran class was
  added after the reader and passed when written; it was added because dropping a class from the set left the suite green, and
  the mutation runs below are what show it pins them. The kinds table of the page against the code was written after the page and
  passed on its first run; the mutation below is what shows it pins it. Two assertions were added after a mutant survived: the
  stack-fault count against a capacity count that had tied it, and a leading colon kept on a non-delegation row.
- **Mutations.** Eighteen one-line mutants (the token pointer rule, the measured-zero rule, both sort keys, the run-class filter,
  the job rule, the acceptance rule, the example-reason tie-break, the orphan inner row, the window, the group order, the cache-hit
  rule, the three join defaults, the flag parser, the duplicate-row check and the dispatch itself), each restored byte for byte
  with `git diff --quiet` exit 0 afterwards. One mutant survived: a marker guard that the job rule makes unreachable, which was
  removed rather than tested. The cache-hit rule got its own test row and its mutant is red. A later round ran twenty-one more, on the
  rules added after review: the window's sign and unit, the default table and the last-colon cut (four); each of the eleven
  classes the never-ran set then held, dropped from the set one at a time (eleven); and each term of the zero-ownership rule, which
  is the never-ran lookup, the served-door term, the requester term and the three conjuncts of the no-node term (six). Seven of the
  eleven set members survived at first, because no test pinned them and the suite stayed green when one was dropped, and the table
  test above was written for them; after it every one of the twenty-one mutants was red.
  The review round ran thirty-seven more, each restored byte for byte with `git diff --quiet` exit 0 afterwards, and two survived
  at first. The refusal column: the stamp deleted from the writer, and the register's refusal branch, its counter, the
  run-class guard on the excluded count and its `continue` each removed (five). Kinds: one class refiled, the `other` entry
  removed, the stack-fault count read from the capacity count (survived, because the test's two counts tied; the test now has
  them differ), the kind tie-break reversed, the empty-register guard widened and the kind constant fixed (six). Unreadable
  lines: the counter, the found flag, the whitespace trim and the stderr note (four). `--failures=false`: the stripped
  arguments, the last-word rule and the strip itself (three); `since_days` (one); the model cut made unconditional and the
  leading-colon trim made unconditional (the second survived until a non-delegation row with a leading colon was added) (two).
  The never-ran set: `write` put back (one), and each of the ten classes it holds dropped one at a time (ten). The investigated
  rule: the default-text comparison removed, the uninvestigated list keyed on the row instead, and the fix cell left out of the
  test (three), a class moved in the page's kinds table and a restated cell put back on the shipped page (two). A mutant that did
  not build (an unused import, an unused parameter) was discarded and rerun in a form that does, and is not counted. No mutation
  log is committed; the tests are what keep the rules pinned.
