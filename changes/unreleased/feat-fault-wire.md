### Added - the fleet node publishes each card's driver faults (`gpu_faults`), retains incidents, and labels restart and sleep bursts

- **What it does.** `/fleet/health` gains `gpu_faults`: per card, the driver records and incidents of the last 30 days, the
  bursts at a restart or sleep transition, the bursts of the last 24 h, and NVSentinel's repeat rule where it is met. It
  is read in the background every 10 minutes (one read took 3.4 s on the reference box), served from memory, absent
  until the first read lands and on a node with no NVIDIA card. fleet-serve also appends each ended incident to
  `<state root>/gpu/faults.jsonl` and writes the summary to `<state root>/gpu/faults-summary.json`; `gpu faults` merges
  the retained incidents the OS log no longer holds, marked `retained`. Facts only: nothing reads them for placement in
  this release.
- **Restart and sleep context.** `gpu faults` labels a burst that overlaps a restart (User32 1074, EventLog 6005/6006/6008,
  Kernel-General 12/13, Kernel-Power 41 on Windows; a boot's first journal entry on Linux) or a sleep transition
  (Kernel-Power 42/107, Power-Troubleshooter 1; the kernel's `PM: suspend entry/exit`), and leaves it out of the repeat
  rule. Kernel-Power 566 is not read: it is a session transition (244 in the reference box's 30 days). Each burst now
  keeps its classes per card, so the repeat rule counts retained bursts too.
- **Why.** The 0.199.0 report was local and forgot what the OS log dropped. The fleet had no way to see which card has
  been faulting, and two of the reference box's 19 incidents were restart teardown counted like faults under work.
- **Check.** On the reference box, `gpu faults --days 30` labels 2026-10-15 22:02 and 2026-10-21 08:43 as restarts
  (User32 1074 then Kernel-General 12) and still fires the repeat on card `65`. The retention path with a fresh state
  root appended 19 incidents, then 0; five hand-written bursts on a day the log does not cover were listed as retained,
  counted on card `b5` and fired the repeat rule there, without counting the 19 live incidents twice. Details:
  [gpu-lease.md](docs/systems/gpu-lease.md#retention-and-the-fleet-nodes-summary),
  [FLEET-NODE.md](docs/FLEET-NODE.md#driver-fault-summary-gpu_faults).
