---
status: Accepted
date: "2026-09-17"
---

# 0050 — Placement ranks quality-adequate seats by expected completion; the deal respects every node's headroom; busy means a job is in flight

## Context

The 2026-09-17 scheduling diagnosis (`plans/2026-09-17-harness-scheduling-diagnosis.md`, read verbatim for this
PR) measured three cards idling at 0 % while sessions queued 18 deep on one seat, and traced it to five
independent defects, all in `internal/delegate`:

- **route=auto's busy reading was the GPU lease alone** (S-01): `attempt()`'s default case set `busy :=
  leaseInfo.Held` and nothing else, so a local seat already carrying more in-flight requests than the fleet's
  own concurrency cap still won `Place` unconditionally whenever no lease happened to be held — the
  overwhelming majority of runs.
- **Nothing ranked by expected completion** (S-02): `fit.go`'s mechanical branch returned `-window` — the
  SMALLEST adequate seat wins, with no rate term at all — and `betterRemote`'s four keys (saturated,
  provably-free, queue depth, GPU utilization) carried none either. The fleet already published `seat_rate`;
  placement read it nowhere. Measured on 642 same-goal contracts: the fastest seat (Aorus, 27.0 s median) was
  also the MOST accepting (90.3 %) — ranking among quality-adequate seats by speed cost no quality on this
  fleet.
- **`remoteEligible` gated on context fit only, never on whether one turn fits the wall** (S-03/S-05): a seat
  publishing `min_turn_sec: 1594` was fully eligible for a 300 s contract.
- **Every subtask placed itself independently** (S-11/S-13): `runConcurrency` siblings each called `fetchViews`
  and `Place` on their own, within milliseconds of each other, and could read the same free slot on the same
  node before any of them had dispatched — `saturated()`'s own doc names this exact staleness as why it demotes
  rather than excludes.
- **A declared-but-idle lease hard-excluded a remote** (S-15): `LeaseBusy` — the node's own verdict that its
  reservation window is long enough to route around — was OR'd into the same hard exclusion as an exclusive
  text lease, so a 6-hour reservation over quiet cards (`held-idle`, already computed server-side and read by
  nothing) routed work away from a node that could run it.

The operator's own framing of the underlying complaint (verbatim, session 5003a02f): *"delegation is going to
weaker resources first (slower GPUs take more time to respond regardless of VRAM amount), instead of delegating
to the most powerful resource available first (so we keep better interactivity)"* — read together with INV-5
("quality outranks speed; no acceptance criterion, pass rule, ranking or seat decision may contain a wall-time
term") required an explicit, operator-signed exception before a wall-time term could enter placement at all.
That exception — the **INV-5 rider** — was recorded 2026-09-17 in the harness master plan's register (Part A):
a seat decision may carry a wall-time term only as **(i)** a feasibility floor computed from the contract's
FITTED final at the seat's published rate — never the seat's max-final `min_turn_sec` — applied as a demotion
by default and a refusal only below a minimum viable final, naming the arithmetic; and **(ii)** an ordering key
among seats that have already passed the capability/adequacy gate, with power-of-two-choices among near-ties so
independent dispatchers do not herd. Acceptance criteria and pass rules stay wall-time-free.

## Decision

`internal/delegate` gains an expected-completion ranking layer (`eta.go`) that sits strictly AFTER the existing
capability/adequacy gate (`remoteEligible`, `adequate`) and strictly BEFORE the queue-depth/GPU-utilization
tie-breakers `betterRemote` already had:

1. **`route=auto`'s busy reading widens to the local seat's own load** (W-01): `busy := leaseInfo.Held ||
   local.inflight >= cfg.FleetConcurrencyLimit() || local.loading`, where `local` is `probeLocalBusy` read ONCE
   per Run and cached on the runner — the same one-probe invariant `route=spread` already held via
   `spreadLocalBusy`.
2. **A feasibility floor at the minimum viable final** (W-05, amended 0.128.1): `feasibleFinal` builds a
   `seatrate.SeatPolicy` from the node's published `seat_rate`/`seat_budget` exactly as the delegator's
   poll-bound sizing (`autoPollBound`) already does, sizes the contract's EFFECTIVE wall (explicit
   `timeout_sec`, or `seatrate.AutoWallFor` for a `timeout_auto` contract), and asks one question of it: can
   the seat produce one tool step plus a minimal 64-token final at its measured rate inside that wall — no
   think block, no structured re-pack, and no cold-load charge, because admission pays the cold load outside
   the wall (D-64). A seat that cannot is excluded, naming the arithmetic ("one step and a 64-token answer
   need 42 s at 5.4 tok/s, the wall is 20 s"); an unknown rate is no opinion. As first shipped (0.128.0) this
   item fitted the configured final against `seatrate.FinalBudgetFloor` over the wall MINUS a tri-state
   cold-load charge; the deploy smoke refused a cold Aorus a 60 s contract it completes in ~25 s, so 0.128.1
   moved the fitted final and the cold load where the rider puts them — into the eta (item 3).
3. **Expected-completion ranking among quality-adequate seats** (W-11): `etaFor` estimates cold + the node's own
   queue wait (`queueWaitFor`, from `jobs_running`/`jobs_queued`/`max_concurrent_jobs`/`recent_agent_wall_sec`,
   or the node's own `queue_wait_estimate_sec` when it publishes one) + the generation time for the final
   FITTED to the whole wall (0.128.1: not the wall minus cold; capped at the wall, so an eta never exceeds cold +
   queue + wall). `betterRemote` and `scoreFit` (route=spread's own ranking axis) both fold this in per the contract's
   inferred shape: reasoning-shaped work still ranks window first, eta only as its tie-break; mechanical work
   now ranks eta first (replacing "smallest adequate seat wins"), window as its tie-break. Two candidates within
   20 % of each other are a near-tie, resolved by a deterministic FNV-1a draw seeded from the job id
   (power-of-two-choices) so K independent dispatchers spread across near-tied seats. An unknown rate on either
   side keeps today's window-only ordering.
4. **One joint deal for route=auto/remote, respecting per-node headroom** (W-06): `RunWith` now computes the
   WHOLE Run's `auto`/`remote` placement in one pass over one fleet snapshot (`dealAutoRemote`), the same
   invariant `route=spread`'s `dealSpread` already held. Per-node headroom (`max_concurrent_jobs − jobs_running
   − subtasks already committed by this deal`) is checked before ranking; a node at 0 headroom gets nothing —
   no floor — and the next-best candidate is tried. Exhausted headroom with at least one otherwise-eligible node
   routes to the existing capacity wait; no eligible node at all keeps the pre-existing "no eligible remote"
   outcome.
5. **A quiet lease demotes instead of excluding** (W-14): only an exclusive or draining hold, or a busy lease
   that is not a plain text reservation, still hard-excludes; a plain non-exclusive non-draining text lease the
   node calls busy stays eligible and ranks last (`leaseBusyDemoted`, a new first key in `betterRemote`).
6. **`placement_reason` narrates every reachable remote** (item 8, register D-105): one word per node from the
   vocabulary `chosen | queue | cap | slow | lease | cold | probe | unfit(ctx) | noschema`, appended to the
   existing `route=auto`/`route=remote` prefix (kept byte-identical — `fleet_smoke_cmd.go` parses it).
7. **Long-poll and a Retry-After courtesy retry** (item 7, register D-106): the delegator's poll sends
   `?wait=12`; a dispatch 503 carrying its own `Retry-After` is honored with a bounded wait-then-retry to the
   SAME node, invisible to the re-placement loop.
8. **`offload_status` publishes the same in-flight signal** (item 9, W-31): `in_flight` (a job-registry count,
   never GPU utilization or a lease alone) and a one-word `verdict` on every fleet node row and the local seat
   entry. **Scoped to `offload_status` in this PR** — the operator's original W-31 ask names `gpu status` and
   `fleet-ui`/`top` too, which keep their existing `gpuactivity`-based vocabulary unchanged here; adopting the
   same words there is a follow-up.
   - `jobs_admitting` is read for TWO different purposes that intentionally disagree: `in_flight`/`verdict`
     above SUBTRACT it (an admitting job holds no card yet, so it must not read as "busy" to a human), and
     `queueWaitFor` prefers the node's own `queue_wait_estimate_sec` — computed FROM `jobs_admitting` — when
     published. W-06's headroom key does NOT subtract it: an admitting job has already claimed one of
     `max_concurrent_jobs`' worker slots and will occupy the card once admission finishes, so counting it as
     free headroom would over-commit that worker. Display answers "is the card working right now"; headroom
     answers "is this worker slot claimable" — different questions, correctly different answers.

## Consequences

- A mechanical contract now lands on the fastest quality-adequate seat instead of the smallest one, subject to
  the INV-5 rider's constraints (a demotion/ordering key only, never an acceptance criterion).
- A declared-but-idle lease no longer strands a quiet node; only a genuine fence (exclusive, draining, or a
  busy non-text hold) excludes.
- `runConcurrency` sibling subtasks under `route=auto`/`remote` stop competing for the same stale snapshot —
  the joint deal accounts for headroom across the whole batch before any of them dispatches.
- `Place`'s and `betterRemote`'s signatures both grew a `seed string` parameter for the P2C draw;
  `PlaceVision`'s exported signature is unchanged (it mints its own seed internally, since its one caller,
  `internal/visionremote`, is outside this PR's file scope).
- The published `placement_reason` string is longer on a resolved auto/remote placement (the per-node verdict
  clause) but its existing prefix is untouched.
- Cost, honestly stated: a remote that is eligible but merely a few percent slower than the incumbent can now
  be preferred where the old rule would have kept the incumbent (P2C intentionally trades a small, bounded
  amount of "always the argmax" for herding avoidance) — bounded by the 20 % near-tie band and reversible by
  narrowing or removing it if measurement shows it costs more than it saves.

## Alternatives considered

- **A hard refusal on the seat's `min_turn_sec`** (its max-final worst case) — REJECTED by the INV-5 rider
  itself: the diagnosis's own roast round found this would defer nearly every contract on a fleet whose
  `min_turn_sec` reflects a cold load plus the full configured budget, not what a given contract's fitted final
  actually needs.
- **A pure argmax on expected completion, no P2C** — REJECTED: modeled to herd every independent dispatcher onto
  the single fastest seat the moment two are close, recreating the "one seat queues 18 deep" shape the
  diagnosis measured, just against a different node.
- **Leave `route=spread`'s ranking untouched, ship W-11 for `auto`/`remote` only** — REJECTED for consistency:
  the same fleet should not answer "which seat is best for this mechanical contract" differently depending on
  which route asked, so `scoreFit` folds in the identical eta/window axes (without P2C, since spread's own
  rotation-order tie-break already de-herds a Run's own siblings).

## Related code

- `internal/delegate/eta.go` — `etaFor`, `queueWaitFor`, `feasibleFinal`, `betterRanked`, `etaPreferred`,
  `p2cDraw`, `scoreFitRanked`.
- `internal/delegate/gate.go` — `Place`, `betterRemote`, `leaseFences`, `leaseBusyDemoted`, `PlaceVision`,
  `visionEtaBetter`.
- `internal/delegate/fit.go` — `scoreFit`.
- `internal/delegate/run.go` — `dealAutoRemote`, `placeAutoRemote`, `headroom`, the RunWith joint-deal
  precompute, `runRemote`'s `?wait=`/Retry-After handling.
- `internal/delegate/placement_reason.go` — `oneWordVerdict`, `placementVerdictLine`.
- `internal/delegate/nodeview.go` — the 0.127 health activity fields (`JobsAdmitting`, `SeatLoaded`,
  `SeatStarting`, `LeaseExclusive`, `LeaseDraining`, `RecentAgentWallSec`, `QueueWaitEstimateSec`).
- `internal/mcpserver/mcpserver.go` — `nodeVerdict`, `localSeatVerdict` (inside the `offload_status` handler).

## Related docs

- `docs/systems/fleet-node.md` — "Placement routes and the retry" / "Expected-completion ranking and the joint
  deal".
- `docs/systems/mcp-server.md` — `agent_delegate`'s `route` argument.
- [0049 — the ampere-16 vLLM seat](0049-ampere-16-vllm-seat-is-the-3bit-gsq-27b.md) — the seat-rate numbers this
  ADR's feasibility/eta arithmetic reads are the same ones that ADR's live-cutover measured.
