### Fixed - `update` run inside `gpu reserve` no longer waits out the lease queue behind its own parent

- **The gap.** `local-offload gpu reserve --wait 30m --drain --for 20m -- local-offload update --component llama` hung.
  The llama smoke gate takes its own lease (`gpu reserve --drain --unload-seat -- update --gate-child`), and that inner
  reserve queued behind the outer lease its own parent held, for the whole `update.gate.lease_wait`. The update then
  ended inconclusive with nothing changed. The house rule that every GPU job goes through `gpu reserve` sent every agent
  into it.
- **What it does.** The gate checks whether this process already runs under a live lease (`GPU_LEASE_EPOCH` equals the
  epoch of a live lease in `GPU_LEASE_DIR`). When that lease drained the seat and cleared it (`--drain --unload-seat`),
  the gate runs its measurement under it and logs "running under the caller's GPU lease (epoch N)". When it did not, the
  gate answers inconclusive at once, saying to run `update` without the wrapper or to wrap it with
  `--drain --unload-seat`. Outside a lease nothing changes. No other component's gate takes a lease.
