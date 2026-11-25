---
status: Accepted
date: "2026-11-25"
---

# 0103 — Busy means no free slot, and the local seat's line follows the engine

## Context

Two readings decided whether this box's own agent seat took a contract, and both were fixed figures.

**Busy.** `route=spread` read the local seat as busy at ONE request in flight (operator decision 2026-09-06, 0.113.20). A deal
made while the seat held anything gave its slots to the remotes, and a subtask the deal had kept off the seat could not have it
back in the capacity wait until the seat held nothing at all (`localStillBusy`). The decision was made when nothing bounded the
runs of a box, and K sessions stacked their subtasks on one seat. The run-cap line (register C-42, C-60) has bounded them since,
for every process of the box, and the seat it bounds serves many sequences at once.

What it did, read from the ledger of the reference 3-card Windows box: of 91 contracts in one night 26 were refused; 17 of those
waited in line 16 to 24 minutes and were placed nowhere. Rebuilt in two-minute samples, 23 of 46 had a contract that was refused
later standing in line while the seat ran fewer runs than its cap. Over 14 days the slowest node of the fleet, the one that was
always online, took 587 contracts and refused 310 of them, with a median of 466 s for the ones it finished.

**The cap.** `fleet_max_concurrent_jobs` is 4 when nothing sets it. On a seat with 32 sequences and a 115k-token KV pool four
small contracts use a third of the pool and leave each run 25 tok/s; four large ones fill it. Steady decode there: 28 tok/s in
all with one request, 99 with four (25 each, 30 % of the pool), 125 with six (21 each, 45 %), 137 with fourteen (10 each, 96 %).

## Decision

1. **Busy is a seat that cannot take one more run now** (`runner.noFreeSlot`), for the agent seat and for every place the delegator
   asks: the requests in flight are at the cap and the engine takes no more, a load is in progress, or a load cannot start (another
   vLLM seat holds the cards, or the host is short of memory for it). `route=spread` (the deal and the capacity wait) and the retry's
   landing read it as `route=auto` already did. `agent_spread_local_slot: "always"` is unchanged. The lane doors (`LocalSeatBusy`,
   ADR 0078) keep "anything in flight": they ask about another seat, whose line the run registry cannot see.
2. **A cap nobody set is where the line starts.** With `fleet_max_concurrent_jobs` unset, a run that finds the line at 4 and is first
   in it asks the engine at every poll (`seatload.ReadRoom`), and is admitted when the engine is a loaded vLLM that queues nothing,
   preempted nothing between two readings a second apart, gives every running request 10 tok/s or more, and stays under 85 % of its
   KV pool with every run on the seat and this one counted at their present average, at least 10 % of the pool each. A delegator
   asks the same question for a run that is not registered yet. A number the operator sets is the limit, as before, and the workers
   a node publishes to the fleet are the key's either way.
3. **Runs past the cap enter one at a time.** Only the first in line is asked about, and only when every run that holds a slot has
   produced something: a run just admitted has not shown what it costs.
4. **One free slot is one waiting subtask's** (`claimLocalSlot`): the subtasks of a call that read the same free slot in one tick no
   longer all go and stand in the seat's own line, which a run cannot leave.

## Alternatives considered

- **Raise the default cap.** Rejected: a number is right for one prompt size. Eight 40k-token contracts preempt each other on the
  pool that serves eight small ones.
- **Keep "any request in flight" and only fix the wait.** Rejected: the deal would still send a call's subtasks to the remotes
  whenever another session had one request on a seat with thirty-one sequences free.
- **Read the number of sequences from the serving config and use that as the cap.** Rejected: 32 is what the engine accepts, not what
  its KV pool holds.

## Consequences

- One seat can now hold more than one session's share of a deal, up to its line. What the 2026-09-06 decision protected, the wall of
  a subtask on a stacked seat, is protected by the line and by the 10 tok/s rule.
- A deal on a full line costs about a second more (two readings of the engine), once per two seconds per run.
- The thresholds (10 tok/s, 85 %, 10 % per run) were chosen from one box's curve. A seat that never has a request running has no rate
  and is not asked past its cap.
- Supersedes the busy half of the 0.113.20 local-slot rule and the spread half of "busy means a job is in flight" in ADR 0050 for the
  LOCAL seat; a remote's busy reading is unchanged.
