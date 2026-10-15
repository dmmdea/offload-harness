### Fixed - the warm-watch reload test no longer fails on a starved host: its stand-in now holds the load until the watch has polled it twice, instead of for a fixed 300 ms

- **The flake.** `TestWarmKeepsWaitingForALoadItSawStartingThroughOneUnreadableRunningAfterAReload` pins that one unreadable `/running` poll in the
  middle of a load the watch saw starting does not end the warm. It needs two polls of the starting seat, the second one failing, and its
  stand-in kept the seat "starting" for a fixed 300 ms. On a starved CI runner the watch polled once inside the hold, the load had landed by the
  next poll, and the test's own check fired ("the stand-in never failed a poll mid-load (polls while starting=1): the test proved nothing") on a
  docs-only run. The check was right to fire, since the test then proves nothing. The product was not wrong; the test depended on a race.
- **The fix (test code only; no production code changed).** The stand-in (`warmOrderSwap`) gets `holdPolls`: a load that outlives its health
  request lands only after that many `/running` reads have been answered while the seat was starting. The read is counted after its answer is
  written, so the poll that decides the test is served before the seat changes. A wall-clock cap (`holdCap`, 10 s) ends the hold anyway, so a
  watch that never polls fails the test instead of hanging it. The test sets `holdPolls: 2` and runs at two poll cadences, 10 ms and 400 ms; the
  second is slower than any fixed hold the load could have had, which is the starved host made deterministic and keeps the stand-in from going
  back to a fixed hold unnoticed. Its check now counts the poll the stand-in actually failed. A test that does not set `holdPolls` keeps the fixed
  `warmHold`, so the other tests that share the stand-in behave as before.
- **Proved** (stress mode of the remote gate runner on an idle node, `--stress 24:8 --burn 6 --env GOMAXPROCS=2 --test-args '-test.run=^TestWarmKeepsWaiting'`,
  192 runs):
  - Red, deterministic: the unfixed test with the watch's poll interval set to 400 ms (a hook for the measurement, not committed) failed 5 of 5
    locally and 192 of 192 on the node, every one with the message above. Load alone did not reproduce it: the unfixed test passed 192 of 192 on
    that command and 384 of 384 on `--stress 48:8 --burn 12 --env GOMAXPROCS=1`, because it takes a stall of about 300 ms between the answer
    and the next poll, which a busy shared runner produces and these loads did not.
  - Green: the fixed test, 0 failures in 192 on the same command and 0 in 384 on the heavier one. The one other user of the changed path,
    `TestAnUnloadSeatAcquireStartsOnAnEmptyCardWhenTheWarmOutlastsTheHealthWait`, 0 failures in 48 (`--stress 12:4 --burn 4`).
  - Mutant, product: the unreadable-poll case of `watchWarm` loses its "last reading found the seat starting" condition, so the watch calls the
    state unknown and gives up on the poll and the load is sent again. The fixed test failed 192 of 192 at both cadences ("health requests=3
    (want 2) loads=2 (want 1)") and 3 of 3 locally. The mutant was built as a scratch commit and never touched the branch or the working tree.
  - Mutant, self-check: a stand-in that never fails a poll, and a stand-in whose cap ends the hold at once, each failed 3 of 3 locally at both
    cadences on "the stand-in never failed a poll mid-load".
- **Same family, not changed.** The other tests of `gpu_warm_watch_test.go` and `gpu_warm_reload_test.go` that wait out a timer (a goroutine
  that flips the seat after 100 to 150 ms, a 400 to 800 ms hold against a 100 to 150 ms client timeout, a floor on the number of re-sends inside
  a grace) depend on a clock, not on a count of polls, and have no check that goes vacuous; they are left as they are.
