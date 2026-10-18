### Fixed - the vLLM seat launcher mounts the cache-server share at the store address with the lowest ping round-trip time when several of its addresses lie on the pinned network

Residual of register B-55, in `setup/templates/vllm-seat/seat_fg.sh` (shell rendered verbatim into the seat, so one file; no renderer and no installer changes). The previous B-55 fix mounted
the FIRST mDNS answer inside the pinned source address's IPv4 network. When the store host has two answers on the SAME network, a wired address and a Wi-Fi address, "first inside the network" is
arrival order, and the Wi-Fi path is about ten times slower for writes, so the cache tier degraded.

- **The fix.** With more than one distinct answer inside the pinned network, `l2_pick_address` measures each candidate's minimum round-trip time with
  `ping -I <source address> -c 5 -i 0.2 -q -W 1 <address>` (the first number after `=` on the `min/avg/max` line, so it reads iputils and busybox summaries) and mounts the lowest. Each ping runs under
  `timeout -s KILL 3`, one after the other, so the measurement takes at most 3 s per candidate and a start can never hang on it. Equal minimums go to the earlier answer. With exactly one answer inside the
  network nothing changes and no ping runs; an answer outside the network is never pinged, whatever its time would be.
- **Why the minimum.** Measured on the reference workstation with `ping -c 5 -i 0.2 -q`, three runs each: the wired address had a minimum RTT of 0.425-0.477 ms (average 0.57-0.62 ms), the Wi-Fi address a
  minimum of 0.983-2.288 ms (average 1.5-26 ms). The minimum separates them by more than 2x on every run; the average is noisy on the Wi-Fi path.
- **Fallback.** A candidate whose ping fails (no reply, cut off by the timeout) shows `no reply` and is never chosen over one that replied. When ping is missing (`not measured`), every ping fails, or no time
  can be read from its output (`RTT unreadable`), the choice is the first answer inside the network by arrival order, the rule before this change. The `seat_fg:` line says which rule chose and lists
  each candidate, for example `mDNS answers: A B C, inside N/P: A (min RTT 0.43 ms) B (min RTT 1.10 ms); chose A, the lowest round-trip time`; the fallback reads
  `chose A, the first inside the network in arrival order (round-trip times could not be compared: <why>)`, with three reasons (ping is not available on this box; no candidate replied to ping; no round-trip time
  could be read from ping's output), and a single replier reads `chose B, the only candidate that replied to ping`. A one-answer line is byte-identical to before.
- **Limit, stated.** The round-trip time is a heuristic for the better path, not a throughput measurement: a store whose fast path has a higher latency than its slow path would be picked wrong, and a store whose
  fast path drops ICMP loses to a slow path that answers it. `SEAT_L2_MOUNT_SRC` naming a reserved or static address (an IPv4 literal) or a name that is not `.local` still overrides everything: nothing is looked up
  and nothing is pinged. A share that is already mounted is still left alone (the address is chosen only when the share is mounted), so a start after this change does not remount a share an earlier start put on
  the Wi-Fi address; unmount it while no seat uses it and start the seat again.
- **Test seam.** `SEAT_FG_TEST_PING_CMD` names a command that stands in for ping (called as `<cmd> <source address> <address>`; it prints a minimum RTT in ms, or exits non-zero for no reply). It is documented in the
  script with the other two seams, is read nowhere else, and nothing in a rendered seat sets it.
- **Tests (red first).** `seat_fg.kv-and-store.tests.sh` gains 24 cases against two ping stand-ins (the seam, and a `ping` found on `PATH` that prints what iputils or busybox print; the real `ping` is never reached, and the
  case without ping runs on a `PATH` that has none): the second or the first or the last or the middle candidate lowest, times compared as numbers (12.5 above 9), a tie, a no-reply first, second or one of three, every
  ping failing, a ping command that does not exist, no ping on `PATH`, unreadable output, a hanging ping cut off at 3 s (both shapes), the exact ping command line and the minimum taken instead of the average
  (a candidate with minimum 0.45 and average 26.0 beats 1.00 and 1.20), a busybox summary, one answer inside the network (no ping, both shapes), answers outside the network never pinged, and `SEAT_L2_MOUNT_SRC` as an
  address literal or a non-`.local` name (no lookup, no ping). On `origin/main`'s launcher 20 of the 24 fail; the other 4 are guards that pass on both sides (the one-answer and the `SEAT_L2_MOUNT_SRC` cases), and all
  49 earlier cases pass on both. The existing case "the FIRST answer inside the network wins" keeps its assertions and is renamed to say what it now proves: with no round-trip time to compare, the first one wins.
  Mutants of the launcher, each caught: the highest time taken, a tie going to the later answer, no source address on ping, 3 probes instead of 5, the average read instead of the minimum, no timeout on ping, no timeout on
  the seam, every answer pinged and not only those inside the network, a failed ping counted as 0, the fallback taking the last answer, unreadable output and a missing ping both reported as no reply, a single in-network answer pinged too, the single-replier wording
  dropped. The Go wrappers (`launcher_kv_store_run_test.go`, `launcher_kv_store_pins_test.go`) name all 73 cases and pin the ping line as one whole command, its timeout, the strict comparison, the one-answer branch, the
  minimum read and that ping is started in exactly one place.
- **Docs.** `docs/OPERATOR-GUIDE.md` and `docs/systems/cache-server.md` describe the tie-break, the fallback and the limit in place of the old "cannot tell them apart" paragraph.
- **Not verified here.** No live store and no real network: every ping in the tests is a stand-in, and the figures above are the reference workstation's own measurement. The real `ping` output shape was read on a
  loopback address only.
