### Fixed - a spent post-ready bound makes the same decision on the seat probe's tick as on the stall timer

- **A production race, not a test timing assumption.** A request held in the post-ready part of the cold-load hold (the
  seat loaded for it and read ready, no byte yet) is judged at the bound by two callers: the stall timer, which asks the
  engine, and the seat probe's tick, which polls `/running`. While a tick's read is in flight the timer sees the probe busy
  and leaves the decision to the tick (`onStall`), and the tick used to file the stall without asking anyone. Two legs of
  that one defect:
  - **Engine leg.** The seat reads ready (or cannot be read) and the engine works for the request's siblings. The tick
    stalled a request that was waiting its turn; the timer would have entered the busy hold.
  - **Loading leg.** The straddling read says the seat is **loading** again (evicted after it read ready). The tick took
    no engine read and no loading hold either: it stalled with the cold-load ceiling unspent and a reason that
    contradicted itself, `no byte for 1s after the seat read ready ... the seat: starting`. The same read one poll
    earlier enters the loading hold, and the timer path (the engine read reports `Loading`) enters it too.
- **One decision for both callers.** `silenceVerdictLocked` now decides, for the timer and the tick alike, what a silent
  request becomes; before, each caller carried its own copy of the conditions and they had drifted apart. At a spent
  hold bound, by what the seat reads:
  - **Post-ready bound spent, seat loading.** Timer: the engine read reports `Loading`, back to the loading hold. Tick
    before: stalled, no engine read (the defect). Both now: back to the loading hold, with the cold-load ceiling still
    counted from the first silence, so a load that never ends is still stalled at the ceiling, as a cold-load stall.
  - **Post-ready bound spent, seat ready or unreadable.** Both read the engine: the busy hold while it works for
    someone, the stall when it is flat. Unchanged (the unreadable seat says nothing about the engine, and the production
    engine read answers for itself).
  - **Cold-load ceiling spent, any seat state.** Both stall, without an engine read. Unchanged.
  - **No engine probe installed** (`EngineFlat` 0; the real wiring always installs one beside the seat probe). The
    timer stalls at a spent bound without reading the seat; the tick stalls on a ready or unreadable seat as before and
    sends a loading one back to the loading hold. Left as it is.
- **The stall reason can no longer contradict itself.** A post-ready stall cannot carry a loading seat state any more: a
  loading read at a spent post-ready bound re-enters the loading part, and a load that outlasts the ceiling reads
  `stalled: seat still loading after Ns in cold-load (... the seat read "starting", so no byte could arrive)`. Pinned: the
  reason never says `seat read ready` when the last read said `stopping`.
- **A stale timer callback.** The first form of this fix still stalled the loading leg a second in, about a quarter of
  the time in loaded runs (8 of the first 34 loaded runs and 4 of the first 38 unloaded ones failed, with `stalled: seat still
  loading after 1s in cold-load (allowed 30s ...)`): the timer callback that had fired at the bound was waiting for the
  monitor's lock, ran after the tick had re-armed the timer for the longer ceiling, and stalled a request the tick had
  just held. In the hold the timer is always armed for what is left of the current part, so a callback that finds time
  left is not due: it re-arms for that time and returns.
- **Found as a CI flake.** `TestPostReadyHoldReadsTheEngineBeforeStalling` failed once on a hosted Linux runner with
  `a post-ready request whose engine works for its siblings was stalled: stalled: no byte for 0s after the seat read
  ready, in cold-load (... post-ready bound = max(0s, 2 x 0s prefill allowance) ...)`. In that test the probe tick falls
  on the bound's own instant (a 10 ms poll, a 40 ms prefill allowance and a 100 ms bound), so who decides is a coin toss.
- **Evidence for the engine leg (race rigor).** Counts are runs of the test binary, 3 `-cpu` settings each.
  - Red, original test on the parent: `go test -c -o agent.test ./internal/agent/`, then in `internal/agent`
    `agent.test -test.run '^TestPostReadyHoldReadsTheEngineBeforeStalling$' -test.count=150 -test.cpu 1,2,4`:
    3/450 failed, with the CI line above.
  - Red, deterministic: `TestPostReadyBoundExpiringDuringASeatReadReadsTheEngine` (the first seat read made in the
    post-ready part returns only after the bound is spent, so the tick always lands after it), committed before the
    fix, with `-test.run '^TestPostReadyBoundExpiringDuringASeatReadReadsTheEngine$' -test.count=20 -test.cpu 1,2,4`:
    60/60 failed with the same reason.
  - Green with the first form of the fix: the deterministic test 0/60 (same command); the original test
    `-test.count=400 -test.cpu 1,2,4`: 0/1200. Mutant (the engine-read branch of the tick disabled), both tests kept: the
    deterministic test 60/60 failed and the original test failed 26/1200.
  - The deterministic test must not depend on the scheduler (review finding). Its first form slowed the seat read only
    when it began in cold-load, and gave the bound 100 ms against a 40 ms prefill allowance, a window of about 60 ms. On a
    starved runner the stall timer and the first tick both ran after the bound, every seat read happened in prefill, the
    slow read never started and the test failed with `the test never made the post-ready seat read slow; it proves
    nothing` (a false red, never a false pass): 1/100 failed with 2 spinners on the test's core and 3/100 with 6 (binary
    pinned to one core, `-test.cpu 1`). The test now gives the post-ready part a 1 s bound, so the first cold-load tick
    starts far inside it; the slow read waits on the monitor's own state until the bound is spent, and the test polls
    for the busy hold instead of sleeping.
- **Evidence for the loading leg and the decision (race rigor).** The tests wait on the monitor's own state, never on a
  wall-clock share: the loading-hold test, the stall-at-the-ceiling test, the reason-wording test, the unreadable-seat
  pin and the cold-load-ceiling pin (all in `internal/agent/liveness_spentbound_test.go`, with the same straddling-read
  hook as the engine test, the post-ready part 1 s), a table that calls the tick and the stall timer's callback by hand
  for every seat state and part (so the two callers are compared like for like, with no timing at all), and a pin that a
  timer callback finding time left in the hold is not due. Counts are runs of one test binary,
  `-test.run '^(TestSpentPostReadyBound|TestSpentColdLoad|TestSpentHoldBound|TestLoadingAfterASpent|TestStallTimerCallback|TestPostReadyHoldReads|TestPostReadyBoundExpiring)'`,
  built with `go test -c -o agent.test ./internal/agent/` and run in `internal/agent`.
  - Red, on the code before the decision change, `-test.count=20 -test.cpu 1,2,4` (60 runs): the loading-hold test 60/60
    failed (`a post-ready request whose straddling seat read said loading was stalled with 29s of the cold-load ceiling
    unspent: stalled: no byte for 1s after the seat read ready ... the seat: starting`), the stall-at-the-ceiling test
    60/60 (`stalled in phase cold-load (post-ready=true) at 1s`), the wording test 60/60 (`the last seat read said
    stopping, but the reason says the seat read ready`), and the table's two loading rows 60/60 each (`tick: got
    {stalled:true phase:cold-load postReady:true}, want {stalled:false phase:cold-load postReady:false}`). The unreadable-seat
    and cold-load-ceiling pins passed on that code: they pin behaviour the fix keeps.
  - Red for the stale callback, on the code before the guard, same command: 60/60 on each of its two rows (a callback
    that found 50 minutes of loading ceiling left stalled the request; one that found post-ready time left read the
    engine).
  - Green, final code, unloaded, `-test.count=50 -test.cpu 1,2,4` (150 runs of each test): 0 of 150 failed, for each of the nine tests (the two engine-race tests included).
  - Green, final code, under CPU load: the test binary and 6 busy-loop processes pinned to one core of a 36-thread host,
    `-test.count=150 -test.cpu 1` (150 runs of each test): 0 of 150 failed, for each of the nine tests. (The unloaded and the loaded run went at the same time.)
  - Mutants, each applied by an exact-snippet edit that fails unless it matches once, the sources compared byte for byte
    with the committed ones afterwards:
    - The new code reverted, the first fix kept (the parent's `liveness.go`): unloaded `-test.count=20 -test.cpu 1,2,4`
      (60 runs) the loading-hold, stall-at-the-ceiling, wording, table and stale-callback tests failed 60/60 each, and
      the two engine-race tests and the two pins passed 60/60; loaded `-test.count=40 -test.cpu 1` (40 runs) the same five
      failed 40/40.
    - Only the stale-callback guard removed: the stale-callback test failed 60/60 unloaded and 40/40 loaded, and the
      loading-hold test 4/60 unloaded and 5/40 loaded, the stall-at-the-ceiling test 4/60 and 4/40 (the failure above).
    - The tick made to stall on an unreadable seat read (`-test.count=10 -test.cpu 1,2,4`, 30 runs): the unreadable-seat
      pin and the table's unreadable row failed 30/30, everything else passed.
    - The decision's clause for a spent cold-load ceiling removed (same command): the table's `cold-load ceiling spent,
      seat loading` row failed 30/30, everything else passed. Widening `engineCheckablePhaseLocked` to every cold-load
      part alone failed 0/12 (`-test.count=4`): the decision's clause already shields it, so that mutant is
      equivalent; with both applied the table's three cold-load-ceiling rows and the cold-load-ceiling pin failed 12/12.
  - One false red seen once in the unloaded runs before the guard (`the test never made a cold-load seat read slow; it
    proves nothing`, at 0.46 s) was not reproduced in the 300 runs after it; the message now names the phase and the
    cause of a run that never reached the race.
- **Not fixed here (cosmetic).** The stall note prints its bounds with `%.0f` of seconds, so a sub-second bound reads
  `0s` (`allowed 0s`, `max(0s, 2 x 0s ...)`). Production bounds are minutes, not seconds: the post-ready bound is
  `max(120 s, 2 x the waiting phase's allowance)` (the floor `postReadyFloor` is 120 s, and no allowance is under the
  60 s liveness floor), so it is never under 120 s, and the cold-load ceiling is `max(600 s, 2 x the seat's measured cold
  load)`, clamped to the run's ceiling. Only the compressed bounds of a test show the `0s`.
- **Not verified here.** The race was exercised with scripted seat and engine probes only; no live seat or lease was
  used, and the timer-before-tick interleaving is pinned by calling the callback by hand, not reproduced on real
  hardware.
