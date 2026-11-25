### Fixed - the agent seat's measured need: one record per cold load, and no record of a load the host gave memory back during

- **What was wrong.** Two defects in the seat's self-measurement (0.238.0), both found on the reference 3-card Windows box
  by reading the store after real loads.
  - A cold load that several contracts waited on was recorded once per contract. In the first night of 0.238.0 three cold loads left
    seven records; one load is there four times, with the same two figures. The check "was this load already recorded" reads the
    key's last-write time, and the footprint store's merge of another writer's host window (`Footprints.reloadMergeLocked`) took the
    window and the run count but not that time. The check was also made outside the store's critical section, on a store opened
    before the lock that serialises it.
  - A load that began while another process was giving memory back was recorded as clean with figures far under the seat's.
    That is the load that follows a wait for host memory: it starts the moment the memory returns. Two such loads read 15.7
    and 21.9 GiB touched and 28.7 and 20.7 GiB of commit, against 34.2 and 56.9 GiB for the same seat on a quiet host. The
    measurement followed only the trough of the available memory and the peak of the commit charge.
- **What it does now.**
  - The check is the store's own (`Footprints.RecordHostOnce`): inside its critical section, after the merge of what another process
    wrote, which now carries the later last-write time. One load is one run, whichever contract or process records first.
  - The measurement also follows the two readings the other way. A run is not clean when the available memory rose above
    the baseline, or the commit charge fell under it, by more than three times the baseline's spread or 1 GiB
    (`Result.GaveBackGiB`), or when the baseline's six readings differ by more than 1 GiB. A load only takes memory, so that
    movement is something else's, and the peaks under-read by it.
  - What became of a completed load's measurement is one line on the log of the process that started it: `recorded` with the two
    peaks, the baseline's spread and what the host gave back, or `this measurement is not recorded: <why>`.
- **What did not change.** The need in force where the low and the repeated records already sit: the reader takes the 90th
  percentile of the window (34.6 GiB touched and 59.3 GiB of commit on the reference box). They leave the 20-run window as loads are
  recorded. A process that grows during the load, or shrinks where the seat's own growth hides it, is still the percentile's to answer.
- **Check.** On the reference box, through this build's MCP door. Two contracts of one call waited on one cold load of 5 min 24 s:
  both ran on the box's own seat and succeeded, the store gained ONE run (13 to 14: 51.9 GiB of commit, 32.4 touched, baseline spread
  0.25 GiB, 0.7 GiB given back), and one line said so. In an earlier run of the same two contracts another model unloaded on its idle
  limit during the load: both measurements were logged as not recorded, with that reason, and the store did not move. The give-back
  rule has not been seen to fire on a real load yet; the figures it is set against are the two under-read loads above.
