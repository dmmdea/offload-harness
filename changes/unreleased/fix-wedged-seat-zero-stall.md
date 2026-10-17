### Fixed - a stall filed with no silence in it: a stale stall-timer callback no longer judges the run's next phase, and the wedged-seat fixtures give the run's setup and a slow progress write room

- **Two observations, three causes; no single cause covers both.** `TestRunAgentTaskWedgedSeatFilesSeatDownPrefix` failed once in a full `./internal/pipeline` run
  on a loaded box with `stalled: no progress for 0s in admission`, and an earlier fragment recorded a two-hold run that ended `stalled: no progress for 0s in tool
  (allowed 120s; 1 tok so far)`. Both are a stall filed with no silence in it (the `0s` is `%.0f` of a silence under half a second, or of none). The hypothesis that a
  stall timer armed for one phase fires after the monitor has moved on is true of the second and false of the first, and a third failure sat behind the first:
  - **Production (the tool-phase note):** the stall timer's callback cannot say which arm of the timer launched it.
  - **Fixture policy (the admission failure):** the admission phase of the seat-down fixtures was allowed 200 ms, the compressed liveness floor.
  - **Fixture scale (found once the admission failures were gone):** a slow progress write at the prefill's announcement, against a window of 500 ms.
- **The production defect (`internal/agent`).** The stall timer is a `time.AfterFunc`, and `Timer.Reset` cannot recall a callback that has already been
  launched. A timer that expires while `Phase`, `ToolPhase` or `Progress` holds the monitor's lock launches `onStall`, which parks on the lock and takes it the
  moment the method lets go, after the run has moved on and the timer has been re-armed. It then judged the new phase against the new last-progress time and
  filed a stall with no silence in it: `no progress for 0s in tool (allowed 60s; 0 tok so far)`. In a prefill it went on to the seat probe and the busy hold
  instead, so a request entered the hold a few milliseconds into its phase. The monitor has an `epoch` counter, but a callback launched by `AfterFunc` cannot
  capture it, and `onStall` compared it only after its own seat read, so nothing checked it at the lock. Measured on the wedged-seat test pinned to two cores
  with six busy loops: the callback ran 320 times in a prefill and 17 of them (5%) found less than the allowance gone since the last progress.
- **The fix.** `onStall` returns at once when the phase counts its allowance from the last progress (admission, prefill, decoding, tool, re-pack) and less than
  the allowance has passed since it (`stallCallbackStaleLocked`, new file `stallstale.go`). The arm that is current fires by itself, because `Timer.Reset` on an
  `AfterFunc` timer either moves the pending call or schedules a new one, so no stall is lost. The re-looks the monitor makes after a due fire (the probe's poll,
  the engine's poll) happen with the allowance already spent, so they are never taken for stale. The cold-load hold and the busy hold arm their timers by their
  own arithmetic and are left to the code that owns them (the cold-load hold's own stale-callback check is the one in the post-ready fix). Touched in `internal/agent`:
  the condition of `onStall`'s first guard in `liveness.go` (one line) and the new method in `stallstale.go`; `onProbeTick`, the probe-path switch, `fileStallLocked`
  and `checkEngineLocked` are unchanged. A callback made by hand with no silence is now what a stale callback is: `TestParkedMonitorIgnoresATimerThatFiresAnyway`
  called `onStall` right after `Phase` and expected its control arm to stall, so both of its arms now back-date the last progress (the control still stalls; a
  mutant without the park clause still fails the parked arm).
- **The admission defect (`internal/pipeline`, fixture policy).** The admission phase of the monitor is the run's own setup between its start and the loop's first
  request. Its allowance is `max(admission budget, floor)`, and with the admission gate off (the seat-down tests use `agent_admission_wait_sec: -1`, budget 0) it is
  the floor alone, which those tests compress to 200 ms so that a wedged seat is seen quickly. On a starved process the first request went out later than that (in
  one instrumented run, more than 200 ms after the monitor started in 35 of 112 runs, the slowest 6 s later), and the run was stalled in admission before it had
  asked the seat for anything: `stalled: no progress for 2s in admission (allowed 0s; 0 tok so far)`, or, when the busy hold ended it, `seat down: the seat's engine
  did no work for 0s while this request waited in admission (allowed 0s; ...)` with no step in flight and so no recovery wait. A stall filed in the admission phase
  cannot be a stale callback: nothing moves the last progress without leaving the phase, and all 75 admission firings in that run were due (silence at least the
  allowance). `livenessAdmissionFloor` (60 s, the value the floor has in production) is now the least allowance of the admission phase, so a test that compresses
  `livenessFloor` leaves it alone. Nothing changes at production values: gate off 60 s, a 5 s budget 60 s, the default 300 s budget 300 s
  (`TestLivenessPolicyAdmissionAllowanceIsUnchangedInProduction`). Touched: `LivenessPolicyFor` and the new variable in `internal/pipeline/liveness.go`.
- **The slow-write defect (fixture scale, no product change).** The loop announces the prefill to the run's observer (on a fleet node, the job record) after the
  monitor has started the prefill's stall clock and before it registers the model call. A write longer than the clock plus the busy hold's flat bound (200 ms and
  300 ms in these fixtures) lets the monitor file a seat-down verdict while no call is in flight, and a verdict with no call to cancel ends the run without the
  recovery wait: `seat down: the seat's engine did no work for 0s while this request waited in prefill (allowed 0s; ...)`. On a loaded host the write measured 200 to
  500 ms and, with the admission failures gone, the wedged-seat test failed 12 of 450 runs under load that way. `TestRunAgentTaskWedgedSeatSurvivesASlowProgressWrite` makes the write
  700 ms with a progress sink (`core.WithProgressReport`), so the failure is deterministic. The wedged-seat fixtures take their scale from one helper,
  `compressWedgedSeat` (floor 200 ms, cold-load ceiling 700 ms, busy-hold flat bound now 1.5 s instead of 300 ms), so the window is 1.7 s, and the wall bound of
  the four runs, which only has to tell the 700 ms cold-load ceiling from no bound, goes from 8 s to 30 s (the runs take about 2.5 s unloaded). At the production
  floors (60 s and 120 s) the window is minutes and a job-record write is nothing. The other seat-down tests keep their own, shorter scale.
- **Tests, written first.** Red before the fixes, on `1604928b`:
  - `liveness_stalecallback_test.go` queues the method that moves the run on ahead of an expired timer's callback on the monitor's lock (no hook in the product):
    `a stall timer armed for decoding expired as the run moved into a tool and filed "stalled: no progress for 0s in tool (allowed 60s; 0 tok so far)"`,
    `a stall timer expired as a token was being booked and filed "stalled: no progress for 0s in decoding (allowed 10s; 1 tok so far)"` and
    `the stale callback filed "stalled: no progress for 0s in tool (allowed 1s; 0 tok so far)" before the tool's second had passed`; a control that real silence
    still stalls, with a silence at least as long as its allowance, passes before and after.
  - `agenttask_seatdown_admission_test.go`: `the admission phase may be silent for 200ms with the floor at 200ms, want at least 10 s`.
  - `agenttask_seatdown_slowwrite_test.go`: `TestRunAgentTaskWedgedSeatSurvivesASlowProgressWrite` with the fixtures at the 300 ms flat bound they had: 3 of 3 runs failed with `seat down: the seat's engine did no work for 0s while this request waited in prefill (allowed 0s; ...)` and no `did not come back` wait.
- **Proved.** Counts are runs of one compiled test binary (`go test -c -o <exe> ./internal/pipeline/`; an unfixed build uses `-overlay` to put the `1604928b` files back).
  "Under load" is the binary with `-test.cpu 1` pinned to two cores together with four (or six) busy-loop processes pinned to the same two cores (started before the run,
  killed by PID after it).
  - **Cause B, deterministic** (`go test -count=20 -run 'TestStallTimerExpired|TestIgnoredStale|TestRealSilenceStill' ./internal/agent/`): with the stale clause removed from
    `onStall`'s guard (the unfixed monitor) 60 of 60 failed (three tests of 20 runs) and the control passed 20 of 20; with the fix 80 of 80 passed, and
    `-cpu 1,2,4 -count=10` with the parked-monitor test 150 of 150.
  - **Cause A, deterministic** (`TestSeatDownPipelineAdmissionOutlastsTheCompressedFloor`): unfixed policy 5 of 5 failed; fixed 60 of 60 passed (`-cpu 1,2,4 -count=20`)
    and 50 of 50 under load.
  - **Cause C, deterministic** (`TestRunAgentTaskWedgedSeatSurvivesASlowProgressWrite`): with the helper's flat bound put back to 300 ms 30 of 30 failed (`-cpu 1,2,4
    -count=10`); fixed 60 of 60 passed (`-cpu 1,2,4 -count=20`) and 50 of 50 under load.
  - **The original test under load**, `-test.run '^TestRunAgentTaskWedgedSeatFilesSeatDownPrefix$' -test.count 150 -test.cpu 1`:
    - Unfixed build: 1 of 150 failed with four busy loops and 2 of 150 with six, all in the admission phase, one of them the observation's own line
      `stalled: no progress for 0s in admission (allowed 0s; 0 tok so far)`. An earlier series on a busier host failed 25 of 300 runs (3 of 40, 7 of 60, 8 of 100, 7 of 100), all in admission.
    - With the admission and stale-callback fixes and the old fixture scale: 12 of 450 failed (5 of 300 in three series of 100, 7 of 150 in a later one), every one in the
      prefill, the slow progress write above. This is what the third fix is for.
    - Final code: 0 of 150 with four busy loops, 0 of 150 with six, and unloaded `-test.cpu 1,2,4 -test.count 50` 0 of 150.
- **Not verified here.** No GPU lease, live seat or fleet node was used. How often the stale callback occurs at production allowances (60 s and longer) is not measured:
  it needs a silence that ends within microseconds of its allowance, which is rare there and common at the tens of milliseconds the tests use. The other seat-down
  tests (`TestRunAgentTaskStallsAWedgedEngine` and its siblings) share the shorter 200 ms and 300 ms scale and were not run under load for this change.
