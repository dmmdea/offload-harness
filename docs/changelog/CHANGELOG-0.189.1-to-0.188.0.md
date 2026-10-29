# Changelog archive: 0.189.1 to 0.188.0

Verbatim entries moved out of the root `CHANGELOG.md` so the file GitHub renders stays under its size limit.
History, never edited. Relative links inside the entries are written from the repository root.
Newest first; the live changelog is [`../../CHANGELOG.md`](../../CHANGELOG.md).

## [0.189.1] - 2026-10-18 - A fleet node requires the fleet bearer on every lane, the GPU lease judges a stated commit on the commit limit, a lease record being rewritten is read rather than judged absent, and the vLLM seat launcher fits its context to the KV pool and mounts the store on the pinned network

### Fixed - the tests that hold a fleet node's bearer rules cover every route, and the texts that describe the rules say what holds

- **A new route cannot skip the bearer rules.** The node now registers its routes, and the durable queue's, through one small
  interface, and a test records every pattern the node serves and compares it with the tables the credential tests run over. A
  route that takes a body (POST, PUT, PATCH, or a pattern that names no method) must be a row of the door table, which is what
  puts it under the checks that a caller without the bearer costs the node no body byte and is refused with one journal line; any
  other route must be a row of that table or an entry of the two public routes (health and the jobs feed), which are read-only. A
  row that names no route fails too. The hand-kept door count that stood in for this is gone. Nothing a caller sees changes.
- **A route cannot reach the node's mux where that table cannot see it.** A source test follows the mux `Handler` builds through
  every use of its name: it goes only to `registerRoutes` (once) and to the ingress guard, so a call from another package that
  takes the mux and mounts routes on it, the mux copied or captured, or a bare mux served fails. `registerRoutes` hands its
  registrar only to the queue's `Mount`, `Mount` uses its router only to register on it, and the queue's package registers a route
  nowhere else. The registrations that stand in `registerRoutes` and `Mount` are counted and compared with the patterns the table
  test's node registered, so a route registered under a condition that node does not meet, or in a loop, fails whatever the
  condition. Nothing a caller sees changes.
- **"Answered the same way" is pinned.** For `/fleet/health` and the jobs feed, a request that presents a bearer the node does not
  hold (a wrong one of the key's length and of another, the scheme in lower case, `Basic`, a bare `Bearer` with and without a
  space, a 64 KiB bearer, a 64 KiB value with no scheme, and the right key under another scheme) must get the same status, the same
  response headers (the `Date` and `Content-Length` a server sets per response aside) and the same body as a request with none, on
  the same node, and the view it is compared with must be the one that withholds the private fields. The one thing a presented
  credential changes is the journal, which gets one throttled line in the usual shape. Nothing a caller sees changes.
- **The list of sentences that must stay out of the guides is checked against what they replaced.** Each row of the door-claims
  lint now carries the words of its file as they stood before the bearer covered every lane, and the lint is run over them as if
  they were the file, so a row that matched nothing in any version fails. Two rows that did are replaced (the operator guide's real
  sentence about the health route) or dropped (the decision record is held by the amendment test), and the list now covers the
  health route, the media lanes, the config row that named the agent lane as the only gated one, the flow that called the contract
  unauthenticated, the systems page's three sentences, the poll handler's comment and two test comments in the node's package.
- **The systems page says every lane needs the bearer.** Its Auth section no longer says the token gates exactly two lanes, and it
  no longer says a node with no token is unchanged or that the legacy speech-to-text lane rides the bearer only when the node has
  one: a node with none answers only on loopback and refuses everything else with `403`.
- **The release floors are the first release whose client sends the bearer.** Withdraw 0.146.0, text 0.154.0, compose 0.163.0, stt
  0.165.0, the roster probe's token check 0.167.0 and media 0.173.0, in the node guide, the operator guide and the entry that
  introduced them; four of the six were wrong. The speech-to-text asker, its upload door and their two config keys are dated 0.165.0
  in the six files that named an earlier release.
- **The run-graph door is described as it is.** The cost rationale for capping the missing-weights list says a holder of the fleet
  bearer can send a graph that names thousands of files, in the node guide, the media systems page and the comments in the
  package that checks the files, where it said any peer could.
- **The refusal of a composition's own code says what the bearer does.** A dispatch of `compose-video` that carries its own `html`
  or `project_dir` is refused as before. The error says the door renders the node's vetted templates and points at the project
  door of a node that opted in, instead of saying the bearer is limited to templates, which it is not on such a node. The
  composition guide says the same.
- **The operator guide's health example keeps the token off the command line.** It reads the `Authorization` header from a file
  only the operator can read (`curl -H @file`).
- **Tests, red first.** The lint's own proof of its rows failed on the two rows that matched nothing, the extended list failed on
  eight stale sentences and then on three more, and the tightened composition refusal test failed on the old wording, each before
  the change; the route table and the same-answer matrix pass today by design and were shown to fail on a route added without a row,
  on a challenge and a verdict header added for a presented bearer, and on the task list emptied for one. The cases of the source
  test for a route mounted from `Handler` and for a route registered under a condition were committed failing before the rules
  that make them pass, and each shape was also run against the node's own sources on a gate runner and failed there.
- **Not verified here.** No live node was reached: the route table and the same-answer matrix run against in-process nodes, and the
  release numbers were read from the repository's history, not from a deployed fleet.

### Fixed - the GPU lease admits a commit a caller states on the commit limit, and a commit nobody stated on physical RAM as before, beside the memory that is available

- **The defect.** The host-RAM brake admitted a lease only while `committed memory now + the lease's declared need + what running leases have yet to load` stayed under physical RAM less the headroom. Committed memory is not memory in use. On the reference 3-card Windows box (127.7 GiB RAM, 171.7 GiB commit limit) a reading of 70.5 GiB committed with 75.6 GiB physically free refused a 72 GiB arm, and a coder-model arm whose process tree touched 27.8 GiB and was charged 58.1 GiB could not be admitted at all (70.5 + 58.1 = 128.6 against 119.7). A job that commits twice what it touches does charge twice, but the line that charge can run into is the OS commit limit, not physical RAM.
- **The rule.** A grant needs two conditions, each against the line that is real for it (`gpuprobe.HostRAMAdmitsLoad`, the one function the grant, the read-only check, the card allocator's pre-filter and the deferred-lease wait all call): the working set the job touches must leave the headroom of the memory that is available (`available - touched need - what running leases have yet to touch >= headroom`), and its commit must fit a line that depends on who said what the commit is. A commit the caller **states** (`--ram-commit`, even one equal to `--ram`, or a caller that measured one) is held to the OS commit limit (`commit charge + commit + what running leases have yet to be charged <= commit limit - headroom`). A commit **nobody stated** (every caller that declares one figure: the media admission, the seat warm-back, a bare `gpu reserve --ram N`) is the touched figure standing in for one, and is held to the line it always had: `commit charge + the figure + what running leases have yet to touch <= physical RAM - headroom`, on every OS. A quantity declared as 0 is not judged. A touched need above physical RAM less the headroom, or a commit above its line less the headroom, is refused at once. A refusal names the condition that failed and its numbers (`needs 28.0 GiB, only 30.0 GiB of 127.7 GiB physical is available, 8.0 GiB headroom`; `needs 58.1 GiB of commit, committed 150.0 of 171.7 GiB commit limit, 8.0 GiB headroom` for a stated commit; `needs 46.8 GiB, committed 84.6 of 127.7 GiB physical, 8.0 GiB headroom` for one nobody stated), and both when both failed.
- **Why the default is not the commit limit.** Held to the limit, every caller that declares one figure would be admitted where the old line refuses: two coder arms declared as one figure each (27.8 / 27.8) are both admitted at the 70.5 / 75.6 reading (126.1 projected against 163.7, 20.0 GiB left available) although the real charge after both load is 70.5 + 2 x 58.1 = 186.7 GiB against a 171.7 GiB limit, and so is a single figure of 49.2 to 67.6 GiB. Those stay refused, and the two arms are admitted only on commits stated and fitting the limit (arm 1 at its measured 58.1; a second arm up to 35.1 GiB stated once arm 1 has loaded). Taking the commit from the private peak the footprint store records, so the media admission and the warm-back could state one, is not done here.
- **Interface.** `gpu reserve --ram <GiB>` keeps its meaning as the touched working set. `--ram-commit <GiB>` states the commit; unset, none is stated and the commit is the `--ram` figure held to physical RAM. An explicit `--ram-commit 0` is refused rather than read as "same as `--ram`", a commit needs a `--ram` above 0 to go with, and a `--ram` or `--ram-commit` that is NaN or Inf is refused by name (NaN passed every flag check and failed at the lease record with a JSON error). A job whose commit is smaller than its working set (mapped files) cannot say so by leaving the flag out: it is judged on the larger figure, and `--ram-commit` states the smaller. The hold bound (`--ram-hold-max`, `gpu_hostram_hold_max_sec`) and the front-waiter behaviour are unchanged.
- **The record and the surfaces.** The lease record carries `host_ram_commit_gib` and, for `--unload-seat`, `host_ram_wait_commit_gib` (additive; present means stated, even when equal to the touched figure, and omitted otherwise; an older reader judges the lease by `host_ram_gib` alone). The detached holder takes `--ram-commit` and `--ram-wait-commit` whenever one was stated. `gpu status` shows both declarations of a live lease (`host RAM 10.0 GiB touched, 60.0 GiB commit`), and the `host_memory` block of `gpu status --json` and of `offload_status` gains `declared_live_commit_gib`, `pending_commit_gib`, `commit_limit_enforced`, `touch_room_gib`, `commit_room_gib`, `unstated_commit_room_gib` (what a lease that states no commit could declare now, which `touch_room_gib` alone overstated) and `verdict_why`; the lease rows gain `host_ram_commit_gib` and `host_ram_wait_commit_gib`. `internal/mcpserver/testdata/offload_status_default.golden.json` was regenerated on purpose for those additive keys (`OFFLOAD_UPDATE_GOLDEN=1`; no existing key changed).
- **The verdict word follows the rule.** `OK`, `NEAR` and `OVER` judge the lines the grant applies: the available memory, the commit charge against physical RAM (`OVER` above it, `NEAR` inside the headroom of it: the line a commit nobody stated is held to, on every reading) and the commit limit where the OS enforces one. The paging incident's reading (3.2 GiB available, 162.9 GiB committed on 127.7 GiB of RAM) reads `OVER` and leads the `offload_status` line, as it did before this change. It still does not claim the box is paging, and an admitted stated commit can itself take the box above physical RAM (the coder arm's measured 58.1 GiB does), which then reads `OVER` by design.
- **What the status line, `NEAR` and the warm-back say.** On a reading whose commit limit the OS does not enforce, `gpu status` now says a commit a lease states is not judged on it and one it does not state is still held to physical RAM (it said the commit condition is not applied, which was false for a bare `--ram`). `NEAR` holds back the leases judged on the line that is near, not every declaring lease: a host that reads `NEAR` on the physical-RAM commit line alone still admits a lease that states its commit. The seat warm-back states no commit, so it is held to physical RAM less the headroom and not to the commit limit. "Other processes' allocations fail past the commit limit" is written everywhere with its condition, the page file at its maximum. `gpu_ram_docs_test.go` refuses the retired wordings, and the behaviour each corrected sentence describes has a test beside it.
- **The commit limit moves.** The Windows limit read is the current one (`ullTotalPageFile`). With a system-managed page file Windows grows the file as the charge nears the limit and the next reading carries a larger limit (187.7 GiB on 2026-10-09, 171.7 GiB on 2026-10-17 on the reference box), so a stated commit's bound moves with it and "other processes' allocations fail past the limit" holds once the page file has reached its maximum. The configured maximum is not read: for a system-managed file it is the number 0. The docs say so.
- **Fail closed.** A reading from a platform whose commit limit is always enforced that carries none (a Windows total page file of 0, or strict Linux with `CommitLimit` 0) used to skip the commit condition; it is now an unreadable reading (the grant waits, the verdict is `unknown`, the Windows conversion returns no reading). On Linux a `vm.overcommit_memory` that cannot be read or parsed used to mark the limit soft; the limit now stays hard unless the mode is known to be 0 or 1.
- **Linux.** A stated commit is judged against the commit limit only where the kernel enforces one: `vm.overcommit_memory` = 2. In the default heuristic mode (0) and in mode 1 the kernel grants allocations past `CommitLimit` and `Committed_AS` counts mappings a process reserved and never touched, so the reading is marked soft (`HostMemory.CommitLimitSoft`) and a stated commit is judged by neither line (it loses the commit limit and the physical line; only the available condition applies to it). A commit nobody stated is still held to physical RAM there, as before. Two Linux nodes of the fleet were both in mode 0 on 2026-10-17, so on them a stated commit is moot.
- **Not-yet-loaded accounting, per quantity.** What running leases have yet to touch is their declared working set less what their processes hold resident; what they have yet to be charged is their declared commit less what their processes hold privately (private bytes are the currency of the commit charge: `PrivateUsage` on Windows and, on Linux, the private writable mappings, `VmData`, because `Committed_AS` charges a mapping when it is made). Each is pooled per holder and never below zero, and a holder that cannot be read holds nothing. The holder-memory test seam now reports both.
- **The Linux private figure is `VmData`.** It was `RssAnon` plus `VmSwap`, the touched part of a process's private mappings. On a strict-overcommit host a mapping a lease committed and never touched (a CUDA host reservation, a large allocation) is inside `Committed_AS` and was counted again as the part of the lease's declared commit still to be charged: a child that committed 256 MiB untouched moved `Committed_AS` by 333 MiB and read 2.1 MiB private. `VmData` leaves out the main thread's stack and shared memory, so it can read a little low, which leaves more pending and can only over-refuse. The same reader feeds the process-tree sampler, so on Linux the `host_peak_gb` the footprint store records is now the private writable mappings, not their touched part.
- **How it was proved.** Red first at each layer, with the failure lines in the commit messages: `internal/gpuprobe` (the table of conditions, the stated and unstated lines with the coder arms and the band, the Linux reader through its file seam, the Windows counters conversion and, through a stand-in MEMORYSTATUSEX, the call that wires them), `internal/gpulease` (both directions through the grant, the pending accounting, the deferred wait, the record), the allocator pre-filter (a stated commit and a pending commit that only the commit line decides) and the root package (the flags, the holder arguments, the unload-seat wait, `gpu status`). Mutants of the rule (each condition removed, the commit compared to physical RAM again, the stated flag dropped), of the allocator path, of the status room keys and of the Windows wiring went red; they are listed in the pull request. The documented readings are pinned by tests, among them the coder arm of 27.8 GiB touched and 58.1 GiB charged against the 70.5 of 171.7 committed and 75.6 available and the two-lane incident of 2026-10-09 at any available figure. The tests' fake hosts that moved to a commit limit equal to physical RAM keep the arithmetic of every existing commit scenario; a refusal of a commit nobody stated names physical RAM.
- **Not verified here.**
  - No lease was taken and no seat was loaded: every test runs against a stand-in host reading, so nothing here shows the admission on a live box under a real lease.
  - The coder-arm and llama.cpp figures (27.8 and 3.6 GiB touched, 58.1 and 32.1 GiB charged) and the 70.5 of 171.7 GiB reading are session measurements, not re-run for this change.
  - The Linux strict-mode branch has no live host: both Linux nodes read are in mode 0, so the strict path is exercised through the reader seam only.
  - The Windows commit limit under page-file growth at run time, and the 1:1 charge of VRAM allocations to commit on nodes other than the reference box, are not measured. The Windows-only reader test did not run on the Windows gate runner (it was unreachable); it ran on the development machine.
  - A stated commit equal to the touched figure is judged on the commit limit by design: stating one is the caller's measurement, and the rule does not check it.

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
  about 100 ms) and returns every other failure at once, except an absent name with a fresh staged file beside it, which the read gate below waits for; a file that stays unreadable is waited out once, not once per read (the last bullet of this group).
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
  - `readSharedSync` / `readSharedJson` retry those two until a 100 ms deadline (pausing 5 ms with `Atomics.wait`, which `atomic-out.mjs` and
    `igpu-engine.mjs` already use on the main thread), and return `null` at once for any other error and for a missing file with nothing staged beside it, so a released lease still reads absent with no
    wait. (The first form retried those two and nothing else; since the read gate below, on Windows an absent name with a fresh staged file beside it is waited for too, up to 250 ms.) The wait runs only on Windows (`policy.waitsOutDenied`), for the reason the Go side gave up the wait elsewhere. The three Node readers, `checkInheritedLease` and
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
      over a card claim (it creates one once and deletes it); the claim was renamed over here all the same, "harder on its reader than production", until a long run showed that to be a false red (below), and it is read and never replaced now.
    - Measured, the file alone, three runs each. Windows: 612, 702 and 738 renames, every one a replacement; 6, 3 and 1 of them refused and retried; 0 link refusals; 208, 154 and 154
      loops of the five readers (six file reads a loop, about 900 to 1,250); 0 misses; 674, 662 and 685 ms. WSL (Linux, Node 22; the scratch root in the distro's own temp directory): 18,252,
      15,894 and 16,200 renames, every one a replacement; 0 refused; 154, 153 and 151 loops; 0 misses; 462, 461 and 465 ms. The refused count on Windows is small and varies with the host and its load (1 to 6 in
      600 to 740 renames in these first runs; 3 to 11 in 612 to 630 after the writer observed each rename, below; 13 to 37 in 650 to 970 renames in later runs of the test by reviewers and in the audit round; the release lane
      counted 14 to 30 a run on its own), so the test leans on the writer going on to the first refusal; it has not seen a run that needed the cap.
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
  - The reads that wait under the lock are of a file that exists and is refused, and (the read gate, below) of a name that is absent with a fresh staged file beside it. For the instant `Renew`
    swaps a live lease's heartbeat that is one wait that clears; what keeps the sweep from calling a live lease stale is the reclaim rule (an unreadable heartbeat is unknown, and unknown is never stale), and the wait
    only narrows the window in which the heartbeat is unknown. Nothing that is missing or deleted with nothing staged beside it pays any wait, which was the cost to rule out: it would have grown by about
    100 ms per released lease.
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
    milliseconds of a rename keeps its whole wait. Where the wait is off (not Windows) nothing is ever remembered. A read that finds nothing
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
  - **The rule** (`heartbeatAt` in `internal/gpulease/gpulease.go` returns `(value, known)`; `settled.go` holds the helper). The content of `hb.<epoch>` when it parses and is not earlier than the stamp
    written at the acquisition (a later review found that a prefix of the 13 digits of a renewal, zero and a negative number parse, and were taken as the heartbeat, floored at that stamp: they are now
    treated as content that says nothing). The file's modification time when the file is there but its content is no use (empty, not a number, or a number earlier than the acquisition, which is what a
    torn write leaves) or the read failed for any reason but absence: `Renew` writes a staged file and renames it over the live
    name, so a heartbeat renewed a moment ago has the time of that write, and one nobody renews still goes stale after the TTL. The stamp from the acquisition only when the read found the name absent and
    the file is SETTLED absent (no staged file younger than 3 s beside the name, and a second look at the name, taken after the look at the staged file, still finds nothing): a holder that never renewed.
    A read that failed with anything else and then finds the file gone says nothing (unknown), as does every other case: a staged file beside an absent name, or the read and the look at the file both
    failed. `reader.judge` passes unknown to `Reclaimable` as no stamp, which it already read as not stale. The pid and process start time test of the wrapper-gone half of the rule is the same code as before,
    so a dead holder, and a recycled pid, are still reclaimed with an unreadable heartbeat; the one thing this round added to that half is the launch-marker clause below (a marker that is being installed
    holds the lease of a dead wrapper for as long as it is being installed).
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
    off Windows `readSharedFile` and the Node reader answer an absence at once, with one open, no stat and no pause. `statSettled` is not the same off Windows: it takes its looks (the name, the
    staged file, the name again: three stats for an absence that is settled) and answers `errRenameInFlight` at once, with no wait, beside a staged file younger than 3 s, so on every platform a
    crash leftover under 3 s old beside an absent record makes the stat sites answer "cannot be examined" and not "absent" until it ages out. On `ENOENT`: a file that is never renamed over (a card claim) is absent, at the cost of one open. A staged file younger than 3 s
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
  - **One name for a staged file.** Every writer takes its staged name from `stagingPath` (`meta.json` stages as `meta.json.restamp`, as `restampWholeLocked` always has; a claim has none; everything else as
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
    their own counter, `gatePauses`), the Node `[3,3,1]` read counts, and Linux's zero waits for the reads (and zero stats for them; the stat sites take theirs without a pause,
    `TestStatSettledTakesNoPauseWhereTheGateIsOffAndWaitsWhereItIsOn`).
  - **The Windows rename measurements, against the fixed readers** (the release lane's harnesses, one host, run once each and bounded; the Windows gate runner did not answer ssh, so these ran on the workstation). The Go writer
    that imitates `renameReplacing` against the fixed Node reader, 150 s: 457,000 reads, 300,720 renames of which 1,787 were refused, 0 nulls. The same harness with the raw reader for 60 s, as a control: 244,000
    reads, 92,502 renames, 0 absent answers, so this host showed no episode in that run and the 0 above does not by itself show the gate worked. The release lane's 4000-loop copy of the stress test: 4,005 loops, 24,030
    file reads (the copy's diagnostic printed 48,060; see below), 9,252 renames (9,252 a different inode, 42 refused), 0 nulls. The lane saw 3 of 4 runs of that copy fail on the reader before the gate. The gate is shown by the injection tests above, not by these.
  - **The same measurements again, at the head of the audit round** (the readers unchanged since; one host, once each, bounded, the Windows gate runner again unreachable, so on the workstation with one reader
    process and one writer). The Go writer against the fixed Node reader, 150 s: 710,200 reads, 191,220 renames of which 7,230 were refused, **0 nulls**. The raw control, 60 s: 295,200 reads, 83,268 renames
    (2,714 refused), **1 raw `ENOENT`**: the live name absent for 16.0 ms, back after 241 re-reads. So this host did show the episode in the control (about once in 83,000 renames), and with the fixed reader
    710,200 reads over 191,220 renames, where two or three were to be expected, gave no null; one run each does not make that a rate. The release lane's 4000-loop copy of the stress test with the fixed readers
    copied in: 4,005 loops, 6,462 renames (66 refused), 0 nulls, 4.3 s. That copy's own diagnostic prints 48,060 file reads for those loops because it multiplies by 12; a loop reads 6 files, so the figure is 24,030.
  - **Known limits of the gate and of the rule.**
    - **The directory-listing blind spot.** `reader.v2` and `reader.legacy` and `Waiters` list a directory, and a listing taken in the instant of a rename does not show the name; an unlocked
      caller then sees that lease as not live. A grant or a sweep lists under the epoch lock, where no record can be in flight (its writers hold the lock and the sweep has removed their leftovers first) and the
      heartbeat is read by its name; a card claim, which is never renamed over, stands as the second arbiter; advisory callers (status, admission and routing checks) can still see the lease as free for that
      instant. Three listings that decide something were found open in review and are closed: `readLaunchMarkers` (a fresh staged file where a name is not listed is unknown), the keeper's scan of every heartbeat
      for a whole-node record (the same), and the release that names no epoch (`unreadableCardRecords` counts a fresh staged card record whose live name is not listed as a record that cannot be read, so
      `gpu release` and `ReleaseTarget` refuse where they released the other card lease, or said nothing was held). The release decides from a listing without the lock, and the keeper's scan, which the
      locked paths use, is a listing that a rename in flight blinds: those were the two places a locked re-read was not the safeguard.
    - **`Renew`'s in-place fallback.** When `renameReplacing` is exhausted, `Renew` writes the heartbeat in place (`os.WriteFile`, a truncate and then a write). A reader that meets it half written
      reads an empty or torn file, which the rule now ages by the file's time (fresh, so alive) and no longer by the acquisition stamp; it was not changed.
    - **The first publish of a file** (no live name yet, a staged file beside it) is waited for like a rename and is, until it lands, an I/O failure and not an absence.
    - **A heartbeat that is unreadable and unexaminable for good** (the read fails and so does the stat, with an error that is not an absence) is unknown, and unknown is never stale and, for the
      keeper, alive: only the wrapper being gone (pid and process start time) or `gpu release` frees such a holder if it is alive but hung.
    - **A wrapper that stops its own tree** spares another lease's ComfyUI instance only when it can read that instance's marker (`PIDsOfOtherLeases`): a marker it cannot read is not spared, the
      direction it read before; the other direction would leave the command of a lease that was lost running on the card. **A detached hold's drain and unload** are scoped by `leaseDevicesOf`,
      which reads a lease whose record is absent as the whole node and says so on stderr. A lease it does not find in the listing whose record cannot be read is not that (a later review: its own held lease
      read as not live while its record was mid-rename, and a seat on a card it does not hold was unloaded): it asks `RecordState`, lists again when the record is readable by then, and otherwise fails the
      maintain step with the lease held.
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
  - Left as it is, and in the known limits above: `PIDsOfOtherLeases`, where neither direction is safe. `keeperAlive`, the claim loop and `removeStaleClaim` are the same code path as the first fix.
    (`leaseDevicesOf` was listed here too, and a later review showed a safe direction for the case of a record that cannot be read; see the known limits.)
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
  - **The worst hold is bounded by a cap, not only by the order.** Before the cap it was N x 250 ms, once per file per 5 s per process: the nominal 2 s at eight files, past the measured 2.19 s at nine. The first two
    levers are in (the sweep above takes the record and restamp leftovers, and the memory of a wait that ran out, from the gate, makes it once per file); what remained was the heartbeat's leftover, which only
    `Renew`'s writer owns. What keeps it to milliseconds in practice is the order: the probe every grant makes without the lock judges ALL the leases, not only the conflicting ones, and pays the waits first
    (`TestTheProbeBeforeTheLockPaysTheGateWaitsSoTheLockIsHeldWithoutOne`: a card grant over live leases on other cards, and a whole-node grant over leases whose holders are gone, count the gate's pauses taken
    with the lock held and want none; `TestTheWholeNodeVerificationPaysAtMostOneWaitPerFile` counts one wait per file). A later review showed what the order does not cover: leftovers that appear between the probe
    and the lock, fresh (younger than 3 s) and unread for 5 s, on nine or more leases, held the lock past the spin, and the second acquirer failed with `the epoch lock ... did not clear` (a queued acquire left
    the queue with that error). So the hold is capped: the gate takes at most one second (`lockGateCap`, half the spin) of one hold of the epoch lock (`beginEpochHold`, registered by the cleaned lease directory in
    `withEpochLock`), and once a hold has run that long every further read of a lease file in that directory that would wait for a rename in flight answers the I/O failure at once. That is the answer a wait that
    ran out gives, and every decision under the lock reads it as held (unknown is never stale), so nothing is decided differently: the wait would only have turned "unknown" into a heartbeat that is fresh. The cap
    applies to the reads of the lease directory the hold is of, by whoever makes them in this process (a read in another thread during a spent hold gets the I/O failure instead of a wait, which every reader of it
    treats as held or unexaminable), and never to a read outside a hold. The launch markers live in another directory and are not covered. Red, at the unchanged code
    (`epochlock_cap_test.go`): `9 leases: the verification waited 2.25s on the gate with the epoch lock held, the spin of a second acquirer is 2s (450 pauses)`, `12 leases ... 3s ... (600 pauses)`, and for a card
    grant whose leftovers land after its probe `the grant waited 3s (600 pauses) on the gate with the epoch lock held`; after, green with three, nine and twelve leases, and the four tests of the cap's scope
    (a read outside a hold waits its whole budget, another directory's hold does not shorten a wait, the hold is forgotten when the lock is let go, and a lease directory spelled with forward slashes, a trailing separator or a
    "." element still meets its hold: a first form registered the directory as it was given and never met the clean paths read inside it). Not measured on a Windows host again: the figures above are the
    ones before the cap, and the cap is shown on the gate's clock.
  - **Known limits.** A lock broken as stale after 30 s (`epochLockStale`) lets a second holder in beside a first that only stalled; its sweep could remove the first's staged file, which fails that rename with an error
    and corrupts nothing. The 5 s memory is the gate's choice and is not a measured figure. The measurements are one host's.
- **A further review of this round found seven more places, and all are fixed red first** (six are below; the seventh, the cap on the gate time of one hold of the epoch lock, is the passage on the worst hold above).
  - **The keeper's scan of every heartbeat was blind to a heartbeat mid-rename (a second grant).** With the record of a whole-node holder old and unreadable, the keeper cannot name the epoch and lists the
    heartbeats of the lease directory (`keeperEvidence`, epoch 0). A listing does not show a name a rename is installing, and the scan skipped staged names, so a live holder whose heartbeat was mid-rename read
    as having no sign of life and its `meta.json` was removed by the claim loop. Red (`keeper_unknown_test.go`, at the unchanged code): `a second whole-node lease (epoch 3) was granted over a live whole-node
    holder (epoch 1) whose record cannot be read, while the heartbeat is mid-rename: its live name is not listed and its staged file stands in its place`. A staged heartbeat that is fresh and whose live name is
    not listed is now unknown, as a launch marker's is, and so is a listing that fails (`TestAFailedListingOfTheHeartbeatsIsUnknown`); the control with the heartbeat listed but unreadable passed before and after.
  - **A heartbeat that is a torn number was taken as the heartbeat.** The one non-empty tear of the 13 digits of a renewal is a prefix of them, which parses; `heartbeatAt` floored it at the acquisition stamp
    (hours old for an overdue lease) and the keeper noted it as an instant in 1970, so a fresh file with such a content read as a dead holder. No writer in the tree is known to produce one (`Renew`'s in-place fallback is a
    single write of 13 bytes after a truncate, which a reader should see empty or whole; a reader seeing a numerically valid prefix was not shown), so this was found by the stated rule being wider than the code,
    and it is closed because it could not be excluded. Red (`heartbeat_unknown_test.go`, `keeper_unknown_test.go`, at the unchanged code), for a seven-digit prefix, a two-digit prefix, zero and a negative number, on a card
    lease and a whole-node lease: `the lease of a live holder was granted to a second job (epoch 2) although its heartbeat file was written a moment ago and only its content is torn` and `Leases() = [] with a torn
    but fresh heartbeat, want [1]` (8 of 8); and in the keeper `a second lease (epoch 2) was granted on the card of a live holder whose record cannot be read and whose heartbeat is a torn number written a moment
    ago`, `the record of a live holder was swept over a torn heartbeat written a moment ago` and `the claims ... were swept` (2 of 2). A number below the acquisition (`heartbeatAt`), or below
    `minHeartbeatMs` (the keeper, which has no record to compare with), is no time of renewal and the file is aged by its time; the same content with a file ten minutes old still lets a lapsed lease go.
    This departs from the brief's "parses, then that value, unchanged".
  - **A release with no epoch could release the wrong card lease.** `gpu release` and `ReleaseTarget(0)` decide from a listing without the lock whether exactly one card lease is live. A record being installed is not in
    the listing, so with two leases held the other one was released, and its kept ComfyUI instances stopped, and with one held the answer was that nothing was held. Red (`release_midrename_test.go`, at the unchanged code):
    `ReleaseTarget(0) = (1, nil) with the record of lease 2 mid-rename, want a refusal: which lease is meant is not known`, `ReleaseByEpoch(0) = (true, nil) ...`, `the record of lease 1 was removed by a release of whatever
    is held while the record of lease 2 was mid-rename` and, with the only lease mid-rename, `ReleaseTarget(0) = (0, nil) ... want a refusal: something is held`. `unreadableCardRecords` now counts a fresh staged record whose
    live name is not listed, and both refuse with the error that names it. A named epoch whose record is mid-rename is still named by `ReleaseTarget`, which was not pinned
    (`TestReleaseTargetOfANamedEpochWhoseRecordIsMidRenameNamesIt`, a mutant of that line was green).
  - **A detached hold read its own held lease as not live and widened its drain and unload to the whole node.** `leaseDevicesOf` finds the lease by listing. Red (`gpu_drain_midrename_test.go`, root package, at the
    unchanged code; the record is mid-rename when the holder reads its scope and installed when it first talks to llama-swap): `detachMaintain succeeded while the record of its lease was mid-rename when it read its
    scope, want a failure that leaves the lease held`, `the detached holder took down [agent-pool gemma-4-26b-agent qwen3-vl-8b] ...` (the second is a seat on a card the lease does not hold) and `the holder widened
    its drain and unload to the whole node because its own held lease could not be listed`. It now asks `RecordState` for a lease it does not find. This one had been written down as a limit where neither direction is
    safe; for a record that cannot be read one is.
  - **The stress writer's staged file was as old as its seed (a false red on a correct reader).** A hard link has the time of its seed, which is the start of the run, so past the three seconds a reader counts a staged file
    fresh for, an absent-name episode of the writer's own rename read as a dead writer's leftover and the reader answered `null`. The default size finishes in about 0.6 s and was not exposed; a gated or loaded host, and
    `LEASE_READ_STRESS_SCALE`, are. Red: the seeds are made an hour old and the writer counts the renames whose staged file was older than 2 s, `684 of 684 renames installed a staged file that was already older than 2000 ms`;
    the writer now sets the time of the link to now before it renames it, and the count is 0. The release lane's own 4000-loop copy of the stress test was not changed.
  - **The stress writer renamed over a card claim, which no reader waits for (a second false red on a correct reader, found by running the fix above in long mode).** `LEASE_READ_STRESS_SCALE=12` (7,200 renames,
    2,509 loops, one host, once) answered null for the card claim twice (`readCardClaim` and `checkInheritedLease`), with no staged file stale and none missing. Go creates a claim once and deletes it, nothing is
    staged beside one, and a reader does not wait for an absent claim, so the absent-name episode of a rename over it reads as a lease that is gone. The same run with the claim left out of the renamed files: 3,391
    loops, 7,200 renames, 430 refused, 0 misses (once). Red: the writer counts the renames over a claim and the test wants none, `210 of 630 renames replaced a card claim`; the claim is read in the loop and no
    longer replaced. If the one episode seen is the rate (the claim took 210 of the 630 renames of a default run, about 1 in 2,400 here), about one default run in ten could have met it; one episode is not a rate.
  - **Pins added whose red is a mutant** (the code was right; one-line mutants of it left every test green). Each was killed on a gate runner by a mutant of the line it guards:
    the numbers of the gate by value, with their relations, and tied to Node's (`gate_constants_test.go`: `stagingFresh` at 2 s, 1 s and 300 ms, `absentBudget` at 20 ms, `absentMemory` at 1 s); a staged file of 1.5 to 2.9 s
    beside an absent name is still being installed, and an overdue lease whose heartbeat rename has retried for 2.3 s is kept; a rename that lands after 75 ms is waited out; the budget is a deadline and not a count of pauses,
    for a read, a stat and a read whose look keeps finding the name (`gate_deadline_test.go`, with a timer that sleeps four times what it is asked); the two ends of the wait memory (`gate_memory_test.go`);
    both arms of `statSettled` (`settled_arms_test.go`); the sweep of staged leftovers runs only while the epoch lock is held (`sweep_lock_test.go`); a heartbeat read that failed and whose file is then gone is
    unknown, in the reclaim rule and in the keeper; and every Go writer's `renameReplacing` takes a name `stagingPath` gave (`staging_drift_test.go`, parsed, where the line test saw only two spellings).
- **Not verified here.**
  - The 5 s memory of a refused file is a choice (long enough for the polls of a burst of acquirers, short enough that a new fault is waited on), not a measured figure, and the arithmetic of eighteen files
    is from the 110 ms a wait costs here. The real-ACL before and after runs are one host, a scratch test not committed, and they refuse a file for good; a foreign delete whose handle lingers was not
    reproduced (the harness's own deletes do not make one).
  - Node: the retry was never exercised by a natural refusal. Plain `readFileSync` missed nothing in about a million reads, so that Node can meet `EPERM` from a rename rests on the delete-pending probe and not
    on a miss, and the wait is shown by the injected refusal and a file held delete-pending, not by a natural rename-over. The fixed stress test does make the writer and the readers meet
    on Windows (the writer's renames are refused 3 to 37 times a run on this host, 14 to 30 in the release lane's runs), but not a reader's open be refused: no reader has missed. The 15.4 ms timer tick is one host's; a host where another process has raised the
    timer resolution ticks faster, and the deadline holds either way.
  - A copy of `gpu-lock.mjs` outside this tree (a media repository's own, which `gpu doctor` audits) now needs `lease-read.mjs` beside it, as it already needs `lease-progress.mjs` and
    `comfy-lifecycle.mjs`; a copy that has the format signature but not the new module fails at import. Not checked against any such copy.
  - The stress test ran on Linux under WSL (Node 22, seven runs: three of the first form and four after the writer observed each rename) before this was pushed, not on the CI image (Ubuntu, Node 24): that run is the first one there. Not run on macOS.
    The Windows runs were six, on one host (Node 26), three of each form; the host's own noise was not varied.
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
  - The read gate: no natural episode (an absent name beside a staged file) was met by a run of a fixed reader, so the gate is shown by injection and not by a live rename (the one natural episode seen,
    a raw `ENOENT` with the name absent 16.0 ms in 295,200 reads, was the raw control reader's, above); the full `internal/gpulease` package was
    not run on Windows (the Windows gate runner did not answer ssh), only the targeted tests of this change, three times each, on the workstation. The worst epoch-lock hold of a whole-node acquisition was measured once, on one
    Windows host, in a scratch test that is not in the tree, with a rename stall made by holding the destination open (a refused and retried rename of 119 ms), not by a natural episode, and not with N above eight.
  - `windowAndKill` still reads the lease twice. A lease that ended between the two reads (the 2 s render killed and released) would also show
    `got []`; it needs a stall of seconds between two adjacent statements and was not seen.

### Fixed - the vLLM seat launcher no longer loops on a pinned KV pool too small for SEAT_MAX_LEN (new SEAT_KV_NEED_GIB) and mounts the cache-server share at the store address on the pinned network, not a random one

Two production defects of `setup/templates/vllm-seat/seat_fg.sh` (registers B-54 and B-55), found on the reference workstation. The launcher is shell rendered
verbatim into the seat, so both fixes are one file; the tests run that file start to finish against stand-ins. No renderer and no installer changes.

- **B-54, the defect.** With `SEAT_KV_HEADROOM_GIB` set the launcher pins the KV pool (`--kv-cache-memory-bytes`) at the smaller free memory of the seat cards, less
  `SEAT_NONKV_GIB`, less the headroom, floored at `SEAT_KV_FLOOR_GIB` and capped at `SEAT_KV_CAP_GIB`, and it always passed `--max-model-len` = `SEAT_MAX_LEN`. A card
  holding ~2.2 GB of desktop graphics memory gave 1.95 GiB, floored to 2.0 GiB, and vLLM 0.28 refused every start: `2.66 GiB KV cache is needed, which is larger than the
  available KV cache memory (2.0 GiB)` for 163,840 tokens (it estimated 98,784 would fit). llama-swap restarts the seat on every request, so the seat looped: about 98
  failed starts in two days.
- **B-54, the fix.** A new optional knob `SEAT_KV_NEED_GIB`, the `N GiB KV cache is needed` figure vLLM prints in that refusal. With it set and the pinned pool below it,
  the launcher passes `--max-model-len -1` instead of `SEAT_MAX_LEN` and prints one `seat_fg:` line naming the pool, the need, `SEAT_MAX_LEN` and that the seat serves less than
  `SEAT_MAX_LEN`. Pool at or above the knob: nothing changes. Knob unset and the pool floored: `SEAT_MAX_LEN` is kept, because an auto-fit could exceed the length the operator
  set, and a WARNING names `SEAT_KV_NEED_GIB` as the fix (no behaviour change without the knob). A non-number is a warning and is ignored. The pin's banner line is unchanged
  and still printed first. The knob acts only while the pin is on.
- **B-54, why `-1` does what is claimed (read in vLLM 0.28.0, not assumed).** `vllm/config/model.py` accepts `-1` (`max_model_len: int = Field(default=None, ge=-1)`), keeps it as
  `original_max_model_len` and resolves the working value to the model's own derived maximum, so an auto-fit never exceeds the model. With `kv_cache_memory_bytes` set,
  `vllm/v1/worker/gpu_worker.py` skips memory profiling and returns exactly that value as the available KV memory (through `reserve_mm_ipc_gpu_memory`, which returns it unchanged
  for a model without multimodal config); `vllm/v1/engine/core.py` hands it to `get_kv_cache_configs`, which, when `original_max_model_len == -1`, calls
  `_auto_fit_max_model_len` with that value (`vllm/v1/core/kv_cache_utils.py`), a binary search for the largest length that fits it, and then runs the usual admission check at the
  fitted length. `vllm/v1/engine/core_client.py` copies the fitted length back to the front end. So the auto-fit is computed against the PINNED pool, not against
  `--gpu-memory-utilization`.
- **B-55, the defect.** The share is mounted from `//<name>.local/<share>` with `SEAT_L2_MOUNT_SRCADDR=auto` pinning the cifs source address, and the name goes through the system
  resolver, which returns ONE address. The store host has two mDNS responders for its name: one pinned to its wired NIC (the right answer) and another agent that answers on every
  interface with that interface's own address (a Wi-Fi address on the same /24, container bridge addresses, a VPN address). The mount landed on the Wi-Fi path (writes ~24 MB/s, so the
  launcher degraded L2 under `SEAT_L2_MIN_MBPS`), on an unreachable bridge address, or on the VPN address, which refuses the pinned source. Every start for three days ran without L2.
- **B-55, the fix.** For a cifs mount whose source host is a `.local` name and whose source address is pinned (`auto` or explicit), the launcher collects ALL the mDNS A answers for
  the name with a raw query to `224.0.0.251:5353` (QU bit set, sent three times in about a second, out of the pinned source address) and mounts `//<address>/<share>` at the FIRST
  answer inside the pinned source address's IPv4 network (the prefix is read from `ip -4 -o addr`), with the same options. One `seat_fg:` line prints every answer seen and the one
  chosen, and the L2 status file reads `ok <stamp> mbps=<n> addr=<address>` (the field is appended only when an address was chosen or read from an already-mounted share, so other
  seats' lines are byte-identical). If the
  helper fails, times out, finds no answer, or no answer is on that network, or there is no python, the mount is by name exactly as before and the line says why. Names that are not
  `.local`, address literals, nfs mounts and an unset pin never reach the lookup. The degrade messages keep their text (a Go test pins them) and name the address when one was chosen.
- **B-55, a share that is already mounted (migration).** The address is chosen when the share is MOUNTED, which is once per WSL session; llama-swap restarts the seat on a swap and the next
  start finds the share mounted. That start does not mount again, does not unmount it (another seat may be using the share, and with two addresses of the store on one network a new pick at every
  start could trade a good mount for the Wi-Fi one, so there is no automatic remount by design) and asks no mDNS. For a cifs share with a pinned source address and a `.local` name it prints one
  `seat_fg:` line with the share's source and server address (read with `findmnt -n -o SOURCE,OPTIONS -M <dir>`, the `addr=` option) and, when the share is mounted by name, how to choose again; the status
  file reads `ok <stamp> mbps=<n> addr=<that address>`; a write-floor or probe degrade appends `[the share was already mounted at server address <a>; a start does not choose the address again]`.
  **After deploying this, a share an earlier start mounted at the wrong address stays there** until it is unmounted while no seat uses it (`umount <SEAT_L2_MOUNT_DIR>`, then restart the seat), or the WSL
  distro is restarted. Not verified on the reference box: whether its share is still mounted today.
- **B-55, the bound.** The helper is a here-doc inside `seat_fg.sh` (no file to install), run with `$VENV/bin/python` or else `python3` under `timeout -s KILL 3`; it listens for 1 s on
  its own and ends. A start can therefore never wait on it for more than 3 s.
- **Limit, stated.** "The first answer inside the pinned network" cannot separate two addresses of the store that sit on the SAME network (a wired and a Wi-Fi address on one /24).
  The line printed shows every answer, and the operator's override is to name the wired address in `SEAT_L2_MOUNT_SRC` as a reserved or static IPv4 literal (mounted as given; never a DHCP lease) or as a
  name that is not `.local` that the box resolves to it (not looked up; a `.local` name pinned in `/etc/hosts` is not honoured, because the lookup asks mDNS directly). The query uses the legacy-unicast form (source port
  other than 5353), which the mDNS RFC makes every responder answer by unicast; a responder that ignores it would be a "no answer" and falls back to the name.
- **Test seams.** `SEAT_FG_TEST_MDNS_CMD` (a command standing in for the lookup) and `SEAT_FG_TEST_MDNS_DEST` (`host:port` for the helper's query) exist for the test and are documented
  in the script where they are read; nothing in a rendered seat sets them.
- **Tests (red first).** `setup/templates/vllm-seat/seat_fg.kv-and-store.tests.sh` runs the real launcher from a scratch copy against stand-ins for every command that touches the box
  (the MP server's unit is never stopped or started, nothing mounts or unmounts, no GPU is asked, nothing binds a port) and reads the argument list `vllm serve` was exec'd with, the mount command
  line, the printed lines and the status file: 16 KV cases and 33 store cases. The store cases include the embedded helper driven alone against a loopback responder that answers the way real ones
  do (unicast, query id and question repeated, compression pointers, other names and types mixed in, junk first, only the question it must ask answered), and against a simulated clock and socket
  (the default destination is the mDNS group and port 5353, 2 or 3 queries, a window of about 1 s, a lost first query still answered, an answer 0.7 s late still heard); a /28 and a /16 pinned address;
  and a share that is already mounted (left alone, described, never unmounted). On `origin/main`'s launcher 37 of the 49 cases fail and 12 are guards that pass on both sides (pool above or at the
  knob, knob unset and not floored, no pin, `SEAT_MAX_LEN` default; non-`.local` name, address literal, nfs, no pin; a mounted share with a non-`.local` name, with no pin, and an nfs one). Two Go wrappers
  (Linux) run it and name every case, and a portable text pin covers the hard bound, the scoping, the single `mount` command, the helper's default destination as the code line that reads it (the
  comment above the helper says the same words), that no line runs `umount`, and that the already-mounted report is the else branch of the mount check. Mutants of the launcher, each caught: the
  comparison turned round, `-1` never set, the warning never printed, number validation loosened two ways, the first answer taken whatever its network, the last on-network answer taken, no timeout
  around the lookup, no `.local` guard, no source-pin guard, the status line without the address, the mount ignoring the chosen source, the helper accepting any owner name, no de-duplication, no QU
  bit; after review also a wrong default mDNS port and a wrong default group in the code, a hard-coded /24 mask, a listen window of 0.1 s and of 0.5 s, a single query, the already-mounted report
  removed, the launcher unmounting, the status ignoring the address it read, the degrade note dropped, the advice to unmount printed for a mount made at an address, the report ignoring the `.local`
  scope. A variant that asks twice survives on purpose: the specification allows 2 or 3 queries.
- **Docs.** `docs/OPERATOR-GUIDE.md` and `docs/systems/cache-server.md` describe both behaviours; the template seat env lists the pin family as a commented-out block. No Go renderer
  emits any `SEAT_KV_*` line, and `SEAT_KV_NEED_GIB` stays that way on purpose: like the headroom, floor and cap it is a per-box measured figure (it depends on the card, the model and
  `SEAT_MAX_LEN`), so a rendered default would be a guess.

### Changed - every call a delegator makes to a fleet node provably carries the fleet bearer

- **What this is.** A node is about to require the fleet bearer on every lane it serves (the next entry in this release),
  so the order of the rollout is delegators first, nodes second. This entry is the delegators' half: no client code
  needed a change, because every client already set `Authorization: Bearer <fleet_auth_token>` whenever the token is
  configured, and what was missing was a test that says so. Each fleet client was read request by request: the media,
  compose, vision, text, stt and accelerator clients (dispatch, job poll, output fetch, and the input-carrying media-job
  and compose-project doors), the delegator (agent dispatch, job poll, withdraw, queue submit, queued-job poll, node
  health view), the roster probe and its token probe, the fleet view's poller, the PAIR relay and the node's own claim
  loop. No `render/` script and no integration talks to a node directly.
- **Pinned, per client.** A new `TestEveryRequestCarriesTheFleetBearer` in each client package drives that package's
  request builders against a recording node (`rostertest.AuthNode`) for every task type and door the package sends, and
  fails unless the node received at least one request and every one carried exactly `Bearer <token>`.
- **Pinned, for clients that do not exist yet.** `TestEveryFleetClientRequestSetsTheBearer` (root package) reads the Go
  source of every fleet client package, and of any file that names a `/fleet/` route, and judges each request on its own.
  It fails on a request built by `http.NewRequest` or `http.NewRequestWithContext` that no bearer-setting call reaches (a
  second request in the same function needs a call of its own, and so does a name that is assigned again); on a call to
  `http.Get`, `http.Post`, `http.Head` or `http.PostForm`, or to the same four methods on an `*http.Client`, which leave
  nowhere to set a header; and on a `Do` of a request that was not built there, was not handed in, and carries no bearer
  call. A bearer-setting call is `Header.Set` or `Header.Add` of `Authorization` with a value that begins `Bearer ` on that
  request, or a call to a function of the same package (or of another package of the module) that does exactly that to
  the request it is given. A call named `auth` counts only if the function it names does that; a string that says
  `Authorization`, a header set on a different request and a value that is not a bearer count for nothing. A request that
  deliberately goes without is listed in `fleetBearerAllowlist` with the reason (the node's own forward to its local
  engine, the identity read of a cascade base that may not be a fleet node, PAIR's loopback endpoints, the terminal
  view's read of the dashboard it runs against); a stale entry fails too, and a second bare request in an allowed
  function is a new site. The lint follows a client through the names it is held under (a parameter, a field, a package
  variable, a package variable that re-exports another package's client, a function that returns one, and a local that
  copies, points to or dereferences any of these), not through types, so a client held under a name it cannot tie to
  `*http.Client`, and a request handed over through an interface or a function value, are not seen. The shapes it must
  report are fixtures in `testdata/fleetbearerlint/red` (27: each shorthand on the package and on a client, on a re-exported
  client, on a client a function hands back and on a local copy of one, a `Do` of a request built by a helper or in its own
  argument, a second request left bare, a helper named `auth` that sets nothing, a helper of another package that sets
  nothing, a stray `Authorization` string, a header set on another request, a value that is not a bearer), and the requests in
  `testdata/fleetbearerlint/green` must pass (a helper of another package that sets the bearer included); one test holds the
  lint to both.
- **Mutant.** Removing the `auth` call from the text client's poll turned both its package test and the source test red. Two more mutants turned the source test red: the accelerator client's `auth` helper setting another header, and the delegator's withdraw request no longer setting the bearer. A `Get` in place of the `Do` in the media client's JSON read, and a `Post` in place of the `Do` in the chat client's node lane, which hold their client as a re-exported package variable and as a local copy of a field, turn it red too (the lint that followed names without copies let both through). One-line changes to the lint itself (functions that return a client, a `Do` on an inline request, a helper of another package, the field rule, the package-variable rule, the dereference and pointer rules, and the package-variable resolution cut to one pass) each turn the fixture test red.
- **Not verified here.** No live node was called; every node in these tests is an in-process loopback server.

### Security - a fleet node requires the fleet bearer on every lane, and a run-graph manifest may name only pinned node packs and models under models/

- **The defect.** The node gated the agent, vision, text and door lanes on the fleet bearer and admitted every other task type
  with no check: `image-gen`, `video-gen`, `animate`, `audio-gen`, `run-graph`, the legacy `stt` lane when no token was set,
  and the poll of any such job. A `run-graph` manifest names `node_packs`, which the node `git clone`s and pip-installs into
  its ComfyUI environment, so any peer that could reach a node's listener beyond loopback could make the node run code of its
  choosing, with or without a token configured.
- **Every lane is gated.** `tokenGated` returns true for every task type (the special case for the legacy `stt` lane is gone).
  With a `fleet_auth_token` a request without the bearer is `401`; a poll or withdraw of any job, a re-dispatch of a known job id
  and every read of `GET /fleet/media/{name}` (whatever the name: a name with a separator, a dot-dot or no name at all is
  `401` for a caller without the credential, because the check runs before the name is validated, and is `400` only for a
  holder) follow the same rule, and the jobs feed omits every row's `error` text without it.
  The check runs first, before the request body is read on `/fleet/dispatch`, `/fleet/vision` and `/fleet/text` (a caller
  without the credential costs the node no buffering, and a malformed body, a wrong content type or an oversized body is `401`,
  not `400`), before the envelope and task validation and before the task type is looked up, so such a caller learns nothing
  about the node's lanes. `admit` keeps its own check as a backstop.
- **Every refusal of a credential is journaled, at every door.** The dispatch, vision, text, stt upload, PAIR relay, chat, kvslot,
  compose-project and media-job doors, the poll and withdraw of a job, the artifact read and the five queue routes each write one
  line per refused request (subject to the throttle) and keep the status and body they answered with. The line carries the
  caller's address, the door (the task type for a lane, `dispatch` for `/fleet/dispatch`, whose task type is in a body the
  node no longer reads for a caller without the credential) and the reason (no bearer, a wrong one, or no token configured),
  never the credential or the payload. Before this, only the lanes that go through `authorizeGated` wrote a line; chat, kvslot,
  compose-project, media-job, poll, withdraw and the queue routes answered `401` in silence. A closed door (`403` for
  compose-project and media-job on a node that has not opted in) is not a credential refusal and writes no line. The journal
  cannot be grown by a caller: the throttle is keyed on the caller's address and the reason, shared by all the doors, never
  on the door or the task type (which the caller chooses), at most twenty lines are written a minute across callers, and the
  first refusal after that minute writes one line counting the rest. A line is the door, the caller's host and one of three
  constant reasons and nothing else, whatever the request carried. `/fleet/health` and the jobs feed are open on purpose, so a
  request with no bearer there writes nothing; one that presents a bearer which is not the node's writes the same line (door
  `health` or `jobs-feed`) under the same throttle and is still answered the reduced view.
- **Two answers can come before the `401`.** A door that is closed on the node (`compose-project` and `media-job` when the
  node has not opted in) answers `403` before the bearer, as it always did; health already publishes whether each is open, so
  the `403` tells a caller nothing it cannot read there. Under `fleet_ingress_guard: enforce` the guard answers a wrong content
  type `415`, a `Host` outside the allowed names `421` and an `Origin` header `403` before any route runs; in the default `warn`
  mode it only logs, and the `401` comes first. Both are in the docs now.
- **Choose a long random token.** The bearer opens every lane and a node does not slow repeated wrong bearers, so
  `docs/FLEET-NODE.md` and `docs/OPERATOR-GUIDE.md` tell the operator to generate the value with `openssl rand -hex 32`; the
  installer and the config check accept any non-empty value, as before.
- **Loopback stays tokenless; everything else fails closed.** A loopback node with no token serves as before, with a
  byte-identical ack and poll (pinned). A node that listens anywhere else with no token refuses every task type with `403 fleet
  lanes require fleet_auth_token on a non-loopback listener` (the status is unchanged; the sentence no longer says "agent lane"),
  and `fleet-serve` refuses to start there, naming `fleet_auth_token`, the listen address and the config file it read (or that
  none was found), so a node that will not start says which file to fix. The startup line now says which
  lanes are open: `loopback only: every fleet lane is tokenless on <addr>`, or `every lane requires the fleet bearer`. The
  `UNAUTHENTICATED` warning is gone because it is no longer true. `local-offload doctor`'s token rows say the same.
- **Health stays open, with its private fields withheld.** Without the bearer, on a node that has a token, `/fleet/health`
  still carries identity, versions, capacity, counts, lanes and model names, and drops the lease's free-text `reason`, the store
  steward's status (its `root` is a path on the node's disk) and the weight-file rows (`image_recipes`, `media_recipes`). A
  bearer holder, and a caller of a loopback node with no token, read the whole answer.
- **A token holder can have the node render, not run code.** A manifest may name a node pack only when its `repo` AND `commit`
  are a row of `render/node-packs.json`: VideoHelperSuite, ComfyUI-MultiGPU at its documented archival commit (upstream and
  mirror) and ComfyUI-GGUF. The match is exact (no case folding, trimming, `.git` suffix or abbreviated commit), keys are read
  as the runner reads them, and the directory name must be the row's or the one the repository gives. The node refuses at the
  door with `400` naming the pack, before a job, a temp file or a lease exists (`mediacap.CheckManifestNodePacks`, called by
  `buildRunGraph`); the runner refuses again before its first tooling probe, model download or git command (typed defer
  `NODE_PACK_NOT_ALLOWED`; a table that cannot be read refuses every pack). The Go table is a copy pinned to the JSON by a test.
  Adding a pack is a reviewed edit to both.
- **A manifest's models land under `models/` and come from a listed host.** A manifest's `models[]` makes the node download each
  `source_url` to `path` inside the ComfyUI tree before ComfyUI starts, so a `custom_nodes/` path, a `..` climb, an absolute
  path, a drive or a stream name would place code where ComfyUI loads it or write outside the install. An entry is taken only
  when its path is `models/<class>/<file>` (backslashes read as separators; no empty, `.` or `..` segment; nothing beginning
  with a space or ending with a space or a dot; no control character or `:*?"<>|`) and its `source_url`, when it has one, is
  `https://<host>/...` on a plain DNS name that is, or is a subdomain of, a `model_hosts` entry of `render/node-packs.json`
  (`huggingface.co`, `hf.co`, `civitai.com`, `github.com`, `githubusercontent.com`; no user name, port or IP literal). The node
  refuses at the door with `400` naming the entry (`mediacap.CheckManifestModels`); the runner refuses again, before it looks
  for, hashes or downloads any file, with the typed defer `MODEL_NOT_ALLOWED`, having read every entry before provisioning the
  first. One table of paths and sources drives both tests. A `sha256` is not required, because the manifest carries its own hash
  and one the caller supplies authenticates nothing. A workflow whose models come from another host is refused until the file
  gains a row. A `models[]` entry with no `path` is refused at the door as well (the runner's parser already refused it).
  Left open: a holder can still put a file of any type under `models/<class>/` (a weight file in a pickle-based format is
  code when ComfyUI loads it), and the node follows redirects from a listed host.
- **Rollout.** Delegators first, then nodes. Every fleet client in this repo sends the bearer whenever a token is configured
  (the previous entry pins it), but a node that gains the rule refuses three kinds of caller, and they must be put right
  first. A delegator with no `fleet_auth_token`, or a wrong one, a media-only box included (media, stt and compose-video calls
  needed none before), gets `401` on every dispatch, poll and artifact fetch; the media client reports that as a `config`
  defer, and the node's `/fleet/health` reaches it without the lease reason, the store and the weight-file rows, so its node
  view degrades with no error. A delegator older than the first release whose client sends the bearer on a lane has no client that
  sends it: withdraw 0.146.0, text 0.154.0, compose 0.163.0, stt 0.165.0, the roster probe's token check 0.167.0 and media 0.173.0. Software
  outside this repo that sends no bearer stops working on every lane. The delegator change is deployed fleet-wide first; before
  the node change is deployed to a node, `fleet_auth_token` must be present in the config its `fleet-serve` reads. A node that listens beyond loopback with no
  token will not start after the upgrade, so give it the token first. `setup/install.sh` takes `--token-file PATH` on a node
  install, stores the key in the node's config (readable by its owner only, handed to the `--user` account with `sudo chown`
  when that is not the installing user, never printed) and refuses, before it writes anything, to register the unit without
  one, so a fresh install no longer crash-loops; the Windows launcher template says the key must
  be in the config before the task is registered. ADR 0023 carries the amendment, and `docs/FLEET-NODE.md` and
  `docs/OPERATOR-GUIDE.md` the new rule.
- **A node swap refuses a node that could not start afterwards.** `node-swap` reads the config it resolves and records only
  whether `fleet_auth_token` is there; the engine refuses, as its first step and on a dry run too, a node whose health URL is
  beyond loopback and whose config holds no key (the step is named `fleet-key`, the message names the key and the config). A
  loopback or standalone node is not checked; `--skip-fleet-key-check` (`-SkipFleetKeyCheck` on the Windows launcher) leaves
  the check out for a box whose config is not the node's.
- **Tests, red first.** The failing tests were committed before the change: tokenless remote `run-graph` (401, logged), the
  fail-closed `403` for every task type, loopback with no token allowed, the bearer holder admitted with unchanged bytes,
  unpinned and wrongly-pinned packs refused before any clone (Go and Node, the satisfier asserted never to reach the checkout
  or build a clone command), startup refusal, health without private fields, media files gated; then, in a second red series,
  hostile model paths and sources refused (Go door and Node satisfier, from one shared table), the refusal naming the config
  file, the refusal journal bounded against a caller who varies the task type, and the installer's key precondition. Mutants
  each turn their test red: the `tokenGated` widening removed, the Go pack check removed, the Node pack check removed, the
  startup refusal removed, the Go model check removed, the Node model check removed, the config path dropped from the refusal,
  the journal's task-type key restored, the node-swap key check removed. A third red series pins the order and the journal:
  a counting body reader that must be read zero bytes of a 20 MiB body on `/fleet/dispatch`, `/fleet/vision` and `/fleet/text`
  without the credential (the node read 1048577 bytes on dispatch and text and 8065537 on vision before it answered), a malformed, wrong-type and oversized body without the
  credential that must each be `401`, the answers a bearer holder and a loopback node with no token still get, and a table over
  every door (a node with a token, no bearer and a wrong one; a node with none beyond loopback) asserting the door's status and
  body and exactly one journal line, with the throttle shared across the doors. Mutants each turn their test red: the check
  taken out of the text route, the withdraw's and the queue's and the no-token refusal's line dropped, the throttle keyed on
  the door again, `admit`'s backstop removed.
  Two more tests pass on the code as it stands and are pinned by mutants instead: the jobs feed shows a failed media job's
  error text only to a reader with the bearer (the job goes through the real dispatch door; narrowing the mask to agent runs
  turns the test red), and no door copies a presented credential into its journal line (a wrong bearer and two malformed
  headers sent to every route of the door table, to a node with no token beyond loopback and to the re-dispatch of a known
  job, with the line and the answer searched for the presented value and for the node's key; appending the header to the line
  turns every case red).
  A fourth series pins what the third left to the code alone, and each of its tests passes on the code as it stands and turns
  red under a one-line change: the media route's order (a name that is not a bare name, without the bearer, was `400`
  before the bearer was looked at; it is red first, then fixed); the journal's throttle key against every field a caller
  chooses (the User-Agent, the presented bearer or scheme, the forwarding headers, the host, the query, the source port,
  the job id and task type; a key that followed any of them lets one caller spend the whole minute's lines); one note per
  refusal, counted through the journal's own tally because the throttle hides a second note; the window's summary line
  carrying no presented credential; and `gatedJob` for every task type, with a failed run-graph job hidden on the poll and the
  feed (leaving one more task type out of it turns the test red).
  A fifth series: every door that takes a body (the chat lane, both kvslot routes, the PAIR relay, the compose-project and
  media-job doors, the stt upload, the four queue routes with a body, and dispatch, vision and text) is sent a 20 MiB body
  without the bearer, with a wrong one, and on a node with no token beyond loopback, and must have read none of it (reading the
  body first in the chat, kvslot, PAIR relay, compose-project doors and the queue guard turns every one of those cases red);
  a request whose headers and path are the caller's own text is answered with a journal line compared whole against the
  exact shape (a mutant that appends the User-Agent, URL and X-Forwarded-For turns every door red); a wrong bearer at
  health and the jobs feed writes one throttled line, shared with the other doors, and none when there is no bearer, the right
  one, or no token (red before the change: 0 lines where 1 is wanted); and the bearer lint has a fixture for a client that sends
  its first request bare and sets the bearer only on a second request under the same variable name (it goes unreported when
  the lint stops ending a request at the next assignment of its name).
  A sixth series pins the wording: the refusal of a composition's own `html` or `project_dir` must name the fleet bearer and a
  vetted template and must not call the door open (red before the change: the text said media dispatch is not token-gated), a
  table of sentences that no longer hold must be absent from the files that carried them, and a decision record that still
  calls a lane tokenless must carry the amendment section that links ADR 0023's.
- **Every description of the node says each lane needs the bearer.** The error a caller reads when it sends a composition's own
  `html` or `project_dir` over the fleet says compositions are trusted code, that the dispatch door renders only the node's vetted
  templates, and that a whole project goes to a node that opted in to the project door. The code comments, the node guides, the systems pages and the decision records that called
  a media door open, said health ignores the bearer, or named the agent and vision jobs as the only gated ones now say what
  the node does, and every decision record that calls a lane tokenless or open carries the dated amendment that points at ADR 0023's.
  Nothing else about the node changes.
- **Pins to confirm.** The MultiGPU pin is the documented one. The VideoHelperSuite and GGUF commits are the ones one reference
  node's checkout was at, not documented anywhere (`"pin": "unverified"` in the file); confirm them against the fleet before a
  workflow depends on them, and add a row for any other pack a workflow needs.
- **Not verified here.** No live tailnet node was reached and no real clone or pack install ran; every node in these tests is
  an in-process loopback listener, and the runner's satisfier ran against injected git, tooling and filesystem fakes. A
  workflow whose manifest names a pack outside the table, or a model outside `models/` or from a host outside the list, is now
  refused until the table gains a row. The installer's key handling ran against a stub binary and a temporary prefix; no unit
  was registered, and no node was swapped (the swap check ran against fakes, and the launchers' forwarding is a source test).
  A node that was installed before this change has its unit and config as they were: the token has to be in its config before
  it is upgraded, or it will not start. That a ComfyUI imports a `custom_nodes` package at start-up was not run here; the model
  rule closes the write whatever it does.

## [0.189.0] - 2026-10-17 - result-cache keys are length-prefixed and typed so no request can be served another request's cached answer, a stale stall-timer callback files nothing against the next phase, a spent post-ready bound reads the engine before the probe, a lint keeps sub-second wall-clock ceilings out of the Go tests, and CONTRIBUTING's seam-test rule

### Added - CONTRIBUTING says a seam test must not inject the seam it certifies

A test that certifies a wiring (a function variable, an interface, a config key or a door that hands work from one part of the harness to
another) proves nothing if it assigns a stand-in to that very seam and asserts on the value it just put there: it passes with the
production wiring deleted. A mutation of that kind has survived a green seam test in this repository before, and the red-first practice of
this series (a behaviour-free stub first, then the failing test) makes the rule more relevant, not less.

- **What changed.** `CONTRIBUTING.md` gains a section, "Tests that certify a seam", between "Proving a test can fail" and "Keeping tests out
  of the OS temp dir". It states the rule and three working habits: fake the far side of the seam and never the seam; assert on something the
  production path produces; show that the test goes red with `scripts/mutation-check.sh` and quote the `RED` verdict in the pull request.
  It names `TestRunNoEligibleRemoteNamesTheRealCause` in `internal/delegate/run_test.go` as the worked example. It ends with one line for a
  reviewer: for each test that claims to cover a wiring, ask which production line it goes red without.
- **What did not change.** No code, no test and no lint. There is no mechanical gate for this rule: a source lint for "a test assigns a
  seam variable and asserts only on the value it injected" would have to decide what counts as a seam, which only the reader can, so the rule
  lives where contributors and reviewers read it.
- **Checked.** `go test -run 'TestDocsLint|Changelog|Fragment' .` and `go run ./cmd/changelog check` pass; the section was read back
  against the example it cites.

### Fixed - the result-cache key can no longer be forged from request text: every part is length-prefixed, callers pass each ingredient as its own part, the params part is typed, and every existing cache key changes once

- **One-time effect on upgrade: every existing result-cache key changes, so the cache starts cold.** The first call of each kind after the upgrade misses and
  runs the model, then stores its result under the new key. The cache itself needs nothing: it is a transparent accelerator, a miss does what the request would have
  done with no cache at all, and there is nothing to configure or migrate. (Some output file names and exemplar `params_key` values change once too; the
  `StableParamsKey` entry below says which.) Entries stored under the old keys are never looked up again, and the cache has no
  eviction, so they stay in the cache file as dead weight until the file is removed. (The ask lane's own cache lives in memory for the life of one server process,
  so it has nothing stored to orphan.)
- **The defect (issue #4).** `cache.Key` joined its parts with a NUL byte and hashed the result, so `Key("a", "b")` and `Key("a\x00b")` were one key. One part of a
  key is arbitrary request text, and text that carries a NUL can forge the boundary between two parts, so a request could be served a result that was stored for a
  different request. The empty cases collided as well: `Key()` and `Key("")`, and `Key("", "")` and `Key("\x00")`, were each one key. `TestKeySeparatorCollision` had
  pinned the collision as known behaviour, with a `t.Skip` that was to fire once the encoding was fixed.
- **The encoding (`internal/cache`).** The preimage is a version tag, `offload/cache.Key/v2`, then for each part its byte length as 8 bytes big-endian and its
  bytes; the key is the hex SHA-256 of that. A length prefix is self-delimiting, so no byte inside a part can be read as the boundary between two parts, and the
  empty list, one empty part and two empty parts are three keys. The doc comment on `Key` states the encoding, why a separator cannot work for text that may hold
  any byte, and the rule for callers: pass each ingredient as its own part and never glue fields into one part with a separator of your own. The same rule is in
  `docs/systems/offload-pipeline.md`, under "How a result-cache entry is keyed". `Cache.Put` refuses an empty key (`ErrEmptyKey`): no real key is empty, and the
  pipeline uses `""` for "this call has no key and must not be cached".
- **The same defect one level up (`internal/pipeline`).** Length-prefixing cannot repair a part that a caller built by gluing fields, because the encoding cannot
  see inside a part. Five key ingredients were built that way, and each is now separate parts or goes through `cache.Key`, so the whole key has one framing and not
  a second one:
  - `runVisionGen` keyed `req.Input + "|" + the media identity` as one part, and the identity is the only carrier of the source file's content, so `("a|b", "c")` and
    `("a", "b|c")` named one entry and one file's cached description could be served for another. They are two parts now (`visionKeyFor` in `mediagate_test.go`, the
    test mirror, follows).
  - `cacheKeyForTierRender` folded the render into the model name (`model#render`); the render is a part of its own, so a model named `x#y` and the model `x` rendered
    `y` no longer share an entry.
  - `exemplarCacheTag` ended each exemplar field with a NUL, and an exemplar's input and output are past request and answer text that can hold one, so
    `(Input "a\x00b", Output "c")` and `(Input "a", Output "b\x00c")` had one tag while the prompts they inject differ.
  - `templateCacheTag` joined the system prompt, the grammar and the user template the same way. Both tags are now `partsHash`, which is `cache.Key` over the fields.
  - The transcription key is a named function, `transcribeCacheKey`, taking the content digest, model, language and protocol as parts, with the model once (the old
    key carried it twice). The joined `digest|model=...|lang=...|proto=...` string stays only as the input of the on-disk media stem, so no output file is renamed.
    This seam was not reachable end to end: the old key also carried the model as a part of its own, and the digest is a hash of the file. It is hardening.
  - `askcache.Key` keeps its own per-field length prefix as a second layer, so its keys do not rest on `cache.Key` alone. Its comments changed (and the comment in its
    test that said `cache.Key` joins with a NUL), and its test gained one assertion: `Key("ab", "c")` against `Key("a", "bc")`.
  - One glued part is left on purpose: the video media identity (`vid:<digest>|fps=...|n=...|w=...|frames=...`, and the per-window `vidw:` form) is built from the file
    digest and numbers and passed as one part. None of it is caller text, so it cannot forge a boundary; the documentation names it as the exception to "every ingredient
    is its own part".
- **The params part is typed, and params that cannot be keyed skip the cache (`internal/tasks`, `internal/pipeline`).** The result-cache key carried the params as
  `StableParamsKey` printed them, and that prints nested and non-string values with Go's `%v`, which is type-erased and space-separated. Two extract requests whose
  schemas differ only where `%v` cannot see it, a `required` list naming `"a b"` instead of `"a"` and `"b"`, or a `minimum` that is the string `"1"` instead of the
  number 1, had one key, so the second was served the first's cached answer before its own schema was ever checked (a cached reply is validated only on a miss). The
  key now carries `tasks.CacheParamsKey`: the params as JSON with the keys of every map sorted at every depth, typed, so `"5"` and 5, `["a","b"]` and `["a b"]`, a string
  and the bool it spells all differ, while an int 5 and the float64 5 the MCP door decodes, or a `[]string` and a `[]any` holding the same strings, still agree. It is used
  for the result cache only: Run, RunTier, the vision path and the transcription key. **It fails closed.** Params with no faithful typed form (NaN or an infinity, a
  function or channel, a struct, a byte slice, a type with its own JSON or text form by any of `MarshalJSON`, `MarshalJSONTo`, `MarshalText` or `AppendText` on the
  value or the pointer receiver, a `json.Number` that is not a number, a string that is not valid UTF-8, a self-referencing value) make that call neither read nor
  write the result cache: it runs the model every time, logs one line, and sets `cache_bypass` ("params: ...") on the result's meta, which `Run`, vision and transcribe
  write to their ledger row (`RunTier` writes no ledger row, so there it is on the returned meta only). The reason names the params key and a kind, never a value. Such a
  call is not harvested as an exemplar either: the exemplar row prints the params with `%v`, which has no cycle guard, so a self-referencing value would have crashed
  the process after the model answered. There is no fallback to a `%v` rendering or to `StableParamsKey`, because that would hand the call a key a different request
  can reach. Nesting is bounded at 128 JSON levels.
- **The params string for file names (`internal/tasks`).** `StableParamsKey` renders `<key>=<value>;` per entry with nothing to tell a structural `=` or `;` from one
  inside a key or a value, so `{"a": "1;b=2"}` and `{"a": "1", "b": "2"}` both rendered `a=1;b=2;`. It names the render-, video-, animate-, inpaint-, upscale-, edit- and
  audio output files and is recorded as `params_key` on the persisted exemplar rows, so it stays, with the escaping. Each key and each rendered value now has `%`, `;`
  and `=` written as `%25`, `%3B` and `%3D`, in one pass (`%` is the escape character, so it is escaped too; that is what keeps a value that spells `%3B` apart from the
  value `;`). Backslashes are left alone. **Every top-level entry is now unambiguous; nested and non-string values keep Go's `%v` rendering**, so `{"seed": "5"}` and
  `{"seed": 5}`, or a slice holding `"a b"` and a slice holding `"a"` and `"b"`, still render alike. That limit is documented and pinned by a test, because lifting it would
  give every params map with such a value a new file name; it now matters only for file stems and exemplar rows, since the result cache no longer uses this string. The
  escaping does not make output file names injective either: a stem is a hash of the prompt followed directly by this string, with no boundary between the two, so a prompt
  and a params string can still run together. That predates this change; a stem collision only reuses or overwrites a file under a truncated 32-bit name.
  **The output is byte-identical to the old format for every params map whose keys and rendered values contain none of `%`, `;` and `=`** (prompts, numbers, bools, paths
  of either slash direction, nested maps and slices of those), so the output file names and recorded keys of ordinary params do not move, and neither do those of the
  image, edit and upscale params that carry Windows paths. A key or value that does contain one of the three, such as a negative prompt with a semicolon or an equals sign,
  a URL with a query string (`?a=b`), or a percent-encoded URL, renders differently from before: its default output file name changes once (an earlier file under the old
  name is not reused), and its `params_key` on newly written exemplar rows differs. Nothing reads `params_key` back for matching today.
- **Tests, red first.** A test that pins a defect was written before its fix and failed on the old code; a test that pins behaviour the code already has (the
  ordinary-params table, the documented limit, the encoding values, the digit-part entries, the empty-key refusal) is shown red by a mutation instead, listed below.
  - `internal/cache`: `TestKeySeparatorCollision` no longer skips. It asserts 28 named part lists give 28 distinct keys (the NUL cases, every empty-part case, a part that
    spells out a length header, a part that spells out the frames of another list, and two pairs of lists that a decimal length with no delimiter would read as one:
    two digit lists, and a pair whose only long part is ten characters with no digits after the first), names the
    headline pairs as separate checks, and checks that its colliding pairs really did collide under the old `strings.Join(parts, "\x00")`, so the table cannot go stale.
    `TestKeyIsInjectiveOverSmallLists` checks all 2,380 lists of up to three SHORT parts over `{a, NUL, \x01}` (what it cannot reach, a part of ten characters or more, is the named
    decimal-length entries above). `TestKeyEncodingIsPinned` holds four keys to values computed outside the package, so a change to the tag or the length width is a visible edit.
    `TestEmptyKeyNamesNoEntry` holds the empty-key refusal.
  - `internal/pipeline`: `TestRunVisionGenKeepsInputAndIdentityApart` drives `runVisionGen` itself with inputs whose old glued strings are equal; the second call must
    miss, the model must run twice, two entries must be stored, the answers must differ, and a genuine repeat of either must still hit.
    `TestTierKeyKeepsModelAndRenderApart`, `TestExemplarCacheTagKeepsFieldsApart` (tag and key level) and `TestTemplateCacheTagKeepsFieldsApart` (tag level, and key level for the case where
    the grammar agrees) cover the render and the two tags. `TestTranscribeCacheKeyKeepsFieldsApart` has three cases; two fail on the old key, and the third (model text imitating the digest
    boundary) passes on it because the old key repeated the model, which is what the case now holds to the framing. `TestRunTranscribeStoresUnderTranscribeCacheKey` drives
    `runTranscribe` on both protocol legs (whisper, and an HQ model on the OpenAI shape, which keys with no language) and ties the entry it stores to `transcribeCacheKey`; that is what makes the call site a pinned seam (the function's own test cannot see which key the call
    site builds: with the glued key put back at the call site every other transcription test stayed green).
  - Typed params, driven through `Run` (`internal/pipeline`): `TestRunKeepsTypeErasedParamsApart` stores one request and asks the other for both release-review pairs
    (`required` and `minimum`); the asking request must miss and the model must run again. It fails on the old code with "A was served B's cached answer". The
    fail-closed behaviour is pinned the same way for `Run` (NaN, an infinity, a function), `RunTier`, the vision path and the transcribe path: two identical calls both run the model,
    no entry is stored, and `cache_bypass` says why; the same request with keyable params is cached and hit, so the tests are not vacuous. They fail on the old code, where the
    second call is a cache hit. `TestRunDoesNotHarvestParamsThatCannotBeKeyed` sends a map and a list that hold themselves through `Run`: no crash and no exemplar row,
    while the same call with keyable params is harvested. `TestRunCacheBypassNeverEchoesAParamsValue` sends an invalid `json.Number` and checks that its text is in
    none of the log, the meta and the ledger.
  - `internal/tasks`: `TestCacheParamsKeyKeepsTypesAndStructureApart` (eight pairs, each checked against the old `%v` rendering first),
    `TestCacheParamsKeyIsStableAcrossRepresentations`, `TestCacheParamsKeyIsPinned` (exact strings) and `TestCacheParamsKeyFailsClosed` (twenty-four unkeyable values, each an error that
    names a kind and never a value, by every route to an error: the walk, the `json.Number` check and the encoder; a 100-level object stays keyable), and
    `TestCacheParamsKeyRejectsAV2OwnForm` (a type whose only own form is the v2 `MarshalJSONTo`, which `json.Marshal` honours on Go 1.27; it first checks that the
    encoder really renders it its own way). For `StableParamsKey`: `TestStableParamsKeyKeepsEntriesApart` (three pairs, each checked against a copy of the old rendering),
    `TestStableParamsKeyEscapesWhatItMustEscape` and `TestStableParamsKeyIsInjectiveOverASmallAlphabet` (all 93,051 params maps of up to two entries over `{a, %, =, ;}`, strings
    up to two characters) fail on the old format. `TestStableParamsKeyIsInjectiveOverEscapeSpellings` covers what that alphabet cannot reach, a value that spells an escape: all
    160,000 single-entry maps with keys and values of up to three characters over `{a, %, ;, =, 3, B, D}`. `TestStableParamsKeyOrdinaryParamsAreUnchanged` holds a table of
    ordinary maps, Windows and UNC paths and values with backslashes among them, to hard-coded expected strings written from the format and not computed by the function; it
    passes on the old format and the new, which is the promise that file names and recorded keys did not move. `TestStableParamsKeyNestedAndNonStringValuesKeepGoRendering`
    pins the documented limit above (three pairs that still render equal); it goes red if the nested rendering changes, so that change has to be deliberate.
  - `TestRunTier_NoCacheWrite` probed a hand-written five-part key while the key `Run` really uses has seven parts, so its "no entry under the default-model key" check could
    not fail whatever `RunTier` did. The probe now calls `cacheKeyFor` and the test checks that `Run`'s own entry is found under it. With `RunTier` made to write under `Run`'s
    key, the old probe stayed silent and the new one fails.
- **Mutations run.** With each of these put back in turn, the named tests fail and the code is restored byte for byte afterwards. `cache.Key`: the old NUL join; no version tag
  (only `TestKeyEncodingIsPinned`); an 8-byte length cut to 4 (only `TestKeyEncodingIsPinned`); the length missing on the last part; a decimal length with no delimiter
  (`TestKeySeparatorCollision` and `TestKeyEncodingIsPinned`; before the digit entries only the pinned test failed). `Cache.Put` without the empty-key refusal. Call sites:
  the glued vision key; the folded render; the NUL-terminated exemplar fields; the NUL-joined template tag; the glued transcription identity, inside `transcribeCacheKey` and
  at the `runTranscribe` call site (the latter fails only `TestRunTranscribeStoresUnderTranscribeCacheKey`); the requested language passed on the OAI leg (only that
  test's `oai` leg fails); the transcription key without the model; `StableParamsKey` back in
  the key at the `Run`, vision, `RunTier` and transcribe call sites. Fail-closed handling: a fallback from an unkeyable value to `StableParamsKey`; the bypass not recorded;
  the unkeyable flag ignored at the `Run`, `RunTier`, vision and transcribe call sites. `CacheParamsKey`: it returning `StableParamsKey`; no walk over the values; byte slices,
  non-UTF-8 strings or types with their own JSON form allowed; the pointer-receiver half of that check, `MarshalJSONTo` or `AppendText` dropped from it; the
  `json.Number` check off; the encoder's error text forwarded; the walk bound back at 64; the exemplar harvest without the keyability check (a stack overflow); a `fmt` fallback for an unkeyable value (it crashes on a self-referencing map, which is the danger of a
  fallback). `StableParamsKey`: the keys not escaped, the values not escaped, `%` not escaped, `=` not escaped, `;` not escaped, the trailing `;` changed, the backslash
  escaped as well, the escape applied twice, and the nested rendering changed to `%#v`. Removing `field()` from `askcache.Key` on its own no longer turns a test red, because
  `cache.Key` keeps those fields apart; its test says so.
- **Not fixed here (pre-existing, out of scope).** D-136: a cached result is not validated against the requesting call's schema on a hit (validation runs only on a miss), so
  a key collision of any kind would still be served; the typed key closes the known ones. D-137: the result cache has no bound, TTL, compaction or operator lever. D-138:
  output-file stems that glue fields (`composevideo.go:343` hashes a `fmt.Sprint` of its fields with no separator at all; the render, video and edit stems hash the prompt
  and the params string with no boundary). Left alone on purpose, no row: `promptPrefixFingerprint` (`internal/pipeline/identity.go`) and the research retry-cap key
  (`pageKeyFor`, `internal/delegate/processgate.go`) join their fields with a NUL, but they are telemetry and a retry cap, not result-cache keys.
- **Proved.** Remote Linux runs at commit `dca2e015` (the typed-params round; `babdd239` before it passed the same): a gate over `./internal/cache`, `./internal/askcache`, `./internal/tasks`, `./internal/mcpserver`,
  `./internal/changelog`, `./internal/pipeline` and the root package passed (7 of 7), and `./internal/pipeline` passed again on a second remote Linux runner. On Windows, for the
  typed-params round, `go test -p 1 -count=1` passes on `./internal/cache/`, `./internal/askcache/`, `./internal/tasks/` and `./internal/mcpserver/` with `-run TestAsk`, and on
  `./internal/pipeline/` with `-run` over the key, cache, vision, tier, exemplar, template, transcribe, gate, media, image-batch, render, video and legacy-file-name tests
  (skipping one lease-timing test, see below); `go vet` is clean on the touched packages and `go run ./cmd/changelog check` passes. The other callers of `StableParamsKey` (the output-file stems in `internal/pipeline` and
  the exemplar append) were read, and the tests that reach them pass, among them the legacy file-name test in `imagebatch_family_test.go`. That test computes its expectation with
  `StableParamsKey` itself, so it cannot see a change in the output; the hard-coded table above is what holds the bytes.
- **Not verified here.** No live cache file, seat or fleet node was used, so the cold start after an upgrade is read from the code (a key that is not in the store is a miss) and not
  measured. The Windows pipeline run skips `TestEveryMediaRouteLeasesForItsOwnTimeoutAndIsKilledThere`: a lease-window timing test (it spawns real runner processes) that failed
  intermittently under load on this machine, always as "want one live lease while the render runs" within about 50 ms and on a different subtest each time, and passed three runs in a
  row on its own. That flake is the media-route lease-read race (a held lease record read as absent while it is rewritten) that PR #634 fixes, not this change: this diff touches no lease code, and the remote Linux runs of the whole package included the test. A Windows run of the full suite was not made, and the leak-gate tool (it needs the operator's name list) was not run.

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

### Added - a lint keeps a test from failing because a loaded runner stretched a measured time

The starvation fixes of this series (the call-reserve start order, the queued-credit and cut-answer tests, the call-deadline windows,
the lease-renewal helper, the busy-hold ceiling, the warm-watch poll) each found one test that put a number on the clock, after a green
suite went red once, and fixed that one test. The next test to do it was not covered. This change adds the lint that covers the part of
the fault a syntax tree can see. It adds two test files in the root package and one section of `CONTRIBUTING.md`; no product code and no
existing test changes.

- **What it checks.** `clock_lint_test.go` judges every `*_test.go` file of the module (the same walk as `tempdir_lint_test.go`) and fails
  on an upper bound on a measured elapsed time under one second, compared with `>` or `>=` (or the same written the other way round)
  against a constant it can evaluate. The measured time is `time.Since(x)`, a difference with a `time.Now()` stamp, a name that only ever
  holds one of those, that value read as a number (`.Seconds()`, `.Milliseconds()`), rounded, made absolute (`.Abs`, `math.Abs`),
  converted (`int`, `float64`, `time.Duration`, `math.Round`, `math.Floor`) or multiplied or divided by a constant, and the result of a
  function or method of the same package that returns one (`measure(f) > C`, `d := measure(f)`, `measure[int](f)`,
  `results, sum, elapsed := runWithin(...)`, named results, bare returns, helpers that return other helpers' results to any depth). A
  function is found by its name unless the function defines that name itself; a method is found only through a local name that holds a
  value of the type that declares it (a declared type, a composite literal, `new(T)`, the method's own receiver), so a method of another
  type, or of a type from another package, with the same name is not the helper. The constant is a literal, a unit product such as
  `250*time.Millisecond`, a const of the same package (read from its test files and from its other files, because a test refers to
  `leasePollInterval` as often as to a neighbour's const), or a const or once-assigned local of the same function. A package name is
  trusted only when every package-level declaration of it, a var, a function or a type included, is a const with the same value. Names
  are settled to a fixed point, so a chain of any length resolves the same way on every run. The key is `file:elapsed>C`.
  `clock_lint_allowlist_test.go` lists the 28 keys (33 sites) the tree holds, each with its count and a reason that names what the bound
  rules out and how wide the margin is; a thin margin is written down as thin. The list is kept true in both directions, as the temp-dir
  list is: a new bound, a larger count, a fixed site whose count or entry stays, and an entry without a reason each fail. A test file
  that imports `time` with a dot cannot be read and fails by name.
- **Why one second.** A bound of a second or more is not reviewed by the lint. That is where a bound starts to be a hang guard, and
  where the list stops being something one person can review: the tree holds 28 keys under 1 s, 61 under 2 s, 84 under 3 s and 113 under
  5 s. The floor is a ratchet, not a proof of safety: a 2.5 s ceiling failed under starvation too, so a bound just above it is still
  yours to judge. Raising `clockBoundFloor` and listing what it then finds is the next step.
- **What it cannot see** (the header of the file has the full list, and `CONTRIBUTING.md` repeats it). It reads Go only: the
  JavaScript tests under `render/` and the shell scripts have the same fault and no lint here. Bounds against a value it cannot evaluate
  (a field, a parameter, a poll interval, a constant of another package, a const written with `iota` or by repetition of the line
  above). Measured times it cannot follow: a sum with another duration, `max` or `min` of one, a helper kept in a closure variable or in
  another package, a method called through a package-level variable, a field or an interface, a value stored in a field or sent on a
  channel, and an instant held as an integer. That last shape has a live site: `internal/delegate/callwait_test.go` stores
  `time.Now().UnixNano()` in an atomic integer and bounds `time.Duration(lastDispatch.Load() - horizon.UnixNano())` at 400 ms, a
  sub-second ceiling the lint does not see (it is not in the list and is not changed here). `elapsed < C`, which is overwhelmingly a
  lower bound and is not read, in its two shapes that are not: a fixture schedule (a fake server that answers one way until `C` has
  passed and another way after, so a starved runner moves the flip relative to the code under test) and a poll deadline written as a
  loop condition (`for time.Since(start) < C { ... }` followed by a failure). The lower edge of a sampling window (`el > lo && el < hi`)
  is not told from a ceiling; such a site is listed with that reason. And the other shapes of the same fault: a fixed sleep before an
  assertion, a short call or context window handed to the code under test, a poll deadline taken from `time.Now().Add(C)`, a helper that
  takes a short timeout. `case <-time.After(C): t.Fatal(...)` ceilings were counted when it was written: none is under a second.
- **Red first, in four rounds.** (1) With an empty allowlist the lint failed on the real tree with 22 findings, the first being
  `gpu_warm_watch_test.go fails when a measured time is above 500ms (3 time(s)) and is not in clockBoundAllowlist`; with the list written
  it passed. A scratch test file holding `time.Since(start) > 250*time.Millisecond` turned it red and named that file and the form, and was
  removed. (2) Reading package constants from the package's other files made six more keys (eight sites: five files in
  `internal/modelaffinity`, one in `internal/pairworkloads`, each a bound of `leasePollInterval/2` or `nodeInfoTimeout/2`) fail the
  real-tree test before they were listed. (3) Sixteen forms of a measured time that the lint did not read (arithmetic, conversions,
  `Round`, `Truncate`, helper results of every shape) were pinned as cases that failed against the scanner as it stood, and pass now.
  (4) Seventeen cases failed against that scanner and pass now: a chain of twelve dependent locals, of forty renamed constants and of
  twelve renamed stamps and measured times (each scanned a hundred times in one test, because the order of a Go map changes between
  iterations; the whole set also passes with `-count=20`), a package const that is also a var, a function or an unevaluable const in
  another file, a method of an imported type, a closure, a parameter or a func field with a helper's name, a method called through a
  field or a call result, a generic helper, `Duration.Abs` and `math.Abs`, `Round` and `Floor`. The tree's keys did not change.
- **Mutations.** Single-line mutations of the scanner, each run with `scripts/mutation-check.sh` against the scanner's own tests, were all
  `RED`: six on the first round (dropping `>=` or `<=` from the bound operators, treating a bound of zero as a bound, reading any `Sub` as
  an elapsed time, no longer failing on a count below the listed one, a local no longer shadowing a package const), ten on the second
  (reading test files only for constants, trusting a const declared with two values, a quotient read as a product, `Truncate` unread, a
  multi-value definition unread, a bare return unread, a zero return read as not elapsed, two declarations of one helper trusted, a call
  through an imported package read as a local helper, a dot import no longer named) and ten on the third (locals, helpers and package
  consts back to a fixed number of passes, a const trusted with a declaration of another kind, a local that no longer hides a function,
  `math` functions and `Duration.Abs` unread, a name with disagreeing definitions typed, a method found by its name alone or keyed by it).
- **No new finding in the open changes.** The same scanner run on all the Go files of the four open pull requests found no site outside
  the list.
- **`CONTRIBUTING.md`** gets a section, "Keeping tests from failing on a slow machine", beside the temp-dir one: prefer waiting on the
  event the product signals, with a generous guard against a hang; what the lint covers and what it does not see; the command that
  regenerates the keys. Fixing a listed site means lowering its count, or deleting the entry when it was the last.

## [0.188.0] - 2026-10-15 - a graph job declares the weight files its own loaders name instead of the largest family the box binds, doctor says whether the boot-time GPU power profile was applied this boot, a recipe audit verb scores every stored recipe with the measurement gate, ADR 0094 (a lease names its cards, a card has one holder), compaction-eval switches for the two optional rungs, a self-improvement chapter, and four timing-flake fixes

### Added - ADR 0094, a lease names its cards and a card has one holder; dated pointers in ADRs 0018, 0026, 0039 and 0041; the lease guide's Known gaps located and corrected

- **What it records.** One decision record for the rule the card-naming changes share, written from the code at commit `3be61a3c`
  and not from the older records. A lease names its cards or the whole node; two leases conflict when either is whole-node or
  their card lists share a card; the grant creates each card's claim exclusively under the epoch lock, and a whole-node grant
  verifies the other side; the fence is per epoch; nothing frees a card from a holder that may still be using it except an
  explicit release by epoch, which checks no liveness; the writer is a per-host switch behind a green reader audit; every
  consumer asks about its own cards and a doubt reads as every card; a busy card is a place in line. Every behaviour cites
  `path:line` and a symbol. Status Accepted covers only that part. The decisions README keeps the move to Accepted with the
  operator, so the release review may set the status back to Proposed.
- **What it does not decide.** A separate section lists what is not built and names the constraint the Accepted clauses put on
  it: a takeover that stops a job and then frees its claim (three status surfaces name `gpu takeover`, which is not a verb), a
  release that waits for a dead tree and a writer for the `quarantine.<id>` sidecars, an overdue policy beyond the request, a
  media plan that always names its cards (the voice, iGPU and sd.cpp lanes, a graph with no device, and every plan that cannot
  name a card hold the whole node; the card-only pair is refused instead), a pool of local workers across cards, and a scripted acceptance of the whole path.
- **Pointers.** ADRs 0018, 0026, 0039 and 0041 each get a dated amendment of one or two sentences pointing at it, in the
  convention of that file; their decisions are unchanged. The ADR index has the row. ADR 0055 is cross-referenced from the new
  record only.
- **The lease guide.** `docs/systems/gpu-lease.md` keeps its one Known gaps section and puts three groups in front of the older
  gaps: what is not built, the windows the host-RAM hold bound's entry lists as open (each now with the file and symbol it lives
  in), and the text that disagrees with the code. Where the guide and the code disagreed, the guide was corrected: it said the
  session registry's writer was not wired in, and the MCP server writes it when it starts (`registerSession`); what stays
  unverified is whether a given Claude Code build hands its MCP server the session id.
- **Disagreements found and left for their owners** (they sit in code comments and accepted ADRs, which this change does not
  edit): `devices.go` still calls the dropped whole-node record "pending acceptance"; `term.go` and ADR 0070 repeat the registry
  sentence; `proctree/stop.go` says the yield request uses the stop primitives, and nothing does; ADR 0026 says the default
  lease window is stamped on nearly every media lease, and the routes pass their own timeout; ADR 0055's out-of-scope note on the
  reclaim rule reads as if progress played no part, and it decides a term's label.
- **Tests.** Documents only, no code. The existing number-and-index test (`TestADRNumbersAreUnique`) was red on the commit that
  added the record alone ("not linked from docs/architecture/decisions/README.md") and is green with the index row; the docs lint
  and the leak gate run in the root package.

### Fixed - the gate page says what a judge receipt does and does not bind

- **The receipt is checked for its shape, not for what it hashes.** `gate verdict` accepts a model judge's receipt when
  its scorer id is the judge identity, `passed` is true and both hashes are 64 lower-case hex characters. It does not
  compare `pairs_hash` with a pairs file or `build_sha256` with any build, so a receipt written by an older build still
  certifies a scorer that kept its id. The page said the receipt "binds" the build; it now says the receipt records it,
  states what the gate checks, and says that a scorer whose behaviour changes gets a new receipt or a new id by
  convention, which the gate does not enforce. Docs only.

### Added - `docs/systems/self-improvement.md`: how the harness improves itself on evidence, one page that maps the discipline, the failure register, the trace-corpus readout, the measurement gate and SHA-bound review evidence

- **The gap.** Four pieces of the evidence loop each have a page of their own, and nothing said how they fit: that a readout never decides, that only the measurement gate adopts, that a rate nobody measured is `null` (and a trace-corpus total reads 0 with the recorded count beside it), and which of the pieces is the one that can say "better". A reader met `BELOW_GATE` in the trace-corpus report and could take it for an ADOPT.
- **What the page holds.** The discipline (measure, gate, adopt or keep the incumbent; quality is the only gate and no pass rule carries a wall-time term), then one section per part that is on main, each naming the verb or tool that runs it, what it answers, the rules that keep it honest, and the code and the page that own the detail: `local-offload ledger --failures` (the six closed kinds, refused attempts set aside, unparsed lines and a missing ledger counted, the cause and fix kept as a person's), `local-offload trace-corpus` (harvest, readout, freeze, check; the pager re-fetch rate against its 10 % gate; the compaction pass count; every rate reads `null` when unrecorded and a total reads 0 beside `recorded_runs` and `unrecorded_runs`), `local-offload gate` (freeze, check, verdict, schema, selftest; the rules in short; the judge receipt), and `offload_review_diff` (the diff fingerprint, the optional base and head commits echoed and never resolved, gates first, the five outcomes with `NOT_RUN` never a PASS, the second fingerprint check, the durable trace beside the ledger). It also says what each command writes (nothing, only its `--out` file, or, for `gate selftest` with its real scorer and for a review, what a pipeline call writes: ledger rows and the result cache, the run's ledger row and the review trace), and lists the entry points, the invariants, the privacy notes, the tests and the pitfalls.
- **Two parts marked "not on main yet".** The evidence-preserving reducer rung and the recipe store with its audit verb each get one short paragraph: proposed, what it will do in one sentence, and that it stays off until its own gate measurement passes. Their code is not described because it is not merged.
- **The index.** `docs/README.md` links the page under Systems. `docs/architecture/decisions/README.md` is not touched.
- **Every claim was read against the code at this head**, not carried over from another page: the closed kinds and reason codes, the refusal rule, `PagerGate` and the verdict names, `NoteCompaction`'s three call sites, the gate's constants (version 3, `min_pairs` 16, `max_excluded_share` 0.20), the receipt fields, the exit codes, `Decide`'s order, `MaxDiffBytes` and the trace file mode. Where an ADR is the only source of a figure (the first trace-corpus readout) the page says so and does not re-measure it.
- **Tests.** Red first: the index line was committed alone and `TestDocsLint` failed with `docs\README.md: broken relative link "systems/self-improvement.md"`; the page turns it green. A review then found the page wider than the code in two places, and each got a failing test before its fix (`self_improvement_docs_test.go`, the only Go file on this branch): the page said every command but `ledger --failures` writes only its `--out` file, when `gate selftest` with its real scorer also writes ledger rows and the result cache; and it said the trace-corpus readout's totals read `null`, when only its rates do (its totals are sums that read 0). Each test pairs a page check with a pin on the code (a stub seat for `gate selftest`, a run that recorded nothing for the readout), so a later change to the code fails the test instead of leaving the page wrong. `docs/systems/measurement-gate.md` carried the same writes sentence and is corrected with its own test; the header comment of `gate_cmd.go` carries it too and is left for a Go change.
- **Checks.** `TestDocsLint`, `TestADRNumbersAreUnique`, the changelog and fragment tests, `TestOpenSpec`, `go run ./cmd/changelog check`, the docs-tier currency test, and the keyed leak gate scans over the new page and this fragment.
- **Not verified here.** Nothing on this branch needs a GPU lease or a live seat, and none was taken. The page cites `gate selftest` (which reaches a seat for its scorer), a live `ledger --failures` run and a `trace-corpus harvest` over real logs, and none of the three was run for this change; what the page says about them comes from reading the code and the tests. The `gate selftest` pin runs the real scorer against a stub seat with every path redirected to a temp dir, so it shows the ledger and cache writes without a seat; a live selftest was not run.

### Added - compaction-eval can switch on the search-results and log-collapse rungs

- **The verb can now measure the two rungs.** `compaction-eval run`, `freeze`, `ab` and `kvbench` take `--search-results` and
  `--log-collapse`, added to `--gcf --skeleton`. They flow through `agent.ReplayOpts` and `compeval.LadderOpts` into the same
  production ladder every other flag reaches, and the report's `ladder` names the arm (`gcf+skeleton+search`,
  `gcf+skeleton+logs`, `gcf+skeleton+search+logs`). Until now neither rung could be replayed, so neither could be judged by the
  measurement gate.
- **No default or production path changes.** The rungs stay off in the live loop, `compact()` is untouched, and the pinned
  default-ladder golden is unchanged. Without a rung flag, `compaction-eval run` prints the same bytes as before; two golden
  outputs recorded from the verb before the change pin that.
- **`kvbench --arms on`** runs only the ladder arm and skips the raw arm, which a candidate-versus-incumbent comparison does not
  need (an unknown or repeated arm is refused; with one arm there is nothing to pair, so the paired totals stay empty).
- **`kvbench`** samples now carry **`sent_sha256`**, the sha256 of the transcript the ladder produced, taken before the nonce salt
  (its canonical encoding, not the HTTP body). It does not depend on the run's nonce, so two runs with their own fresh nonces
  prove a pair a no-op when the hashes of a step are equal, and a different hash proves the rung rewrote the request.
  `--nonce N` sets the body salt; reuse it only on a cold seat, since a warm seat serves the second run's unchanged prompts from
  the first run's cache. The calibration controls draw their own nonce every run (`control_nonce` in the report), so a reused
  `--nonce` no longer replays the first run's negative control into the cache and fails the second run.
- Each behaviour has a test that failed first: the replay options (not passed to the ladder), the labels, the rung
  turning on in `run`, `ab` and `kvbench` on transcripts whose older turns hold a search listing or a repetitive build log, the
  other rung's entry staying unchanged, `--arms on`, a no-op pair with equal `sent_sha256` and a changed pair with a different
  one, one body nonce and one control nonce across the requests of a run, the same `sent_sha256` for one transcript under two nonces,
  and a second run on a multi-prompt-cache seat that reuses the first run's nonce staying admissible.

### Not verified here

- No model-backed run: `ab` was exercised with a fake scorer and `kvbench` with a fake serving endpoint, so no accuracy or
  forwarded-token result for either rung exists yet. That needs a corpus whose older turns carry search listings and build logs,
  and a seat.
- `ab` was not driven through the verb itself (it opens the live pipeline); its flag reaches `RunAB` through the same options
  value `run` and `kvbench` use, and `RunAB` is tested with both rungs.

### Added - doctor says whether the boot-time GPU power profile was applied this boot

- **The defect.** A box can apply a GPU power limit and a clock lock at boot with a separate tool. When that step is
  skipped, refused or blocked the card runs at stock limits, and the only record was in the tool's own log. Nothing in
  `doctor` or the fleet view said so, so a box could run for days at stock limits with every row green.
- **What it does.** `doctor` prints one `power:` row from the state record the tool writes on every run (refusals
  included). `OK` says a profile was applied since this boot began, how long ago and how many steps returned success.
  `WARN` says no profile was applied this boot (once the box has been up ten minutes; a box just started gets a `SKIP`
  that says the boot apply is not yet due), or names the outcome that left the profile out (dry run, refusal, failure,
  empty plan, the tool's crash guard, which says only that the run applied nothing and how each kind of cookie ends,
  never that the card is at stock limits), or says the apply was partial (a step short of the plan, a lever skipped
  other than by the tool's own design, a driver that differs from the profile's stamp). `UNKNOWN` names why the file
  cannot be trusted (unreadable, not JSON, another schema, no usable time) and is never reported as absent. No state
  file is one `SKIP` line saying nothing is recorded, not an error. The file is found only by
  `LOCAL_OFFLOAD_POWER_PROFILE_STATE`, which holds the path of the tool's state file; there is no platform default, so
  a box sets the variable and reads `SKIP` until it does.
- **What it does not do.** It reads the record, not the card: it does not compare the recorded limit and lock with the
  driver's live values, because the harness has no reader of a card's power limit or clock lock. That comparison, and a
  `power_profile` block on `/fleet/health` so the delegator and `offload_status` can show it for every node, are the
  follow-ups. The row is informational like the capacity row and never changes doctor's exit code. It prints no path,
  host name, card id or the tool's free-form detail.
- **Tests.** Written red first (a stub row that prints nothing, 12 failing tests), one per behaviour: absent file, whole
  apply this boot, last record older than the boot, boot apply not yet due, every outcome that leaves the profile out,
  partial applies and skips by design, unknown boot time, untrustworthy files, the record shape the tool really writes,
  the path resolver, the boot-time reader on Windows and Linux, the age text, and the doctor registration. Seven mutations
  of the row (age truncated not rounded, by-design skips counted, boot check off, grace off, absent read as unknown,
  driver mismatch ignored, dry-run flag ignored) each fail at least one test. A second pass added tests for the
  `cookie_blocked` wording, the filter that keeps a record's free text and control characters out of the row, the skip
  list read as an array, a bare string and null, the clock slack (inside it, at its edge and past it), a record with no
  `schema_version`, a timestamp with stray whitespace, the ten-minute boot-apply boundary and the read-failure reason;
  nine more mutations of the row (outcome filter off, plain list decoder, slack zero, slack widened, slack bound
  exclusive, schema-less message folded, timestamp trim off, grace bound inclusive, permission message dropped) each
  fail at least one of them.
- **Docs.** `docs/OPERATOR-GUIDE.md`, next to the capacity and `lan ip:` rows.

### Added - `recipe-audit` and the recipe store: ADR 0036's rules for what a proposer may build, and the audit that publishes or demotes a candidate rule set

- **What it does.** A recipe is a candidate edit to a seat's `agent_env_rules`, kept as a typed record (`internal/recipe`):
  `id`, `seat_tag` (the capability class of the seat it was distilled for), `rules` (the closed vocabulary), `provenance`
  (the acceptance-passing runs it came from) and `status` (`proposed | published | demoted`), plus the `audit` that set the
  status. `local-offload recipe-audit` (one `case` line and one usage line in `main.go`) runs a set of contracts on one seat
  twice, with and without the recipe, under the GPU lease, judged by the acceptance DSL, and publishes the recipe only on a
  measurement-gate ADOPT with the candidate's mean steps no worse than the incumbent's. Anything else demotes it; a demoted
  recipe stays in the store with its audit. No proposer is built.
- **The amendment (ADR 0036, "Amendment 2026-10-15").** Candidates are closed-vocabulary data, never code. The proposer
  proposes and never applies. Only the branch, the parallelism and the stop class are replay-scorable (`deny_tools`,
  `allow_tools`, `max_calls_per_tool`); rules that rewrite what the seat reads (`max_observation_tokens`,
  `observation_strip`, `rewrite_error`, and `arg_limits`, which changes what a call returns) need a live A/B. Recipes are
  distilled only from acceptance-passing runs and carry a seat-capability tag. No proposer is built until trace-bearing rows
  number in the hundreds; the amendment says so and builds none. The ADR index row is unchanged: its text is the ADR's title.
- **Strict decoding.** The record has six keys and refuses any other, in the rule set and the audit too, so `rules_code` is an
  unknown key. A rule set that changes nothing is refused, so is a recipe with no provenance, a duplicate run id, an id that
  is not a slug (the id is the file name, so it cannot name a path) and a `published` record that does not carry an ADOPT
  audit with steps no worse. Every string must be shaped like rule data: lower-case identifiers for tool and argument
  names, one line of plain ASCII (letters, digits, spaces and `. , : ' _ / -`, at most 160 bytes) for a `rewrite_error`
  text, and a printable-ASCII pattern of at most 200 bytes that does not match the empty string (such a pattern fires on
  every result). The shapes keep the store from holding anything that is not data; what keeps a recipe harmless is that an
  env rule never grants or denies an effect.
- **The store.** One `<id>.json` per recipe (default `<data home>/recipes`, or `--store`), canonical bytes, written to a
  temporary name and renamed over the old file. A file whose record names another id than its name is refused, and a listing
  fails by file name on a bad record. `internal/recipe/scoring.go` classifies every key of the vocabulary as replay-scorable
  or live-A/B; a reflection test fails when `core.AgentEnvRules` gains a key nobody classified.
- **The verb.** `recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json
  --engine-build STR [--store DIR] [--attempts N] [--input-out FILE]`. It refuses with a typed refusal and exit 2, before any
  seat call, when there is no `GPU_LEASE_EPOCH`, when the epoch is not a live lease in the lease directory
  (`gpulease.EpochIsCurrent`), when `--seat-tag` is not the recipe's, and on input the store or the gate will not take (an
  undeclared or repeated task, a declared task with no contract, a contract with no acceptance check, a baseline that froze
  an incumbent or a judge other than `{deterministic, acceptance-dsl}`, a primary class other than `unit_interval`, an
  efficiency class other than `tokens`, a baseline that is not the frozen bytes, a build the gate cannot stamp). It never
  takes a lease and asks again before every seat call: a lease that moves on mid-audit ends it with exit 2 and records
  nothing, and so does a seat whose pin moves or is missing (`seat_pin`, below). The gate input is built as
  `gate verdict` builds it (`eval.CheckBaseline`, `eval.Gate`, the build hash on the result); `--input-out` writes it, and a
  test pins that `gate verdict` on that file gives the printed result byte for byte. gate_cmd.go is not edited.
- **Decisions taken, with the default.**
  - The candidate arm replaces the box's table with the recipe's rules (the semantics of `local-agent --env-rules`); the
    incumbent arm is the box's own table, untouched. Each task runs on both arms, attempt after attempt, and which arm goes
    first alternates by task.
  - A finished run is judged by the acceptance DSL (pass scores 1, fail scores 0), which is why the primary must be on the
    unit interval. A runner error and an infrastructure, config, contract, capacity or write defer are `infra_error`. A budget
    the seat itself spent without finishing is `task_fail`, because the seat used it and did not finish: the step budget, a
    tool call cut at the completion budget twice, a final answer the think block starved twice. A budget a clock or a
    caller ended (the wall, a ceiling, a canceled parent, a re-pack the clock could not buy) is `timeout`. The gate imputes
    the candidate's unfinished attempts at the worst score and skips the incumbent's.
  - Steps are the verb's own check beside the gate: the mean over the tasks that finished on both arms, candidate no greater
    than incumbent, at least one such task. The gate's efficiency classes are tokens, money and memory, never a count of
    steps. A declared `tokens` metric is read from the run's forwarded prefill tokens and is unresolved when the run has no
    prefill accounting. A money or memory metric could never be resolved, so a baseline that declares one is refused.
  - The era is the seat that served the runs. The harness pins the served seat on every served run
    (`wire.SeatConfigSHA256`, from `agent.ProbeSeatPin`); both arms' stamps carry the harness build, the engine build and a
    hash of the seat alias, its endpoint, that pin and the env-rule table the arm ran under, so a model, quant or window
    swapped behind the alias moves the stamps and the gate's era marker, and the arms differ by the table. The pin must be the
    same on every served run of both arms: one that moves mid-audit, a served run with none, and an audit in which nothing
    was served are `seat_pin` refusals (exit 2, nothing recorded), the first two at the run that shows it. A run that is
    scored as a verdict about the task counts as served, a spent budget and an abstention included, because the harness
    stamps the pin before every terminal branch; a run that ended before the seat served it (a runner error, an
    infrastructure defer, a ceiling or a canceled parent) carries none by design and is not held to one. `--engine-build`
    stays required: it names the engine in the operator's words.
  - `--seat-tag` is required and must equal the recipe's: a recipe is evidence about one class of seat. The tag is a slug the
    operator chooses; the harness checks its shape and the match, not a vocabulary.
- **Tests.** Red first, each committed failing before the code that fixed it: the unknown top-level key, `rules_code` inside
  the rules, free text and code outside the vocabulary (18 cases), the empty rule set and missing provenance, the published
  record without the audit its status needs (seven cases), the store and the scoring partition; then the verb's refusals (no lease, a lease
  this process does not hold, a lease lost between runs, a seat-tag mismatch, 19 kinds of bad input), publish on an ADOPT
  with fewer steps, demote when the steps got worse, demote on a block and on a tie, the gate-input parity, the outcome
  mapping and the metric mapping. A review then found more to pin: a primary off the unit interval, a money or
  memory metric, an unhashable build and a short contracts file (each refused before the seat is asked); a spent completion
  budget read as a timeout; a seat swapped behind the alias (the pin in the stamps, a pin that moves, a served run with no
  pin, nothing served); and the steps rule's tie, its unfinished attempts, `--attempts` and the arm order, which five
  surviving one-line mutants had left unpinned. A fake seat stands in for the model; one test runs the verb against a real lease taken in a
  temporary lease directory and again after it is released. Mutations run and killed: the up-front and the per-run lease
  checks removed, the seat-tag check removed, the steps check removed from the publish rule, publish on any verdict, the steps
  comparison inverted, a tie in steps demoting, a pair that needs only one arm, an unfinished attempt counted as steps,
  `--attempts` ignored, the arm order never alternating, the candidate arm stamped with the incumbent's rule table, an
  unmeasured run read as zero tokens, a spent step budget read as a timeout, a frozen incumbent
  allowed, an undeclared task allowed, a contract without acceptance allowed, the candidate arm running the box's table,
  unknown fields allowed, the text charset widened to code characters, the empty-match pattern allowed, a record published
  without an audit or with worse steps, a proposed record carrying an audit, tool names unchecked, duplicate provenance
  allowed, a record accepted from the wrong file, and a replay key reclassified.
- **Not verified here.**
  - A real audit. Nothing in this change ran a model, took a lease or loaded a seat: the seat is a fake, and the real seat
    (`recipePipelineSeat`, one pipeline per arm through `RunAgentContract`) is covered only by the unit test of the config
    each arm runs under. The first real audit has to run under `gpu reserve` on a live seat and read the printed report.
  - That the 16-task floor of the gate is reachable with contracts a recipe actually changes the behaviour of, and what
    `--attempts` above 1 does to the cost of an audit on a real seat.
  - That the forwarded prefill tokens of a real run are present on the wire result of the seat in question; the metric is
    tested on scripted results.
  - That a real seat's pin reaches `wire.SeatConfigSHA256` on every served run, and how often the probe's three-second
    bound misses it: an unpinned served run ends the audit, so a flaky probe costs a re-run. The pin check is tested on a
    fake seat that stamps or withholds the field.
  - That the budget-class split (the seat's own spent budget against a clock or a caller) matches live defers: it is read
    from the defer sites in `internal/pipeline/agenttask.go` and tested on scripted results.
  - That a provenance run id names an acceptance-passing run. The store checks the shape of the ids; the delegation log is
    the evidence and nothing here reads it.
  - That a published recipe helps in use: nothing applies one. An operator copies its rule set into the seat's table.

### Fixed - the busy-hold tests no longer fail on a starved host: they keep the fake's request silent until the run is in the hold, and the ceiling test asserts the hold instead of a share of its ceiling

- **The flake.** `TestRunAgentTaskCeilingWhileHeldIsNotABudgetDefer` failed in CI on a change that touched only `internal/eval`
  (`precondition: deferred=true queued_ms=394 reason="ceiling 1s reached while held behind the seat's other work ..."`). It required
  `queued_ms >= 500` of a 1 s ceiling. `queued_ms` is the time booked in the busy hold, and the hold cannot begin until the request's own allowance
  has run out (200 ms in the test) and the monitor's first look at the engine (the seat probe, then the engine probe's two reads) has returned;
  the hold is booked from the moment that look returns. A run held for the rest of its life therefore reports about ceiling minus allowance minus
  the look's latency: 795 ms on a quiet host, 394 ms on the CI runner. The product was right (the class was already capacity, the reason already
  said "held"); the test assumed a schedule.
- **A second failure the same test had.** Under load on an idle node it failed two ways. Once as above (`queued_ms=267`, class right). Three
  times as `ceiling 4s`, `2s` and `1s reached while producing (0 tok at 0.0 tok/s)` with `queued_ms=0`: the run's hold had not been entered when
  the ceiling fired. In the first two the elapsed figure is above the 1 s ceiling, so the ceiling timer itself fired seconds late: the process was
  starved for longer than the ceiling, the stall timer and the ceiling were both overdue when it woke, and the ceiling fired while the first look
  was still in flight. No assertion on queued time can hold there; the ceiling needs room.
- **The fix (test code only; no production code changed).**
  - The ceiling test's precondition is that the ceiling found the run in the hold, read from the job record (the last report is `queued`) and
    `queued_ms > 0`, never how much of the ceiling the hold took. The ceiling is 8 s, so the path to the hold has room, and the silent request now
    waits on its own context, so the test ends with the run and no longer sleeps 6 s. The decision it pins is unchanged: a run held when its
    ceiling fires is not a budget defer and does not say "while producing".
  - Five more tests had the same shape: a fixed silence (1.5 s, 2 x 800 ms, 900 ms, 800 ms, 600 ms) followed by an assertion that the hold booked a fixed
    share of it (`queued_ms` at least 800, 900 and 400) or that the job record showed the hold at all. They fail the same way when the first look is
    late. A `holdWatch` reads the job record, and the fake keeps the request silent until the record has said `queued` and then for a stated time
    (600 ms, 2 x 300 ms, 400 ms, 100 ms, 350 ms); the tests assert at least that time. `TestRunAgentTaskHeldRunDoesNotFeedTheSeatRates` gets the same wait on
    its held arm; its control arm is unchanged.
- **Proved** (stress mode of the remote gate runner on an idle node: W concurrent copies of the compiled test binary by N runs each).
  - Red under load, the ceiling test unfixed: `--stress 4:20 --burn 6 --env GOMAXPROCS=2 --test-args '-test.run=^TestRunAgentTaskCeilingWhileHeld'`,
    4 of 80 failed (one `queued_ms=267`, three `while producing` with `queued_ms=0`), all inside a stretch where the node ran slow (runs of 11 to 29 s
    against 6 s). Load alone is not a reliable red; the same command on the fixed test passed 80 of 80, but the node had no slow stretch that time,
    so the rows below carry the proof.
  - Red, deterministic: the unfixed test with its first engine read answered 600 ms late (a scratch commit, not on the branch), `--stress 4:10`:
    40 of 40 failed with `queued_ms` 192 to 196 and the reason still saying held, which is the CI message.
  - Green: the fixed test with the same late read, 0 of 40; with the read 3 s late, 0 of 40; on a Linux node under `--burn 4`, 0 of 32 for the
    six tests together.
  - Mutant, product: `CeilingError.Held` always false: the fixed test failed 40 of 40 on the decision line (filed `class=budget`, `while
    producing`). The pipeline filing every ceiling as budget while the reason still says held: 40 of 40 failed on the same line.
  - Siblings, red: three of them unfixed under `--stress 4:40 --burn 6 --env GOMAXPROCS=2`: 7 failures in 160 runs, six of them `queued_ms`
    misses (159, 213 and 0 against 800; 861, 696 and 602 against 900) and one the stall described last, and 14 to 36 of 120 each in a slow stretch
    of the node. With the late first read, `--stress 4:5`:
    the 1.5 s test 20 of 20, the two-hold test 20 of 20, the job-record test 17 of 20. The 900 ms decoding test passes that stimulus (its
    `queued_ms` stays near 700, so the delay does not reach its hold) and failed under load only.
  - Siblings, green: the same late read, 0 of 20; load, 0 of 160 on the command above. Paired, the six tests together under `--stress 12:10 --burn 6
    --env GOMAXPROCS=1`: unfixed 1 of 120 (the two-hold test, `queued_ms` 686 against 900), fixed 0 of 120. Mutants: the monitor reporting no queued time failed all four `queued_ms` tests, 12 of 12; a later hold replacing the booked time
    instead of adding to it failed the two-hold test, 12 of 12 (`queued_ms` 301 to 305 against 600).
- **Same family, not changed.** `TestRunAgentTaskStallsAWedgedEngine`, `TestRunAgentTaskStallsAThrashingEngine` and
  `TestRunAgentTaskWithoutASeatAddressKeepsTheOldRule` keep a request silent for 6 s and fail only when the process is starved for more than that
  (they failed in the same slow stretch and not otherwise); the control arm of `TestRunAgentTaskHeldRunDoesNotFeedTheSeatRates` needs a stall of
  seconds to be held. A fixed silence is the point of those tests, so they are left as they are. One two-hold run in the red set ended `stalled: no
  progress for 0s in tool (allowed 120s; 1 tok so far)`: a stall filed with no silence at all, which looks like a stall timer that fired for the
  phase before. It is not a test matter and is not changed here.

### Fixed - the gate's power note projects a pass/fail record with the interval the gate reads it on

- **73 ties, not 240.** The power note projected every record's half-width as `hw * sqrt(n / n')`. A pass/fail record
  (every pair a tie, a win or a loss of the whole range) is read on Tango's interval, whose half-width for an all-tie
  record is `z^2 / (n + z^2)` and shrinks about as `1 / n`. So 16 tied tasks read +-0.194, the gate resolves a 0.05 margin
  at 73 ties, and the note asked for 240, more than three times what a saturated pair needs. For a pass/fail record the
  note now projects with Tango's interval at the projected size, holding the observed shares of wins and losses, and
  `tasks_needed` is the smallest count whose half-width is below the margin (or the observed effect in superiority). Every
  other record keeps the `1 / sqrt(n)` form. No verdict changes: the note adds no statistic.
- `TestGatePowerNoteAllTiePassFailUsesTheGatesOwnInterval` (16 ties, margin 0.05) failed on the parent with
  `tasks_needed = {N:240}, want 73`; `TestGatePowerNote` now pins the Tango projection for its pass/fail superiority record
  and the `1 / sqrt(n)` projection for its non-pass/fail record. A mutant that turns the Tango branch off fails both.

### Fixed - the iGPU deadline tests and the compose deadline test no longer measure the host's schedule: clocks held still, a ceiling that holds on every schedule, and a marker instead of a wall clock

- **The defect.** `render/igpu-deadline.test.mjs` and `render/compose-hyperframes.test.mjs` each failed once in a full `node --test "render/**/*.test.mjs"` run on a loaded machine and passed alone. Which test failed in each, and with what message, was not recorded, and load alone did not bring either failure back (see "Not reproduced by load alone"). Reading the two files found four assertions whose outcome depends on the schedule and not on the rule they pin. Each was made to fail on demand with an injected stall (see "Red"):
  - `processStartMs: the moment this process started`: `Math.abs(Date.now() - start - process.uptime() * 1000) < 100` and the `makeDeadline` plus or minus 50 ms check. `processStartMs` subtracts two clocks read one after the other, so a thread that loses its slice between the two reads moves the result by the length of the stall.
  - `a hanging engine is killed at what is LEFT of the budget`: `timeout after [2-6]s` (video, audio) and `[3-9]s` (animate). The ceiling of those ranges is the rule (the runner spent at least the injected 3 s before it spawned the engine, so the timer is armed for at most the budget minus 3 s; a deadline counted from `main()` would say the whole budget). The floor is not: it holds only while the runner starts within 3.5 s (6.5 s for animate), and node's own start, module loading, an antivirus scan of the stub and a starved CPU all spend budget.
  - `render: the deadline kills the step and fails TIMEOUT`: `Date.now() - t0 < 7000` around the whole run, which also measures a runner that starts slowly and a test process that is descheduled after the run returned.
- **The change (test code only; no production code changed).**
  - The arithmetic of `processStartMs` and `makeDeadline` is pinned over clocks the test holds still (`Date.now` and `process.uptime` replaced inside one synchronous block and restored in `finally`): the start is the clock minus the rounded uptime, the default deadline is 10 s after that start, `remainingMs` is never 0 while active, `enforce` throws exactly at the deadline, an explicit start is used as given. The real clocks are asked only what holds on any schedule: the process started before the test began, and at least 300 ms ago after a 300 ms sleep.
  - The hanging-engine test keeps the ceiling (budget minus the 3 s slow start: 5 s for video and audio, 9 s for animate) and drops the floor. The one thing the floor caught that matters, an engine that never ran, is now told apart explicitly: the message must be the engine's `timeout after Ns (killed)`, so a start-up that spent the whole budget reads "the engine was never killed by its timer" and not a regex mismatch.
  - The compose test no longer reads a wall clock. The stub `check` step sleeps 20 s against the 2 s deadline and writes `check.slept` after the sleep; a step that is killed in its sleep never writes it. The marker could only appear if the kill came 18 s late (it was a 5 s margin before).
- **Red (deterministic; the stalls are throwaway preloads, not committed).** Each command is one test run with `NODE_OPTIONS=--require <hook>`, on an idle Linux node and again on an idle Windows node.
  - Unit test, a 150 ms stall inside `processStartMs` between the two reads (a preload that wraps `process.uptime`): `node --test --test-name-pattern=processStartMs render/igpu-deadline.test.mjs` failed 5 of 5 runs on Linux and 3 of 3 on Windows, at the `< 100` assertion; 0 of 3 failed without the hook.
  - Hanging engine, video and audio, 3600 ms of extra start-up in the runner on top of the 3 s the test injects (a preload that busy-waits before the runner's own code): `node --test --test-name-pattern="a hanging engine is killed" render/igpu-deadline.test.mjs` failed 3 of 3 (Linux) and 2 of 2 (Windows) with `sd-cli timeout after 1s (killed)` against `[2-6]`; the runner was right, 1 s is what was left.
  - Hanging engine, animate, 6500 ms of extra start-up in the animate runner only: failed 3 of 3 (Linux) and 2 of 2 (Windows) with `depth-anything timeout after 2s (killed)` against `[3-9]`.
  - Compose, the test process stalled 5000 ms after every runner call that carries `--timeout-sec` (a preload that wraps `spawnSync`): `node --test --test-name-pattern="the deadline kills the step" render/compose-hyperframes.test.mjs` failed 3 of 3 (both systems) at `Date.now() - t0 < 7000`; 0 of 2 failed without the hook.
- **Green.** The same four commands on the changed tests, hooks kept: 0 failures in 5, 3, 3 and 3 runs on Linux (and 0 in 3, 2, 2 and 3 on Windows); without the hooks 0 failures in 3 and 3 runs. Under the load of the next bullet, with the changed tests: 0 failures in 16 runs of `igpu-deadline` and 0 in 30 runs of `compose-hyperframes`.
- **Mutant.** On a copy of the production code: `processStartMs` returning `Date.now()` (the deadline counted from the call) made the changed `igpu-deadline` file fail in 3 of 3 runs on Linux and 2 of 2 on Windows, on six tests (both unit tests, the hanging-engine test and the three "a start that already spent the budget" tests); only `makeDeadline`'s default start changed to `Date.now()` failed the exact unit test (the real-clock unit test still passed). `runProcess` ignoring the deadline (`const ms = 2147483647`) failed the compose test in 3 of 3 (Linux) and 2 of 2 (Windows) runs; `killTree` made a no-op, so the step is reported as timed out but not killed, failed it in 2 of 2. All mutations were restored byte for byte.
- **Not reproduced by load alone.**
  - Windows, 8 cores, Node 24: concurrent copies of one file with no CPU spinner (the first spinners launched exited at once, a quoting defect in the rig): `compose-hyperframes` 98 runs and `igpu-deadline` 28 runs, 0 failures, at up to 16 concurrent copies. With 16 real spinners (CPU at 100 %) and 8 copies: `compose-hyperframes` 24 runs, 0 failures. With 8 spinners at High priority and 8 copies the 8 loaded runs all failed: five tests per run took 90 to 330 s instead of under a second and ended on the helper's own `spawnSync` `timeout: 60000` (`null == 1`, `actual: null`), the other 61 passed (the deadline test among them); the 16 runs after the spinners expired passed. That is a hang guard met at a slow-down of about 100 times, not the field failure, and it is left alone.
  - Linux, 12 cores, Node 24 (load average 28 to 29, 24 spinners plus the copies): `igpu-deadline` 16 runs and `compose-hyperframes` 30 runs, 0 failures.
  - The full render suite, one idle run and six runs two at a time: the same two to three failures every time, none in these files (two tests that need a llama-swap the test node does not have and one `comfy-render` test killed by a libuv assertion in that Node 24 build); they are properties of that node.
- **Left alone, no evidence.** The helper's 60 s `spawnSync` limit (above). `runCompose`'s `finally { rmSync(work) }` has no retry, so an EBUSY or EPERM on Windows would replace a typed `ComposeError` with an untyped one; no run produced it (660 kill-then-remove cycles of a child holding the folder as its working directory on an idle Windows node, 60 alone and 600 four at a time: 0 failures), so it is noted and not changed. `sandbox().done()` in `igpu-testkit.mjs` removes with `rmSync` and no back-off, the shape the comfy-keep fix retired in `scratchDir`; also not seen. `processStartMs` could read `performance.timeOrigin`, one constant with no subtraction, which would make the unit exact on the real clocks; that is a production change and was not made here.

### Fixed - a graph's host-RAM need is the weight files its own loaders name, for a helper's `--graph` and for the `run-graph` lane, not the largest family the box binds

- **What.** `gpu reserve -- node render/comfy-render.mjs ... --graph <file>` (and `comfy-video`) and the pipeline's `run-graph` lane
  (`local-offload run-graph --graph`) posted the caller's own workflow, so the lease could not size it and declared the media class
  default: the largest ComfyUI family the box binds. They now read the graph. `hostneed.ForGraphFile` lists the weight files the graph's
  loaders name with the reader the node door already uses (`mediacap.ScanGraph`), finds each under the box's ComfyUI model roots (new
  `mediacap.ModelFileSizes`, the lookup of the door's held-by-name check, returning sizes) and sizes the set by **the family estimate's own
  rule**, now one function both use (`hostneed.fitToCard`): the **sum, each file once, in full when together the files do not fit the card**
  (the largest card of the lease, less the runner's reserve; a card that is not known counts everything), **0 when they fit**, with the same
  wording and the new source `graph loaders`. The lane's closure is `pipeline.graphRAM`, used by the real admission (`runRunGraph`) and by
  the lane probe (`graphLaneFree`). An explicit `--ram` still wins. The card is fitted with the call's own reserve (`--reserve-vram` of a helper call, `reserve_vram` of the lane; a value the runner would not
  take is the default). Doubt keeps the class default exactly as before: a graph that is not a JSON object (a UI-format workflow
  included), a file over 8 MiB or not a regular file, a graph that names no weight file, ONE named file that is not in the model tree, is
  empty, or is not a safe relative path, or a weight-like name anywhere in the document that the sizing did not cover (the whole JSON is
  walked, nested objects and lists included, for a string ending in the scan's six weight extensions or `.bin .pt2 .pkl .pickle .onnx
  .engine .trt .npz .h5`; the scan itself is unchanged). A size that is not known never shrinks a declaration.
- **Why.** Measured on a 64 GiB node that binds a Wan 2.2 video pair: a 512x512 qwen-image graph that loads about 23 GiB (6.76 GiB
  checkpoint, 16.33 GiB text encoder) was declared 32.89 GiB, and the host-RAM brake held it ("waiting for host RAM: needs 32.9 GiB";
  23.0 committed + 32.89 > 63.7 - 8.0 headroom) although the box could run it. With the loaders' own sum the declaration is 23.09 GiB.
- **Shape.** `internal/hostneed/graph.go` (`ForGraph`, `ForGraphFile`, `SourceGraphLoaders`), wired in `RenderCall.Estimate`'s `--graph`
  branch; `fitToCard` factored out of `estimateWith` (no change to a family's figure); `mediacap.ModelFileSizes` beside
  `MissingModelFilesUpTo` (a name held under two class directories counts as the larger; an empty file is unresolved);
  `pipeline.graphRAM` replaces `classDefaultRAM` at the two graph call sites (`runRunGraph`, `graphLaneFree`) and nowhere else;
  `classDefaultHint` says the lease could not size the graph's files. No change to the lease record, the grant or the queue.
- **Tests.** Red first (two stub commits, tests failing on their assertions): a graph naming two resolvable loader files declares their
  sum with the new source, through `ForGraph`, `ForGraphFile` and `Resolve` on a media and a text lease; the same files declare 0 on a card
  they fit and their sum on one they do not, at the family estimate's edge (a set equal to the usable VRAM fits), and the graph and the
  family estimate agree on the same files at every card size; one unresolved file, an empty file, a name that climbs out of the tree, a
  non-graph document, an oversized one, a graph with no loader and a box with no ComfyUI install keep the class default unchanged; `--ram`
  wins; the incident's two files sum to 23.09 GiB against a 33.1 GiB class default. In the pipeline, the graph lane declares those 23.09 GiB
  (not the class default) through `declaredRAM`, declares 0 on a 32 GiB card, keeps the class default for a file the box does not hold or
  a graph that is not there, the real `runRunGraph` path admits the incident's graph on a host that cannot take the class default and holds one it cannot size, and two new cases of the lane probe's differential test against the real wait-0 admission (a host with room
  for the graph's files and not for the class default is served; the same host with a graph naming a missing file is refused). Fixtures
  are sparse files (`os.Truncate`) of a few MiB, or a size table for the incident's GiB figures; no test starts a process. Mutants run and
  killed: an unresolved name skipped, no-loader graphs declaring 0, no size cap, first match instead of the larger, no safe-relative check, the reserve not read on the graph path or in the lane, the whole-document walk off (a partial sum declared).
- **Docs.** `docs/systems/gpu-lease.md` (the "What a lease declares" table, the hint paragraph, the admission paragraph, the warning
  paragraph) and `docs/systems/media-generation.md` (Host RAM, the graph lane question).

#### Not verified here

- No real graph was run and no ComfyUI was started; the sizes are those of sparse fixtures and of a table that stands for the measured files.
- Known gap, left as it is: a measured host peak does not raise a run-graph declaration, today or after this change. The lane sets no
  footprint key (`withFootprint`), so `calibratedRAM` returns it unchanged; no key was added here.
- Limits of the sizing: a weight is recognised by its extension, so a graph that names one by an extension outside the list above, or builds a
  name at run time, is sized by the files that were seen and can be low. The declared figure is an estimate from file sizes, not a
  measurement of the resident memory of a run.
- Known gap: the pipeline lane's family routes (image, edit, video) are still fitted with the config's reserve and not the request's
  `reserve_vram`, which reaches only the runner; the fix needs their call sites in `pipeline.go`, outside this change.
