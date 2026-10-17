### Added - a lint keeps a test from failing because a loaded runner stretched a measured time

The starvation fixes of this series (the call-reserve start order, the queued-credit and cut-answer tests, the call-deadline windows,
the lease-renewal helper, the busy-hold ceiling, the warm-watch poll) each found one test that put a number on the clock, after a green
suite went red once, and fixed that one test. The next test to do it was not covered. This change adds the lint that covers the part of
the fault a syntax tree can see. It adds two test files in the root package and one section of `CONTRIBUTING.md`; no product code and no
existing test changes.

- **What it checks.** `clock_lint_test.go` judges every `*_test.go` file of the module (the same walk as `tempdir_lint_test.go`) and fails
  on an upper bound on a measured elapsed time under one second, compared with `>` or `>=` (or the same written the other way round)
  against a constant it can evaluate. The measured time is `time.Since(x)`, a difference with a `time.Now()` stamp, a name that only ever
  holds one of those, that value read as a number (`.Seconds()`, `.Milliseconds()`), rounded, converted (`int`, `float64`,
  `time.Duration`) or multiplied or divided by a constant, and the result of a function or method of the same package that returns one
  (`measure(f) > C`, `d := measure(f)`, `results, sum, elapsed := runWithin(...)`, named results, bare returns, helpers that return
  other helpers' results). The constant is a literal, a unit product such as `250*time.Millisecond`, a const of the same package (read
  from its test files and from its other files, because a test refers to `leasePollInterval` as often as to a neighbour's const), or a
  const or once-assigned local of the same function. The key is `file:elapsed>C`. `clock_lint_allowlist_test.go` lists the 28 keys
  (33 sites) the tree holds, each with its count and a reason that names what the bound rules out and how wide the margin is; a thin
  margin is written down as thin. The list is kept true in both directions, as the temp-dir list is: a new bound, a larger count, a
  fixed site whose count or entry stays, and an entry without a reason each fail. A test file that imports `time` with a dot cannot be
  read and fails by name.
- **Why one second.** A bound of a second or more is not reviewed by the lint. That is where a bound starts to be a hang guard, and
  where the list stops being something one person can review: the tree holds 28 keys under 1 s, 61 under 2 s, 84 under 3 s and 113 under
  5 s. The floor is a ratchet, not a proof of safety: a 2.5 s ceiling failed under starvation too, so a bound just above it is still
  yours to judge. Raising `clockBoundFloor` and listing what it then finds is the next step.
- **What it cannot see** (the header of the file has the full list, and `CONTRIBUTING.md` repeats it). It reads Go only: the
  JavaScript tests under `render/` and the shell scripts have the same fault and no lint here. Bounds against a value it cannot evaluate
  (a field, a parameter, a poll interval, a constant of another package). Measured times it cannot follow: a sum with another duration, a
  helper kept in a closure variable or in another package, a value stored in a field or sent on a channel. `elapsed < C`, which is
  overwhelmingly a lower bound and is not read, in its two shapes that are not: a fixture schedule (a fake server that answers one way
  until `C` has passed and another way after, so a starved runner moves the flip relative to the code under test) and a poll deadline
  written as a loop condition (`for time.Since(start) < C { ... }` followed by a failure). The lower edge of a sampling window
  (`el > lo && el < hi`) is not told from a ceiling; such a site is listed with that reason. And the other shapes of the same fault: a
  fixed sleep before an assertion, a short call or context window handed to the code under test, a poll deadline taken from
  `time.Now().Add(C)`, a helper that takes a short timeout. `case <-time.After(C): t.Fatal(...)` ceilings were counted when it was
  written: none is under a second.
- **Red first, in three rounds.** (1) With an empty allowlist the lint failed on the real tree with 22 findings, the first being
  `gpu_warm_watch_test.go fails when a measured time is above 500ms (3 time(s)) and is not in clockBoundAllowlist`; with the list written
  it passed. A scratch test file holding `time.Since(start) > 250*time.Millisecond` turned it red and named that file and the form, and was
  removed. (2) Reading package constants from the package's other files made six more keys (eight sites: five files in
  `internal/modelaffinity`, one in `internal/pairworkloads`, each a bound of `leasePollInterval/2` or `nodeInfoTimeout/2`) fail the
  real-tree test before they were listed. (3) Sixteen forms of a measured time that the lint did not read (arithmetic, conversions,
  `Round`, `Truncate`, helper results of every shape) were pinned as cases that failed against the scanner as it stood, with eight
  controls that must stay clean, and pass now; no sub-second site was new.
- **Mutations.** Single-line mutations of the scanner, each run with `scripts/mutation-check.sh` against the scanner's own tests, were all
  `RED`: six on the first round (dropping `>=` or `<=` from the bound operators, treating a bound of zero as a bound, reading any `Sub` as
  an elapsed time, no longer failing on a count below the listed one, a local no longer shadowing a package const) and ten on the second
  (reading test files only for constants, trusting a const declared with two values, a quotient read as a product, `Truncate` unread, a
  multi-value definition unread, a bare return unread, a zero return read as not elapsed, a helper name that two declarations answer
  differently trusted, a call through an imported package read as a local helper, a dot import of `time` no longer named).
- **No new finding in the open changes.** The same scanner run on all the Go files of the four open pull requests found no site outside
  the list.
- **`CONTRIBUTING.md`** gets a section, "Keeping tests from failing on a slow machine", beside the temp-dir one: prefer waiting on the
  event the product signals, with a generous guard against a hang; what the lint covers and what stays a review duty; the command that
  regenerates the keys. Fixing a listed site means lowering its count, or deleting the entry when it was the last.
