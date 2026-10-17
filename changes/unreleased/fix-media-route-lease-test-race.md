### Fixed - a held lease no longer reads as absent, or as stale, while its record or its heartbeat is rewritten: the Go readers share delete and wait out the delete-pending refusal, and the Node readers wait it out too (the media-route lease test flake, a double-booking found behind it, and the Node fence); the reclaim rule and the whole-node grant's locking change with it

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
  about 100 ms) and returns every other failure at once; a file that stays unreadable is waited out once, not once per read (the last bullet of this group).
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
    for 1, 2, 5 and 10 ms alike, measured), so that was 308 ms. Red: with a fake clock whose pause is 15.4 ms the helper took `20 pauses of 15.4 ms, want 7`; in real time, while that test still had an upper bound,
    `gave up after 299 ms (20 pauses), want about 100 ms and not the 308 ms that 20 timer ticks cost` (the upper bound was dropped with the other wall-clock bounds, below; the
    fake-clock test is the proof of the 308 ms case now, and the real-time test keeps its lower bound, `gave up after N ms (N pauses), want the bound (about 100 ms)`). Fixed, against a file held delete-pending for 1.5 s the helper gave up at
    102 to 111 ms (3 runs); against one held for 40 ms it returned `null` (the file was then gone) at 43 to 67 ms.
  - **Node miss counts, before and after.** A writer in another process renamed files over `e/7.json`, `meta.json` and `cards/<card>.claim` of 40 scratch lease roots in
    rotation: a Go process with the write pattern of `renameReplacing` (write beside, rename over, retry a refused rename), and a Node process with the same; in half the runs
    a hard link of a seed file instead of a fresh file, at tens of times the rate (a fresh file costs tens of milliseconds on this host; see the caveat after the counts). 4 or 8
    reader processes called the real `readEpochRecord`, `readLease`, `readCardClaim`, `checkInheritedLease` and `leaseProgressFile` in a tight loop, with a raw
    `readFileSync` of the same paths beside them to record error codes. 40 s a run; four configurations a matrix (quiet; 30 busy-loop processes with a Go writer and 4 readers,
    with a Node writer and 4, with a Go writer and 8); a 36-thread Windows host that other work was also loading.
    - Plain `readFileSync` (before): 8 runs, 1,048,185 file reads against 55,218 renames (1,651 of the writer's renames met an open reader and were retried): **0 misses** of
      any of the five readers, and no error code on the raw reads in those runs (a longer harness, under the read gate below, did meet the live name absent once in about 74,000 renames).
    - Through `lease-read.mjs` (after): 8 runs, 499,320 file reads against 20,726 renames (512 retried): **0 misses**, no error code on the raw reads.
    - So Node misses nothing in this setup, loaded or not. A Node open costs about 0.65 ms here, so each reader holds its file long enough that a rename mostly waits for it (the
      retried renames), and the delete-pending instant is microseconds wide. The helper is a guard against an instant this setup could not provoke; it is shown by the injected
      refusal and by the held delete-pending file above, not by a natural miss.
    - Caveat found in review. A hard link of ONE seed file is a different file from the one in place only on the first pass: afterwards the live name and the seed are one inode, and
      renaming a link over another name of its own inode replaces nothing. That scratch script is not kept and was not re-run, so for the hard-link half of these runs the renames
      counted above are renames issued and not replacements, and that half is weaker evidence than it reads. The fresh-file half replaced on every rename.
  - **Tests** (`render/lease-read.test.mjs`, 17; `render/lease-read-stress.test.mjs`, 1).
    - Red, before any wait (the helper was the plain read): 9 of the 17 failed on their assertions, among them `refused opens are waited out` (`+ null  - '{"epoch":7}'`),
      `a refusal that never ends ...` (`1 reads, want the first and 20 retries`), and, through the real readers, `the fence holds a held card lease current through refused reads
      of its record and of its claim` (`a null record for a card lease falls through to meta.json ... actual: false, expected: true`). Green and meant to stay so: a missing file is read
      once with no pause, any other error code is not retried, a permission error is returned at once off Windows, a released lease reads absent through every reader with each file
      opened once, and the stress. The stress test cannot be red on the old readers (above); it is a guard on the fixed ones, and it is only a guard if its renames really replace a
      file (the next bullets).
    - The stress test is sized by work, not by the clock: the writer makes 600 renames and stops once the reader has made 150 loops. A first form that ran the writer for 3 s failed on a
      loaded host (`the writer made 1 renames in 3000 ms`); the same work took 1.4 s, 8.4 s and 26 s in the same hour.
    - **The stress test's first form proved nothing on Windows and failed on Linux** (a review finding; it passed on Windows when it was written and was not run on Linux). The writer
      hard-linked ONE seed file to `<live>.tmp` and renamed that over the live name. After the first pass the live name and the seed were the same inode, and renaming a link over
      another name of its own inode replaces nothing. On Linux `rename(2)` then does nothing and leaves `<live>.tmp` where it was, the next `linkSync` threw `EEXIST` (at the link of the third pass, 36 renames in at six roots) outside the
      retry, the writer died before it wrote its `done` marker, and the reader spun to the 60 s cap. Red, run under WSL (Node 22) on the head before the fix, with the writer's own
      stderr `Error: EEXIST: file already exists, link '<root>/1/e/7.json.seed' -> '<root>/1/e/7.json.tmp'` and then `not ok 1 - the readers never answer null for a lease file that is
      there ...`, `duration_ms: 60266`, `error: 'Unexpected end of JSON input'`. CI runs `node --test render/*.test.mjs` on Linux, so that was a red build. On Windows the same
      loop passed, but a reviewer counted 9 real replacements in 4,761 renames (one for each of nine files; the six lease roots the test uses make it 18) and no refused rename: the others put a link onto its own inode, which Windows allows even over a name a
      reader holds open, so no reader ever met a refusal and "no miss" pinned nothing.
    - **Now every rename replaces, and the test proves it.** Each file has two seeds (`<live>.seed` and `<live>.seed2`: two inodes, one body) and the writer alternates them, so each
      rename installs a different inode than the one in place, as `writeEpochRecord` and `Renew` do with a fresh file. The writer reports `replaced` and the test requires `replaced == renames`: first as a prediction from the writer's own bookkeeping of the seed it had installed last, and since
      this round as an observation of the live name (below). The link is retried on a refusal like the rename, and a writer that throws still writes `done` and reports the error, so the test fails
      at once with the cause and not at the cap. It also counts the refused renames (`retries`) and requires some on Windows (the writer keeps going until the first, within the
      cap) and none elsewhere, where `rename(2)` is atomic and never refused: a run in which the writer and the readers never met now fails instead of passing. Go never renames
      over a card claim (it creates one once and deletes it); the claim is renamed over here all the same, which is harder on its reader than production is.
    - Measured, the file alone, three runs each. Windows: 612, 702 and 738 renames, every one a replacement; 6, 3 and 1 of them refused and retried; 0 link refusals; 208, 154 and 154
      loops of the five readers (six file reads a loop, about 900 to 1,250); 0 misses; 674, 662 and 685 ms. WSL (Linux, Node 22; the scratch root in the distro's own temp directory): 18,252,
      15,894 and 16,200 renames, every one a replacement; 0 refused; 154, 153 and 151 loops; 0 misses; 462, 461 and 465 ms. The refused count on Windows is small and varies (1 to 6 in
      600 to 740 renames on this host; the release lane counted 14 to 30 a run on its own), so the test leans on the writer going on to the first refusal; it has not seen a run that needed the cap.
    - Mutants of the test, one at a time, in a scratch copy of `render/` (the tree untouched), the cap cut to 6 s. The writer restored to one seed: red on Windows, `only 18 of 11052
      renames installed a different inode than the one in place` (3,730 loops, 0 refused); red under WSL in 281 ms and not at a cap, `the writer stopped on an error after 36 renames:
      EEXIST EEXIST: file already exists, link '<root>/1/e/7.json.seed' -> '<root>/1/e/7.json.tmp'`. The one seed with the `replaced` assertion removed too, on Windows: red on the
      next assertion, `no rename was refused in 10872 renames against 3622 reader loops (capped: true): the writer and the readers never met, so this run proves nothing`.
    - **The writer observes the replacement, and the test says what it pins** (the release lane's findings on the stress test, this round). `replaced` was predicted from the writer's own
      bookkeeping of the seed it had installed last, which a rename that installs nothing cannot falsify. The writer now takes the inode of the live name right after each rename and counts a
      replacement only when it is the seed just installed and not the inode it saw in place before. The name can be absent for tens of milliseconds after a rename that was not refused (the read
      gate's episode), so an ENOENT there is looked at again and counted (`absentAfter`), and only a name that stays absent for 5 s is an error. Mutant, the rename made a no-op (`unlinkSync(tmp)`
      in its place), run to the 60 s cap on Windows: the old assertion stayed green (the old test failed later, on the refused-rename count, and on Linux, where none is wanted, it would have
      passed), the new one is red with `only 0 of 82962 renames were observed to put a different inode than the one in place under the live name`. There is no separate red commit for it: it is a
      change to a test harness, and the mutant is the red.
    - Measured after that change, the file alone. Windows, three runs: 612, 612 and 630 renames, every one an observed replacement; 11, 3 and 4 of them refused and retried; 274, 185 and 153 loops
      (six file reads each, about 1,640, 1,110 and 920); 0 link refusals; 0 live names met absent after the writer's own rename; 0 misses; 634, 509 and 476 ms. WSL (Linux, Node 22; the file on a
      mounted drive, the scratch root in the distro's own temp directory), four runs, all passed; for three of them 10,944, 13,374 and 11,862 renames, every one observed, 0 refused, 153 to 155 loops,
      0 misses, and the fourth took 298 ms in the test. No natural absent-after-rename episode was met in any of them, so `absentAfter` stayed 0.
    - The rest of the lane's findings on it: a throw in the writer's setup (the stats of the seeds and of the live names) was outside the `try` and skipped the `done` marker, so the reader ran to the
      cap, and is inside now; the diagnostic counted 12 file reads a loop and the five readers make 6 (1 + 1 + 1 + 2 + 1); `LEASE_READ_STRESS_SCALE=<n>` multiplies the renames, the loops and the cap for
      a longer run on a gated host, and unset the committed size runs. The header names what the test pins (no natural miss under rename-over at its size) and what it does not (any of the waits:
      with the permission wait taken out it still passed 5 of 5 on Windows), and names the tests that are the proof: `lease-read.test.mjs`, `gpu-lock.test.mjs` and `lease-progress.test.mjs` for the
      permission wait (10 of 81 went red with it out), `lease-read-gate.test.mjs` and `gate_test.go` for the absent name, `heartbeat_unknown_test.go`, `keeper_unknown_test.go` and
      `tree_marker_unknown_test.go` for the reclaim rule. CI runs only the Linux half of the test (`node --test render/*.test.mjs` is a step of the Ubuntu build job), so its Windows assertions
      (some of the writer's renames refused) run on the gated hosts and the workstations only.
    - Four wall-clock upper bounds beside counters were dropped (one in Node, three in Go), because a loaded runner stalls a test for longer than they allowed: `took < 250` in the real-time bound test of
      `lease-read.test.mjs`, and the `2 x removeAttempts x removePause` (200 ms) bounds in the three tests of `readrelease_test.go` that count opens and pauses. The counters stay and
      are the proof; the Go tests log their times (`t.Logf`) and the Node real-time test logs none, keeping its lower bound only. The lower bound of the real-time test stays, and the 308 ms case (20 pauses of a 15.4 ms tick) stays pinned by the fake-clock test, which
      wants 7 pauses. The header of `lease-read.mjs` no longer says Go renames over a card claim.
    - Mutants of the helper, one at a time, each caught: every error retried (3 tests red, among them `a file that is not there is read once ...`); one pause short of the bound (1);
      the platform gate ignored (`off Windows a permission refusal is returned at once`); `EACCES` not retried (1); no pause between reads (4; a first run of this mutant hung the
      test, so the scripted read now ends a runaway loop and fails). `node --test` on `lease-read.test.mjs`, `lease-read-stress.test.mjs`, `gpu-lock.test.mjs`,
      `lease-progress.test.mjs` and `no-visible-windows.test.mjs`: 97 tests, all pass under WSL; the same files without the stress file, 96 tests, all pass on Windows.
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
  - The only reads that wait under the lock are of a file that exists and is refused. For the instant `Renew` swaps a live lease's heartbeat that is one wait that clears, and it is what keeps the sweep
    from calling a live lease stale, so it stays under the lock. Nothing that is missing or deleted pays it, which was the cost to rule out: it would have grown by about 100 ms per released lease.
    A released record held open by a foreign handle that shares delete was measured too: the name was gone at once on this Windows build, so `Leases()` answered absent in 0.5 ms with 0 pauses and
    the grant of another card took 4.9 ms (a scratch test, not committed). `gpu status` goes through `Leases()` and `InspectDir`, which these tests cover in the package.
  - A file that is refused and STAYS refused did multiply the wait under the lock, and a review found it: the first form of this bullet called it one bounded wait, and it was one per read. That is
    the next bullet.
- **A file refused for good is waited out once, not once per read.** A review of the release lane's checks found that a lasting refusal (a deny-read ACL on a lease file, a foreign delete whose handle
  never closes) paid the whole wait on every read of the file, and a grant reads each record five times and each heartbeat seven inside one hold of the epoch lock (the sweep, the scan for the
  unreadable and the verify each go over them).
  - **Red, then fix** (`readrefused_test.go`; the refusal is the open seam's, permanent for chosen paths; three leases held and renewed in a scratch root, a fourth card asked for; Windows, the wait on
    as in production). Before: one refused record, `the grant took 100 pauses for 1 refused file(s), want at most 20` (545 ms, the record opened 105 times, 84 of them under the lock); one refused
    heartbeat 140 pauses (769 ms); six refused files 360 pauses (1.96 s); and five reads of one refused file `took 100 pauses in all, want 20 (one wait)`, reads 2 to 5 each `made 21 opens, want 1`.
    After: 20, 20 and 120 pauses (117 ms, 120 ms and 654 ms), and 20 for the five reads.
  - **The fix** (`internal/gpulease/hostram.go`). A file still refused when the wait ends is remembered for `refusedMemory` (5 s). A read of it in that time opens it once, so a repair is read at once, and takes
    no pause if it is still refused; the first open that succeeds forgets it. The memory is per file, and a refusal that clears inside the wait is never remembered, so a heartbeat refused for the
    milliseconds of a rename keeps its whole wait and the sweep still cannot call a live lease stale because of it. Where the wait is off (not Windows) nothing is ever remembered. A read that finds nothing
    remembered takes no lock.
  - **Tests.** The red tests above, and three guards: a repaired file is read at once and a refusal that begins after the repair gets the whole wait again; another file's passing refusal (three refused
    opens) is still waited out and read after one file gave up; a file is waited out again once the memory runs out (shortened to 40 ms for the test). Mutants, one at a time, each caught: no memory
    (the 4 count tests red), the memory not forgotten by a good read (the repair test), the memory not per file (6 red), a memory that never runs out (the expiry test). The same tests pass in a
    cross-compiled test binary on Linux (WSL2, unprivileged user, memory-backed temp dir), beside the Linux pin.
  - **Real faults, not injected** (a scratch test run through a build overlay, not committed; Windows; `icacls` deny-read on files of a scratch lease root; two acquirers, the second 30 ms after the first).
    Before the fix (the overlay putting the previous `hostram.go` back): one refused record, first acquirer 541 ms and second 958 ms, 200 pauses, `Leases()` 110 ms with the file still denied; three
    records and three heartbeats refused, 1.97 s and 3.57 s, 720 pauses, `Leases()` 326 ms. After: 113 ms and 111 ms, 40 pauses, `Leases()` 0.5 ms; and 648 ms and 624 ms, 180 pauses, `Leases()` 0.6 ms.
    (The pauses are two waits, not one, because the second acquirer reads the file while the first is still in its wait: nothing is remembered until a wait ends.)
  - **What it leaves.** Each refused file still costs one wait (about 110 ms) the first time it is read in a window, wherever the read is, under the lock included. A second acquirer's 2 s spin is reached
    when about eighteen files are refused at once, against four before. A refusal that begins again within the 5 s, with no successful read of the file in between, is read without a wait; the
    refusal that began it had just outlasted a whole wait, and the next read's own open still looks.
- **The Linux pin.** `TestOnLinuxConcurrentRenameOverOfARecordAndItsHeartbeatIsNeverMissedAndNeverWaitedFor` (`readrace_linux_test.go`, Linux only): `Restamp` rewrites the held lease's record and
  `Renew` its heartbeat, 3,000 each, concurrently, while two goroutines of each of `Leases()`, `EpochIsCurrent` and a heartbeat read poll. It asserts 0 misses, 0 refused opens and
  `readRetries` (a counter of the pauses `readSharedFile` takes, added as the seam) at 0. The test binary was built with `GOOS=linux go test -c` and run in a Linux environment under WSL2 as an
  unprivileged user, with a memory-backed temp dir (a drvfs rename is not an ext4 rename): 50 of 50 passed, 118,283 `Leases()` reads, 138,177 `EpochIsCurrent` reads and 196,607 heartbeat reads,
  1,246,093 opens, 0 misses, 0 refused, 0 pauses. It has no red on the code as it is, because `rename(2)` is atomic; mutants: the heartbeat rewritten in place (truncate, then write) failed 3 of 3 with
  `the heartbeat could not be read on 265 of 3290 reads` (and 300 of 3330, 277 of 3255); a pause counted on every read failed with `readSharedFile paused 23845 times on Linux, want 0`.
- **The reclaim rule changed: an unreadable heartbeat is unknown, and unknown is never stale.** A later review found that the wait above only narrows the window, because
  `heartbeatAt` still mapped every read failure (including a rename in flight, where the live name is absent for tens of milliseconds with the staged file beside it) to the stamp written at
  acquisition. The reclaim rule needs a stale heartbeat AND a lapsed window, and a lease past its window is kept by its heartbeat alone, which `Renew` replaces by a rename WITHOUT the epoch lock; an
  acquirer's probe and its locked sweep each read it, so one episode spanning both read a live lease as stale, deleted it and granted its cards (or the whole node) to a second job.
  - **The rule** (`heartbeatAt` in `internal/gpulease/gpulease.go` returns `(value, known)`; `settled.go` holds the helper). The content of `hb.<epoch>` when it parses (unchanged). The file's
    modification time when the file is there but its content is no use (empty, torn) or the read failed for any reason but absence: `Renew` writes a staged file and renames it over the live
    name, so a heartbeat renewed a moment ago has the time of that write, and one nobody renews still goes stale after the TTL. The stamp from the acquisition only when the file is SETTLED
    absent (no staged file younger than 3 s beside the name, and a second look at the name, taken after the look at the staged file, still finds nothing): a holder that never renewed. Unknown
    otherwise (a staged file is beside an absent name, or the read and the look at the file both failed), and `reader.judge` passes unknown to `Reclaimable` as no stamp, which it already read as
    not stale. The wrapper-gone half of the rule (pid and process start time) is untouched, so a dead holder, and a recycled pid, are still reclaimed with an unreadable heartbeat.
  - **Red, then fix** (`heartbeat_unknown_test.go`, every platform; the episode is modelled on disk by renaming the live name away and leaving the staged file, so the tests do not depend on how a reader
    waits). A lease with a 50 ms window and the clock ten minutes on, renewed, its heartbeat then mid-rename; a second job asks for the same card, for the whole node over a card lease, for a card
    over a whole-node lease, and for the whole node over a whole-node lease. Before the change all four fail with `Leases() = [] while the holder's heartbeat is mid-rename, want [1]` and `the lease of a
    live holder was granted to a second job (epoch 2) because its heartbeat was mid-rename`, and the holder's next `Check` reads `lease is gone (epoch 1)`; a heartbeat that can be neither read nor
    examined, an unreadable one written a moment ago, and an empty or torn one written a moment ago fail the same way. They pass after.
  - **Guards that pass before and after.** An unreadable or corrupt heartbeat nobody has renewed for ten minutes still lets a lapsed lease go; a holder that never renewed, and a staged file left by a
    writer that died, are settled absent and keep the acquisition stamp; a lease inside its window, or younger than the heartbeat TTL, stays held; a recycled pid is reclaimed with an unreadable heartbeat; a
    renewed heartbeat has the fresh file time of its staged file (`Renew` sets no time by hand and nothing else writes the heartbeat).
  - **Known limits.** A holder that is alive but hung, with a heartbeat that stays unreadable and unexaminable for good, is not reclaimed by the heartbeat rule: only the wrapper being gone, or `gpu release`, frees
    it. The keeper's look at a heartbeat (the unreadable-record case) follows the same rule since the audit below, so there too only the wrapper being gone or `gpu release` frees a holder whose heartbeat stays unknown for good. The platform-independent rule takes no wait; the read gate that waits out an in-flight rename is separate.
- **The read gate: an absent name is an answer only once it is settled.** The wait above is for a file that is there and refused. A rename that was NOT refused can also leave the live name
  absent (`ENOENT`) for 11 to 75 ms as a reader sees it (one rename took 119 ms to return), about once in 74,000 renames on the host that showed it, with the staged file still beside it; the mechanism is
  unverified. Both readers took that for "not there": the Go reader called a held record or heartbeat absent, and the Node reader answered `null`, which for a card lease falls through to `meta.json`
  and fails the fence. The reclaim rule no longer depends on the Go read (above); this makes the reads themselves right.
  - **The rule** (`internal/gpulease/gate.go`, `settled.go`; `render/lease-read.mjs`). Windows only, on the same switch as the wait for a refused open (`deniedOpenMayClear`, `policy.waitsOutDenied`);
    off Windows an absence is answered at once, with no stat and no pause. On `ENOENT`: a file that is never renamed over (a card claim) is absent, at the cost of one open. A staged file younger than 3 s
    beside the name (a negative age, a clock that stepped back, and a staged file that cannot be examined all count as fresh) is waited for: pause 5 ms and read again, until 250 ms have passed since the
    first such look. A deadline, never a count of pauses (the Node timer sleeps a whole 15.4 ms tick). With no fresh staged file, ONE stat of the name itself, taken after the look at the staged file: the rename
    can end between the read and that look, and the name is then back. A name that is there is read again within the same deadline; a name that is not is absent. If the 250 ms run out, Go returns a
    `*fs.PathError` wrapping `errRenameInFlight` (an I/O failure and never `ErrNotExist`, so `Check` answers `RecordUnreadableError`, which `confirmFence` retries, and not "the lease is gone") and
    Node returns `null` (the fence aborts: loud, fail-safe, and Node only reads). Constants: 250 ms is about twice the longest absent name seen and is paid only beside a fresh staged file; 3 s is the longest a
    writer keeps retrying its rename (2 s) plus a second, because a retry chain was seen to take 2.29 s.
  - **A wait that ran out is remembered for 5 seconds, so a stuck staged file costs one wait per file.** A grant reads each record about five times and each heartbeat about seven inside one hold of the
    epoch lock, so a budget per read held it for 1.75 s for one heartbeat with a staged file a writer left behind a moment ago, and a second acquirer's spin allows 2 s. The memory (separate from the
    memory of a lasting permission refusal, which an absence is never fed to) answers the same I/O failure at once while the staged file is still fresh; it ends when the staged file is gone, when a read of the
    name succeeds, and after its time. Node keeps none: it reads a file once per fence or beat.
  - **The sites.** `readSharedFile` (every record and heartbeat read) and `statSettled` (the stat sites): `statRecord` (so `Restamp`, `restampLocked`, `ReleaseTarget`, `RecordState` and `releaseByEpochV2`, each
    of which already reads a non-absent error as "cannot be examined"), the keeper's look at the record, `claimIsFresh` (an unexaminable claim now counts as a claim in progress and not as debris; it read every
    error as "not fresh", which is a removal), the waiter stat (an in-flight answer is not a confirmed absence, so the record is not pruned) and the first look of the yield ask. The waiter record, the yield
    request, the launch markers (`tree.go`, `infer.go`, and the five reads in `internal/comfyinst`, through the exported `ReadSharedFile`) were plain `os.ReadFile` reads and go through the shared open. The two
    yield stats under the epoch lock stay plain: the only writer of that file runs under the same lock, so no rename is in flight there and a staged file would only hold the lock.
  - **One name for a staged file.** Every writer takes its staged name from `stagingPath` (`meta.json` stages as `meta.json.restamp`, because `.tmp` is the epoch counter's; a claim has none; everything else as
    `<name>.tmp`), and the readers that list or clear staged files ask `isStagedName`, `withoutStagedSuffix` and `stagingPath`. `staging_drift_test.go` fails on a hand-built staged name outside `stagingPath` and
    on a suffix `stagingPath` can return that `STAGING_SUFFIXES` in `render/lease-read.mjs` does not list. Node looks for both suffixes beside every name but a claim, a superset of Go's
    (which stages `meta.json` only as `.restamp` and the rest only as `.tmp`): one extra look, and no per-file table to drift.
  - **Red, then fix** (every platform: the tests run the gate on a clock that only a pause moves, and end the rename at a chosen open or stat). Before the change: a read with a fresh staged file beside an absent
    heartbeat returned `readSharedFile = "", open ...: The system cannot find the file specified. while the rename of the heartbeat was in flight, want the new content 42`; one whose rename never ends returned
    the not-exist error (`the wait ran out and the reader called the file gone; Check would then call the lease gone`); the straddle returned `want the content of the file that is there on the second look`;
    `statRecord` returned `GetFileAttributesEx ...: The system cannot find the file specified. while the rename of the record was in flight, want nil once it ends`; `keeperSignOfLife` found no sign of life in a
    record that was being replaced (`a keeper that is alive reads as gone`); `claimIsFresh` was false for a claim that cannot be examined; a waiter record whose rename never ends read as a confirmed absence; a second
    asker went on to ask an epoch whose request file was mid-rename (`tick 2 of a second asker = asked true`); and in Node ten of the twelve gate tests failed (`a null here is a held lease called not current`; the two that pass are the guards: a claim is one read and no stat, and nothing waits off Windows).
  - **The bound, counted.** A grant of a free card with three held leases whose heartbeats each have a fresh leftover beside them paused 1050 times and ran 21 budgets out before the memory (3 files x 7 reads), and
    150 times and 3 budgets after (`TestAFreshLeftoverStagedHeartbeatCostsAGrantAtMostOneWaitPerFile`). Seven reads of one stuck heartbeat paused 350 times before and 50 after. The existing counts are unchanged:
    a released lease is one open per file and no pause (`TestAReleasedLeaseReadsAbsentAtOnceAndNoReadWaitsForIt`, `TestAMissingOrDeletedRecordCostsOneOpenAndNoWaitUnderTheEpochLock`; the new looks are stats and have
    their own counter, `gatePauses`), the Node `[3,3,1]` read counts, and Linux's zero waits and zero stats.
  - **The Windows rename measurements, against the fixed readers** (the release lane's harnesses, one host, run once each and bounded; the Windows gate runner did not answer ssh, so these ran on the workstation). The Go writer
    that imitates `renameReplacing` against the fixed Node reader, 150 s: 457,000 reads, 300,720 renames of which 1,787 were refused, 0 nulls. The same harness with the raw reader for 60 s, as a control: 244,000
    reads, 92,502 renames, 0 absent answers, so this host showed no episode in that run and the 0 above does not by itself show the gate worked. The release lane's 4000-loop copy of the stress test: 4,005 loops, 48,060
    reads, 9,252 renames (9,252 a different inode, 42 refused), 0 nulls. The lane saw 3 of 4 runs of that copy fail on the reader before the gate. The gate is shown by the injection tests above, not by these.
  - **The same measurements again, at the head of the audit round** (the readers unchanged since; one host, once each, bounded, the Windows gate runner again unreachable, so on the workstation with one reader
    process and one writer). The Go writer against the fixed Node reader, 150 s: 710,200 reads, 191,220 renames of which 7,230 were refused, **0 nulls**. The raw control, 60 s: 295,200 reads, 83,268 renames
    (2,714 refused), **1 raw `ENOENT`**: the live name absent for 16.0 ms, back after 241 re-reads. So this host did show the episode in the control (about once in 83,000 renames), and with the fixed reader
    710,200 reads over 191,220 renames, where two or three were to be expected, gave no null; one run each does not make that a rate. The release lane's 4000-loop copy of the stress test with the fixed readers
    copied in: 4,005 loops, 6,462 renames (66 refused), 0 nulls, 4.3 s. That copy's own diagnostic prints 48,060 file reads for those loops because it multiplies by 12; a loop reads 6 files, so the figure is 24,030.
  - **Known limits of the gate and of the rule.**
    - **The directory-listing blind spot.** `reader.v2` and `reader.legacy`, the keeper's scan of the heartbeats (epoch 0), and `Waiters` list a directory, and a listing taken in the instant of a
      rename does not show the name; an unlocked caller then sees that lease as not live. Every destructive path re-reads under the epoch lock (the sweep, the grant, the release), and a card claim,
      which is never renamed over, stands as the second arbiter; advisory callers (status, admission and routing checks) can still see the lease as free for that instant. The launch markers are the
      one listing that was closed (`readLaunchMarkers` also looks for a fresh staged file where a name is not listed).
    - **`Renew`'s in-place fallback.** When `renameReplacing` is exhausted, `Renew` writes the heartbeat in place (`os.WriteFile`, a truncate and then a write). A reader that meets it half written
      reads an empty or torn file, which the rule now ages by the file's time (fresh, so alive) and no longer by the acquisition stamp; it was not changed.
    - **The first publish of a file** (no live name yet, a staged file beside it) is waited for like a rename and is, until it lands, an I/O failure and not an absence.
    - **A heartbeat that is unreadable and unexaminable for good** (the read fails and so does the stat, with an error that is not an absence) is unknown, and unknown is never stale and, for the
      keeper, alive: only the wrapper being gone (pid and process start time) or `gpu release` frees such a holder if it is alive but hung.
    - **A wrapper that stops its own tree** spares another lease's ComfyUI instance only when it can read that instance's marker (`PIDsOfOtherLeases`): a marker it cannot read is not spared, the
      direction it read before; the other direction would leave the command of a lease that was lost running on the card. **A detached hold's drain and unload** are scoped by `leaseDevicesOf`,
      which reads a lease it cannot find as the whole node and says so on stderr.
    - Plain `os.ReadFile` of the epoch counter (read and written under the epoch lock, so no rename of it is in flight at a read), the tokens, the orphan marker, the seen sidecar and the reader
      audit marker remain, none of which gates a fence or a reclaim.
- **The audit of every consumer of the new error, and what it found.** A read or a stat that gives up on a rename in flight answers `errRenameInFlight`, an I/O failure and never an absence, and the
  rule for every consumer is that the lease is held or the file cannot be examined, never free and never gone. Every call site that branches on `isIOFailure`, `RecordUnreadableError` or
  `fs.ErrNotExist` on a lease path, and every consumer of the gated reads, was read against it; the table is in `docs/systems/gpu-lease.md` (*What each consumer does with a lease file it cannot
  read, or finds mid-rename*). Most already fail closed (the fence, the restamps, the releases, `RecordState`, the claim tests, the waiters, the term and the host-RAM hold). Two did not, and are
  fixed red first:
  - **The keeper read a heartbeat it could not tell as no sign of life.** A record that exists and cannot be read is held while its keeper is alive, and the keeper's evidence is the heartbeat, which
    `Renew` replaces by a rename without the epoch lock, so under the lock the sweep and the grant hold it is the one lease file that can be in flight. With the record old and unreadable and the
    heartbeat mid-rename, unreadable, or torn, the sweep removed the live holder's record and claims, and the next card grant and a whole-node grant both succeeded. Red
    (`keeper_unknown_test.go`, 4 cases, at the unchanged code): `a second lease (epoch 2) was granted on the card of a live holder whose record cannot be read, while the heartbeat is mid-rename`,
    `the record of a live holder was swept while the heartbeat is mid-rename: GetFileAttributesEx ...: The system cannot find the file specified.`, `the claims of a live holder were swept while ...` and
    `a whole-node lease was granted over a live card lease whose record cannot be read, while ...`, each for a heartbeat mid-rename, one that can be neither read nor examined, one that cannot be
    read but was written a moment ago, and a torn one written a moment ago. `keeperEvidence` now reads the heartbeat as the reclaim rule does and `keeperAlive` reads unknown as alive. Guards that pass
    before and after: a heartbeat never written, an unreadable or torn one with an old file time, and one holding a time from minutes ago all still leave the record to be swept.
  - **A launch marker that was being installed was skipped, so a dead wrapper's lease was reclaimed with its instance still on the card.** The marker is what ties a ComfyUI instance outside the
    wrapper's job to its lease; Node restamps it through a staged `.tmp` file and a rename. Red (`tree_marker_unknown_test.go`, at the unchanged code): `the lease of a dead wrapper was granted to a
    second job although its launch marker was being installed`, for a marker whose name is absent with a fresh staged file beside it and for one that was listed and whose read ended as a rename in
    flight. A marker that is being installed is now unknown (`treeUnknown` on the reader's copy of the record), and `Reclaimable` does not free a lease whose wrapper is gone while it is set. It lasts as
    long as the staged file is fresh (3 s at most); a marker that is unreadable for another reason is skipped as before, so one marker that is lastingly refused cannot hold every dead wrapper's
    lease. Guards that pass before and after: a staged leftover a minute old, no marker at all, and a marker whose process is gone.
  - Left as they are, and in the known limits above: `PIDsOfOtherLeases` and `leaseDevicesOf`, where neither direction is safe. `keeperAlive`, the claim loop and `removeStaleClaim` are the same
    code path as the first fix.
- **This round also changes how a whole-node grant takes the lock, and what a grant sweeps under it.** Not only the readers and the reclaim rule changed.
  - **The claim is created under the epoch lock** (`TryAcquire`, `internal/gpulease/gpulease.go`). The claim is an exclusive create of `meta.json`, the name a holder's restamp replaces by a rename under the epoch lock.
    The grant issued the epoch under the lock, let it go as `bumpEpoch` returned, and then built the record and made the claim with nothing held. The release lane measured an exclusive create of that form
    succeeding on a name whose rename-over was stalled in 2 of 4 episodes, and in both the stalled rename then completed over the created file, so the creator's claim was replaced by the holder's record
    (the creator would be fenced at its first `Check`). Reaching it takes a contested acquisition whose loser lands inside the winner's first restamp, which is rare; but "the lock is taken in `bumpEpoch`, so it
    cannot" was false. The grant now takes the lock once around the epoch bump, the encoding of the record and the create and write of the claim, the shape of a card grant. The record's slow parts (the holder's process
    start, the owner and the session registry behind it, the term plan) do not need the epoch and are read before the lock (`draftRecord`) and finished inside it (`finishRecord`, now the one place the acquisition
    stamp `RenewedAtMs` and the other instants are written). The verification, the reclaim and the sweep of sidecars take the lock themselves and run after the section, so `afterClaimHook` still fires with
    the lock let go. A grant that loses the exclusive create lets the lock go and leaves the winner's claim alone.
  - **Red, then fix.** `TestAWholeNodeGrantCreatesItsClaimWhileHoldingTheEpochLock` reads the lock at the instant of the create through a new test seam (`beforeClaimHook`). Before: `wholenode_lock_test.go:35: the
    claim was created without the epoch lock held: an exclusive create can then land inside a restamp's rename of the same name`. After: green, 3 of 3 runs. Guards that pass before and after: a grant that loses the create
    lets the lock go and leaves the winner's record untouched; the record's process reads happen before the lock. The create inside a real stalled rename is not reproduced by a test (it needs a rename that stalls in the
    OS call, which cannot be forced without a production-only hook); the lock-held assertion stands in for it.
  - **The staged files whose writers hold the lock are swept under it, at any age** (`sweepStagedLeftoversLocked`, `devices.go`). `meta.json.restamp` and `e/<epoch>.json.tmp` are staged and renamed only by code that holds
    the epoch lock, from before the file is staged until after the rename returns (the stall is inside that call, in the lane's probe), so to someone holding the lock one is a dead writer's leftover whatever its age; left
    alone, a fresh one makes every reader of the absent name wait a whole budget for a rename that will never end. It runs first in the debris sweep (every card grant, and the verification a whole-node grant makes), before
    any record is read, and in the whole-node grant's own section (the restamp file only). One try and no retry, so a file a scanner holds is left for the next sweep. `hb.<epoch>.tmp` is never swept: `Renew`
    replaces the heartbeat without the lock. Before: `sweep_leftovers_test.go:44: meta.json.restamp (fresh) is still there after a grant that held the epoch lock`,
    `:69: 7.json.tmp is still there after a grant that held the epoch lock (fresh)` and `:96: the verification paused 50 times for a rename in flight, want none: the leftover it waited for is a dead writer's and was
    there to sweep`, each for a staged file a moment old and an hour old; after, green, 3 of 3 runs. The guard that the heartbeat's staged file and the live files stay passes before and after.
  - **The worst epoch-lock hold of a whole-node acquisition, measured against the 2 s a second acquirer spins** (one Windows host, the real gate and the real clock, a scratch test run once and not in the tree; the
    spin gave up after 2,186 ms against a lock that was never released). The grant's own section, 20 grants: 0.5 to 1.6 ms, median 1.1 ms; with a rename that takes 119 ms inside it (the epoch file held open without
    delete sharing, so the rename is refused and retried), five grants: 121.8 to 124.5 ms, and a second acquirer that arrived 30 ms in waited 92 to 98 ms and got the lock. That section reads no lease file, so the gate
    cannot add to it. The verification section (a separate hold) with N live card leases whose heartbeats each have a fresh leftover beside them and no live name, nothing remembered: N=1 253 ms, N=3 761 ms, N=8
    2,030 ms (one 250 ms budget per file); with the leftovers already waited for outside the lock, as the probe every grant makes first does: 1.6, 2.7 and 5.9 ms (the probe paying 256, 760 and 2,019 ms).
  - **Said plainly: the worst hold is bounded, not removed.** Cold it is N x 250 ms, once per file per 5 s per process: the nominal 2 s at eight files, past the measured 2.19 s at nine. Both levers are
    in (the sweep above takes the record and restamp leftovers, and the memory of a wait that ran out, from the gate, makes it once per file); what remains is the heartbeat's leftover, which only `Renew`'s writer owns. What
    keeps it to milliseconds is the order: the probe every grant makes without the lock judges ALL the leases, not only the conflicting ones, and pays the waits first. `TestTheProbeBeforeTheLockPaysTheGateWaitsSoTheLockIsHeldWithoutOne`
    (a card grant over live leases on other cards, and a whole-node grant over leases whose holders are gone) counts the gate's pauses taken with the lock held and wants none, after checking that the probe paused outside it;
    `TestTheWholeNodeVerificationPaysAtMostOneWaitPerFile` counts one wait per file for the verification. The cold hold needs leftovers that appear between the probe and the lock, fresh
    (younger than 3 s) and unread for 5 s. On a host with eight or more cards, each with a lease whose writer died in the last 3 s, that could outlast a second acquirer's spin; it would then fail loudly
    (`the epoch lock ... did not clear`) and never grant. Not built against: the host measured has three cards.
  - **Known limits.** A lock broken as stale after 30 s (`epochLockStale`) lets a second holder in beside a first that only stalled; its sweep could remove the first's staged file, which fails that rename with an error
    and corrupts nothing. The 5 s memory is the gate's choice and is not a measured figure. The measurements are one host's.
- **Not verified here.**
  - The 5 s memory of a refused file is a choice (long enough for the polls of a burst of acquirers, short enough that a new fault is waited on), not a measured figure, and the arithmetic of eighteen files
    is from the 110 ms a wait costs here. The real-ACL before and after runs are one host, a scratch test not committed, and they refuse a file for good; a foreign delete whose handle lingers was not
    reproduced (the harness's own deletes do not make one).
  - Node: the retry was never exercised by a natural refusal. Plain `readFileSync` missed nothing in about a million reads, so that Node can meet `EPERM` from a rename rests on the delete-pending probe and not
    on a miss, and the wait is shown by the injected refusal and a file held delete-pending, not by a natural rename-over. The fixed stress test does make the writer and the readers meet
    on Windows (the writer's renames are refused 1 to 6 times a run on one host, 14 to 30 in the release lane's runs), but not a reader's open be refused: no reader has missed. The 15.4 ms timer tick is one host's; a host where another process has raised the
    timer resolution ticks faster, and the deadline holds either way.
  - A copy of `gpu-lock.mjs` outside this tree (a media repository's own, which `gpu doctor` audits) now needs `lease-read.mjs` beside it, as it already needs `lease-progress.mjs` and
    `comfy-lifecycle.mjs`; a copy that has the format signature but not the new module fails at import. Not checked against any such copy.
  - The stress test ran on Linux under WSL (Node 22, three runs) before this was pushed, not on the CI image (Ubuntu, Node 24): that run is the first one there. Not run on macOS.
    The Windows runs were three, on one host (Node 26); the host's own noise was not varied.
  - Go: the claim files (`cards/<card>.claim`) are still read with `os.ReadFile`, as the epoch counter and the tokens are; the Node side reads claims through the helper. The waiter records are read through the shared open now.
  - No GPU job, lease on a real card or seat was used: every lease in these runs is in a scratch lease root, with the stub runner.
  - The double-booking was shown with a held `DELETE`-access handle in a scratch lease root, which models the instant of the rename, and by a tight `Renew`
    loop in a review. It was not seen with a real `gpu reserve` command that outran its window, with a waiter queued, on a real card. The chance in
    production per renewal (every 15 s) with a waiter polling once a second was estimated, not measured.
  - The delete-pending wait is sized by the claim removal's bound (about 100 ms), not by a measured duration of the instant. Zero misses in the runs above
    says the bound covers what was seen; a stall of the renaming process longer than that, under a heavier load, would still lose a read, and the stress
    test, which now allows no miss, would fail on it rather than hide it.
  - An unreadable heartbeat no longer falls back to the acquisition stamp (see the reclaim rule above). What is not changed: a holder that is alive but hung, with a heartbeat that stays unreadable and
    unexaminable for good, is freed only by its wrapper being gone or by `gpu release`.
  - Linux: the reader there is `os.Open`, and a permission error on it is returned at once (the gate above). The gate and the pin were shown in a cross-compiled test
    binary of `internal/gpulease` run in a Linux environment under WSL2, as an unprivileged user with a memory-backed temp dir, with the tests named in this fragment and a
    measurement script, not with the whole package or `internal/pipeline`, which were compiled and vetted for Linux (`GOOS=linux go vet ./internal/gpulease/
    ./internal/pipeline/`), not run there. The race itself is Windows-only. No lease on a real card and no live lease directory was used.
  - The claim file has the same two retries with no platform gate: `createClaim` retries a permission error and `removeClaim` retries any error,
    `removeAttempts` times at `removePause`. They are older than this branch, concern the claim file and not a lease record, no finding covers them, and
    they were left as they were.
  - The epoch counter and the tokens are still read with `os.ReadFile`; no finding covers them.
  - The read gate: no natural episode (an absent name beside a staged file) was seen in the Windows runs above, so the gate is shown by injection and not by a live rename; the full `internal/gpulease` package was
    not run on Windows (the Windows gate runner did not answer ssh), only the targeted tests of this change, three times each, on the workstation. The worst epoch-lock hold of a whole-node acquisition was measured once, on one
    Windows host, in a scratch test that is not in the tree, with a rename stall made by holding the destination open (a refused and retried rename of 119 ms), not by a natural episode, and not with N above eight.
  - `windowAndKill` still reads the lease twice. A lease that ended between the two reads (the 2 s render killed and released) would also show
    `got []`; it needs a stall of seconds between two adjacent statements and was not seen.
