### Fixed - `go test ./internal/pipeline/` no longer sits out the fake seats' simulated hangs

- **The gap.** The package ran close to Go's default 10-minute package timeout (CI runs `go test ./...` with no
  `-timeout`; a contended Windows run was killed at 10 minutes). Production timers were already compressed per test, but the
  fake servers that model "the seat accepts the request and never answers" slept a fixed 3 to 10 seconds after the
  pipeline under test had already given up, and `httptest.Server.Close` waits for every handler, so each such test paid the
  whole simulated hang.
- **What changed.** Test code only. The scripted llama fake gains `loopCtx` and `upstreamModelsCtx`, which are handed the
  request's context, and the fixed sleeps that only simulate a hang (cold-load, silent-seat, busy-hold, ceiling, cancel,
  admission and breaker tests) now wait with `holdUntilHungUp`, which returns the moment the client disconnects. The fake's
  `repackDelay`, `chatFallbackDelay` and `propsDelay` hold the same way. Sleeps that model a real duration an assertion
  depends on are unchanged, and no production timer or timeout moved.
- **The 429 case.** `TestRunAgentTaskRepackWireFailuresAreInfrastructure` took 90 s in its rate-limiter case, because a 429
  retries on the 90 s default contention budget. The test now sets `SeatContentionWaitSec` to 1; the class it asserts does not
  depend on how long the retries ran. The default is unchanged.
- **Also.** The fake's grammar lane answers 500 when a test never scripted `repack`, instead of panicking inside the
  handler (seen in `TestRescueRepackRefusesAHollowObjectFromTheNode`).
- **Docs.** No `docs/` page describes the pipeline test fakes, so none changed.
