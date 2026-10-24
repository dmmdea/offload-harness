### Fixed - a render is no longer held back by the host RAM that its own unload frees

- **What was wrong.** A media lease (image, video and audio renders) was compared to the host's memory inside the lease grant,
  and the llama-swap models the render unloads were unloaded only afterwards, by the node runner. The comparison counted memory
  the render was about to free as held, waited for it, and deferred the render. On a 6 GB node a 4B llama-swap seat held 8 GiB of
  commit; an sd.cpp render that declared 6.5 GiB was told `waiting for host RAM: needs 6.5 GiB, committed 18.4 of 31.2 GiB
  physical, 8.0 GiB headroom` and never ran. `gpu reserve --unload-seat` had the same defect and was fixed first (F82).
- **What it does now.** When the host is short for a render's figure and at least one llama-swap model its own unload takes is
  resident, the lease takes the cards declaring nothing, the pipeline unloads those models (with a drain, under the lease), and
  only then asks the host. The wait is bounded by the call's `gpu_wait_ms` but never under 30 s after the unload, and never past
  `gpu_hostram_hold_max_sec`; past it the lease is released and the call is refused with the same words and place in line as
  before. A render with nothing of its own resident, an impossible figure, an unreadable host or no llama-swap endpoint keeps the
  old order exactly. The lane probe uses the same decision, so a ticket the grant admits is not reported as held by host RAM.
- **Check.** The incident's shape through `acquireMediaLease`: a resident seat, a host short by less than its footprint; the lease
  is granted declaring nothing, the seat is unloaded through llama-swap (the memory stack stays), then the figure is admitted and
  the record shows it as the declared need with the wait fields cleared. Details: `docs/systems/gpu-lease.md`, "A render lease
  follows the same order".
