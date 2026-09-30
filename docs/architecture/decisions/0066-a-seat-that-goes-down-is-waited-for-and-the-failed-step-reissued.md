---
status: Accepted
date: "2026-09-30"
---

# A seat that goes down is waited for, and the failed step is re-issued

## Context

On 2026-09-29 the reference workstation's three-card flagship engine died ten
times (the engine core waits 120 s for a hung pipeline step, then the API server
exits; median up-time between deaths, 6.7 minutes). ADR 0061 made the diagnosis
of a silent request right: an engine that works for others holds the run, an
engine that does no work is a stall. It did not make a death survivable, and it
said so ("Not solved here: a run lost to an engine that DIES mid-run still
ends"). Each death was filed as a separate per-run `stalled:` — four decode
stalls in one second, then every run that arrived while the seat could not
start burned its floor on it — although the agent loop holds its whole transcript
and needed nothing but the seat back. The delegator then treated each such defer
as terminal, so the contracts were lost rather than placed elsewhere. And while
the launcher refused to restart the seat, every request got llama-swap's
`500 src=llama-swap`, which the client waited out on the 90 s contention budget
and the run reported as `seat contended ... (peers hold its slots — raise
concurrencyLimit)`: the wrong words, for 23 minutes, about a seat that was down.

A second defect sat under the same walls. The measured prefill rate is the rate
of ONE request, the stall allowance is sized from it, and the rate itself is
learned from whatever run happened to finish: timed while other requests shared
the seat, a sample teaches the store a rate the seat never has alone
(`prefill_tok_s` swung 255-1321 tok/s on one seat in one day), and the printed
arithmetic ("2357 tok / 281 tok/s x 1.5 + 30 s") described a seat the request
did not have. The store's cold-load figure had the mirror problem: a run that
joined a load near its end recorded the few seconds it saw as the load (12.4 s
against a real 178-271 s), and several runs waiting on one load each appended it
(`[12.4, 234.6, 234.6, 203.5, 203.5]`).

## Decision

1. **A seat is DOWN on positive evidence, and there are two kinds.**
   - *Wedged*: the engine's work fingerprint stayed flat for the flat bound (ADR
     0061) while llama-swap still reads the seat loaded. It is the busy hold's
     engine-flat verdict, retyped; the typed error wraps the stall it replaces, so a
     reader that only knows the stall still sees the arithmetic.
   - *Died*: the seat left llama-swap's `/running` inside an established hold
     (two consecutive reads), or a model call failed like a dead seat (a stream cut,
     a 5xx, a refused connection) AND a fresh read shows llama-swap listing the seat
     starting or stopping, not listing it, or the engine's own local address refusing
     connections while llama-swap still lists it. The failure alone is never the
     evidence: a 5xx from a seat that reads ready and readable is an ordinary error.
     Absence on the FIRST look of a silent request stays the ADR 0055 rule (a removed
     seat, a renamed alias, a restarted llama-swap: PR #458 finding 1). A refusal of
     llama-swap's own address (the service is down) is not the engine refusing.
   - A *thrash* (the engine keeps stepping but produces no token) is neither: the engine
     is alive and overloaded, a re-issue would only feed it another request, and it stays
     the plain stall of ADR 0061.

2. **The run waits, in one bounded episode, and the failed step is re-issued.** The
   monitor cancels only the model call in flight (its step scope); the run's context
   stays alive. The loop then waits for the seat under the cold-load hold — phase
   `cold-load` — and re-issues the same step (same transcript, same budget, no step
   spent). The wait is one *episode*: it begins at the first verdict, outlives the calls
   that meet it, and is bounded by the cold-load ceiling counted from that first verdict
   (and by the run's own ceiling); it never restarts per attempt.
   - A wedged seat is not recovered by being listed ready: the counters must move again or
     a restart must be seen, or the re-issue would only feed the frozen engine. If
     llama-swap lists the seat ready but its engine cannot be read from here, and the run
     has seen the seat down since the verdict, llama-swap's word is enough.
   - A seat that llama-swap does not list is waited for by nobody — llama-swap starts a
     stopped seat on the next request — so the re-issue is the start *trigger*, and the
     cold-load hold covers the load. It is not a recovery. If the start fails (on
     2026-09-29 the launcher refused to start the flagship for 18 minutes and every request
     got an instant HTTP 500), the episode goes on: the run waits one poll and triggers
     again, until the bound, instead of turning each queued request into two quick 500s and
     a defer. A start that never succeeds ends the run typed at the bound, naming the
     attempts and what the seat last looked like.
   - A seat that was *seen serving* again earns exactly one re-issue. If the re-issued call
     fails again before its answer, the seat came back and died under the same request, and
     the run ends typed instead of waiting for it forever.
   - A recovery is counted, and the wait booked, when the re-issued call's first byte
     arrives — never for a re-issue that recovered nothing. `seat_down_wait_sec` covers the
     whole episode, including the time llama-swap held the re-issued call while it started
     the seat. A run recovers at most twice; the budget counts outages that landed.
   - A refused connection while llama-swap still lists the seat ready is not counted as
     "seen down" (the fallback to llama-swap's own word would otherwise re-issue onto a
     dead engine): the run waits, bounded by the same ceiling, for llama-swap to notice.

3. **What does not recover ends the run, typed.** The reason opens `seat down: `
   (`core.SeatDownReason`); the class stays `infrastructure` (a new class would read
   as unknown on a node without this decision); the wire carries `seat_recoveries` and
   `seat_down_wait_sec`. The delegator treats a `seat down:` defer as a retryable
   infrastructure defer, the third after the two admission-time ones (the coherence
   defer of D-118, the warm-up defer of C-76): the fault is
   a property of this seat, the contract is sound, the cure is another node. It is
   safe to re-place because a node-filed defer is an observed terminal. The retry's
   budget is credited the node's admission plus its wait on the dead seat (capped at
   one contract wall), because that time was not work. Two gates that guard a
   *verification* retry do not apply to it: the first-pass floor is not raised by the
   dead seat's own `min_turn_sec` (the retry goes elsewhere, and the retry seat's own
   floor still applies once it is chosen), and the retry is not refused because the
   alternative node is busy. D-46's case is a retry that joins a generating seat on the
   budget that was left; a seat-down defer produced nothing, carries credited budget, and
   busy is a place in line, never a refusal (INV-4) — the node's own queue is the line.
   A seat-down defer that has nowhere to go (route local, or no untried eligible node)
   says so in `retry_note`. The loop's recovery does not
   cover the structured re-pack (its request is not a step, and the finished answer is
   already in hand), so a seat lost there — the monitor's wedge verdict, or a transport
   failure that a fresh read confirms as a dead seat — ends the run typed the same way,
   with `(during the structured re-pack)` appended; the finished answer stays in
   `output`.

4. **The wording follows the status, and a seat that is not serving is not waited on as
   contention.** `seat contended:` is for a 429 only (llama-swap's concurrency limit). A
   5xx is `seat not serving:`. The run's seat check is a gate on the contract's shared
   contention budget: before the budget waits out any busy answer except a 429, it asks
   whether the seat is serving, and while it is not the wait is refused at once. The chat
   client returns the typed seat-down to the recovery wait; the structured re-pack, which
   draws on the same budget from another client, stops sleeping the same way and files its
   `seat down:` defer instead of after up to 90 s.

5. **The allowance knows the load, and only runs known to be solo teach the rate.**
   `StallPolicy.AllowanceLoad(phase, pending, load)` divides the prefill rate by the
   number of requests sharing the seat and stretches the re-pack bound by it (load 1
   is `Allowance`, exactly; the floor is still the definition of a silent seat). The
   load comes from this box's run registry when a prefill or re-pack begins and again
   at its first delta, and from the engine's own running + waiting gauges — read by the
   busy hold, and once at each call's first delta when the registry saw nobody else, the
   only way to see a peer this box does not know of (a cascade call, another process). The
   stall reason prints it.
   - The busy hold's flat bound keeps that stretched allowance only for an engine that
     cannot see a prefill in progress (llama-server on `/slots`; a vLLM exposition with no
     KV-usage gauge). Any other engine moves its fingerprint through a prefill, so a flat
     fingerprint is a hung engine whatever is queued behind it and it keeps ADR 0061's solo
     bound. The load that sizes the bound is the one the engine had when it last did work,
     never the latest reading: a hung engine's HTTP front end keeps accepting requests, its
     waiting count grows for as long as it is hung, and a bound that followed it receded
     faster than the silence lengthened.
   - A run moves `prefill_tok_s` only if it was *known* solo. A run nothing could answer for
     (no run registry, no engine gauges, or an engine read still pending when it ended) has
     an unknown load, which is not solo: the store refuses it like a shared one, and an
     unopenable registry is logged.
   - A run that waited out a seat going down moves neither rate: its re-issued call is the
     request that waits out llama-swap's load.
   - Cold loads: only a warm-up that CONFIRMED the load (a 200, or `/running` lists the seat
     ready) is recorded; a failed start is the cost of a failed start, not a measurement. One
     load seen by several runs is one entry — an observation ending within ten seconds of
     ANY entry of the window of five merges into it, keeping the longest measurement, because
     runs report when they end, not when the load did. A load older than everything in a
     full window is dropped rather than pushing a newer one out. A run that waited out a load
     it saw start in its admission pre-flight records the wait from the first sighting to
     ready, which is a lower bound of that load.

## Consequences

- A seat that dies or wedges costs its runs a wait (about one cold load, bounded)
  instead of their work; one that does not come back costs them a typed defer the
  delegator re-places, not a stall nobody retries.
- A wedge that never clears is now held for up to the cold-load ceiling (10 minutes
  by default) before it is filed, in case the seat restarts. The ceiling is a bound,
  not a promise: most deaths recorded on 2026-09-29 restarted the seat within a couple of
  minutes of the first silent step, but the 23:01 death did not — the launcher refused to
  start it for 18 minutes and the seat was up again after 23, longer than the ceiling. A
  run on that seat waits to the bound and then ends typed, and the delegator re-places it
  with the wait credited; the launcher's own crash cleanup is what shortens that outage.
- A run whose seat reads shared waits longer before its first engine read (the
  prefill allowance is sized for the load). The busy hold's flat bound follows the load
  only on an engine that cannot see a prefill, so a wedge on such a seat is called later:
  for a 13,000-token prefill at 280 tok/s with five requests sharing it, the first engine
  read comes at about 380 s instead of about 100 s and the flat bound is about 380 s
  instead of 120 s, and the recovery wait follows. That is deliberate — a seat whose
  prefill is invisible to its counters would otherwise be called wedged while a shared
  prefill is still legitimately running — and a death that breaks the stream is caught at
  once by the failed call, not by the flat bound. An engine that does see its prefill keeps
  the solo bound whatever is queued. The hold, not the allowance, carries correctness.
- `prefill_tok_s` no longer follows the traffic a run happened to meet; a box that
  is never idle, or whose seat cannot be read, learns no prefill rate and keeps the assumed
  one. A load that outlasts the admission budget is not recorded (the run's own cold-load
  hold sees it mid-run, and nothing carries that to the store): the cold-load figure stays
  what complete measurements made it.
- Nodes without this decision keep filing `stalled:` for the same outage until they are
  upgraded; for them the delegator's change is inert.
- The wire carries `seat_recoveries` and `seat_down_wait_sec`, and the delegation corpus
  keeps them; the ledger rows carry the outcome as `reason_code` `seat_down` (ADR 0064)
  and not yet the two numbers.
- Not solved here: why the engine hangs (py-spy and NCCL traces at the next hang;
  register A-123), the launcher that refuses to restart a seat while its own
  orphaned workers hold the port (its crash cleanup shipped separately, register C-72;
  deploying it is the operator's), a seat-down recovery in the MCP `agent_run` door
  (it builds no liveness monitor), and placement and admission (re-placement after a 503,
  dealing by capacity).

## Alternatives considered

- **Re-issue at once, without waiting.** Rejected for a wedge: the new request
  would land on the frozen engine. Kept as the start trigger for a seat that is gone,
  where the request is what starts it.
- **Spend the step's single re-issue on the first start attempt.** This was the first
  design, and the review found what it does when the start keeps failing: two quick 500s and
  a typed defer while the outage lasts 18 minutes, a reason that says the seat "came back",
  and a recovery counted for a re-issue that recovered nothing. A failing start is one
  bounded episode instead.
- **Wait passively for a seat nobody lists.** Rejected: llama-swap starts a stopped seat on
  the next request, and a run that is only waiting sends none.
- **A seat check in the structured re-pack's client.** Rejected for the gate on the shared
  contention budget: every client draws on the budget, so one refusal covers them all and no
  client grows its own copy of the rule.
- **Treat absence from `/running` as down on the first look.** Rejected: ADR 0055's
  review (PR #458) showed absence alone is also a removed seat, a renamed alias or a
  restarted llama-swap. Absence inside an established hold, or after a failed call,
  is different evidence.
- **Cancel the run and let the delegator resume it.** Rejected: the transcript lives
  in the node's loop; a re-placed contract starts over, so the in-run re-issue keeps
  everything and the delegator's re-placement is the fallback.
- **Shorten the engine's own step timeout so it dies sooner.** Out of scope, and
  measured unsafe below the 75 s self-recovered stall recorded on 2026-09-17.
- **A new defer class.** Rejected for the same reason as D-118: a class an older
  node does not know reads as unknown; the prefix is the contract, as it is for the
  coherence defer.
- **Scale every allowance by the load and drop the busy hold.** Rejected: the engine's
  own counters answer the question directly (ADR 0061); the load only keeps the
  printed arithmetic honest and the flat bound proportionate.

## Related

ADR 0061 (the busy hold this builds on), ADR 0055 (walls are ceilings, liveness is
progress), ADR 0032 (a peer-held seat is waited for), decision D-118 (the coherence
defer this follows), register rows C-72 (seat-down outcome), C-66 (liveness walls
charge queueing to prefill).
