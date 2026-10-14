### Fixed - a lease that unloaded the seat no longer holds its cards idle while the host cannot meet its RAM: the wait after the unload is bounded, a detached hold releases itself even if its reserve dies early, and a class-default figure is warned about

- **What it did.** On 2026-10-14, on the reference box, `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that
  is not a recognised render helper>` with no `--ram` declared the media class default (46.8 GiB, the largest render family). F82 takes the
  cards, drains, unloads the seat and only then asks the host, still holding the cards, for the whole `--wait`. The seat was not even
  loaded (the unload freed nothing), committed RAM was 84.6 of 127.7 GiB with 8 GiB of headroom, so the figure could not be met, and the
  lease held all three cards idle for about 50 minutes while a production lease queued behind it, renewing its window with no work running.
- **The bound.** F82's order is kept (the drain and the unload first: it breaks a real deadlock); only the hold after it is bounded.
  New config key `gpu_hostram_hold_max_sec` (default 600, chosen and not measured; `0` = the old wait inside `--wait`; a negative value or
  one above 7 days is refused by name, the accessor clamps before it converts to a `Duration` so a huge value can no longer wrap into "no
  bound", and the reserve says once that it clamped) and `gpu reserve --ram-hold-max <duration>`, which overrides it for one reserve (`0` is
  a value). The bound is counted from the moment the unload is done and is not raised to the drain floor. When it passes without the host's
  admission the lease is released (the seat warmed back as for any release), nothing runs, nothing re-queues, and the reserve exits non-zero
  with one line that names the need, the host's committed / physical / headroom figures, how long the cards were held, and what to do (pass
  a smaller `--ram` when the figure is the class default, or wait for the host). The text says only what is true when it is built ("the
  lease is being released"); a release that then fails prints `LEASE NOT RELEASED (epoch N): ...; run gpu release [--warm-seat] --epoch N`
  and the error the command returns ends with the failure (`the lease is STILL HELD`). `--warm-seat` is advised only when a warm-back is
  owed, because the manual release has no guard against loading a seat nothing unloaded.
- **A detached hold cannot outlive its reserve.** The bound used to be enforced only by the foreground reserve: if it died the hidden holder
  kept the cards for good. The record now carries a moment by which the lease is released: an OUTER bound, now + the hold bound + 60 s,
  stamped as soon as the holder has the lease and re-stamped every 30 s by the LIVE reserve while it drains and unloads (the drain's own
  deadline is `--wait` away, 8 h by default, which is no bound), so a reserve killed during the drain or the unload, however long `--wait`
  is, is covered within about the bound; replaced by the wait's own end (the earlier of the bound and `--wait`) when the wait begins, and
  taken off only when the host admits the figure (a wait that ends any other way leaves it, so a reserve that dies the instant its wait ends
  is covered too). The same re-stamp closes the too-EARLY direction: an unload is not bounded by the drain's deadline (the maintenance
  client allows 15 minutes per request), and a long unload used to make the holder release a live reserve's lease mid-unload, leaving the
  seat owed and cold; a live reserve now loses its lease to its holder only if it cannot re-stamp for more than the bound + 60 s + 30 s. A failed drain's lease, which the detach form keeps held, is released by the holder at the last stamped moment unless the
  operator frees it sooner, and the error says when. The hidden `gpu hold` reads its record
  on every poll and releases its lease 30 s after that moment if the lease still asks for the host (instances stopped, the seat warmed back
  only if the lease owes it); a live reserve pushes the moment out before its own minutes-long release so the two never race or double the
  warm-back, and a reserve that dies in the middle of that release is still covered. The detached wait **fails closed** on its stamp: tried
  four times before the host is looked at, and if it never lands the wait is not run and the lease is released (an outer bound already on the
  record stays). A detached wait that `--wait` ends first used to keep the lease, idle and non-exclusive, indefinitely: it releases now, as
  the wrapper form always did. The hidden `gpu hold` verb has no wait of its own and takes no flag.
- **The holder's own failure paths.** A fence read that fails is no longer taken for a lost lease: `Lease.Check` tells a record it could not
  READ from a verdict (gone, fenced out), the holder confirms a failed fence with further looks, retries a failed read for 30 s with one
  line, and says what it let go on. Its own release is retried four times; when it gives up it prints `LEASE NOT RELEASED`, marks the record
  (`holder_gave_up_ms`, additive), and exits non-zero instead of exiting 0 into a log in the temp directory, and `gpu status` says that
  nothing polls the lease any more, also once the holder has exited and the record reads as a stale holder (`gpu status` printed "free"
  and the stale-holder line with no word of the marker).
- **A wrapper no longer kills its command over a record it cannot read.** On Windows under load a heartbeat that raced an atomic replace
  of the lease record read the lease as gone: the wrapper's tick printed `LEASE LOST` and killed the wrapped command, the drain's
  heartbeat cancelled the drain, and the drain loop released the lease and queued again. One shared helper (`confirmFence`, the hidden
  holder's, now used by the wrapper's tick, the drain heartbeat and the drain loop) looks again, tells a verdict from a failed read, and
  waits out a window; on a record it still cannot read the wrapper says so once, keeps the command running and the drain going, and
  releases and requeues nothing. A heartbeat write that fails while the lease is still ours no longer kills the command either.
- **A record that exists but cannot be read is a held lease.** `Inspect()` and `Leases()` judge who holds the cards and skip a record they
  cannot read, and the release paths believed them (reproduced on Windows by replacing the record with a directory). `Manager.RecordState`
  looks at the record itself (absent, held, unreadable; unreadable is held) and now decides every "the lease is gone": the release
  confirmations, the holder's own release (it can no longer return nil and exit 0 over a lease it could not touch), `gpu release`'s
  post-check, and the whole-node `Lease.Release`, which returns a wrapped `*RecordUnreadableError` instead of nil. Bare `gpu release`
  with no `--epoch` on a card-scoped host names an unreadable card record (`epoch N: its record cannot be read; run again or pass --epoch
  N`) instead of printing "no lease is held" and exiting 0. `Restamp` keeps the cause (`%w`) and says "cannot be read" rather than "the
  lease is gone"; the host-RAM wait looks again three times, 100 ms apart, before it believes a missing lease is gone; the detached
  reserve's release proceeds over an unreadable record (stop the instances, warm back, release, report). The holder's release keeps its
  error while a claim of the epoch remains (a card lease removes its record before its claims, so "the record is absent" alone is not
  "released"), and the stop line over a record it cannot read says so instead of "the lease is no longer ours". A release's answer "no lease was held" is believed only if the epoch is off the record: the detached
  reserve, the wrapper's final release, the holder and `gpu release` no longer report a held lease as released or as "no lease is held",
  and `ReleaseByEpoch` answers "nothing held" only for an absent record. The holder's grace before it steps in is 30 s, so a reserve stalled
  under paging is not given a parallel warm-back.
- **Visibility.** The waiting line says when the unload freed nothing on the lease's cards (the seat was not loaded) and names the bound; the
  bound still applies, a cold seat is not failed at once. A failed unload of another resident is counted and said ("N unloads failed"), never
  read as "freed nothing". A stamp that cannot be written is reported once. The class-default line is now a `WARNING` that the declared
  figure may hold the cards while the host cannot meet it, naming `--ram <GiB>` (the figure itself is unchanged). `gpu status` says how long
  a waiting lease has held its cards and when it is released if the host still lacks the room; past that moment it says "the holder, if one
  is running, releases the lease at its next poll; `gpu release --epoch N` frees it now" for a detached lease (`host_ram_hold_until_ms` on
  the record, additive; `host_ram_hold_until` and `holder_gave_up` on the lease row). The ComfyUI instances kept under a lease are stopped
  only by a caller that fenced the epoch first, and a source test now pins each caller by the function that holds the call across the whole
  package.
- **Open, stated.** A reserve that dies in the few milliseconds between the holder taking the lease and the first outer stamp leaves no
  stamp; with the hold bound at 0 there is no outer bound (only `--wait` ends the wait); a holder cannot write its give-up marker onto a
  record it cannot read, so over such a record the exit status and its log are the only word; the reserve measures its wait on a monotonic
  clock and the holder compares wall-clock milliseconds, so after a suspend the holder can release sooner (never later) than the stamp
  says; the warm-back's guard still abandons a warm on a failed `Check` (it kills and releases nothing; the seat stays owed); the
  hidden-holder process itself is exercised in-process, not as a spawned child.
- **Docs.** `docs/systems/gpu-lease.md` (the F82 section: the order, the hold bound, the outer bound, "Releasing on its own", each form, the
  third consequence) and the OPERATOR-GUIDE config row (in the guide's one key table; it has no GPU-key table) and host-RAM paragraph. F82 is
  recorded in no ADR (ADR 0009 and ADR 0080 were checked: their host-RAM and unload passages are about other mechanisms), so there is no
  ADR amendment.
- **Tests, red first.** Every commit pair is a failing-test commit followed by its fix: the bound (a fake host whose RAM never fits and a
  reserve with `--unload-seat` that must release the cards, warm the seat back, run nothing and exit non-zero inside a 200 ms bound, wrapper
  and detached, through the flag and the config key; red ran 251 s with every bound test failing because the reserve held the cards for the
  whole wait, 123 s for the one that keeps the production 2 minute floor; green 22 s), the ceiling, failed unloads, stamp errors, the
  detached round (the holder releasing its own lease after the stamp passes, the reserve that dies mid-release or the instant its wait ends,
  `--wait`-ended detached waits, a cold seat never warmed, a failed release reported, `gpu status` after the stamp passes, the fence
  confirmation), and the third round (the outer bound, the fail-closed stamp, the holder's failure paths, the confirmed release, the
  production grace, the clamp line), and the fourth (the unreadable-record round: `RecordState`, bare `gpu release`, the wait and restamp,
  the stale-holder line, the live outer bound, and an instance-stop scan over whole declarations). The two tests that pit the holder
  against a live parent hold the holder's clock back and hold the reserve AT its push-out (a hook, not a poll of the record), and the
  holder reads its clock before its record, so no scheduler delay can decide them.
- **Mutations** (each compiled, was proved applied by a changed sha256, turned a named test red, and was restored byte-for-byte): bound
  ignored; bound error swallowed so the command runs; lease not released (wrapper, detached); seat not warmed back (detached); warning
  dropped; config 0 read as the default; `--ram-hold-max 0` ignored; unload always counted as freeing something; bound never stamped; stamp
  cleared as a wait ends (whole, and only when the end comes before its moment); status never naming the bound; the holder ignoring a passed
  stamp; the holder acting on an admitted lease; a `--wait`-ended detached wait keeping its lease; the automatic release warming a seat it
  does not owe (parent and holder); a failed release not reported (wrapper, detached); the failure claiming the release; no clamp, no ceiling,
  no flag ceiling; no push-out before the parent's release; the stamp not written at the start of the wait or only for the bound; a failed
  stamp said at every look or not reported; a failed unload not reported; the holder letting go on one failed fence read; and, for the third
  round: no outer stamp, an outer stamp without the hold bound, an outer stamp with the bound off, the holder exiting 0 on a failed release,
  not retrying it, leaving no marker, status promising a dead poll, a failed read taken for a verdict (card and whole-node leases), no retry
  of a failed read, the holder letting go without a word, the released bool ignored, `gpu release` claiming no lease is held, a record that
  cannot be examined read as nothing held, the detached wait not failing closed, a required stamp tried once, the returned error not ending
  with the failed release, `--warm-seat` always advised, a stamp landing on an admitted lease, the production grace back to 5 s, the clamp
  silent, and a stray direct stop of the ComfyUI instances in another function, in a file of its own, and through a lease object in an
  unlisted function.
  The fourth round ran no mutation battery, by order: its proofs are the red-then-green pairs, the reviewer's slow-unload probe (red on the
  tree before the live re-stamp), one stray `stopKeptInstances` in a package-level initializer turning the instance-stop scan red, and a
  starvation tally for the push-out race.
