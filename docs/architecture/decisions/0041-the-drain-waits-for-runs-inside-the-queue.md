---
status: Accepted
date: "2026-09-14"
---

# 0041 — The drain waits for runs, inside the queue budget, and "busy" says what the cards are doing

Release: 0.117.0 (register D-93)

## Context

ADR 0039 made `gpu reserve` queue behind a held card and made a text holder that cleared the
seat stamp its lease `exclusive`, so the admission gate keeps models off the cards for the
window. Both landed. On 2026-09-14 a session then ran the sanctioned command,
`gpu reserve --wait 8h --drain --unload-seat`, twice, and both attempts failed with

```
gpu reserve: draining agent-pool: 1 in flight   (×61)
error: drain of agent-pool did not finish within 2m0s (last: 1 in flight)
```

The lease was free both times, so `--wait 8h` was over in an instant; the failure came from
the next phase. Reading the log and the seat together showed three independent defects, each
sufficient on its own:

1. **The drain's deadline was a fixed two minutes, outside the queue budget.** The "1 in
   flight" was one legitimate 27B step: llama-swap's log shows the completion returning 200
   after 3m27s, which is exactly a 4,096-token step at the seat's own measured 23.6 tok/s.
   No value of `--wait` could outlast it, because `--wait` bounded the lease and not the drain.
2. **`--unload-seat` stamped the lease exclusive at acquire, and the gate blocks every
   admission under an exclusive lease.** The drain therefore blocked the very run it was
   waiting for: a deadlock resolved only by the drain's two minutes or the run's wall.
3. **The drain read the engine's gauge, but the harness's unit of work is a multi-step run.**
   The gauge reads zero for the seconds between one step and the next. The third reserve
   attempt landed in that gap at 10:08:29, unloaded the seat the instant the first step
   returned, and the run's second step waited behind the fence until its 600 s wall
   (ledger row 10:15:00, `wall timeout after 600s`, 4,096 tokens generated). The warm-up path
   then loaded the seat straight past the fence for the next run (10:20:03), onto cards a
   render held.

Behind all three sat the gap the operator named: every surface said "busy" and nothing said
what the cards were doing. `gpu status` and `offload_status.gpu_lease` reported a holder pid
and an age; the drain printed a count; llama-swap said a seat was loaded. None of them said
whether work was in flight, whose it was, how far along, or whether the holder was using the
cards at all. A session that cannot tell "held and working" from "held and idle" reads every
"busy" as "refuse".

## Decision

1. **The drain is bounded by the queue budget.** `--drain-timeout` defaults to the rest of
   `--wait` (measured from when the reservation began queueing), never under two minutes; an
   explicit `--drain-timeout` still wins. A reservation queues behind in-flight work exactly
   as it queues behind a holder. The deadline error names what was in flight, the seat's own
   turn arithmetic (`seat-rates.json`), and how to wait longer; work in flight is never
   interrupted.
2. **A lease is stamped `draining` during the drain and `exclusive` only after it.** The new
   record flag cordons the seat: `modelaffinity.BlocksNewRun` refuses a NEW run (the launcher
   holds at the cordon for its admission budget, then defers with the holder named), while
   `blocksLoad` — what every request passes — is unchanged, so runs already in flight finish
   their steps. `Lease.Restamp` turns the flag into `exclusive` in place, under the epoch lock,
   without moving the epoch. `--exclusive` without `--drain` still stamps at acquire.
3. **Runs register.** Every agent loop launcher (`agent_run`, the contract runner used by
   local delegation legs and fleet jobs) writes one record under `<state root>/gpu/activity/`
   before admission — seat, kind, origin, goal excerpt, phase, step, tokens — updated on every
   step (`agent.RunObserver`) and every 15 s, removed at the end, swept by the lease's own
   staleness rule (dead or recycled pid, heartbeat past 120 s). The drain waits until the
   engine's gauge AND the registry are empty on two consecutive reads; a seat still loading
   counts as busy (register D-92). Registration happens before the warm-up, and the launcher
   holds at the cordon there, so the fence now covers the load path too.
4. **"Busy" says what the cards are doing.** `gpu status [--json]` and
   `offload_status.gpu_lease` carry a `verdict` — `working`, `held-working`, `held-idle`,
   `loaded-idle`, `busy-outside`, `stale-holder`, `free` — and an `activity` block: the seat's
   load state and in-flight count, the registered runs, a utilization and memory sample of
   every card with the processes on them, and the holder's command (the wrapper form stamps
   its argv into the record). The drain's progress line is built from the same reading and
   is printed on change (count, load state, a run's step), with a reminder every five minutes.

## Consequences

- A drain behind a real 27B step now succeeds in the time the step takes, and a drain behind
  a multi-step run succeeds when the run ends — never in the gap between its steps.
- New work placed on a draining box waits its admission budget and defers as `capacity`
  with the holder's reason; the delegator already routes around a held card (0.113.14), so
  in practice the wait is paid only by a local `agent_run` started during a drain.
- A held lease over idle cards is now named as such (`held-idle`, with the holder's command
  and heartbeat age). The verdict is a reading, not a reclaim: a stale record is reported as
  `stale-holder` and reclaimed by the next acquirer, exactly as before.
- The registry is advisory and best-effort: an unwritable state root logs once and the run
  proceeds unregistered (the drain then relies on the gauge, and says so). The record is
  replaced atomically where the platform allows and written in place after a bounded retry
  where a reader's open handle blocks the rename (Windows).
- The wrapper form heartbeats its lease for the drain's whole length (the reclaim rule needs a stale
  heartbeat AND an expired window; a drain that now runs for the queue budget can outlast `--for`).
  The cordon, the admission pre-flight and the warm-up share one admission deadline, and the cordon
  wait is reported as admission time; on both doors the wall starts after the cordon.
- `Restamp` is a read-modify-write of the claim under the epoch lock with an epoch check; the
  window between its read and its rename is microseconds, and the only writer that could land inside
  it is an operator `gpu release` (whose effect the holder's next `Renew` reports as a lost lease).
- Not changed: interactive single-shot text calls (the ~46 ms ones) neither register nor
  hold at the cordon — the seat's own gauge covers them; a `media` lease keeps its class
  rule; the fleet node's `/fleet/health` lease block is unchanged.

**Extended 2026-09-15 (0.125.0, register D-110):** the capacity defer this ADR describes is no longer the
first answer for `offload_review_diff` under a FOREIGN text hold — the handler asks `delegate.ForeignFence`
before building its local loop and routes the review to an eligible fleet seat (route `remote`, under
`remoteEligible`'s ctx-fit floor); the wait-then-`capacity` path remains for the holder's own inherited lease
and for the case where no remote qualifies, and its reason now carries the holder's declared window.

**Extended 2026-09-22 (media holds):** decision 2 now holds for `gpu reserve --class media --drain` too.
The lease record used to drop the draining stamp on a media lease (`Draining: opts.Draining && class ==
ClassText`), so a media drain fenced the runs in flight from acquire — every request of the run the drain was
waiting for waited on the drain: this ADR's deadlock, reopened for the media class, ended only by the run's own
budget, while the run's unfenced probes loaded the seat onto the held cards. The stamp is now recorded for
either class; `blocksLoad` never blocks under a draining hold that is not exclusive, `BlocksNewRun` cordons a
draining hold of either class, and the media class fences once `maintainSeat` clears the stamp. Pinned by
`TestADrainingMediaHoldAdmitsRunningWorkAndFencesAfterTheDrain` and
`TestDrainingIsRecordedForEitherClassAndTheCommandIsClipped`. The probes themselves pass the fence since the
same change (ADR 0026, extended 2026-09-22).

## Evidence

llama-swap log 2026-09-14 (two drain windows of 61 polls at 7 ms each, the 3m26.9s
completion between them, the unload at 43.8 s); seat wrapper log (`start requested
10:02:05`, `seat up 10:05:03`, `stop requested 10:08:29`, `start requested 10:20:03` under
the exclusive lease); ledger row `ts 1789398900` (`agent`, `agent-pool`, `tokens_out 4096`,
`latency_ms 774468`, `wall timeout after 600s`); `seat-rates.json` (`agent-pool` 23.58 tok/s,
cold loads 175–287 s). Tests: `TestDrainWaitsForARegisteredRunAcrossTheStepGap`,
`TestReserveDrainsUnderADrainingStampAndTurnsExclusiveAfter`,
`TestADrainingTextHoldRefusesNewRunsButAdmitsRunningWork`,
`TestRestampTurnsADrainingLeaseExclusiveWithoutMovingTheEpoch`, `TestAssessVocabulary`.
