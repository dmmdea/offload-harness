### Fixed - a commit is held to the ceiling the commit limit can reach, not to where the limit stands

- **What was wrong.** The host-memory rule held a commit to the CURRENT commit limit (a stated commit) or to physical RAM
  (a commit nobody stated), and called the first "the conservative choice". On Windows with a page file that can grow, the
  default, neither is a line the OS enforces. Windows keeps the limit only 14 to 28 GiB ahead of the commit charge and
  raises it when a request does not fit, so a load that commits more than that spare was refused whatever memory the host
  had. On the reference 3-card box (127.7 GiB RAM) a desktop with 36 agent sessions sat at 125 to 141 GiB committed with 67
  to 71 GiB of RAM available, and the agent seat (36 GiB touched, 61 GiB of commit) was refused on every local contract of a
  day: `needs 61.0 GiB of commit, committed 125.2 of 152.7 GiB commit limit, 8.0 GiB headroom`. That node's ledger holds
  3,397 refusals on the commit-limit sentence and 2,903 on the physical-RAM one, 16 to 40 percent of each day's agent
  contracts for a week. The figures the previous release wrote into the node's config moved the toll and did not remove
  it. The same lines locked out every media lease that declares host RAM, and `gpu status` and `offload_status` read
  `HOST RAM OVER (committed 130.4 of 127.7 GiB physical RAM ...)` to every session on a box with 71 GiB available.
- **What it does now.** The reading carries a commit CEILING: the current limit plus the room each page file still has to
  grow, its maximum (read from the kernel, `SystemPageFileInformationEx`) less its size, bounded by the free space of its
  volume less a tenth of the volume. A stated commit is admitted while the charge, the commit and what running leases have
  yet to be charged stay under the ceiling less the headroom. A commit nobody stated, on a host whose limit can grow, is
  taken as 2.1 times the touched figure (the ratio measured on that box) and held to the same line, and so is what a lease
  already granted has yet to load: a lane granted a moment ago is in neither counter. The host verdict on such
  a host rests on available memory and the ceiling. A refusal says `committed X of a commit limit that is A GiB now and can
  reach B GiB`, and the status JSON gains `commit_ceiling_gib`. The lane auto-plan pre-filter now passes the commit a
  graph's estimate states, as the grant already did.
- **What did not change.** The touched-set condition: the available memory less the need, less what running leases have yet
  to touch, must leave the headroom. That is what keeps a load from paging, and it is the only thing that does. Where the
  page file cannot grow (a fixed file, a file at its maximum, no file) and on Linux the ceiling is the current limit and
  every line is the one it was.
- **What it costs.** A grant that leans on the ceiling makes the page file larger on disk, and it stays that size until the
  host restarts. Commit is a promise, not use: the file backs it without being written.
- **Check.** Before the change, on that box: a 19 GiB commit that touched nothing, requested with 13.7 GiB of spare commit,
  was granted in 0.75 s (limit 154.7 to 160.8 GiB, available memory 67.1 GiB before and after); the kernel reported the
  file at 33.1 GiB of a 116.3 GiB maximum on a volume with 338 of 930 GiB free, a ceiling of 244.0 GiB. With the change,
  `gpu status` on the same box reads `host memory: OK - 127.7 GiB physical, 71.2 GiB available, commit 130.7 of 160.8 GiB
  (the page file can take that limit to 244.0 GiB, the line a commit is held to)` where the released binary reads `OVER`.
  One real contract, pinned local, seat absent, started at 131.9 GiB committed: `succeeded 1, deferred 0`, output `pong`,
  wall 329.5 s. Sampled every 2 s through the load: the charge peaked at 186.8 GiB while the limit grew from 160.8 to
  188.0 GiB; available memory bottomed at 37.1 GiB; pages written out stayed at 0 per second and page-file use fell from
  0.70 to 0.38 percent of a larger file; the video editor and the encoder running beside it stayed up. The seat's VM was
  charged 57.2 GiB at the peak and held 35.0 GiB of working set, against the 61 and 36 the node declares.
