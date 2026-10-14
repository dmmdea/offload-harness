### Fixed - the root test suite no longer leaves an empty state root in the OS temp dir for every copy of its test binary that lost the state dir, and a suite that cannot remove its throwaway root now says so and fails

- **The defect.** Plain `go test` runs left `lo-root-state-*` folders in the OS temp dir: 58 on one machine at a census on 2026-10-14,
  57 of them empty (the 58th held a ledger file). A few `lo-delegate-state-*` and `lo-mcpserver-state-*` folders had also been seen; no
  complete run of those two suites left one, and what made them was not measured. Two causes were on the table: (a) a re-launched copy of
  the root test binary makes a state root of its own and ends by `os.Exit` inside a test or is killed by its parent, so the removal in
  `runIsolated` never runs; (b) the removal fails (a process still holding a file or the folder on Windows) and `_ = removeAll(dir)` drops
  the error. The measurement decided it: the root package's gpu tests alone left 13 roots in one run, every one with no contents (a failed
  removal of a used root leaves its contents, a root nobody used is empty), while a complete run of `internal/delegate` and of
  `internal/mcpserver` left nothing. The tests that start a real `gpu reserve` wrapper take `LOCAL_OFFLOAD_STATE_DIR` out of the copy's
  environment (`treeEnv`) or empty it in their own (`isolateFromMachineState`), so the copy fails `inheritedSuiteRoot` and, by design, gets
  a root of its own (the net fails closed). That root lived in the OS temp dir, and the copies it belongs to never reach the removal.
  Cause (a) is the leak. Cause (b) was not seen in any measured run (no removal failed in the runs made with the report in place:
  `internal/delegate`, `internal/mcpserver` and the root package's gpu tests), but the old code dropped the error, so a failed removal could
  not have been seen either; it is now retried, reported and fails the package.
- **The fix (test code only).** In the root package `runIsolated` now makes a copy's root inside the throwaway root of the suite that
  launched it (`relaunchRootParent`: a `relaunch` folder in the root `LO_ROOT_SUITE_STATE_ROOT` names), when that variable names a
  throwaway root of the package that is still on disk; the suite removes it with its own root when it ends. The copy keeps a root of its
  own and pins the state dir, home and origin exactly as before, so the safety net is unchanged; with no live suite root named it still
  goes to the OS temp dir, and when the variable names a throwaway root that cannot be used (gone, not a folder, no room for the
  `relaunch` folder) the fallback says why on stderr instead of happening without a word. In all six suites that make a throwaway root
  (the root package, `internal/delegate`, `internal/mcpserver`, `internal/mediaremote`, `internal/pairworkloads`, `internal/pipeline`) the
  removal is now tried once more after 250 ms, and a root that is still there is named on stderr with the error. A suite that passed then
  exits 1 and a suite that failed keeps its own code: `go test ./...` and `go test .` print the stderr of a package that passed only with
  `-v` (measured on a probe module), so the line alone reached no one, and a stuck copy of the test binary can now hold the whole suite
  root, because its own root lives inside it. The copies the other suites start are covered by their own
  `TestARelaunchedTestBinaryLeavesNothingInTheTempDir` (`internal/delegate` and `internal/mcpserver` start none), so the nesting is the
  root package's only.
- **Tests, written first and red.** `TestARelaunchedCopyThatLostTheStateDirLeavesNothingInTheTempDir` starts a copy with the state dir
  taken out or emptied, once ending by `os.Exit` from inside a test and once killed by its parent, with the temp dir pointed at an empty
  folder: red before with `left 1 entries in its temp dir, want none: [lo-root-state-...]` in all four cases, and `not inside the suite's
  root`. `TestRunIsolatedRetriesARemovalOnceAndReportsWhatStaysBehind` (in the root, `internal/delegate`, `internal/mcpserver`,
  `internal/mediaremote` and `internal/pairworkloads`) and `TestFinishIsolationRetriesARemovalOnceAndReportsWhatStaysBehind` (in
  `internal/pipeline`, with `TestMainRemovesItsRootThroughFinishIsolation` pinning the wiring of its `TestMain`) were red with `the
  removal was tried 1 times, want 2` and `code = 0, want 1`; `TestRelaunchRootParentSaysWhyItFellBackToTheTempDir` was red with `the report
  "" should contain "root tests:"`. `TestACopyWithNoLiveSuiteRootKeepsARootOfItsOwnInTheTempDir` and
  `TestRelaunchRootParentFollowsOnlyALiveThrowawayRoot` pin the fail-closed side. Green evidence: the same gpu test selection, run with the
  temp dir pointed at an empty folder, left 13 roots before and none after. Mutation-checked once per rule, each red and restored byte for
  byte: nesting off, the prefix check removed, the existence check removed (root); in each of the six suites no retry, a retry when the
  first removal worked, no report, a report when the retry worked, no exit code for a root left behind, and an exit code that hides a
  failing suite's own; the `internal/pipeline` wiring; and, in the root, no report for each of the three ways the fallback happens and for
  the error text.
- **The top-level `Test<Name><n>` folders.** Those are `t.TempDir()`'s own parent folders, left when a test's process dies before its
  cleanup runs. No test was found that makes one without cleaning it, and no source makes one by name (no `os.MkdirTemp` call takes
  `t.Name()`). Measured with the temp dir and `GOTMPDIR` (where `go test` puts the `t.TempDir()` parents) both pointed at empty folders
  and both listed at the end, which a probe test that exits after `t.TempDir()` shows the census does see: a whole run of every other
  package of the module with both folders listed (115 packages with tests, `internal/pipeline` included; in it one timing test of
  `internal/delegate` failed under load on the unfixed tree, and that failed run left nothing either), and the root package's gpu tests (before and after the fix, plus the five tests of
  `gpu_signals_harden_test.go`) left no `Test<Name><n>` folder. The one inspected on the machine that found the leak
  (`TestARefusedUnloadOfAResidentSeatWritesNoDebt<n>`) still held a live lease file, so its test was killed mid-run; a complete run of that
  test leaves nothing. The rest of the root package's tests were not run (the whole suite is too heavy to run locally), so they are
  unmeasured, not cleared.
- **What this does not cover.** A suite killed from outside (a `go test` timeout, a kill, Ctrl-C) never reaches any
  deferred removal and still leaves its root and the `t.TempDir()` folders of the tests it was running; no test code can clean that, and
  the tripwire (`scripts/test-temp-tripwire.sh`) is what reports it. A copy started with an environment that lacks
  `LO_ROOT_SUITE_STATE_ROOT` keeps its root in the OS temp dir. The one retry waits 250 ms: a process that holds the root longer no longer
  passes silently, it fails the package.
