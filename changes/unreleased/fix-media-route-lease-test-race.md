### Fixed - a held lease no longer reads as absent while its record is rewritten: the lease records are read through a handle that shares delete (the media-route lease test flake)

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
- **What was tried first and dropped.** A bounded retry of the failed read. Under saturating load the failure lasted a median 11 to 22 ms, a p90 of 85
  to 168 ms and up to 1.4 s, and 14 to 22 percent of the failures outlasted an 80 ms bound, so a bound short enough for a status call still left misses.
  The open is the cause, so the retry and its test were removed.
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
- **Not verified here.**
  - No GPU job, lease on a real card or seat was used: every lease in these runs is in a scratch lease root, with the stub runner.
  - A stray miss remains possible under extreme load. With the shared open in, `EpochIsCurrent` missed 1 read in 25,939 in one of 6 runs and 1 in 8,242 in
    one of 20 under the 30 burners (the stress test then ran four spinning readers, which also starved the writer's rename once); none of 25 runs with
    the read errors logged, and none at any stage of `EpochIsCurrent` in 40 runs, so the cause is not found. The stress test allows 3 misses a run for it;
    the defect misses dozens.
  - Linux: the reader there is `os.Open` as before (`GOOS=linux go vet ./internal/gpulease/ ./internal/pipeline/`); the race is Windows-only and no Linux
    host ran the stress.
  - The other files the package renames over (the heartbeat `hb.<epoch>`, the epoch counter, the tokens, the waiters) are still read with `os.ReadFile`.
    A heartbeat read that fails falls back to the acquisition stamp, which matters only past the heartbeat window; it was not measured and is not changed.
  - `windowAndKill` still reads the lease twice. A lease that ended between the two reads (the 2 s render killed and released) would also show
    `got []`; it needs a stall of seconds between two adjacent statements and was not seen.
