### Fixed - the queued-credit test and the cut-answer test no longer fail on a starved CPU, and still fail when the rule they pin is broken

`TestRunQueuedCreditExcludesNonQueuedIntervals` and `TestTheCutSaysWhatTheNodeAnsweredToTheWithdraw` each failed once inside a
full-package run on a loaded Windows machine. Both pass alone; the product was right in both, and each test read a slow runner as a
rule violation. The change is to the two tests only.

- **Root cause, queued credit.** The old test laid its 5xx gap on a clock started when the fixture was built (200 ms to 700 ms) and
  held the give-up to a 1350 to 2500 ms window, timed from the start of the call to the return of the call. A timeline of the old
  test under starvation (37 of 192 runs failed) shows three shapes. 20 failed at the ceiling check (`gave up after 2.5s to 3.1s - the
  wait is no longer bounded`) with the give-up itself on time: the last poll was 1.58 s to 1.65 s in, and the call returned 1.0 s to
  1.45 s later, which is the withdraw request, the telemetry rows and the intent close-out of a starved runner. 14 never saw the gap:
  the first poll reached the node 0.7 s to 1.56 s into the fixture's clock, after the 5xx window had passed. 3 saw the delegator go
  silent for 0.48 s across the gap, so the node answered the 5xx window fewer than five times.
- **Root cause, cut answer.** Each of the seven cases gave its call a 300 ms deadline in which the delegator had to probe the node,
  place the job, dispatch it, record the intent and read its first `accepted`. The cut can only ask a node to take back a job the
  node holds, so a deadline that passed before the dispatch was acked left nothing to ask: no DELETE reached the node, the reason
  said the subtask was never placed, and the case failed on `the node was asked [], want exactly one request`. In all 74 failing
  runs the health probe was logged as cut by the call deadline before it answered.
- **The change, queued credit.** The gap now opens after one `accepted` answer and closes only once the node has answered it five
  times and half a second has passed since its first 503, so the delegator always reads 5xx between its two sides, and a stall only
  makes the gap longer. The floor is exact and read from the node's own timestamps: from the first `accepted` to the give-up the
  delegator spent the credited time (at least the 3.02 s bound, or it would not have given up) plus every uncredited interval, and
  the gap is one of those, at least as long on the delegator's side as the node's stamps of it. A give-up sooner than the bound plus
  the gap banked part of the gap, however slow the runner was. The give-up is read where the node sees it, at the request that asks
  it to take the job back, which the delegator sends the moment it decides, and not at the return of the call. The ceiling is the
  call's context, a hang guard, and the give-up's own words name the bound it gave up at. The contract is given 3 s instead of 1 s,
  because the execution deadline is the budget plus the credit: the time spent outside the backlog (the gap, the first poll, a stall
  beside either) has to stay under the budget, and it had 0.5 s to spare in a 1.02 s budget and has 2.5 s in a 3.02 s one. A call
  that still ends on the execution deadline after the node saw the delegator silent for a second is a stall and skips, the way the
  reserve tests do; one that ends there with no such silence is a credit that was not banked, and fails.
- **The change, cut answer.** The call window is 2 s. The job stays queued on the node for as long as it is polled and the call always
  ends at its own deadline, so nothing the cases assert depends on how long the window is. The seven cases all end at their own
  deadline, so they run in parallel and the test takes about as long as one of them; the unwind allowance and the poll cadence they
  change are set once, before they start, and restored after the last of them. The unwind allowance is 5 s, so a withdraw answered
  late is still inside its bound, and each call has a 30 s hang guard.
- **Still the same rules.** The queued-wait credit counts only the intervals the call actually spent queued: an interval with a 5xx
  answer inside it is not banked. The published cut says what the node answered to the withdraw, on the result and in the corpus row,
  in the same words for each of the seven answers.
- **Proved under starvation.** The stress shape was 24 concurrent copies of one test, 8 runs each, 6 CPU spinners and `GOMAXPROCS=2`
  on an 8-core idle Windows host. The old queued-credit test failed 11 of 192 runs there, every one on the ceiling check; at 1 and 3
  copies (20 and 60 runs) it never failed and at 8 copies it failed 1 of 160. The old cut-answer test failed 74 of 192 runs there,
  every failing case on the same assertion; on a 16-core host it never failed, in 20, 60, 160 and 200 runs at 1, 3, 8 and 40 copies
  with 6 to 8 spinners. The new tests passed 192 of 192 runs each at the 24-copy shape, and the new queued-credit test would have
  skipped in none of them (a copy that turns the skip into a failure passed 192 of 192). Both passed 60 of 60 on a Linux host (6
  copies, 10 runs, 4 spinners). The full `internal/delegate` package passed on a loaded workstation. Two mutants, each committed on a
  throwaway branch and run at the 24-copy shape: the 5xx poll arm leaving the queued span open (the gap between two `accepted`
  answers credited) failed the new queued-credit test in 192 of 192 runs, each on the floor assertion with the give-up 3.03 s after
  the first poll against a 3.5 s floor; a give-up that drops the node's answer from the cut failed the new cut-answer test in 192 of
  192 runs, six cases each, all on the "carry the node's answer" assertion.
