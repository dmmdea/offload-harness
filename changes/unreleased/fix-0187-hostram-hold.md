### Fixed - a lease that unloaded the seat no longer holds its cards idle while the host cannot meet its RAM: the wait after the unload is bounded, and a class-default figure is warned about

- **What it did.** On 2026-10-14, on the reference box, `gpu reserve --wait 8h --drain --unload-seat --class media --for 40m -- <a command that
  is not a recognised render helper>` with no `--ram` declared the media class default (46.8 GiB, the largest render family). F82 takes the
  cards, drains, unloads the seat and only then asks the host, still holding the cards, for the whole `--wait`. The seat was not even
  loaded (the unload freed nothing), committed RAM was 84.6 of 127.7 GiB with 8 GiB of headroom, so the figure could not be met, and the
  lease held all three cards idle for about 50 minutes while a production lease queued behind it, renewing its window with no work running.
- **The fix.** F82's order is kept (the drain and the unload first: it breaks a real deadlock); only the hold after it is bounded.
  New config key `gpu_hostram_hold_max_sec` (default 600, chosen and not measured; `0` = the old wait inside `--wait`; a negative value is
  refused by name) and `gpu reserve --ram-hold-max <duration>`, which overrides it for one reserve (`0` is a value). The bound is counted from
  the moment the unload is done and is not raised to the drain floor. When it passes without the host's admission the lease is **released**
  (the seat warmed back as for any release), **nothing runs**, nothing re-queues, and the reserve exits non-zero with one line that names the
  need, the host's committed / physical / headroom figures, how long the cards were held, and what to do (pass a smaller `--ram` when the
  figure is the class default, or wait for the host). `--detach` releases the same way (`gpu release --warm-seat --epoch N`); a wait that
  `--wait` ends first behaves exactly as before in both forms. The hidden `gpu hold` verb has no wait of its own and takes no flag.
- **Three more lines of visibility.** The waiting line says when the unload freed nothing on the lease's cards (the seat was not loaded) and
  names the bound that will release the lease; the bound still applies, a cold seat is not failed at once. The class-default line is now a
  `WARNING` that the declared figure may hold the cards while the host cannot meet it, naming `--ram <GiB>` (the figure itself is unchanged).
  `gpu status` says how long a waiting lease has held its cards and when the bound releases it (`host_ram_hold_until_ms` on the record,
  additive; `host_ram_hold_until` on the lease row).
- **Docs.** `docs/systems/gpu-lease.md` (the F82 section: the order, the hold bound, each form, the third consequence), the OPERATOR-GUIDE
  config row (in the guide's one key table; it has no GPU-key table) and host-RAM paragraph. F82 is recorded in no ADR (ADR 0009 and
  ADR 0080 were checked: their host-RAM and unload passages are about other mechanisms), so there is no ADR amendment.
- **Tests, red first.** The seams and failing tests were committed first (`test(gpulease): red - ...`): a fake host whose RAM never fits and a
  reserve with `--unload-seat` that must release the cards, warm the seat back, run nothing and exit non-zero inside a 200 ms bound (wrapper
  and detached forms), the same through the config key, bound 0 keeping the whole `--wait` (flag, config, and the flag beating a configured
  bound), the waiting line with and without a freed seat, the warning text, `gpu status`, the record's stamp (stamped while the wait runs,
  cleared on admission and when the wait ends refused), and the config accessor. Red: 251 s with every bound test failing (the reserve
  held the cards for the whole wait, 123 s for the one that keeps the production 2 minute floor); green: 22 s.
- **Mutations** (each compiled, was proved applied by a changed sha256, turned a named test red, and was restored byte-for-byte): bound ignored;
  bound error swallowed so the command runs; lease not released (wrapper, detached); seat not warmed back (detached); warning dropped; config 0
  read as the default; `--ram-hold-max 0` ignored; unload always reported as having freed something; bound never stamped; stamp left after a
  refused wait; stamp left after admission; status never naming the bound. Two mutants first survived or failed to compile and the tests were
  made to bite (the zero-flag case needed a `--wait` longer than the configured bound it overrides).
