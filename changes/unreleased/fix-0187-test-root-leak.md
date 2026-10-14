### Fixed - the root test suite no longer leaves an empty state root in the OS temp dir for every copy of its test binary that lost the state dir, and every suite reports a root it cannot remove

- **The defect.** Plain `go test` runs left `lo-root-state-*` folders in the OS temp dir, all empty (79 on one machine), plus a few
  `lo-delegate-state-*` and `lo-mcpserver-state-*` ones and the odd top-level `Test<Name><n>` folder. Two causes were on the table: (a) a
  re-launched copy of the root test binary makes a state root of its own and ends by `os.Exit` inside a test or is killed by its parent, so
  the removal in `runIsolated` never runs; (b) the removal fails (a process still holding a file or the folder on Windows) and
  `_ = removeAll(dir)` drops the error. The measurement decided it: the root package's gpu tests alone left 13 roots in one run, every one
  with no contents (a failed removal of a used root leaves its contents, a root nobody used is empty), while a complete run of
  `internal/delegate` and of `internal/mcpserver` left nothing. The tests that start a real `gpu reserve` wrapper take
  `LOCAL_OFFLOAD_STATE_DIR` out of the copy's environment (`treeEnv`) or empty it in their own (`isolateFromMachineState`), so the copy
  fails `inheritedSuiteRoot` and, by design, gets a root of its own (the net fails closed). That root lived in the OS temp dir, and the
  copies it belongs to never reach the removal. Cause (a) is the leak; (b) was real but silent, and is fixed beside it.
- **The fix (test code only).** In the root package `runIsolated` now makes a copy's root inside the throwaway root of the suite that
  launched it (`relaunchRootParent`: a `relaunch` folder in the root `LO_ROOT_SUITE_STATE_ROOT` names), when that variable names a
  throwaway root of the package that is still on disk; the suite removes it with its own root when it ends. The copy keeps a root of its
  own and pins the state dir, home and origin exactly as before, so the safety net is unchanged; with no live suite root named it still
  goes to the OS temp dir. In all three `TestMain`s (root, `internal/delegate`, `internal/mcpserver`) the removal is now tried once more
  after 250 ms and a root that is still there is named on stderr with the error, instead of being dropped. The suite's result is
  unchanged. `internal/delegate` and `internal/mcpserver` start no copy of their test binary, so the nesting is the root package's only.
- **Tests, written first and red.** `TestARelaunchedCopyThatLostTheStateDirLeavesNothingInTheTempDir` starts a copy with the state dir
  taken out or emptied, once ending by `os.Exit` from inside a test and once killed by its parent, with the temp dir pointed at an empty
  folder: red before with `left 1 entries in its temp dir, want none: [lo-root-state-...]` in all four cases, and `not inside the suite's
  root`. `TestRunIsolatedRetriesARemovalOnceAndReportsWhatStaysBehind` (in each of the three packages) was red with `the removal was tried
  1 times, want 2`. `TestACopyWithNoLiveSuiteRootKeepsARootOfItsOwnInTheTempDir` and `TestRelaunchRootParentFollowsOnlyALiveThrowawayRoot`
  pin the fail-closed side. Green evidence: the same gpu test selection, run with the temp dir pointed at an empty folder, left 13 roots
  before and none after. Mutation-checked once per rule, each red and restored byte for byte: nesting off, the prefix check removed, the
  existence check removed (root), and in each of the three packages no retry, a second retry, no report, and a report when the retry
  worked.
- **What this does not cover.** A suite killed from outside (a `go test` timeout, a kill by the RAM guard, Ctrl-C) never reaches any
  deferred removal and still leaves its root and the `t.TempDir()` folders of the tests it was running; no test code can clean that, and
  the tripwire (`scripts/test-temp-tripwire.sh`) is what reports it. Stderr of a passing package is hidden by `go test` in package-list
  mode, so the new removal report shows in a single-package run or with `-v`.
