### Fixed - a held lease no longer reads as absent, or as stale, while its record or its heartbeat is rewritten: the lease files are read through a handle that shares delete (the media-route lease test flake, and a double-booking found behind it)

- **The flake.** `TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere` failed on a loaded Windows host with
  `mediaadmit_lease_routes_test.go:210: want one live lease while the render runs, got []`, in a full `./internal/pipeline` run (659 s) and once more
  on another loaded host, and passed 8 of 8 alone. The test polls until `Leases()` shows one lease, then `theOneLeaseWindow` reads it again; the
  second read saw none.
- **Production defect, not a timing assumption in the test.**
  - A lease record (`e/<epoch>.json`, or `meta.json` for a whole-node lease) is replaced in place while the lease is held: the grant writes it
    `granting` and then `active`, and every `Restamp` rewrites it, each by a rename over the live name (`writeEpochRecord`, `renameReplacing`).
  - While that rename runs the system holds a handle on the old entry with `DELETE` access. Windows checks sharing both ways, so an open whose own share
    mode lacks `FILE_SHARE_DELETE` fails with `ERROR_SHARING_VIOLATION` (rarely `ERROR_ACCESS_DENIED`). `os.ReadFile` opens that way.
  - `readRecordFile` returned the failure, `reader.v2` skipped the record (`readEpochRecord` error, `continue`), and `Leases()` was empty for a lease
    that was held. The first poll can land on the `granting` record, which is live inside its grace window; the second read overlaps the rename to
    `active`. The package fixed the writer's half of this race (`renameReplacing` retries) and treats an unreadable record as held in the acquirer's
    sweep, but the reader's half was open.
  - It was not only a test artifact. `EpochIsCurrent`, the fence a render child asks, read the same record and answered "not current" for a lease
    that was held; `Inspect`, `InspectFor` and `InspectDir` drop a live lease the same way.
  - The failure is early, not a render that had already ended: both loaded failures were 0.05 s and 0.08 s into the subtest, far inside the 2 s timeout.
- **The fix.** `readRecordFile` (`internal/gpulease/hostram.go`) opens the record with the existing `OpenShared`, which shares read, write and delete.
  The reader's open no longer collides with the rename, and it reads the old record whole until the new one is in place. Off Windows it is
  `os.Open`, as before. The lease system page (`docs/systems/gpu-lease.md`) now states the reader's half of the Windows rename race beside the writer's.
- **A second timing assumption, in the test helper.** With the reader fixed, 1 of 36 loaded runs still failed, on a different line:
  `the lease window is 1.046s; the call's own timeout is 2s`. `theOneLeaseWindow` (`mediaadmit_lease_window_test.go`) added `time.Until(ExpiresAt)` to
  `Info.Age`, two samples of the clock taken at different moments, so a stall of the test between them read as a shorter window (a 900 ms stall reads
  1.099 s against 2 s). It now takes `ExpiresAtMs - AcquiredAtMs` from the record, the window the acquirer stamped. Its single-lease check and message are
  unchanged, so the test still fails on a lease that reads as absent. `mediaadmit_lease_routes_test.go` is unchanged.
- **What was tried first and dropped.** A bounded retry of the failed plain read. Under saturating load the sharing violation lasted a median 11 to 22 ms,
  a p90 of 85 to 168 ms and up to 1.4 s, and 14 to 22 percent of the failures outlasted an 80 ms bound, so a bound short enough for a status call still
  left misses. The open is the cause, so that retry and its test were removed. A retry is back below, and it is not that one: that retry bounded a
  sharing violation that happened on every rename and lasted up to 1.4 s, and the shared open removes it. This one bounds a delete-pending instant of a few
  milliseconds that no share mode avoids, and it retries a permission error only.
- **The heartbeat had the same defect, and it is a double-booking, not a missed status read.** A review of the fix found it.
  - `hb.<epoch>` is rewritten by `Renew` with the same rename over the live name. `heartbeatAt` (the reader behind `Leases()`, `Inspect` and the acquirer's
    sweep), `lastHeartbeat` and `keeperSignOfLife` read it with `os.ReadFile`. A read that lost to the rename fell back to the stamp written at acquisition.
  - The reclaim rule needs a stale heartbeat AND an expired window, so once a lease is past its declared window the heartbeat is the only thing that keeps
    it. Past the window the acquisition stamp is stale, so the fallback made a live lease reclaimable, the sweep of the next acquirer removed it, and the
    card was granted to a second job while the first still ran on it.
  - Leases past their window are ordinary: a `gpu reserve` command that runs longer than its `--for`, a detached hold past its window. A waiter polls
    `TryAcquire` once a second. A render the pipeline starts leases for its own timeout and is killed there (the route test above checks it), so it is not
    past its window while it lives.
  - `readSharedFile` (`internal/gpulease/hostram.go`) is the shared open plus read; `readRecordFile` calls it after its fault seam, and the three heartbeat
    readers call it directly, without that seam (a record that cannot be read is when the heartbeat is the evidence of life, `keeper.go`).
- **A shared open still meets one refusal: delete-pending.** For the instant a rename has marked the old entry for deletion and the new one has not taken
  its name, an open of the name fails with `ERROR_ACCESS_DENIED`, however it shares. This is the stray miss the first round could not explain (3 allowed per
  run in `readrace_test.go`). `readSharedFile` retries a permission error `removeAttempts` times at `removePause` (the claim removal's bound, about 100 ms)
  and returns every other failure at once; a file that stays unreadable costs one bounded wait and then the error as before.
  - A behaviour change to note: `readRecordFile` now inherits that wait for a lease record that fails with a permission error, on every platform, Linux
    included (where `OpenShared` is `os.Open`). It is bounded to about 100 ms per read and applies only to a permission error; the fault seam
    (`UseReadFault`) sits in front of the wait, so the tests that simulate an unreadable record are not slowed.
  - `lastHeartbeat` has no caller outside a test; it reads through `readSharedFile` too, so the three readers of `hb.<epoch>` stay alike.
- **Proved** (a 36-thread Windows host; 30 busy-loop node processes started for each run and killed by pid afterwards; scratch paths shown as `<scratch>`).
  - Red under load, the test unfixed: `go test -c -o <scratch>/pipeline.test.exe ./internal/pipeline/`, then 4 concurrent copies of
    `<scratch>/pipeline.test.exe -test.run '^TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere$' -test.count=2 -test.cpu 1,2,4 -test.v`
    under the burners (`node -e 'let x=0;const e=Date.now()+1500000;while(Date.now()<e){x+=Math.sqrt(x+1)}'`, 30 of them): 24 executions, 2 failed
    (`animate_character` after 0.08 s, `upscale_image` after 0.05 s), both with the `got []` line; 22 passed.
  - Reader fixed, helper not yet: the same command with `-test.count=3` (36 executions): 35 passed, 1 failed with the window line above, 0 with `got []`.
    A `go build` and `go vet` of the repository ran during that run, on top of the burners.
  - Green, both fixes in: the same command with `-test.count=3`, nothing else running: 36 executions, 36 passed.
  - Red, deterministic at the route (a scratch test, not committed): after the poll sees the lease, a handle with `DELETE` access (sharing read and
    write) is held on the lease record and the real `theOneLeaseWindow` runs. Unfixed reader: 5 of 5 failed with the `got []` line. Fixed: 5 of 5 passed,
    window 2 s.
  - Red, the reader stress (`TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten`: a held lease, `Restamp` called in a loop, readers polling
    `Leases()` and `EpochIsCurrent`). First form, 300 rewrites and four readers without a pause, `go test -count=3 -run
    'TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten' ./internal/gpulease/` on the unfixed reader: `Leases()` missed 196 of 9,681, 166 of 10,511 and
    320 of 10,079 reads, and `EpochIsCurrent` 867 of 13,385, 736 of 13,977 and 1,062 of 14,574.
  - Red, deterministic (`TestARecordReadSucceedsWhileAnotherHandleHoldsDeleteAccessOnIt`, Windows only): a `DELETE`-access handle is held on a record;
    the control asserts a plain `os.ReadFile` fails with `ERROR_SHARING_VIOLATION`, and the lease read must succeed.
  - Mutant (only the fix reverted, `readRecordFile` calling `os.Open` again): the deterministic test failed 2 of 2 with the sharing violation, and the
    final stress test (150 rewrites, two readers of each kind pausing 100 microseconds) failed 5 of 5: `Leases()` missed 24 to 51 reads of 2,067 to
    3,255 and `EpochIsCurrent` 70 to 95 of 2,266 to 3,471.
  - Green, the final stress test and the deterministic one: `go test -count=10 -run 'TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten|TestARecordReadSucceedsWhileAnotherHandleHoldsDeleteAccessOnIt' ./internal/gpulease/`,
    10 of 10 each; the stress test alone under the 30 burners, `<scratch>/gpulease.test.exe -test.run '^TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten$' -test.count=30`,
    30 of 30.
  - The same rewrite stress with plain `os.ReadFile` against `OpenShared`: 2,714 of 239,868 reads failed, 0 of 491,323.
  - **The heartbeat, red** (`heartbeatread_windows_test.go`, Windows only). A lease with a 50 ms window, the clock moved 10 minutes on, `Renew` called, and a
    `DELETE`-access handle held on `hb.<epoch>` (the control asserts `os.ReadFile` of it fails with `ERROR_SHARING_VIOLATION`). Unfixed, 3 of 3 runs, both tests
    red: `heartbeatAt` and `lastHeartbeat` returned the acquisition stamp, `keeperSignOfLife` the zero time, `Leases() = [] while the holder renews, want [1]`,
    `the card of a live lease was granted to a second job (epoch 2)`, and the holder's next `Renew` returned `gpulease: lease is gone (epoch 1)`.
  - **The heartbeat, green and mutants.** Both tests pass 3 of 3 with the fix, the second one also checking that the holder's next `Renew` and `Check` succeed
    once the handle is closed. One mutant per reader, each reverted to `os.ReadFile` alone: `heartbeatAt` red on its own line and on the second job being
    granted; `lastHeartbeat` red on its line; `keeperSignOfLife` red on its line. All three readers back on `os.Open`: both tests and the record test red.
  - **The delete-pending refusal, measured** (a scratch test, not committed: a held lease past its window, four goroutines reading `hb.<epoch>` through
    `readSharedFile` in a tight loop while `Renew` rewrote it in a loop for 40 s). Without the wait, on the 36-thread host, 30 busy-loop processes running, 4
    concurrent copies: 5 `ERROR_ACCESS_DENIED` in 124,206 reads and 664 renewals, at least one in each of the 4 copies; no sharing violation. Quiet host, 3
    copies: 0 in 718,272 reads. With the wait, 4 copies under the same load: 0 in 108,680 reads and 477 renewals.
  - **The wait, red, green and mutants** (`readretry_test.go`, portable, with the `openSharedFile` seam refusing the open as the system does; and
    `readretry_windows_test.go`, the control: a delete-pending file fails `OpenShared` with `ERROR_ACCESS_DENIED`, a permission error). Unfixed, 3 failing
    tests: `heartbeatAt after three access-denied opens = "1000", want the file's content 424242`, `readSharedFile ... permission denied`, `made 1 opens, want 21`;
    the two guards and the control pass. Fixed: all pass, 3 of 3. Mutants: retrying every error fails the guard (`a missing file was opened 21 times, want 1`);
    no retry fails as unfixed; one retry short fails the bound (`made 20 opens, want 21`).
  - **The stress test now allows no miss** (`strayMisses` is gone). Fixed, 150 runs of the stress test under the 30 burners (the first about 280 s of them
    loaded, the rest quieter as the burners expired): 0 misses of `Leases()` in 282,230 reads and 0 of `EpochIsCurrent` in 480,285, 150 of 150 passed. With the
    open reverted to `os.Open`, 3 of 3 runs red: `Leases()` missed 23, 28 and 31 reads and `EpochIsCurrent` 62, 55 and 53.
- **Not verified here.**
  - No GPU job, lease on a real card or seat was used: every lease in these runs is in a scratch lease root, with the stub runner.
  - The double-booking was shown with a held `DELETE`-access handle in a scratch lease root, which models the instant of the rename, and by a tight `Renew`
    loop in a review. It was not seen with a real `gpu reserve` command that outran its window, with a waiter queued, on a real card. The chance in
    production per renewal (every 15 s) with a waiter polling once a second was estimated, not measured.
  - The delete-pending wait is sized by the claim removal's bound (about 100 ms), not by a measured duration of the instant. Zero misses in the runs above
    says the bound covers what was seen; a stall of the renaming process longer than that, under a heavier load, would still lose a read, and the stress
    test, which now allows no miss, would fail on it rather than hide it.
  - An unreadable heartbeat still falls back to the acquisition stamp (after the wait). That is the safe answer for a heartbeat that is absent, and the unsafe
    one for a heartbeat that exists and cannot be read for longer than the wait; the acquirer's sweep also asks `keeperAlive` for a record it cannot read, but
    not for a heartbeat it cannot read. Not changed here.
  - Linux: the reader there is `os.Open` as before (`GOOS=linux go vet ./internal/gpulease/ ./internal/pipeline/`); the race is Windows-only and no Linux
    host ran the stress.
  - The epoch counter, the tokens and the waiters are still read with `os.ReadFile`; no finding covers them.
  - `windowAndKill` still reads the lease twice. A lease that ended between the two reads (the 2 s render killed and released) would also show
    `got []`; it needs a stall of seconds between two adjacent statements and was not seen.
