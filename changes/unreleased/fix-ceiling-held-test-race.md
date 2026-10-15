### Fixed - the busy-hold tests no longer fail on a starved host: they keep the fake's request silent until the run is in the hold, and the ceiling test asserts the hold instead of a share of its ceiling

- **The flake.** `TestRunAgentTaskCeilingWhileHeldIsNotABudgetDefer` failed in CI on a change that touched only `internal/eval`
  (`precondition: deferred=true queued_ms=394 reason="ceiling 1s reached while held behind the seat's other work ..."`). It required
  `queued_ms >= 500` of a 1 s ceiling. `queued_ms` is the time booked in the busy hold, and the hold cannot begin until the request's own allowance
  has run out (200 ms in the test) and the monitor's first look at the engine (the seat probe, then the engine probe's two reads) has returned;
  the hold is booked from the moment that look returns. A run held for the rest of its life therefore reports about ceiling minus allowance minus
  the look's latency: 795 ms on a quiet host, 394 ms on the CI runner. The product was right (the class was already capacity, the reason already
  said "held"); the test assumed a schedule.
- **A second failure the same test had.** Under load on an idle node it failed two ways. Once as above (`queued_ms=267`, class right). Three
  times as `ceiling 4s reached while producing (0 tok at 0.0 tok/s)` with `queued_ms=0` (the elapsed figure is the ceiling timer firing seconds
  late): the process was starved for longer than the 1 s ceiling, so the stall timer and the ceiling were both overdue when it woke, and the
  ceiling fired while the first look was still in flight, before the run's hold was entered. No assertion on queued time can hold there; the
  ceiling needs room.
- **The fix (test code only; no production code changed).**
  - The ceiling test's precondition is that the ceiling found the run in the hold, read from the job record (the last report is `queued`) and
    `queued_ms > 0`, never how much of the ceiling the hold took. The ceiling is 8 s, so the path to the hold has room, and the silent request now
    waits on its own context, so the test ends with the run and no longer sleeps 6 s. The decision it pins is unchanged: a run held when its
    ceiling fires is not a budget defer and does not say "while producing".
  - Five more tests had the same shape: a fixed silence (1.5 s, 2 x 800 ms, 900 ms, 800 ms, 600 ms) followed by an assertion that the hold booked a fixed
    share of it (`queued_ms` at least 800, 900 and 400) or that the job record showed the hold at all. They fail the same way when the first look is
    late. A `holdWatch` reads the job record, and the fake keeps the request silent until the record has said `queued` and then for a stated time
    (600 ms, 2 x 300 ms, 400 ms, 100 ms, 350 ms); the tests assert at least that time. `TestRunAgentTaskHeldRunDoesNotFeedTheSeatRates` gets the same wait on
    its held arm; its control arm is unchanged.
- **Proved** (stress mode of the remote gate runner on an idle node: W concurrent copies of the compiled test binary by N runs each).
  - Red under load, the ceiling test unfixed: `--stress 4:20 --burn 6 --env GOMAXPROCS=2 --test-args '-test.run=^TestRunAgentTaskCeilingWhileHeld'`,
    4 of 80 failed (one `queued_ms=267`, three `while producing` with `queued_ms=0`), all inside a stretch where the node ran slow (runs of 11 to 29 s
    against 6 s). Load alone is not a reliable red; the same command on the fixed test passed 80 of 80, but the node had no slow stretch that time,
    so the rows below carry the proof.
  - Red, deterministic: the unfixed test with its first engine read answered 600 ms late (a scratch commit, not on the branch), `--stress 4:10`:
    40 of 40 failed with `queued_ms` 192 to 196 and the reason still saying held, which is the CI message.
  - Green: the fixed test with the same late read, 0 of 40; with the read 3 s late, 0 of 40; on a Linux node under `--burn 4`, 0 of 32 for the
    six tests together.
  - Mutant, product: `CeilingError.Held` always false: the fixed test failed 40 of 40 on the decision line (filed `class=budget`, `while
    producing`). The pipeline filing every ceiling as budget while the reason still says held: 40 of 40 failed on the same line.
  - Siblings, red: three of them unfixed under `--stress 4:40 --burn 6 --env GOMAXPROCS=2`: 7 failures in 160 runs, six of them `queued_ms`
    misses (159, 213 and 0 against 800; 861, 696 and 602 against 900) and one the stall described last, and 14 to 36 of 120 each in a slow stretch
    of the node. With the late first read, `--stress 4:5`:
    the 1.5 s test 20 of 20, the two-hold test 20 of 20, the job-record test 17 of 20. The 900 ms decoding test passes that stimulus (its
    `queued_ms` stays near 700, so the delay does not reach its hold) and failed under load only.
  - Siblings, green: the same late read, 0 of 20; load, 0 of 160 on the command above. Paired, the six tests together under `--stress 12:10 --burn 6
    --env GOMAXPROCS=1`: unfixed 1 of 120 (the two-hold test, `queued_ms` 686 against 900), fixed 0 of 120. Mutants: the monitor reporting no queued time failed all four `queued_ms` tests, 12 of 12; a later hold replacing the booked time
    instead of adding to it failed the two-hold test, 12 of 12 (`queued_ms` 301 to 305 against 600).
- **Same family, not changed.** `TestRunAgentTaskStallsAWedgedEngine`, `TestRunAgentTaskStallsAThrashingEngine` and
  `TestRunAgentTaskWithoutASeatAddressKeepsTheOldRule` keep a request silent for 6 s and fail only when the process is starved for more than that
  (they failed in the same slow stretch and not otherwise); the control arm of `TestRunAgentTaskHeldRunDoesNotFeedTheSeatRates` needs a stall of
  seconds to be held. A fixed silence is the point of those tests, so they are left as they are. One two-hold run in the red set ended `stalled: no
  progress for 0s in tool (allowed 120s; 1 tok so far)`: a stall filed with no silence at all, which looks like a stall timer that fired for the
  phase before. It is not a test matter and is not changed here.
