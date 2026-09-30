---
status: Accepted
date: "2026-09-30"
---

# The whole call has a deadline below the client's abort

## Context

The MCP client aborts a tool call at its own limit (1,800 s in the reference setup) and drops the
response with it. `agent_delegate` and `offload_research` had no deadline of their own: the request
context carried none, the results came back only when EVERY subtask ended, a producing job is polled
to its node's ceiling (up to 14,400 s, ADR 0055), and `offload_research` runs its pages in
consecutive chunks of eight. One slow subtask therefore held a call past the client's abort and took
the finished ones down with it. On 2026-09-27 a call ran 2,103 s and lost a finished 423 s answer
that had been sitting in its results since second 423.

Every open-ended wait makes this worse, so it is the precondition of the next placement change (the
delegator's capacity wait running until a call deadline instead of a fixed TTL): an unbounded wait
must never outlive the client's abort.

A second defect sat on the same door (register C-75). Any failed or lost subtask flagged the whole
MCP result as an error, and results marshalled as summary, sources, results. The MCP client answers an
error-flagged body by keeping only its head and tail, so every partial research reply the workers saw
had its digests (the middle) cut out. The mechanism is an inference from those cut replies, not a
measurement of the client.

## Decision

1. **The two MCP delegation doors own a whole-call deadline.** `agent_call_deadline_sec` (default
   1,500 s; `0` = the default; negative = none) is measured from handler entry, because the client's
   clock starts when it sends the request. It reaches the engine as the absolute
   `delegate.RunOptions.Deadline`, and it also bounds `offload_research`'s page fetch. The CLI verbs
   have no client to abort and take none. The default sits above the longest single subtask that starts
   at once (`timeout_sec` cap 900 s, plus the 300 s admission allowance and the 60 s poll grace the
   delegator holds a job open for: 1,260 s) and below the client's 1,800 s by the margin a response
   needs (the live check is "call wall <= deadline + 30 s"). It does not promise that a healthy subtask
   is never cut: time a job spends queued on a node is credited back to its wall (up to 300 s), and a
   capacity wait comes before placement, so a worst-case auto-sized subtask can run past the default and
   be cut. A workload that needs longer sets `agent_call_deadline_sec`, below the client's abort; a value
   at or above it, or a negative that was meant as a number, is a `doctor` finding and a startup warning.

2. **At the deadline the call returns what has finished and defers the rest.** The deadline is the
   context every placement, probe, poll and local run already honours, so the outstanding work is
   cancelled, not just no longer waited for; a subtask still waiting for a run slot, and the later
   chunks of a batched call, never start. Each unfinished subtask is published as a **budget-class
   defer** whose reason opens `call deadline reached; N unfinished` and then says what that subtask was
   doing (running on a named node under a named job, running on the local seat, not yet placed, never
   started, or not stopping). N is the whole call's count, frozen at the first look so every reason
   states the same number, and it spans every chunk of a batched call. The outcome is applied at the two
   moments an outcome is produced (`finish` and `settle`), so the wire result, the ledger row and the
   corpus row say the same thing; it is never re-applied to a result published later, because "produced
   after the deadline" can only be answered when the outcome is produced. A result that finished,
   including one that failed its acceptance checks, is an answer and is never rewritten: an abstention
   whose retry was cut stays an abstention and carries the retry's fate in its `retry_note`.

3. **The unwind is bounded, and a subtask that ignores its context cannot hold the call.** Cooperating
   goroutines get an allowance after the deadline (a twentieth of the time left when placement began,
   clamped to 250 ms to 10 s) to write their own rows and hand back a truthful result. A
   goroutine still running after it is abandoned: the call returns a "did not stop" defer for it, its
   late answer is dropped by a board that no longer accepts writes, its own rows are still recorded,
   and the ledger closes after the last such goroutine returns. A remote job cut this way keeps its
   intent open (`orphanable`): the node may still finish it, and the recovery pass may still harvest it.
   A subtask nobody ran (never started, abandoned, or cut before it was placed) names no node and no
   seat, as `exhausted()` already does for "no node took it", and is marked `Unplaced`. Once the run
   begins draining its PAIR emitter, a frame from an abandoned goroutine is dropped: `Emit` adds to a
   `sync.WaitGroup`, and an Add racing the last Done of a Wait in progress panics that Done, which
   would take the process down for a frame nobody is waiting for (before this change no goroutine
   could outlive the run).

4. **A call deadline is a result shape, never a failure.** Budget-class defers do not set the MCP error
   flag. The flag itself narrows (C-75): `isError` is set only when NOTHING succeeded and something
   failed, was lost to the stack, or was skipped. A partial result is a successful call whose body says
   what is missing (`summary.failed`, `lost_to_stack`, `skipped`, each subtask's own `failed` /
   `defer_class` / `reason`, and for research `partial` and `error`); the CLI keeps its wider
   exit-code rule. `offload_research` marshals `summary`, `partial`, `error`, `results`,
   `result_sources`, `sources`: the digests before the long sources, and the notes that say pages are
   missing in the head now that the flag no longer does. Every field is kept.

5. **A cut remote job is withdrawn on request, best effort.** Cancelling the poll leaves the job on its
   node, where it could start later on a seat nobody is waiting for. At the deadline the delegator asks
   the node to withdraw each job it is walking away from: `DELETE /fleet/jobs/{id}` with the fleet
   bearer, five seconds, detached from the cancelled context. It is a request, not a claim: a node that
   has not shipped the route answers 404 or 405 and keeps the job (today's behaviour), a job that has
   already started is not the delegator's to cancel, and nothing the call publishes depends on the
   answer. The reason says the node was asked, never that the job is gone. The ask never outlives the
   unwind: its timeout is the lesser of five seconds and three quarters of the allowance, so a node
   that does not answer cannot turn the truthful cut result (its node and job) into an abandoned one.

6. **Progress is reported only to a client that asks, and nothing depends on it.** A request that
   carries a progress token (`_meta.progressToken`) gets `notifications/progress` from the two doors: an
   opening one, one per subtask state change (started; finished and how, counted against the whole call
   across the chunks of a batched one), and a heartbeat every 30 s while nothing changes that says how
   many subtasks are done and how long the call has left. `progress` is a running counter, because the
   spec asks for a strictly increasing value and a heartbeat has no new work to count. The events reach
   the reporter through a bounded queue that drops rather than blocks, so a slow client cannot slow a
   subtask, and a request with no token, or with no session, changes nothing. In the MCP TypeScript
   client SDK the token is sent only when the caller passes `onprogress`, and the request timeout
   restarts on a progress update only when the caller also sets `resetTimeoutOnProgress`
   (`maxTotalTimeout` is the absolute cap). **Whether the reference client does either is
   unverified**; the whole-call deadline does not rely on it.

## Consequences

- No `agent_delegate` or `offload_research` call outlives its deadline by more than the unwind
  allowance (at most 10 s), the flush of any PAIR frames still in flight (bounded at 2 s each) and
  building the response, so the response is meant to reach the client before its abort. That is the
  design's claim, not a measurement: the live check is "call wall <= deadline + 30 s".
- A subtask that would have run past the deadline is cut, not finished. The caller re-issues it in a
  smaller call; a remote job that had started keeps running on its node and is not harvested by this
  call. A call-deadline defer is not evidence about a seat's speed: its reason names the deadline.
- `isError` no longer marks a partial result. A caller that read the flag as "something was lost" reads
  `summary` instead; the counts have not changed.
- `route=queue` is bounded too: a poll the deadline cancelled is reported as a defer (the job stays on
  the holder), not a failure. The queue lane still polls its jobs one after another, so a job that
  finished behind an earlier, slower one is not collected once the deadline has passed.
- Not solved here: the capacity wait still ends on its own TTL rather than on this deadline, and a
  producing job is still polled to its node ceiling when no deadline is set (the CLI).

## Alternatives considered

- **Rely on progress notifications resetting the client's timeout.** Rejected as the mechanism: whether
  a client honours `resetTimeoutOnProgress` is unverified, and a hard deadline works with every client.
  Progress reporting is a separate, optional layer.
- **A per-subtask wall.** Rejected: `timeout_sec` already bounds a subtask's execution, and the client's
  abort is on the call, not on any one subtask.
- **Return at the deadline without waiting for the subtasks to unwind.** Rejected: the results of the
  cut subtasks (their node, their job id) and their telemetry rows would be lost, and the ledger would
  close under them. The allowance is bounded, and the stuck case is handled explicitly (decision 3).
- **Wrap the handler's context only.** Rejected: a cancelled subtask reports a failure ("canceled"), the
  call still waits for slow unwinds, and nothing counts the unfinished subtasks.
- **Keep flagging partial results and fix only the order.** Rejected: the flag says the call failed, the
  client truncates on it, and a partial call did not fail.

## Related code

- `internal/delegate/calldeadline.go` (the deadline, its stamping, the unwind and the batched and
  queued cases), `internal/delegate/run.go` (`RunOptions.Deadline`, the fan-out block in `RunWith`,
  `RunBatched`)
- `internal/delegate/progress.go`, `internal/mcpserver/progress.go` (progress notifications)
- `internal/mcpserver/mcpserver.go` (`callDeadlineAt`, `handleAgentDelegate`, `handleResearch`,
  `delegateIsError`, `researchWire`)
- `internal/config/config.go` (`AgentCallDeadlineSec`, `CallDeadline`)

## Related docs

- [MCP server](../../systems/mcp-server.md), [fleet node](../../systems/fleet-node.md),
  [operator guide](../../OPERATOR-GUIDE.md)
- ADR [0055](0055-walls-are-ceilings-liveness-is-progress.md) (liveness ceilings),
  ADR [0028](0028-delegation-durability-is-a-push-side-intent-ledger.md) (the intent ledger),
  ADR [0030](0030-pull-queue-ships-dark.md) (route=queue)
- Register rows C-67 (this deadline) and C-75 (partial results and result order)
