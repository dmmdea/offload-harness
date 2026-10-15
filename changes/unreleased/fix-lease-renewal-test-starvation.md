### Fixed - the lease-renewal test no longer fails on a starved host: its helper now beats until the test has seen the renewal it needs, instead of for a fixed run

- **The flake.** `TestWrapperRenewsTheLeaseOfAHelperThatKeepsBeating` wraps a helper (a separate process) that beats a progress file every
  100 ms under a lease with a 1 s term and a 1.5 s stall window, and the helper ran for a fixed 3.5 s. On a CPU-starved host the wrapper is
  slow to notice that the helper exited; a term check landed after the helper had stopped writing and before the wrapper saw the exit, read
  a file silent for longer than the stall window, and labelled a healthy lease ("its progress file has been silent for longer than its 1.5s
  stall window"). Reproduced under a heavy parallel load (40 concurrent copies of the test binary on a single P, six CPU spinners): 6
  failures in 160 runs on the unfixed test, every one with four or five renewals logged first and then the label, which is the signature of
  the helper's end and not of late beats. A first attempt that only widened the windows around the fixed run was worse at the same load
  (27 and 41 failures in 160), because a later term check then landed in the same gap.
- **The fix (test code only; no production code changed).** The helper beats until the test writes a stop file (`LO_HELPER_STOP_FILE`),
  beats once more and exits at once; its run length is only a safety cap, and the other tests that use the helper are untouched. The test
  waits, bounded by the test deadline, until the lease record shows a term end stamped by a check made more than one stall window after the
  progress file was created (stall window 3 s, term 5.5 s), then stops the helper. The lease is judged only up to that moment, while the
  helper is beating (never labelled expired); what the wrapper does with the silence after the helper is gone is the rule of
  `TestWrapperLabelsTheLeaseOfAHelperThatStoppedBeating`. A failed run stops the helper and collects the wrapper, so no wrapper is left
  running with `os.Stderr` redirected into the next test.
- **Why it proves the rule.** The progress file's mtime is its creation time, so before this change a helper that never beat still read
  fresh at the first term check and the test was partly vacuous. Now a term end `E` can only come from a check made at or after `E` minus
  the term, in both branches of `nextEnd`; waiting for `E` minus the term to pass the file's creation plus one stall window means that
  check found a file written to after it was created, which only a beating helper provides.
- **Proved** (same load shape for each row, 160 runs, `GOMAXPROCS=1`): unfixed test 6 failures; fixed test 0 failures; mutant, product (the
  progress age read from the lease's acquisition time instead of the file's mtime) 160 of 160 red; mutant, test (the helper never beats)
  160 of 160 red, and 3 of 3 red on an idle host. Linux, 8 workers by 4 runs with four spinners and `GOMAXPROCS=2`: fixed test 0 failures.
