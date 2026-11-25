### Changed - the local agent seat is busy when it has no free slot, not when it has a request, and its line follows the engine

- **What was wrong.** Two fixed figures decided whether a box's own agent seat took a contract. `route=spread` read the seat as busy at
  ONE request in flight (a rule of 2026-09-06, older than the line that bounds the runs of a box), so a deal made while anything ran
  there gave its slots to the remotes, and a subtask kept off the seat could not have it back until the seat held nothing at all. And
  the line was four long whatever the runs were. Read from the ledger of the reference 3-card Windows box: of 91 contracts in one night
  26 were refused, 17 of them after 16 to 24 minutes in line, and in 23 of 46 two-minute samples a contract that was refused later
  stood in line while the seat ran fewer runs than its cap. Over 14 days the fleet's slowest node, the one that was always online,
  took 587 contracts and refused 310 of them.
- **What busy means now** (`runner.noFreeSlot`, [ADR 0103](docs/architecture/decisions/0103-busy-means-no-free-slot-and-the-local-line-follows-the-engine.md)).
  The seat cannot take one more run: its requests in flight are at the cap and the engine takes no more, a load is in progress, or a
  load cannot start. The spread deal, the capacity wait and the retry's landing read it as route `auto` already did. One free slot is
  one waiting subtask's (`claimLocalSlot`). `agent_spread_local_slot: "always"` is unchanged.
- **What the line does now.** With `fleet_max_concurrent_jobs` unset the cap of 4 is where the line starts: the first run in it asks the
  engine at every poll and is admitted when the engine is a loaded vLLM that queues nothing, preempted nothing between two readings a
  second apart, gives every running request 10 tok/s or more, and stays under 85 % of its KV pool with every run and this one counted
  at their average (10 % of the pool or more each). Runs past the cap enter one at a time. A number the operator sets is the limit, as
  before.
- **What did not change.** A remote's busy reading; the workers a node publishes; the lane doors, which still give way to anything in
  flight on the seat they share a card with; a seat of another engine, or one that cannot be read, keeps the fixed line.
- **Check.** On the reference box through the MCP door, two processes. Two contracts held the seat, then eight were dealt by
  `route: "spread"` from the other one: the deal read `busy=false inflight=2`, six of the eight ran on the box's own seat (89 to 137 s
  each), one on each of two remotes (496 s and 234 s), ten of ten finished, none was refused, four ran at once at most with no
  preemption and at most 56 % of the KV pool. In a run before it, from a cold seat, the line grew: `run admitted past the cap of 4 with
  4 on the seat: the engine takes one more: 4 running at 27 tok/s each, none queued, KV pool 42% used`, five at once at 67 % of the
  pool, no preemption. Four answers of the two runs (three from remotes) failed the probe's own keyword check with the asked-for fields in them.
  A line longer than five was not seen.
