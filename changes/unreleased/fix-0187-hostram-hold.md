### Fixed - a lease that unloaded the seat no longer holds its cards idle while the host cannot meet its RAM: the wait after the unload is bounded, a detached hold releases itself, and a class-default figure is warned about

- **What it did.** On 2026-10-14, on the reference box, `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that
  is not a recognised render helper>` with no `--ram` declared the media class default (46.8 GiB, the largest render family). F82 takes the
  cards, drains, unloads the seat and only then asks the host, still holding the cards, for the whole `--wait`. The seat was not even
  loaded (the unload freed nothing), committed RAM was 84.6 of 127.7 GiB with 8 GiB of headroom, so the figure could not be met, and the
  lease held all three cards idle for about 50 minutes while a production lease queued behind it, renewing its window with no work running.
- **The bound.** F82's order is kept (the drain and the unload first: it breaks a real deadlock); only the hold after it is bounded.
  New config key `gpu_hostram_hold_max_sec` (default 600, chosen and not measured; `0` = the old wait inside `--wait`; a negative value or
  one above 7 days is refused by name, and the accessor clamps before it converts to a `Duration`, so a huge value can no longer wrap into
  "no bound") and `gpu reserve --ram-hold-max <duration>`, which overrides it for one reserve (`0` is a value). The bound is counted from
  the moment the unload is done and is not raised to the drain floor. When it passes without the host's admission the lease is released
  (the seat warmed back as for any release), nothing runs, nothing re-queues, and the reserve exits non-zero with one line that names the
  need, the host's committed / physical / headroom figures, how long the cards were held, and what to do (pass a smaller `--ram` when the
  figure is the class default, or wait for the host). The text says only what is true when it is built ("the lease is being released"); a
  release that then fails prints `LEASE NOT RELEASED (epoch N): ...; run gpu release --warm-seat --epoch N`.
- **A detached hold cannot outlive its reserve.** The bound used to be enforced only by the foreground reserve: if it died during the wait
  (a tool timeout, Ctrl-C, a closed terminal) the hidden holder kept the cards for good. The wait now stamps its end on the lease record
  when it BEGINS (the earlier of the bound and `--wait`) and takes it off only when the host admits the figure (a wait that ends any other way
  leaves it: clearing it as the wait ended opened a window of milliseconds in which a reserve that died stranded the cards), the hidden
  `gpu hold` reads its own record on every poll and releases its lease once that moment has passed and the lease still asks for the host
  (instances stopped, the seat warmed back only if the lease owes it), a live reserve pushes the moment out before its own minutes-long
  release so the two never race or double the warm-back, and a reserve that dies in the middle of that release, or the instant its wait
  ends, is still covered. A detached wait that `--wait` ends first used to keep the lease, idle and non-exclusive,
  indefinitely (the incident from a smaller `--wait`): it releases now, exactly as the wrapper form always did. The automatic release warms
  the seat only if the lease owes the warm-back, so a seat that was never loaded is not loaded by it. The holder also confirms a failed
  fence before it lets go (a fence read that lands during a restamp fails for a moment on Windows and the holder's exit releases the lease).
  The hidden `gpu hold` verb has no wait of its own and takes no flag.
- **Visibility.** The waiting line says when the unload freed nothing on the lease's cards (the seat was not loaded) and names the bound; the
  bound still applies, a cold seat is not failed at once. A failed unload of another resident is counted and said ("N unloads failed"), never
  read as "freed nothing". A stamp that cannot be written is reported once. The class-default line is now a `WARNING`
  that the declared figure may hold the cards while the host cannot meet it, naming `--ram <GiB>` (the figure itself is unchanged). `gpu
  status` says how long a waiting lease has held its cards and when it is released if the host still lacks the room; past that moment it
  says who releases it (the holder at its next poll, the wrapper when it exits) and the command that frees it now (`host_ram_hold_until_ms`
  on the record, additive; `host_ram_hold_until` on the lease row).
- **Docs.** `docs/systems/gpu-lease.md` (the F82 section: the order, the hold bound, "Releasing on its own", each form, the third
  consequence) and the OPERATOR-GUIDE config row (in the guide's one key table; it has no GPU-key table) and host-RAM paragraph. F82 is
  recorded in no ADR (ADR 0009 and ADR 0080 were checked: their host-RAM and unload passages are about other mechanisms), so there is no
  ADR amendment.
- **Tests, red first.** Every commit pair is a failing-test commit followed by its fix: the bound (a fake host whose RAM never fits and a
  reserve with `--unload-seat` that must release the cards, warm the seat back, run nothing and exit non-zero inside a 200 ms bound, wrapper
  and detached, through the flag and the config key; red ran 251 s with every bound test failing because the reserve held the cards for the
  whole wait, 123 s for the one that keeps the production 2 minute floor; green 22 s), the ceiling, failed unloads, stamp errors, and the
  detached round (the holder releasing its own lease after the stamp passes, the reserve that dies mid-release or the instant its wait ends,
  `--wait`-ended detached waits, a cold seat never warmed, a failed release reported, `gpu status` after the stamp passes, the fence
  confirmation).
- **Mutations** (each compiled, was proved applied by a changed sha256, turned a named test red, and was restored byte-for-byte): bound
  ignored; bound error swallowed so the command runs; lease not released (wrapper, detached); seat not warmed back (detached); warning
  dropped; config 0 read as the default; `--ram-hold-max 0` ignored; unload always counted as freeing something; bound never stamped; stamp
  left after a refused or admitted wait; status never naming the bound; the holder ignoring a passed stamp; the holder acting on an admitted
  lease; a `--wait`-ended detached wait keeping its lease; the automatic release warming a seat it does not owe (parent and holder); a failed
  release not reported (wrapper, detached); the failure claiming the release; no clamp, no ceiling, no flag ceiling; no push-out before the
  parent's release; the stamp not written at the start of the wait or only for the bound; the stamp cleared as a wait ends (whole, and only when the end comes before its moment); a failed stamp said at every look, or not reported; a failed unload not reported; the holder
  letting go on one failed fence read. Two mutants first survived or failed to compile and the tests were made to bite; a repeated run of
  the holder tests found a flake that was a real race (a fence read during a restamp), fixed above.
