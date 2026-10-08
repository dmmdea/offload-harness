---
status: Accepted
date: "2026-10-08"
---

# ADR 0079 — The ranking eta is not capped at the wall

## Context

[ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) decision 3 ranks quality-adequate seats by an
expected completion, `etaFor`: cold load + the node's own queue wait + the generation time of "the final FITTED to the whole
wall (0.128.1: not the wall minus cold; capped at the wall, so an eta never exceeds cold + queue + wall)". The clamp came with
0.128.1 (commit `d34a149a`, 2026-09-17), when a run's wall was a kill: nothing could generate past it, so an eta past it
described nothing.

[ADR 0055](0055-walls-are-ceilings-liveness-is-progress.md) decision 2 ended that: the contract's wall is "the expectation:
still reported as `wall_sec`, still what the delegator sizes and anchors from, no longer a kill", and a run is ended by a stall
or by a ceiling, `max(3 x estimate, 2 x wall, 1800 s)`. The clamp's premise was gone, and its comment in `eta.go` described a
stop that no longer exists.

What it left behind is a ranking that stops ranking. Fitting the final to the wall makes every seat that cannot finish
inside the wall generate "as much as the wall buys", so its generation time is the wall, or about 0.9 x the wall where the
fit keeps its 10 % safety fraction, whatever the seat's rate. Modelled with fixture rates (not live `seat_rate` readings), a
schema contract with an 8,192-token configured final on loaded seats at an explicit 300 s wall, the code before this ADR read:

| seat rate (tok/s) | 43 | 40 | 22.5 | 20 | 12 | 6.57 | 3.9 |
|---|---|---|---|---|---|---|---|
| eta before (s) | 270 | 270 | 270 | 270 | 270 | 300 | 300 |
| eta after (s) | 48 | 52 | 92 | 103 | 171 | 312 | 526 |

Seven seats, an 11x spread in speed, two distinct numbers. Placement then ordered them by queue estimates and the near-tie draw
(`etaDrawn`), not by speed.

The direction [ADR 0078](0078-a-placement-pin-needs-a-reason-without-one-it-is-a-hint.md) quotes (2026-10-07) is that placement
uses every node, sized by speed among the seats that are already quality-adequate. This ADR is the part of that which corrects a
definition and adds no behaviour.

## Decision

1. **The ranking eta is cold + the node's own wait + the time to produce a reference final at the seat's measured rate, and it
   stops at no wall.** `etaParts` returns the three terms and `etaFor` sums them. The reference final is
   `seatrate.FinalBudgetFloor` tokens (1,024), plus the same again for the structured re-pack when the contract carries an
   output schema, plus the seat's own tool steps and think block (the step budget and thinking policy it publishes, through
   `seatrate.Compute`). There is no `FitFinalBudget` call and no clamp. The contract's wall is not an input: the same seat and
   contract at any `timeout_sec`, or under `timeout_auto`, give the same three terms. The cold load is charged as before, only
   when the seat is KNOWN not to be loaded. This supersedes the clause of ADR 0050 decision 3 that fits the final to the wall
   and caps the eta at it; the rest of decision 3 (the node's own wait, the shape of the contract deciding the axis, the near-tie
   draw) stands. So does ADR 0050's 0057 revision of the unknown-rate clause: a seat that publishes no rate is ranked at
   `fleetTokSPrior`, the median rate the roster publishes (keeping its own wait and cold load), and the window-only order
   remains only when no seat of the roster publishes a rate.

2. **The same final on every seat.** The reference ignores a seat's configured final: a seat configured with a 512-token final
   and one configured with 8,192 price the same final (and the same re-pack) at the same rate. That is the only thing held
   equal. `seatrate.Compute` still charges the think block at the seat's OWN step budget (the step budget divided by its rate,
   when its thinking policy is auto or on), as `etaParts` says, so a seat with a larger step budget or a thinking policy reads
   slower for it, and that is a real cost of the seat. The point of the number is to say which seat finishes the same final
   sooner, and a seat's own final budget is the seat's business (what the node does when it runs the job is unchanged: it fits
   the final to the wall then, `internal/pipeline/agenttask.go`).

3. **Feasibility keeps the wall.** Decision 2 of ADR 0050 is untouched: `feasibleFinal` still refuses a seat that cannot produce
   one tool step and a minimal 64-token answer inside the contract's effective wall, naming the arithmetic. So is everything that
   reads the node's wait and not the eta: the backlog gate, `startsWithinPatience`, the queue budget.

4. **The eta stays a ranking number.** It must not become a patience input, a gate input or a deadline: an estimate of how long
   the same work takes, uncapped, is the wrong thing to refuse or to wait on. Patience and the gates read the node's wait.

5. **The INV-5 rider is narrowed, not widened.** The rider ADR 0050 cites allows a wall-time term only as (i) a feasibility
   floor and (ii) an ordering key among seats already past the adequacy gate. Clause (i) is unchanged. Clause (ii) stays an
   ordering key, but the contract's wall leaves it: the key is now a duration at the seat's rate and carries no term from the
   contract's wall at all.

6. **`placement_reason` prints the same parts.** `eta N s (cold C + G gen)` is built from `etaParts` (`chosenVerdictDetail`), so
   the figures printed are the figures the ranking compared. It used to recompute the generation as eta - cold - wait, and printed
   a bare total for a contract with no wall.

## Consequences

- Every route that ranks by eta re-ranks at once: `auto`, `remote`, the capacity wait, the retry, `spread`. Ordering among small
  seats now follows speed, which is the change. There is no flag, because the old definition was the defect; rollback is a revert.
- **Placement among near-tied seats changes.** The near-tie draw nudges an eta by at most 10 % either way (`etaDrawn`, `eta.go`),
  so two seats whose etas are more than about 22 % apart (1.1 / 0.9) can no longer swap in it. Seats that used to read the wall
  tied, and the draw spread work across them; with their real speeds apart they no longer tie, and the fastest takes the work
  unless it is busier or not idle. Seats within the band still spread by the draw.
- An idle slow seat still beats a busy fast one: `betterRemote` ranks `provablyStartsNow` before the eta, and this ADR does not
  touch that.
- Seats that cannot finish inside the wall are still eligible when they pass feasibility: the wall decides whether a seat may
  take a contract, and the rate decides which seat does. A contract on a 60 s wall can now be placed on a seat whose eta reads
  past 60 s, exactly as before; what changed is that its number says so.
- The wall-fitted figure is no longer shown in any reason. If operators want it, it belongs in `chosenVerdictDetail` later; it
  must not be fed back into the ranking.
- The modelled rates above are fixtures, not measurements. Before a figure from this table is quoted as a measurement, replace
  it with the live `seat_rate` of the seats concerned.
- `scoreFitRanked` folds the eta into an int (`-int(eta x 10) x 2^24 - window`): 1,215 s gives about 2.0e11, fine for a 64-bit
  int, and that int is now compared between adequate seats only. It used to share its range with `fitInadequate`, the marker
  (`math.MinInt32`) that ranked a seat which cannot hold the contract below every seat that can, and the fold passes that value at an
  eta of about 12.8 s on EVERY build, not only a 32-bit one: a probe scored a rated, adequate seat at -8,724,160,512 under this
  ADR's eta and at -4,362,084,352 under the capped one, so such a seat ranked BELOW a seat that could not hold the contract. The
  collision predates this ADR (the capped eta crossed the value too); the uncapped eta reaches it sooner for a slow seat. It was
  latent because `placeSpreadWith` filters through `remoteEligible` before `fitPickWith` scores, and it is fixed rather than left to
  that filter: adequacy is a key of its own (`fitKey`, compared first, ahead of the lease, saturation and free-card keys of
  `fitPickWith`), and an inadequate seat is ranked last and never excluded. On a 32-bit `int` the fold also overflows above 12.8 s,
  which is a separate and older limit (no fleet target seen is 32-bit).

## Alternatives considered

- **Keep the cap and raise it (k x the wall).** Rejected: any cap flattens the seats past it, only further out.
- **Keep the wall fit and drop only the clamp.** Rejected: the fit alone still makes every seat that cannot finish inside the
  wall generate "what the wall buys", so a 22.5 and a 40 tok/s seat both read about 0.9 x the wall.
- **Price each seat's configured final.** Rejected: two seats of the same speed then differ by their budgets, and the ranking
  compares different work. The reference is the package's own minimum adequate final for a grounded extraction.
- **Fold the fix into the next ADR of the series as a clause.** Rejected: this changes a recorded decision, so it is its own ADR
  (docs/STYLE.md), and ADR 0050 carries only a link to it.

## Related code

- `internal/delegate/eta.go` — `etaParts`, `etaFor`, `seatPolicyFor`, `seatWallFor`, `feasibleFinal`, `queueWaitFor`.
- `internal/delegate/placement_reason.go` — `chosenVerdictDetail`.
- `internal/delegate/eta_reference_test.go`, `eta_repack_fit_test.go`, `feasibility_test.go`, `eta_ranking_test.go` — the tests.

## Related docs

- [ADR 0050](0050-placement-ranks-adequate-seats-by-expected-completion.md) — decision 3, superseded in part by this ADR.
- [ADR 0055](0055-walls-are-ceilings-liveness-is-progress.md) — decision 2: the wall is an expectation.
- [ADR 0073](0073-the-capacity-wait-runs-to-the-calls-deadline.md) — the capacity wait and the per-node reserve it left open.
- [fleet-node.md](../../systems/fleet-node.md) — "Expected-completion ranking and the joint deal".
