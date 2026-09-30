---
status: Accepted
date: "2026-09-30"
---

# Liveness judges the seat, not the request: the engine busy hold

## Context

ADR 0055 made a contract end on a STALL (no progress inside the seat's dynamic per-phase allowance) or a
safety CEILING, never on its wall expiring while the seat produces. It judged progress per REQUEST: a
request silent past its phase allowance was a stall. The allowances were sized from one uncontended
request on a cool seat — prefill = uncached tokens / a single-stream rate × 1.5 + 30 s (floor 60 s),
decoding = 20 deltas at the decode rate (floor 60 s), re-pack = a flat 120 s.

Every other reason a request goes quiet looked exactly like a hung engine:

- waiting its turn behind sibling requests in the engine's own queue;
- time-sharing a busy card (four requests running on one vLLM seat share its steps);
- being preempted and recomputed by vLLM when its KV cache fills (a live 27B seat on the ampere-16
  reference box showed 29 preemptions at KV 0.81);
- running on a card whose clocks are capped (a boot power profile plus thermal slowdown held the same
  A2 at 1,072 of 1,770 MHz).

On 2026-09-29 the fleet's agent jobs succeeded 15 % of the time: 96 stall kills, 41 finished answers
discarded in their re-pack, four decode "stalls" filed in the same second on one seat — while the
engines were producing throughout. The design never had a term for the one variable that decides
whether silence is a fault: what the seat's engine is doing for everyone else.

## Decision

1. **Read the engine before calling silence a stall.** When a request's own allowance runs out in
   admission, prefill, decoding or re-pack, the liveness monitor reads the seat's ENGINE
   (`seatload.ReadActivity`): the same two-step read the drain and the spread deal use — llama-swap's
   `/running` first, the engine only when the seat is loaded, always at the seat's own address, never
   through `/upstream` (which loads models and resets the idle timer). It yields a work FINGERPRINT that
   changes whenever the engine takes a step for any request:
   - vLLM `/metrics`: the engine-step count (`iteration_tokens_total_count`), generated and prompt token
     counters, preemptions, and the KV usage gauge — the last because vLLM credits a prompt only when its
     prefill finishes and a step with no output emits no iteration stats, so a long SOLO prefill moves
     nothing but the KV blocks it allocates chunk by chunk. A seat that exports no KV gauge is named in
     the reading (a solo prefill there reads flat);
   - llama-server `/metrics` (`--metrics`): `n_decode_total` (every `llama_decode()`, prompt batches
     included) and the token counters;
   - llama-server `/slots`, only when `/metrics` answers 404 or 501: each slot's task id, processing flag,
     decoded count (`next_token` is an array on current builds, an object on older ones) and
     `n_prompt_tokens_processed`, so a prompt batch moves it too.

   Request gauges (running, waiting, deferred) are reported but never part of the fingerprint: a wedged
   engine core whose front end still accepts requests would otherwise look alive forever. The
   finished-request counter is left out as well, because it counts ABORTS: a hung engine whose clients
   time out would tick it. A seat alias that resolves to more than one loaded model is an error, not a
   guess.

   A second, TOKEN fingerprint holds only the produced-token counters (generated and prompt tokens; for
   `/slots` the decoded and prompt-processed counts).

2. **The busy hold.** If the fingerprint moved since the last look, the request is waiting its turn: the
   run enters phase `queued`, and the monitor re-reads the engine every 10 s for as long as the engine
   keeps working. The run's ceiling still bounds it. The same check runs after a cold load completes (the
   post-ready window of the cold-load hold): a request that stays silent on a freshly loaded seat that is
   busy with others is held, not filed.

3. **A stall is an engine that does no work.** If the fingerprint does not move for
   max(120 s, the waiting phase's own allowance), the seat itself is wedged: that is the stall, filed as
   infrastructure with the engine's silence, the phase the request waited in and the last reading in the
   reason. The phase's own allowance is the floor so a long prefill on a `/slots`-only seat keeps the
   prefill allowance it always had. ADR 0066 files this verdict as a typed `seat down:` outcome the run
   waits out, and sizes the allowance for the seat's load (the load the engine had when it last did work)
   only on an engine that cannot see a prefill (`/slots`, or vLLM without a KV-usage gauge); every other
   engine keeps the bound as written here, whatever is queued behind it.

4. **An engine that steps without producing is thrashing.** If the work fingerprint keeps moving but the
   TOKEN fingerprint does not move for 3× the flat bound, the engine is preempting and recomputing the same
   requests without finishing any (vLLM with its KV cache full). That is a stall too, and its reason says
   so; without this bound a thrashing seat would hold its runs to their ceilings.

5. **Cannot tell is the old rule.** An engine unreadable at the FIRST look leaves the request's silence to
   decide, as before, and the reason says the engine could not be read. Inside a hold an unreadable read
   is no new evidence either way — a llama-server answers `/slots` and `/metrics` only between batches, so
   a read during a long prompt batch times out while the engine works — and the hold continues until the
   flat bound runs out since the engine was last seen moving; an engine that stays unreadable past it is
   filed as unreadable and the reason says the run was held. One read may take up to 45 s. A seat that
   llama-swap reports as loading hands the run to the cold-load hold (0.140.0), whose clock starts when
   the load is seen, not when the hold began.

6. **The delegator follows the hold on evidence, not on the node's word.** On entering the hold and on
   every read that sees the engine move, the node publishes the flat bound plus one poll as the run's
   allowance, anchored at that read (clamped to the time left to its ceiling). The delegator keeps polling
   a remote job while now < last progress + published allowance + grace, so the extension rolls forward
   only while the engine is seen working. A node that stops re-publishing — its engine went flat, its
   process hung, its link died — is given up one allowance plus grace after its last refresh. The first
   draft published the time left to the ceiling once, which followed a dead node to the ceiling.

7. **Contention is measured, not hidden.** The wall a run spent held is reported as `queued_ms` on the
   wire, both ledger rows and the call meta. It counts the hold up to the last moment the engine was seen
   working: the flat or thrash tail that ends in a stall is not contention. A run that was held does not
   feed the seat's measured single-stream rates (decode rate, prefill rate), which size every later
   allowance; its cold-load time still does.

## Consequences

- A busy, preempting or throttled seat no longer kills the runs it is serving; a wedged seat is still
  detected, about one flat bound later than the old floor, and a thrashing one about three flat bounds
  later.
- Four to five local GETs per hold poll per waiting run (llama-swap's roster and `/running` for the load
  check and the seat's address, the engine's `/metrics`, and `/slots` on a llama-server without
  `--metrics`), only while a request is already past its allowance.
- `queued` is a new liveness phase in the job record and status readers.
- Not solved here: a run lost to an engine that DIES mid-run still ends (the busy hold only makes the
  diagnosis right). Waiting for the seat's restart and re-issuing the step (a seat-down outcome) is its
  own change (ADR 0066). Placement and admission (remote 503 re-placement, abandoned accepted jobs, dealing by
  capacity) are their own change too.

## Alternatives considered

- **Scale every allowance by the seat's measured concurrency.** Rejected as the primary mechanism: the
  rates are EWMAs of single best samples and the concurrency a request sees changes during its life.
  The engine's own counters answer the question directly.
- **Raise the floors.** Rejected: a genuinely hung engine would hold every run in flight for the new
  floor, and throttling or queueing depth is unbounded in principle.
- **GPU utilization (NVML) as the signal.** Rejected as primary: a pipeline-parallel worker waiting on a
  hung peer spins at 100 % utilization, and several models can share a card.

## Related

ADR 0055 (walls are ceilings, liveness is progress), ADR 0039 (placement), register rows C-66 (walls
charge queueing to prefill), C-60 (admission refuses instead of queueing; its local run-cap livelock is
fixed in the same release), C-62 (ledger double count, same release).
