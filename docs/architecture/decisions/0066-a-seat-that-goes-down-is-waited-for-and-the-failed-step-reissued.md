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

2. **The run waits and the failed step is re-issued, once.** The monitor cancels
   only the model call in flight (its step scope); the run's context stays alive. The
   loop then waits for the seat under the cold-load hold — phase `cold-load`, bounded
   by the cold-load ceiling and the run's own ceiling — and re-issues the same step
   (same transcript, same budget, no step spent). A step is re-issued once; a run
   recovers at most twice. A wedged seat is not recovered by being listed ready: the
   counters must move again or a restart must be seen, or the re-issue would only
   feed the frozen engine. A seat that llama-swap does not list is waited for by
   nobody — llama-swap starts a stopped seat on the next request — so the re-issue is
   the trigger and the cold-load hold covers the load. If llama-swap lists the seat
   ready but its engine cannot be read from here, and the run has seen the seat down
   since the verdict, llama-swap's word is enough.

3. **What does not recover ends the run, typed.** The reason opens `seat down: `
   (`core.SeatDownReason`); the class stays `infrastructure` (a new class would read
   as unknown on a pre-0.144 node); the wire carries `seat_recoveries` and
   `seat_down_wait_sec`. The delegator treats a `seat down:` defer as the second
   retryable infrastructure defer, after the coherence defer of D-118: the fault is
   a property of this seat, the contract is sound, the cure is another node. It is
   safe to re-place because a node-filed defer is an observed terminal. The retry's
   budget is credited the node's admission plus its wait on the dead seat (capped at
   one contract wall), because that time was not work. The loop's recovery does not
   cover the structured re-pack (its request is not a step, and the finished answer is
   already in hand), so a seat lost there — the monitor's wedge verdict, or a transport
   failure that a fresh read confirms as a dead seat — ends the run typed the same way,
   with `(during the structured re-pack)` appended; the finished answer stays in
   `output`.

4. **The wording follows the status.** `seat contended:` is for a 429 only (llama-swap's
   concurrency limit). A 5xx is `seat not serving:`. The chat client asks the run's
   seat check before it sleeps the contention budget on a llama-swap 5xx; while the
   seat is not serving it returns at once, typed, to the recovery wait.

5. **The allowance knows the load, and only solo runs teach the rate.**
   `StallPolicy.AllowanceLoad(phase, pending, load)` divides the prefill rate by the
   number of requests sharing the seat and stretches the re-pack bound by it (load 1
   is `Allowance`, exactly; the floor is still the definition of a silent seat). The
   load comes from this box's run registry when a prefill or re-pack begins and again
   at its first delta, and from the busy hold's engine gauges (running + waiting); the
   stall reason prints it. A run that ever saw the seat shared does not move
   `prefill_tok_s`. A cold-load observation that ends within ten seconds of the
   newest one is the same load seen by another run: the store keeps the longest
   measurement instead of appending; and a run that waited out a load it saw start in
   its admission pre-flight records the wait from the first sighting to ready.

## Consequences

- A seat that dies or wedges costs its runs a wait (about one cold load, bounded)
  instead of their work; one that does not come back costs them a typed defer the
  delegator re-places, not a stall nobody retries.
- A wedge that never clears is now held for up to the cold-load ceiling (10 minutes
  by default) before it is filed, in case the seat restarts; every death recorded on
  2026-09-29 restarted the seat within about 120 s of the first silent step.
- A run whose seat reads shared waits longer before its first engine read (the
  prefill allowance is sized for the load), and the busy hold's flat bound follows the
  engine's own load the same way, so a wedge on a shared seat is called later. For a
  13,000-token prefill at 280 tok/s with five requests sharing the seat, the first engine
  read comes at about 380 s instead of about 100 s and the flat bound is about 380 s
  instead of 120 s, and the recovery wait follows. Scaling the flat bound is deliberate —
  a seat whose prefill is invisible to its counters (llama-server on `/slots`) would
  otherwise be called wedged while a shared prefill is still legitimately running — and
  a death that breaks the stream is caught at once by the failed call, not by the flat
  bound. The hold, not the allowance, carries correctness.
- `prefill_tok_s` no longer follows the traffic a run happened to meet; a box that
  is never idle learns no prefill rate and keeps the assumed one. A joined cold load
  is recorded as the part of it the run saw (a lower bound), never as a second load.
- Nodes on 0.143 or older keep filing `stalled:` for the same outage until they are
  upgraded; for them the delegator's change is inert.
- Not solved here: why the engine hangs (py-spy and NCCL traces at the next hang;
  register A-123), the launcher that refuses to restart a seat while its own
  orphaned workers hold the port (a repo change to the seat launcher scripts, then a
  deployment the operator approves), and placement and admission (re-placement after
  a 503, dealing by capacity).

## Alternatives considered

- **Re-issue at once, without waiting.** Rejected for a wedge: the new request
  would land on the frozen engine. Kept for a seat that is gone, where the request
  is what starts it.
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
