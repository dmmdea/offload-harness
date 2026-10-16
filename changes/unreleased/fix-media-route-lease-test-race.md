### Fixed - a held lease no longer reads as absent, or as stale, while its record or its heartbeat is rewritten: the Go readers share delete and wait out the delete-pending refusal, and the Node readers wait it out too (the media-route lease test flake, a double-booking found behind it, and the Node fence)

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
  run in `readrace_test.go`). On Windows `readSharedFile` retries a permission error `removeAttempts` times at `removePause` (the claim removal's bound,
  about 100 ms) and returns every other failure at once; a file that stays unreadable costs one bounded wait and then the error as before.
  - **Windows only.** The first form of the wait ran on every platform, and a review measured what that cost where the instant does not exist. Off Windows
    `OpenShared` is `os.Open`, POSIX has no delete-pending state (`rename(2)` is atomic and an open descriptor never holds one up), so a permission error
    there is a lasting refusal (a record written with a mode the reader cannot read) and the wait only delayed reporting it. Measured in a Linux
    environment (WSL2, an unprivileged user, a memory-backed temp dir, mode-000 lease records, scratch lease roots), before the gate: one read of such a
    record 106 ms, against microseconds for the plain read it replaced; `Leases()` 107, 214, 424 and 851 ms with 1, 2, 4 and 8 unreadable records; a lone
    `TryAcquire` on a free card, which reads the records under the epoch lock, 537 ms, 1.08 s, 2.14 s and 4.22 s; and with 8 records two acquirers at
    once, one granted after 4.23 s and the other refused after 2.98 s with `gpulease: the epoch lock at ... did not clear in 2s`. Nothing was
    double-booked: it was latency and a lock timeout, never wrong data, and it needs a record the reader cannot read, which a hardened service writing
    with a restrictive umask for a different reader would produce.
  - The gate is a constant beside `OpenShared`, `sharedOpenMeetsDeletePending` (`true` in `openshared_windows.go`, `false` in `openshared_other.go`), read
    through a variable (`deniedOpenMayClear`) so the wait itself is still tested on every platform. `readRecordFile` inherits the choice: off Windows a
    permission error is one open and the error, as at the start of this branch. The fault seam (`UseReadFault`) sits in front of the read either way.
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
  - **The gate, red, green and mutants** (`readretry_other_test.go`, off Windows; `readretry_windows_test.go`; the test binary built with
    `GOOS=linux go test -c -o <scratch>/gpulease.test ./internal/gpulease/` and run in the Linux environment above as an unprivileged user, the
    Windows tests on the Windows host). Red, before the gate: `TestOffWindowsAPermissionErrorIsReturnedAtOnce`, 5 runs of 4 subtests, 20 of 20 failed,
    `made 21 opens, want 1 (took 104ms)` for both readers, with the refusal supplied by the open seam and with a real mode-000 file. Green, after: the same
    5 runs, 20 of 20 subtests passed, with the three portable wait tests (forced on, 15 of 15) and the Windows default test (3 of 3 on the Windows
    host). The same measurement after the gate, same method: `Leases()` 12, 11, 15 and 106 us with 1, 2, 4
    and 8 unreadable records; a lone acquire 191, 196, 362 and 384 us; two acquirers with 8 records both granted, 5.9 ms and 0.38 ms. Mutants (applied
    through a build overlay, the tree untouched): the gate removed from `readSharedFile`, 20 of 20 subtests red with `made 21 opens, want 1`, the
    three portable wait tests still green; the constant set to `false` in the Windows build, `TestWindowsWaitsOutADeniedOpenByDefault` red 3 of 3
    (`deniedOpenMayClear is false in a Windows build`), the others green.
- **The Node readers had the same defect** (`render/lease-read.mjs`, new).
  - `readLease`, `readEpochRecord` and `readCardClaim` (`render/gpu-lock.mjs`) and the lease-progress reader (`render/lease-progress.mjs`) read the files Go
    replaces by rename-over with `readFileSync` and turned every error into `null`. `checkInheritedLease`, the fence before an irreversible action, uses the
    first three. A `null` record for a card-scoped lease falls through to `meta.json`, which a card lease never writes, so a refused open of the rename's instant
    read as "this lease is not current" and the render aborted.
  - libuv opens with `FILE_SHARE_DELETE`, so the sharing violation of the Go reader cannot happen to Node; the delete-pending instant can. A scratch Go helper made a
    file delete-pending (a handle with delete access, the disposition set) and held it: `readFileSync` threw `EPERM` (errno -4048, syscall `open`) in 0.2 ms, and once the
    last handle closed, `ENOENT`. So the codes to wait out are `EPERM` and its POSIX twin `EACCES`.
  - `readSharedSync` / `readSharedJson` retry those two and nothing else, until a 100 ms deadline (pausing 5 ms with `Atomics.wait`, which `atomic-out.mjs` and
    `igpu-engine.mjs` already use on the main thread), and return `null` at once for a missing file or any other error, so a released lease still reads absent with no
    wait. The wait runs only on Windows (`policy.waitsOutDenied`), for the reason the Go side gave up the wait elsewhere. The three Node readers, `checkInheritedLease` and
    `leaseProgressFile` all go through it.
  - **The bound is a deadline, not 20 pauses.** The first form copied the Go side's count (20 x 5 ms). `Atomics.wait(5)` sleeps a whole timer tick on Windows (15.4 ms
    for 1, 2, 5 and 10 ms alike, measured), so that was 308 ms. Red: with a fake clock whose pause is 15.4 ms the helper took `20 pauses of 15.4 ms, want 7`; in real time
    `gave up after 299 ms (20 pauses), want about 100 ms and not the 308 ms that 20 timer ticks cost`. Fixed, against a file held delete-pending for 1.5 s the helper gave up at
    102 to 111 ms (3 runs); against one held for 40 ms it returned `null` (the file was then gone) at 43 to 67 ms.
  - **Node miss counts, before and after.** A writer in another process renamed files over `e/7.json`, `meta.json` and `cards/<card>.claim` of 40 scratch lease roots in
    rotation: a Go process with the write pattern of `renameReplacing` (write beside, rename over, retry a refused rename), and a Node process with the same; in half the runs
    a hard link of a seed file instead of a fresh file, which is the same rename at tens of times the rate (a fresh file costs tens of milliseconds on this host). 4 or 8
    reader processes called the real `readEpochRecord`, `readLease`, `readCardClaim`, `checkInheritedLease` and `leaseProgressFile` in a tight loop, with a raw
    `readFileSync` of the same paths beside them to record error codes. 40 s a run; four configurations a matrix (quiet; 30 busy-loop processes with a Go writer and 4 readers,
    with a Node writer and 4, with a Go writer and 8); a 36-thread Windows host that other work was also loading.
    - Plain `readFileSync` (before): 8 runs, 1,048,185 file reads against 55,218 renames (1,651 of the writer's renames met an open reader and were retried): **0 misses** of
      any of the five readers, and not one error code of any kind on the raw reads.
    - Through `lease-read.mjs` (after): 8 runs, 499,320 file reads against 20,726 renames (512 retried): **0 misses**, 0 error codes.
    - So Node misses nothing in this setup, loaded or not. A Node open costs about 0.65 ms here, so each reader holds its file long enough that a rename mostly waits for it (the
      retried renames), and the delete-pending instant is microseconds wide. The helper is a guard against an instant this setup could not provoke; it is shown by the injected
      refusal and by the held delete-pending file above, not by a natural miss.
  - **Tests** (`render/lease-read.test.mjs`, 17; `render/lease-read-stress.test.mjs`, 1).
    - Red, before any wait (the helper was the plain read): 9 of the 17 failed on their assertions, among them `refused opens are waited out` (`+ null  - '{"epoch":7}'`),
      `a refusal that never ends ...` (`1 reads, want the first and 20 retries`), and, through the real readers, `the fence holds a held card lease current through refused reads
      of its record and of its claim` (`a null record for a card lease falls through to meta.json ... actual: false, expected: true`). Green and meant to stay so: a missing file is read
      once with no pause, any other error code is not retried, a permission error is returned at once off Windows, a released lease reads absent through every reader with each file
      opened once, and the stress. The stress test cannot be red on the old readers (above); it is a guard.
    - The stress test is sized by work, not by the clock: the writer makes 600 renames and stops once the reader has made 150 loops. A first form that ran the writer for 3 s failed on a
      loaded host (`the writer made 1 renames in 3000 ms`); the same work took 1.4 s, 8.4 s and 26 s in the same hour.
    - Mutants of the helper, one at a time, each caught: every error retried (3 tests red, among them `a file that is not there is read once ...`); one pause short of the bound (1);
      the platform gate ignored (`off Windows a permission refusal is returned at once`); `EACCES` not retried (1); no pause between reads (4; a first run of this mutant hung the
      test, so the scripted read now ends a runaway loop and fails). `node --test` on `lease-read.test.mjs`, `lease-read-stress.test.mjs`, `gpu-lock.test.mjs`,
      `lease-progress.test.mjs` and `no-visible-windows.test.mjs`: 97 tests, all pass.
- **The release lane's checks, on the Go side.** The wait of `readSharedFile` is for a file that is there and passes in milliseconds. It must not be paid for a file that is not there,
  and no read under the epoch lock may multiply it. These are guards: the code was already in place, so none could be red first; each was shown red against a mutant. They run with the
  wait forced on (`waitOutDeniedOpens`), so a read that waited on a missing file shows on every platform.
  - `TestAReleasedLeaseReadsAbsentAtOnceAndNoReadWaitsForIt`: after `Release`, `Leases()` opens `meta.json` once, `EpochIsCurrent` opens `e/<epoch>.json` and `meta.json` once each,
    `Inspect()` `meta.json` once, `heartbeatAt`, `lastHeartbeat` and `keeperSignOfLife` `hb.<epoch>` once each; every one answers absent with 0 pauses. 0 to 0.6 ms each on Windows, 5 to 35 us on Linux.
  - `TestAMissingOrDeletedRecordCostsOneOpenAndNoWaitUnderTheEpochLock`: three leases held, a fourth card asked for, and the first open of each record under the epoch lock deletes it (the
    holder releasing between the sweep's listing and its read). Each deleted record is opened once under the lock; 17 opens in all, 7 under the lock, 0 pauses; 3.6 to 5.0 ms on Windows, 0.5 ms on Linux.
    The grant asks for the absent `meta.json` five times and each ask is one open.
  - `TestReleasingUnderTheEpochLockReadsNoRecordMoreThanOnce`: three releases and a `Leases()` over the rest, 0 pauses, no missing file opened more than the asks name (the listing and the judge both
    ask for the one live lease's heartbeat: two opens, each answered at once).
  - Mutant (`readSharedFile` also retrying a missing file, through a build overlay): all three red. `Leases() opened ...meta.json 21 times, want at most 1`, `Leases() waited 20 times for a file that is
    not there, want 0`, `the deleted record 1.json was opened 21 times under the lock, want exactly 1`; the six asks about the released lease took 760 ms and the grant 1.55 s with 280 pauses.
  - A path that does pay under the lock is a file that exists and is refused (a delete-pending heartbeat of a live lease under a sweep): one bounded wait per such read. It is the wait that keeps the sweep
    from calling a live lease stale, so it stays under the lock. Nothing that is missing or deleted pays it, which was the cost to rule out: it would have grown by about 100 ms per released lease.
    A released record held open by a foreign handle that shares delete was measured too: the name was gone at once on this Windows build, so `Leases()` answered absent in 0.5 ms with 0 pauses and
    the grant of another card took 4.9 ms (a scratch test, not committed). `gpu status` goes through `Leases()` and `InspectDir`, which these tests cover in the package.
- **The Linux pin.** `TestOnLinuxConcurrentRenameOverOfARecordAndItsHeartbeatIsNeverMissedAndNeverWaitedFor` (`readrace_linux_test.go`, Linux only): `Restamp` rewrites the held lease's record and
  `Renew` its heartbeat, 3,000 each, concurrently, while two goroutines of each of `Leases()`, `EpochIsCurrent` and a heartbeat read poll. It asserts 0 misses, 0 refused opens and
  `readRetries` (a counter of the pauses `readSharedFile` takes, added as the seam) at 0. The test binary was built with `GOOS=linux go test -c` and run in a Linux environment under WSL2 as an
  unprivileged user, with a memory-backed temp dir (a drvfs rename is not an ext4 rename): 50 of 50 passed, 118,283 `Leases()` reads, 138,177 `EpochIsCurrent` reads and 196,607 heartbeat reads,
  1,246,093 opens, 0 misses, 0 refused, 0 pauses. It has no red on the code as it is, because `rename(2)` is atomic; mutants: the heartbeat rewritten in place (truncate, then write) failed 3 of 3 with
  `the heartbeat could not be read on 265 of 3290 reads` (and 300 of 3330, 277 of 3255); a pause counted on every read failed with `readSharedFile paused 23845 times on Linux, want 0`.
- **Not verified here.**
  - Node: the retry was never exercised by a natural refusal. Plain `readFileSync` missed nothing in about a million reads, so that Node can meet `EPERM` from a rename rests on the delete-pending probe and not
    on a miss, and the wait is shown by the injected refusal and a file held delete-pending, not by a natural rename-over. The 15.4 ms timer tick is one host's; a host where another process has raised the
    timer resolution ticks faster, and the deadline holds either way.
  - A copy of `gpu-lock.mjs` outside this tree (a media repository's own, which `gpu doctor` audits) now needs `lease-read.mjs` beside it, as it already needs `lease-progress.mjs` and
    `comfy-lifecycle.mjs`; a copy that has the format signature but not the new module fails at import. Not checked against any such copy.
  - Go: the claim files (`cards/<card>.claim`) are still read with `os.ReadFile`, as the epoch counter, the tokens and the waiters are; the Node side reads claims through the helper.
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
  - Linux: the reader there is `os.Open`, and a permission error on it is returned at once (the gate above). The gate and the pin were shown in a cross-compiled test
    binary of `internal/gpulease` run in a Linux environment under WSL2, as an unprivileged user with a memory-backed temp dir, with the tests named in this fragment and a
    measurement script, not with the whole package or `internal/pipeline`, which were compiled and vetted for Linux (`GOOS=linux go vet ./internal/gpulease/
    ./internal/pipeline/`), not run there. The race itself is Windows-only. No lease on a real card and no live lease directory was used.
  - The claim file has the same two retries with no platform gate: `createClaim` retries a permission error and `removeClaim` retries any error,
    `removeAttempts` times at `removePause`. They are older than this branch, concern the claim file and not a lease record, no finding covers them, and
    they were left as they were.
  - The epoch counter, the tokens and the waiters are still read with `os.ReadFile`; no finding covers them.
  - `windowAndKill` still reads the lease twice. A lease that ended between the two reads (the 2 s render killed and released) would also show
    `got []`; it needs a stall of seconds between two adjacent statements and was not seen.
