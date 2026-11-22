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
  +4.0 (2.8). 1 GiB cap every 0.5 s: +0.0, +2.0 and +0.9 over three runs (1.5), the read no slower (7.4 s). A whole cold
  load under the new loop had NOT been measured when this was written: the box stayed above the commit line at which the
  gate admits a cold load, and a load is not forced past that refusal. The launcher's own log line is where the next
  load shows it.
- **Not changed.** Native Linux (the cache there is reclaimable kernel memory, not host commit), the serving-phase cap,
  and everything else in the launcher.
- **Where.** `setup/templates/vllm-seat/seat_fg.sh`, `docs/systems/gpu-lease.md`.
