### Changed - a live driver-fault repeat demotes a card's placement (ADR 0096)

- **What it does.** A repeat on a card is live while its last burst is within 24 h. A repeat here is NVSentinel's rule
  as published in 0.200.0's `gpu_faults`: one fault class in 5 or more bursts within 24 h, restart and sleep bursts left
  out. A remote whose every agent seat for the contract sits on a card with a live repeat ranks right after saturation,
  in the auto placement (`betterRemote`) and in the spread deal (`fitPickWith`). Its seats come from the node's layer
  rows, as for a lease. It is demoted, never excluded, and the placement reason names it `faulted (<class> x<bursts> on
  <bus>, last burst <time>)`.
- **Local seat.** The delegator's own seat reads busy for the deal when a live repeat sits under every local seat of a
  subtask. That reading comes from the summary fleet-serve writes (`<state root>/gpu/faults-summary.json`), and the
  seats are resolved by `placement.LocalSeatCards`. It applies only when a remote could take the work and never for
  route=local. The capacity wait reads the seat afresh, so an overflow subtask may still come back to it.
- **Why.** A card that keeps resetting ends the contexts on it, so work placed there is cut off, deferred and placed again.
  The operator accepted the change on 2026-10-24. Driver events still move no lease verdict, gate, exclusion, drain or
  release: this amends register C-83-B for placement only.
- **Check.** I fetched real health views from three fleet nodes. Clean, node B beat node A. With a seeded live repeat on
  B's card, A won and B's reason read `faulted (nvlddmkm 153 x5 ...)`. With the same repeat ending 30 h ago, the order
  was unchanged.
  - On the reference box's own config (agent seat on cards 1 and 2) with its real summary:
    - a seeded live repeat on card 1 read the local seat faulted;
    - one on display card 0 alone did not;
    - the real repeat of 2026-10-11 (stale) did not;
    - route=local never did.
  - Details: [ADR 0096](docs/architecture/decisions/0096-a-live-driver-fault-repeat-demotes-a-cards-placement.md).
