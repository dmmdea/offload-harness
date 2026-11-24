### Changed - the agent seat's host-memory need is measured by the harness, and the config's figures are the seed

- **What was wrong.** What a cold load of the agent seat costs the host was two numbers an operator typed into the config
  (`agent_seat_host_ram_gib`, `agent_seat_host_commit_gib`). Nothing compared them with a load, so they were right for the
  seat and the day they were typed for and wrong after the next model, launch setting or engine update, in either
  direction: too high refuses loads that fit, too low admits one that does not.
- **What it does now.** Every cold load of the seat that the harness starts is measured (`internal/seatmeasure`): the host's
  available memory and commit charge are read every 500 ms from just before the load until the seat is ready, and the load's
  two peaks are taken over a baseline, on a rolling median so one tick of the desktop is not a peak. A load is recorded only
  when this call started it, it completed, no lease that declares host memory was live, every reading succeeded and the
  load stood out of the host's own movement. A load that outlasts the warm-up's budget is followed until the seat is ready.
  The peaks go into the footprint store beside the renders' (`agent-seat-load`), with its 20-run window and 90th
  percentile. The contract doors, the seat warm-back and `doctor` read the need from there (`hostneed.SeatNeedMeasured`):
  with one or two loads on record each quantity is the larger of the measurement and the config's figure; from three loads
  the measurement replaces it in both directions. A seat with no load on record is judged exactly as before.
- **Check.** One real contract on the reference 3-card Windows box, seat absent, 36 agent sessions open: `succeeded 1,
  deferred 0`, output `pong`, wall 341 s, the cold load running past the 300 s admission budget. `doctor` then read:
  `seat ram: OK vLLM seat ...: measured over 1 cold loads of this seat (touched 34.1 GiB, commit 53.8 GiB at the 90th
  percentile), each held at the larger of the measurement and the seed until 3 loads exist; the config's figures 36.0 /
  61.0 GiB are the seed; a cold load is judged by 36.0 GiB touched and 61.0 GiB of commit`. An independent sampler on the
  load before it, every 2 s, had read 33.1 GiB taken from the available memory and 54.9 GiB added to the commit charge.
