---
status: Proposed
date: "2026-10-13"
---

# An overdue lease's holder is asked to yield; it is never taken from

## Context

A lease that outlives its declared window while its holder is alive and heartbeating is surfaced as `held-overdue` and
nothing else happens to it (ADR 0070): a card is freed only by its holder, by an explicit command, or by the reclaim rule of
a holder that is provably gone (ADR 0018). That is the right default for a job that is working, and it left one gap for a
job that is not: somebody waits for the card, the holder is overdue (its term was not renewed, or it reached the maximum
total), and the only recourse is a human running `gpu release --epoch N`. For a wrapper that command is a hazard of its own.
Once the claim is gone the wrapper's next tick fails its renewal, prints `LEASE LOST` and stops its own job within about 15
seconds, while the card is already free and a second job can be loading onto it. For a detached job it removes the claim and
leaves the job running on the card. Neither is a thing the harness should invite an operator to do by default.

Jobs already declare, at the start, what they would like to happen when they are asked to stop: `--on-yield` (a command that
makes the job stop at a clean boundary) and `--yield-grace` (how long that takes). The lease records both, and until now
nothing read them.

The operator's standing orders bound the answer: nothing in the harness kills a job on its own; no timer or daemon acts on a
lease (plan invariant I5: every step is the action of a process that wants the card, or of an operator command); a claim whose
holder is alive is never freed by the harness (I3); fail loud over autonomous recovery; reader-first rollout (I7).

## Decision

**When a lease is overdue and somebody waits for its cards, the lease's own holder is asked to yield, and the holder runs the
job's own recorded `on_yield`. Nothing is killed, released or taken.** The card frees when the holder ends or releases in the
ordinary way: a wrapper when its command ends, a detached job when its driver stops the job and releases.

- **The policy is a flat config key, `gpu_on_overdue`.** Unset behaves as the harness did before the key existed: nobody is
  asked. `request` asks the holder. `hold` says in words that nobody is. Matching trims and lower-cases. Any other value
  (`yield`, `kill`, `takeover`, a typo) is refused by name at config load; because a one-shot verb goes on after a load error
  and the load installs the policy on every exit, an unrecognised value installs as `hold`, the direction that asks nobody,
  and the holder's first tick says why no request will be made. There is no automatic mode. **This deviates from the plan,
  which named `request` as the default:** the operator decided that asking is opt-in, so the default is the current behaviour.
- **The holder's own tick asks, not a blocked waiter.** The text and seat-admission waiter never goes through `Acquire`, the
  refusal text and every status surface are read-only by contract, and only a live ticking holder can act on a request anyway.
  Waiter presence is the demand signal: a live waiter (not the holder's own process, not one waiting only on host RAM) that
  wants a card the lease holds. The preconditions are checked cheapest first, from the lease's record alone: the policy is
  `request`; the record is labelled expired or past its declared end (a lease the same tick renewed is not asked); the holder
  was not asked yet; the record says the holder runs the request; and then the waiters are read, because reading them prunes
  the dead ones.
- **The same waiter, by pid and start identity, must be seen on two consecutive ticks.** The ask is once per epoch and cannot
  be taken back, so a short `gpu reserve --wait` probe, or a seat admission that leaves after seconds, must not stop a long job
  through its `on_yield` and be gone. The persistence is an in-memory counter on the holder's own 15 second cadence; it adds
  no time constant.
- **The request is a sidecar, `yield.<epoch>`**, beside the lease record, written under the epoch lock after the same
  writability probe and liveness check the orphan marker uses, with tmp and rename: the ask (when, why, who waited, the
  job's declared grace) and a second write of what the holder did about it. Both writes take the lock and check the lease is
  still live, so a late update that races the release cannot bring the file back; both sweeps that end a lease remove it. It is
  additive: a reader that predates it ignores the file, and the preserved old-reader fixture pins that.
- **The holder runs its own in-memory `--on-yield`, never the record's.** The record is shared and can be rewritten; the
  command the holder runs is the one it was started with, once, in its own working directory (which is the directory the
  record names, by construction), through `sh -c` or `cmd.exe /d /s /c`, hidden, with its output discarded (a wrapper's stdout is
  the job's), in a goroutine so the heartbeat goes on, and never waited out or cut short. If the ask cannot be recorded
  nothing is run. The wrapper's command is told `GPU_LEASE_YIELD_FILE`, the path of the sidecar, which exists only once the
  holder is asked, so a job can poll it between its items; a detached job is not the holder's child, so `gpu reserve --detach`
  prints the path and `gpu status --json` carries it.
- **A record gains an additive `yields` stamp** (`wrapper` or `detached`), set at acquire by a holder that runs the request.
  Every 0.18x build already stamps a wrapper version, and none of the older ones ticks this, so the version cannot say whether
  a holder would answer. A record without the stamp reads, on every surface, as "its holder build predates the yield request:
  ask whoever owns it", and is never asked.
- **A detached holder never returns on a yield.** Its job is a separate process; releasing would free the card under a live
  job, the 2026-09-07 incident (ADR 0070).
- **No waiter-facing sentence names `gpu release --epoch N` for a lease whose wrapper is alive.** For a wrapper it says: nothing
  is killed; ask whoever owns it, or stop its job by hand (the wrapper's pid); the claim frees when the wrapper ends. Only a
  detached holder's sentence names the release command, with what it does: it frees the claim at once, and the job keeps running
  on the card until it ends. An ask is described as unanswered once the lease still holds its cards past the job's declared
  grace, or past a display-only ten minutes when the job declared none; nothing keys off that figure, which is chosen and not
  measured.

**This amends plan invariant I5 and the plan's description of the takeover.** The plan puts running `on_yield` in the process
of a takeover command and has a blocked waiter write the request. Here the holder's own tick runs the job's own pre-declared
`on_yield`: the job opted in by recording it, and nothing outside the holder acts. The alternative that stays plan-literal is
file only (the holder writes the sidecar and the job polls it); it is a one-line change in the ticker, and the choice between
the two is the operator's to confirm.

## Consequences

- **A progressing job is not asked.** Under ADR 0070 a renewal moves the declared end, so a job whose owner vouches for it is
  not overdue. Only a lease labelled expired (its owner is gone, or nothing shows it working), or one that reached the maximum
  total (`gpu_max_total_min`, 48 hours by default), is asked. In practice that is attended windows with no progress contract and
  capped jobs.
- **A hung job that ignores the request keeps its card.** The lease stays held-overdue with its heartbeat and fence intact, its
  waiters keep their places in line, and the surfaces move from "asked" to "unanswered" wording. Nothing frees it but its holder
  ending or a human acting. Closing that needs a takeover that stops a tree (the stop primitives exist and have no caller) and
  the hardening of `gpu release --epoch N` over a live tree (plan P12: claim-until-dead routing, `--abandon-tree`, quarantine),
  both later changes. This decision adds no path that removes a claim over a live tree, so it does not need P12 first, and it
  does not close registers C-32 and GPU-OPORDER.
- **The request is node-local.** The fleet node's lease standing wire does not carry it, so a remote delegator cannot see that
  a holder was asked.
- **A seat waiter registered for the whole node** (the card table was unreadable when it queued) conflicts with every lease and
  can ask an expired lease on a card it does not need. That over-ask is conservative, and it needs the same waiter to stay two
  ticks.
- **A holder that exits before its `on_yield` returns** (the wrapper's command ended meanwhile) leaves the sidecar's action at
  "running"; the sidecar goes with the lease.
- **Rollout.** A host is covered once every holder on it is this build; a 20 hour job started before the upgrade carries no
  stamp and is told so. Restart the long-lived processes (MCP servers, fleet daemon) so their surfaces say the same thing.
- **The term check still renews or labels and never frees.** The ask reads the record after the holder's term check ran and
  ignores its result; a tick whose term check errored may ask a lease that the next tick would have renewed, which is why the
  waiter must also have stayed for two ticks.

## Alternatives considered

- **A blocked waiter writes the request** (the plan's wording). Rejected: it needs a write inside `Acquire`'s refusal path, the
  refusal text and the status surfaces are read-only by contract, the seat waiter never enters `Acquire`, and only a live holder
  can act on the request.
- **File only, no `on_yield`.** The plan-literal reading. It leaves every job to poll a path, including those that recorded a
  command precisely so they would not have to. Kept as the one-line alternative above.
- **A `gpu yield --epoch N` or `gpu takeover` verb.** A takeover promises a kill this change does not do, and a yield verb is a
  new surface with no caller; neither is built.
- **After a grace, drop the claim.** A forced release over a live tree is the incident the lease exists to prevent, and it makes
  P12 mandatory first. Rejected.
- **`request` as the default.** The plan's default. Rejected by the operator for this build: the default stays the current
  behaviour and asking is opt-in.

## Related code

- [`internal/gpulease/yield.go`](../../../internal/gpulease/yield.go): the policy, the asker, the sidecar, the holder's ticker,
  the read-only view that words every surface.
- [`gpu_yield.go`](../../../gpu_yield.go): the shell runner and the detach report; `gpu_cmd.go` wires the two holders.
- [`internal/gpulease/explain.go`](../../../internal/gpulease/explain.go),
  [`internal/gpuactivity/holders.go`](../../../internal/gpuactivity/holders.go): the sentences a waiter and a status call read.
- [`internal/config/config.go`](../../../internal/config/config.go): `gpu_on_overdue`.

## Related docs

- [GPU lease](../../systems/gpu-lease.md): "An overdue lease's holder is asked to yield".
- [ADR 0070](0070-leases-have-terms-owners-renew-orphans-are-surfaced.md): terms, owners and the overdue verdict.
- [ADR 0084](0084-a-dead-wrapper-with-a-live-process-tree-keeps-its-claim.md): a dead wrapper's live tree keeps its claim.
- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md): the fenced lease.
