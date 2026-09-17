---
status: Accepted
date: "2026-09-02"
---

# 0032 — A peer-held seat is waited for, not deferred

- Deciders: operator (masterplan #1, 2026-09-01 22:10), harness session
- Supersedes: the reading in `internal/pipeline/agenttask.go` (pre-0.111) that "llama-server does not rate-limit, so a 429 means something in front of the seat answered instead of it"

## Context

On 2026-09-01 several Claude Code sessions delegating in parallel reported the harness "failing to
schedule work and failing to deliver quality":

- "the 27B seat timed out at 600 s and then hit llama-swap 429s (another session held the slots)"
- "the Lenovo 4B seat returned the known phantom 'Go version' answer on 6 of 7 pages"
- "the two `offload_research` calls deferred (first exceeded the 8-contract cap, then llama-swap
  answered 429 concurrency and the Lenovo seat produced the known off-goal digests), so I digested
  the official pages with direct fetches instead"

Every failure ended the same way: the session bypassed the harness and spent cloud tokens — the
exact waste the harness exists to prevent.

### What llama-swap actually does (verified against the v251 source, 2026-09-02)

- **429** is emitted at admission when a model's *reserved* requests (queued + in-flight) reach its
  `concurrencyLimit` (default 10). It carries `Retry-After: 1` and the body `code:"concurrency_limit"`,
  `src:"llama-swap"`. It means *peers already hold the seat's slots* — every parallel session's MCP
  process fans out (`runConcurrency = 4`) against the same limit.
- **Swaps never 429.** A request that needs a model llama-swap is loading, or that would evict a
  busy model, is *queued with no timeout*. That queue is where the "600 s" went: a contract that
  arrived mid-swap spent its whole wall waiting for another session's model to load.
- A process that is not up yet answers **503 "process is not ready"**; a health-check timeout
  answers **500 "unspecific error: health check timed out"** with `src:"llama-swap"`; a generation
  that dies mid-body is a **502**.
- `GET /running` reports residency with the states `stopped|starting|ready|stopping|shutdown`.

### What the harness did with those answers (pre-0.111)

`agent.LLMClient.Chat` returned a plain `chat 429: …` error; `RunAgentTask` filed it as
`DeferClassInfrastructure`; `delegate.Run`'s local placement does not re-place an infrastructure
defer. So a peer-held seat produced a terminal defer and the **caller** re-routed the work to a
weaker seat by hand. (The first draft of this change assumed the harness itself re-placed on 429;
the roast council showed from the code that it did not. The mechanism was corrected before anything
was built.)

`offload_research` allows 12 URLs and builds one contract per usable page, but `delegate.Run`
refuses more than 8 subtasks — a 9–12-page call lost every page.

A research contract's acceptance was the goal-derived anchor (`AnchorCheck`) plus the schema's
first-array check; on pages where no goal token appears, the anchor is empty, and an answer about a
different document entirely ("The latest stable Go version is 1.26…") passed as a result.

## Decision

Four delegator-side changes, all additive, no new wire-schema verb (older fleet nodes keep
validating contracts), no daemon, every wait counted on the wire:

1. **`internal/seatwait` — a contract-scoped wait for a peer-held seat.** One `Budget` per
   `RunAgentTask`, carried in `ctx` and shared by every chat step and the structured re-pack, so a
   10-step loop cannot spend ten budgets. Ladder 1/2/4/8/15 s; a server `Retry-After` wins over the
   ladder and **counts against the budget** (a server that always says "1" still runs out). Retryable
   answers: 429, 503 "process is not ready", 500 with `src:"llama-swap"` / "health check timed out".
   Both seat clients (`agent.LLMClient.Chat`, `llamaclient.Generate`) re-send after the sleep with the
   `modelaffinity` ticket **released** — the contention is other processes' load, and a parked ticket
   only wedged this process's other lanes. Config `seat_contention_wait_sec` (0 = 90 s, −1 = off).
   The wire result carries `contention_wait_sec`; a defer after the budget starts with
   `seat contended:`; a wall consumed by waiting is filed as contention (infrastructure), never as
   a budget defer the delegator would size future contracts from.
2. **Admission pre-flight** (`awaitSeatAdmission`): before the contract's wall starts, poll
   `GET /running` (vendored client) while *any* model on the endpoint is in a non-`ready` state,
   bounded by `agent_admission_wait_sec` (0 = `core.AgentAdmissionSecDefault`, i.e. 300 s since
   0.115.11; −1 = off), fail-open on a probe error; the
   slept time is `admission_wait_sec` on the wire. **Known bound:** the drain phase before a swap
   shows nothing non-ready, so a swap that begins a second later is still charged to the wall — this
   removes the swap *window* from the wall, not the race. The remote-placement poll deadline in
   `runRemote` is unchanged.
3. **`delegate.RunBatched`** runs sequential chunks of `MaxSubtasks` (order kept, `Summary` summed
   by reflection, first error returned *with* the results already obtained); `offload_research` and
   `local-offload research` use it and render a chunk error as `partial: true` instead of dropping
   everything. Sequential on purpose: two chunks in flight would be a 16-wide fan-out.
4. **Document fingerprint + quarantine.** `research.DocFingerprint` appends two regex acceptance
   checks per page — the 12 most frequent distinctive tokens (≥ 6 letters, alphabetic, not in a
   boilerplate stop-list) split alternately, each half an `(?i)` alternation tagged
   `(?P<docanchor>…)`; both must match. A `delegate.Quarantine` (process-scoped, owned by the MCP
   server and passed via `RunOptions`, 30-minute TTL) blocks a fleet node after two fingerprint
   failures inside the TTL; the runner strikes **only** on `docanchor` failures — a user's over-strict
   `contains:` or a thin page's `min_items` never quarantines a seat. `Summary` gains
   `quarantined` and `batches`.

## Consequences

- Worst-case added latency per contract = the two budgets (`seat_contention_wait_sec` +
  `agent_admission_wait_sec`), both counted on the wire
  and both opt-out. The `seat contended:` reason plus `contention_wait_sec` / `admission_wait_sec`
  are the numbers that say whether defers were traded for latency.
- The fingerprint is a **wrong-document tripwire, not a quality gate**: lexical overlap is a poor
  faithfulness metric (Maynez et al. 2020, *On Faithfulness and Factuality in Abstractive
  Summarization*; Cao et al. 2022, *Hallucinated but Factual!*), which is why the bar is "one token
  from each half of twelve", not "many tokens".
- Quarantine is per process, so a node fixed mid-day comes back without a restart and a flaky one is
  re-tried after 30 minutes. The Lenovo fleet-node goal-swap bug itself stays open in `ROADMAP.md`;
  this change makes it fail loudly and stop being re-tried, it does not fix the node.
- **Capacity is the operator's step, prepared here and not applied:** the 429 exists because
  `concurrencyLimit` (10) is shared by every session; raising it on the agent seats in
  `llama-swap.yaml` and/or `--parallel` on the 27B seat adds slots. `docs/OPERATOR-GUIDE.md`
  carries the ready-to-paste diff and the elevated restart.
- Follow-ups noted, not built: a cross-process admission (file-lock semaphore) on the box, and
  applying the fingerprint to every contract that carries a `ContextDoc`, not only research.
- **Extended 2026-09-15 (0.125.0, register D-110):** "waited for, not deferred" holds only while the
  wait can end. A GPU lease held by another process cannot, inside the caller's bound — so
  `offload_review_diff` now asks `delegate.ForeignFence` BEFORE building its local loop and routes the
  review to the fleet at route `remote` under `remoteEligible`'s ctx-fit floor, waiting locally only
  when nothing out there qualifies; and `modelaffinity.LeaseError` carries the holder's declared
  window, so the defer that remains says when to come back.
- **Amended 2026-09-17 (0.126.2, register S-26/S-25/S-24/S-08):** the admission budget above is
  `core.AgentAdmissionSecDefault` = **300 s**, not the 120 s this ADR was written against — it was
  raised in 0.115.11 when the budget took on the seat's own cold load, and the figure is read from
  the constant by every door and by the delegator's poll allowance. Three consequences follow for
  this decision's own rule. (a) The D-110 extension now applies to BOTH agent doors, not only the
  review lane: `ForeignFence` is asked before `AwaitRunSlot` on `runAgentTask` and on `agent_run`, so
  a hold that cannot end inside the budget is a `capacity` defer in milliseconds. "Waited for, not
  deferred" continues to govern a non-fencing reservation and an inherited lease, which is the case
  this ADR actually decided. (b) The pre-flight matches the seat's own `/running` row by alias OR
  canonical id; without that the "my seat is ready" exit could not fire on an alias-bound seat and
  the wait this ADR bounded was paid in full for another model's swap. (c) `agent_run` runs the
  pre-flight at all — it never did, which made this ADR's point 2 true of one door only.
