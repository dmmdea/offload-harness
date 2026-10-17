### Added - a lint keeps a test from failing because a loaded runner stretched a measured time

The starvation fixes of this series (the call-reserve start order, the queued-credit and cut-answer tests, the call-deadline windows,
the lease-renewal helper, the busy-hold ceiling, the warm-watch poll) each found one test that put a number on the clock, after a green
suite went red once, and fixed that one test. The next test to do it was not covered. This change adds the lint that covers the part of
the fault a syntax tree can see. It adds two test files in the root package and one section of `CONTRIBUTING.md`; no product code and no
existing test changes.

- **What it checks.** `clock_lint_test.go` reads every `*_test.go` file of the module (the same walk as `tempdir_lint_test.go`) and fails
  on an upper bound on a measured elapsed time under one second: `time.Since(x)`, a difference of two `time.Now()` stamps, a variable that
  only ever holds one of those, or that value read as a number (`.Seconds()`, `.Milliseconds()`), compared with `>` or `>=` (or the same
  written the other way round) against a constant it can evaluate (a literal, a unit product such as `250*time.Millisecond`, a package
  const of the same package, or a const or once-assigned local of the same function). The key is `file:elapsed>C`.
  `clock_lint_allowlist_test.go` lists the 22 keys (25 sites) the tree holds, each with its count and a reason that names what the bound
  rules out and how wide the margin is; a thin margin is written down as thin. The list is kept true in both directions, as the temp-dir
  list is: a new bound, a larger count, a fixed site whose entry stays, and an entry without a reason each fail.
- **Why one second.** A bound of a second or more is where a bound starts to be a hang guard, and where the list stops being something one
  person can review: the tree holds 22 keys under 1 s, 52 under 2 s, 72 under 3 s and 92 under 5 s. It is a ratchet, not a proof: a 2.5 s
  ceiling failed under starvation too. Raising `clockBoundFloor` and listing what it then finds is the next step.
- **What it cannot see** (written at the top of the file): a bound against a value it cannot evaluate (a field, a parameter, a poll
  interval), a bound written `elapsed < C` that the test requires, a fixed sleep before an assertion, a short call or context window handed
  to the code under test, a helper that takes a short timeout. `case <-time.After(C): t.Fatal(...)` ceilings were counted when it was
  written: none is under a second.
- **Red first.** With an empty allowlist the lint failed on the real tree with 22 findings, the first being
  `gpu_warm_watch_test.go fails when a measured time is above 500ms (3 time(s)) and is not in clockBoundAllowlist`; with the list written it
  passes. A scratch test file holding `time.Since(start) > 250*time.Millisecond` turned it red and named that file and the form, and was
  removed. Run on the revision before the queued-credit fix, the lint is red on that test's fixture clock (`e >= gapStart && e < gapEnd`,
  a 5xx window 200 ms to 700 ms after the fixture was built, the schedule that fix removed); it names the lower edge of the window, which it
  cannot tell from a ceiling, and the site is in the file the fix rewrote.
- **Mutations.** Six single-line mutations of the scanner, each run with `scripts/mutation-check.sh` against the scanner's own tests, were
  all `RED`: dropping `>=` from the upper-bound operators, dropping `<=` from the flipped ones, treating a bound of zero as a bound, reading
  any `Sub` as an elapsed time, no longer failing on a count below the listed one, and a local no longer shadowing a package const.
- **No new finding in the open changes.** The same scanner run on the test files of the four open pull requests found no site outside the
  list.
- **`CONTRIBUTING.md`** gets a section, "Keeping tests from failing on a slow machine", beside the temp-dir one: prefer waiting on the event
  the product signals, with a generous hang guard; what the lint covers and what stays a review duty; the command that regenerates the keys.
