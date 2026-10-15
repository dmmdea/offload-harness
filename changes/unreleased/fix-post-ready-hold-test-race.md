### Fixed - a post-ready bound that runs out during the seat probe's read reads the engine before it stalls

- **A production race, not a test timing assumption.** A request held in the post-ready part of the cold-load hold (the
  seat loaded for it and read ready, no byte yet) is judged at the bound by two callers: the stall timer, which reads the
  engine and enters the busy hold when the engine works for the request's siblings, and the seat probe's tick, which
  polls `/running`. While a tick's read is in flight the timer sees the probe busy and leaves the decision to the tick
  (`onStall`), and the tick, finding the bound spent, filed the stall without ever reading the engine
  (`onProbeTick`). A read that straddles the bound, or a tick that lands first, so stalled a request whose seat was
  loaded and busy with its siblings. The tick now hands a spent post-ready bound on a ready (or unreadable, but not
  loading) seat to the same engine check the timer makes. A load still in progress, a hold with no engine probe and the
  cold-load ceiling itself file exactly as before.
- **Found as a CI flake.** `TestPostReadyHoldReadsTheEngineBeforeStalling` failed once on a hosted Linux runner with
  `a post-ready request whose engine works for its siblings was stalled: stalled: no byte for 0s after the seat read
  ready, in cold-load (... post-ready bound = max(0s, 2 x 0s prefill allowance) ...)`. In that test the probe tick falls
  on the bound's own instant (a 10 ms poll, a 40 ms prefill allowance and a 100 ms bound), so who decides is a coin toss.
- **Evidence (race rigor).** Counts are runs of the test binary, 3 `-cpu` settings each.
  - Red, original test on the parent: `go test -c -o agent.test ./internal/agent/`, then in `internal/agent`
    `agent.test -test.run '^TestPostReadyHoldReadsTheEngineBeforeStalling$' -test.count=150 -test.cpu 1,2,4`:
    3/450 failed, with the CI line above.
  - Red, deterministic: `TestPostReadyBoundExpiringDuringASeatReadReadsTheEngine` (the first seat read made in the
    post-ready part takes 250 ms, so the tick always lands after the 100 ms bound), committed before the fix, with
    `-test.run '^TestPostReadyBoundExpiringDuringASeatReadReadsTheEngine$' -test.count=20 -test.cpu 1,2,4`: 60/60 failed
    with the same reason.
  - Green, with the fix: the deterministic test 0/60 (same command); the original test
    `-test.count=400 -test.cpu 1,2,4`: 0/1200.
  - Mutant, fix reverted and both tests kept: the deterministic test 60/60 failed, and the original test
    `-test.count=400 -test.cpu 1,2,4` failed 26/1200.
- **Not fixed here (cosmetic).** The stall note prints its bounds with `%.0f` of seconds, so a sub-second bound reads
  `0s` (`allowed 0s`, `max(0s, 2 x 0s ...)`). Production bounds are tens of seconds, so no operator sees it today.
- **Not verified here.** The race was exercised with scripted seat and engine probes only; no live seat or lease was used.
