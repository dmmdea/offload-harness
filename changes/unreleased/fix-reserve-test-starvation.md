### Fixed - the two call-reserve start-order tests no longer fail on a starved CPU, and still fail when a subtask is started inside the reserve

`TestASubtaskIsNotStartedInsideTheReserve` and `TestALaterChunkIsNotStartedInsideTheReserve` failed once on a loaded
Windows machine (`Succeeded:3 Deferred:2`, and "started 4 times, want 8"). Both pass alone; the product was right.

- **Root cause.** The launch loop refuses to start a subtask once the call has no more left than the reserve
  (`callDeadline.noRoom`, read at `time.Now()` through `timeLeft`), and the first test gave the first wave a 0.8 s window
  (a 2 s call, a 1.2 s reserve) to start in. A starved CPU that delayed the first starts past that window made the
  reserve refuse them, correctly, and the test read a timing accident as a rule violation. The second test had the same
  shape: its second wave had to start within 1 s of the first wave ending, because both waves ran for a fixed 2 s.
- **The change (tests only).**
  - The seat no longer runs for a fixed time. It holds at a barrier that opens just after the call's own reserve begins
    (`callDeadline.waitHorizon`, the instant the product computed), so the slot of every held subtask frees inside the
    reserve whatever the runner's speed was. The second test lets its first wave go on an event (the fourth start), not a
    sleep.
  - The call's deadline state is built up front, the way `RunBatched` builds it (`RunOptions.call` already honours a
    prebuilt one), so the test reads the call's own span instead of assuming it.
  - The windows are wider (4 s with a 2 s reserve, 6 s with a 3 s reserve), because the clock the reserve is read from has
    no seam and the first wave's start remains the one thing that depends on it. A barrier alone does not widen that
    margin, which is why the windows are wider as well: the first wave now has 2 s and the second wave 3 s, where
    they had 0.8 s and 1 s.
  - A refused subtask is checked against the launch loop's own decision: its reason carries how much of the call it found
    left, and that must be no more than the reserve. A run whose first wave was refused by the reserve for real (a stall
    longer than the margin), or whose freed slot was answered by the call's deadline, is skipped as a stall; it can no
    longer fail.
- **Still the same rule.** A subtask whose slot frees inside the reserve is not started, is an unplaced capacity defer with
  the same reason text, and is not the call-deadline cut. The barrier opens only inside the reserve, so a start of the
  fifth subtask (or of the third wave, or of the second chunk) is a failure on any runner; the start count can no longer be
  made right by a slow one.
