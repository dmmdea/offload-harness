### Fixed - the vLLM seat launcher bounds the guest page cache during the load, not only after it

- **What was wrong.** On WSL2 the launcher's page-cache watcher did nothing until the engine answered `/health`. vLLM
  reads the checkpoint through the guest page cache and keeps none of it mapped once the weights are on the cards, so for
  the rest of the load the whole checkpoint sat in the cache as a second copy, and guest cache is host commit. Sampled on
  the reference box: the engine read 19 GiB in 20 s, about 70 s before it answered, and that dead copy was the top third
  of the load's host peak (commit 49.5 -> 116.0 GiB at the peak, about 95 once serving). The peak is what a cold load has
  to fit on the host, so the contract door's host-RAM gate was refusing loads for memory nothing used.
- **What it does now.** Until the engine answers, the watcher looks every 0.5 s and drops clean cache once the file cache
  is over `SEAT_LOAD_CACHE_CAP_MIB` (default 1024) and 256 MiB above what the last drop left. It does not `sync` there (a
  checkpoint being read has no dirty pages, and `sync` walks every mount). At the first `/health` answer it syncs, drops
  once and logs `seat_fg: page cache bounded during the load: N drops, largest M MiB seen ...`; the first load-time drop
  logs one line too. Once serving it steps every 30 s against `SEAT_CACHE_CAP_GIB`, as before.
- **Why the loop is that tight.** The VM returns freed pages to the host 15 to 25 s late, so pages the guest touched and
  dropped stay host commit meanwhile. A loop that lets the cache grow by GiBs between drops still grows the VM.
- **Check.** A read test on the reference box, the cache's share only: the checkpoint's 18.1 GiB read to `/dev/null`
  inside the guest with a standalone copy of the loop beside it, growth of the VM's private bytes on the host over the
  read. No loop: +17.4 GiB (guest file cache 18.3 GiB). 4 GiB cap every 2 s: +9.5 (cache peak 5.2). 2 GiB cap every 1 s:
  +4.0 (2.8). 1 GiB cap every 0.5 s: +0.0, +2.0 and +0.9 over three runs (1.5), the read no slower (7.4 s). Then two
  whole cold loads of the seat under the new launcher, through the contract door and inside a GPU lease, host commit
  sampled every 2 s: the peak rose +55.3 and +53.4 GiB over the base (the old watcher: +66 to +72), +44.4 and +41.9 75 s
  after ready (as before). Weight read 13.4 and 13.7 s (12.3 to 13.0 s on the old launcher). The launcher logged
  `page cache bounded during the load: 39 drops, largest 18515 MiB seen` and `36 drops, largest 18497 MiB seen`.
- **What it does not do.** It cannot hold the cap during the weight read itself: vLLM maps the two shards while it copies
  them, mapped pages are not dropped, and the cache reached 18.5 GiB both times. What it removes is the copy left after
  that read, which used to stay through the compile and the profiling run.
- **Not changed.** Native Linux (the cache there is reclaimable kernel memory, not host commit), the serving-phase cap,
  and everything else in the launcher.
- **Where.** `setup/templates/vllm-seat/seat_fg.sh`, `docs/systems/gpu-lease.md`.
