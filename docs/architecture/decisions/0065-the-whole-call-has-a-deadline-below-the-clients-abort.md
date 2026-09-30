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
   is never cut: time a job spends queued on a node is credited back to its wall (up to the queue budget
   the node's own estimate sets, [ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md)
   decision 4: 1.5 x its estimate + 30 s, at least 60 s, at most the contract's poll budget; the lesser
   of that budget and 300 s for a node that publishes none), and a capacity wait comes before
   placement, so a worst-case auto-sized subtask can run past the default and be cut. A workload that
   needs longer sets `agent_call_deadline_sec`, below the client's abort; a value at or above it, or a
   negative that was meant as a number, is a `doctor` finding and a startup warning.

2. **At the deadline the call returns what has finished and defers the rest.** The deadline is the
   context every placement, probe, poll and local run already honours, so the outstanding work is
   cancelled, not just no longer waited for; a subtask still waiting for a run slot, and the later
   chunks of a batched call, never start. Each unfinished subtask is published as a **budget-class
   defer** whose reason opens `call deadline reached; N unfinished` and then says what that subtask was
   doing (running on a named node under a named job, running on the local seat, not yet placed, never
   started, or not stopping). N is the whole call's count, frozen at the first look so every reason
   states the same number, and it spans every chunk of a batched call. The outcome is applied where an
   outcome is produced (`finish` for an attempt's end, `settle` for an outcome no attempt produced,
   `exhaustedSettled` for a refusal chain the delegator closes), so the wire result, the ledger row and the
   corpus row say the same thing; it is never re-applied to a result published later, because "produced
   after the deadline" can only be answered when the outcome is produced. A result that finished,
   including one that failed its acceptance checks, is an answer and is never rewritten: an abstention
   whose retry was cut stays an abstention and carries the retry's fate in its `retry_note`.

   The cut decides what an outcome is CALLED, never what was observed. It keeps the run's own wire and
   overrides only `deferred`, `defer_class` (`budget`) and `reason`, so a local run the deadline
   cancels after nine steps and thousands of generated tokens still publishes its steps, tokens, stop
   reason, seat rate and trace, on the result, the ledger row and the corpus row: those are the longest
   runs, the ones the wall sizing and the rigger most need measured. What the outcome itself reported
   beyond the cancellation is quoted, bounded, in the reason (`the run itself reported <class>:
   <reason>` for a defer, `the run itself failed: <error>` for a failure; a cancelled poll's own
   opening clause is dropped, what a give-up appended after it is kept), so a stack failure that lands
   inside the unwind is not erased; "it was cancelled" is written only for an outcome that echoed a
   cancellation. An outcome is "produced after the deadline" by the clock, not by the context's cause:
   `ErrCallDeadline` is the cause a subtask's context carries, so an error that wraps it names the
   deadline, but the rewrite cannot key on it (a client cancel before the deadline is not the deadline,
   and a parent that ends earlier is adopted as the call's own). The wait outcomes built in `settle`
   quote nothing of the wait's own text; the placement narration keeps that history behind the deadline
   marker. A wait the deadline ends AFTER refused attempts is recorded like any other cut, as one
   closing row under a job id of its own (the id of the last refused dispatch already belongs to that
   attempt's row, and a second row under it would double-count one id); the caller is given that id. A
   refusal chain the delegator closes after the deadline has passed (`placement refused`) is cut and
   recorded the same way, as an outcome nobody placed, with the chain quoted behind the marker: the fleet
   read that would have named another node may be the very thing the deadline ended, so a failure saying no
   node was eligible would accuse nodes that were never asked, and would flag a one-subtask call as an error.

3. **The unwind is bounded, and a subtask that ignores its context cannot hold the call.** Cooperating
   goroutines get an allowance after the deadline (a twentieth of the time left when placement began,
   clamped to 250 ms to 10 s) to write their own rows and hand back a truthful result. A
   goroutine still running after it is abandoned: the call returns a "did not stop" defer for it, its
   late answer is dropped by a board that no longer accepts writes, its own rows are still recorded,
   and the ledger closes after the last such goroutine returns. The abandoned result carries the job id
   of the attempt that had not returned (the runner remembers each subtask's latest), the call records
   its own row under that id at return, and the goroutine's late row, if it ends, is under the same
   id, so the caller can reconcile the two; the result still names no node or seat. A remote job cut this way keeps its
   intent open (`orphanable`): the node may still finish it, and the recovery pass may still harvest it. The one
   exception is a job the node has said will never run (a confirmed withdrawal, or its own record of a job it never
   ran): the give-up already cleared `orphanable` for it, the cut leaves that as it found it, and the intent closes as
   it does for any give-up (decision 5).
   A subtask nobody ran (never started, abandoned, or cut before it was placed) names no node and no
   seat, as `exhausted()` already does for "no node took it", and is marked `Unplaced`. Once the run
   begins draining its PAIR emitter, a frame from an abandoned goroutine is dropped: `Emit` adds to a
   `sync.WaitGroup`, and an Add racing the last Done of a Wait in progress panics that Done, which
   would take the process down for a frame nobody is waiting for (before this change no goroutine
   could outlive the run). The drop is logged once per run, naming the job: PAIR's card for it stays
   as it was until PAIR's own staleness sweep.

4. **A call deadline is a result shape, never a failure.** Budget-class defers do not set the MCP error
   flag. The flag itself narrows (C-75): `isError` is set only when NOTHING succeeded and something
   failed, was lost to the stack, or was skipped. A partial result is a successful call whose body says
   what is missing (`summary.failed`, `lost_to_stack`, `skipped`, and each subtask's own `failed` /
   `defer_class` / `reason`); the CLI keeps its wider exit-code rule. `offload_research` marshals
   `summary`, `partial`, `error`, `results`, `result_sources`, `sources`: the summary leads, the digests
   come before the long sources. What says pages are missing is the summary and each result's own
   fields, mapped to their pages by `result_sources`. `partial` and `error` are narrower than their
   names: they mark a batched run that returned an error beside the results it had collected, and
   `RunBatched` returns an error only for what `RunWith` validates (route, subtask count, tailnet
   remotes), which every chunk of one call shares, so no ordinary partial result sets them. They stay
   in the head anyway. Every field is kept.

5. **A cut remote job is taken back through the delegator's one withdraw path, best effort.** Cancelling
   the poll leaves the job on its node, where it could start later on a seat nobody is waiting for. The
   deadline cancels the context the poll runs under, and the poll's cancel exits give the job up like any
   other cancel ([ADR 0064](0064-a-delegator-takes-back-what-it-has-not-started.md), decision 3):
   `DELETE /fleet/jobs/{id}` with the fleet bearer, once, detached from the cancelled context, and not for
   a job last seen running (it has started, and the request could only be refused). The deadline adds no
   request of its own; it reads the answer, so one job is never asked twice. That holds for the queue
   deadline's own ask too: a job it asked about, and the node answered 409 (it had started), is not asked
   again by the give-up that follows before the next poll answers, which reads that answer. It is a
   request, not a claim: a node that has not shipped the route answers 404 or 405 and keeps the job
   (today's behaviour), a job that has already started is not the delegator's to cancel, and what the call
   publishes depends on the answer only for the words that say what it was. A node that confirms has
   taken the job back for good: the reason says so, and the intent closes as `withdrawn` instead of
   staying open for the recovery pass. Anything else leaves the job where it was, and the reason carries
   the give-up's own clause (`withdraw not confirmed: HTTP 405: ...`, `no answer within ...`), quoted
   from what the outcome itself reported. Once the deadline has passed the ask never outlives the
   unwind: its bound is the lesser of the flat five seconds and three quarters of the allowance
   (`withdrawBound`), so a node that does not answer cannot turn the truthful cut result (its node and
   job) into an abandoned one.

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
   unverified**; the whole-call deadline does not rely on it. The plan made progress conditional on
   the client honouring `resetTimeoutOnProgress`, which cannot be checked without that client. It is
   kept, in its own commit: the condition is enforced at run time by the client's own choice (nothing is
   sent without a token), it costs one goroutine and a bounded queue per token-carrying call, and a
   reader who would rather hold it back until the behaviour is measured drops that one commit.

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
  the holder), not a failure. The queue lane polls its jobs one after another, so at the deadline the
  delegator takes one last look at each job whose poll it cancelled (once each, all at once, on a
  context of its own bounded by the lesser of 2 s and three quarters of the unwind allowance) and
  publishes what the holder says: a finished or failed job is returned as that answer, a job still held
  is a call-deadline defer that says queued or claimed and running, and a job the holder could not be
  asked about says so. The unfinished count spans only the jobs that really are unfinished.
- The rigger classifies a cut as its own axis, `call-deadline`, ahead of `timeout` (its wall-timeout
  pattern matches the bare word "deadline"), so a call that ran out of time does not steer a seat's
  timeout share.
- The deadline meets [ADR 0063](0063-placement-holds-instead-of-sleeping-or-refusing.md)'s waits and
  its page cap in three places, and in each the call running out of time is the caller's clock, never
  evidence about a node, a seat or a page. A capacity-wait tick the call's context cuts keeps the
  previous tick's state, as one cut by the wait's own deadline does: a node that answered earlier in
  the wait (cooling down after its own refusal, held out by the backlog gate) is not narrated as a
  failed probe. A retry that stood in line for a busy seat and whose wait the deadline ended says
  `call deadline reached` in its `retry_note`, not that the caller canceled. And the per-page retry cap
  does not count a cut as a failed issue: the cut is a class-budget defer that still names its seat and
  node, which the cap would otherwise read as the seat's own budget, so a research page a call keeps
  running out of time on is not backed off for fifteen minutes.
- The refusal chain and the retry's choice of a node are the same seam under the same rule. A chain the
  delegator closes once the deadline has passed is the deadline's outcome (decision 2), not `placement
  refused`; and the sentence a re-placement read leaves when it names no node says the call's deadline had
  passed in place of `no further eligible remote was available`, a claim about nodes that a read the
  deadline ended cannot support. A retry whose node selection the deadline ended says so in its
  `retry_note` (`retry skipped: call deadline reached before a retry node was chosen`), where an empty
  note would read as there being nowhere else to go.
- Every call-deadline row carries `reason_code` `budget`: the closed set of
  [ADR 0064](0064-a-delegator-takes-back-what-it-has-not-started.md) has no member of its own for a cut, so a
  reader counting `budget` rows tells a cut from a node-side ceiling by the reason's opening
  `call deadline reached`. A code of its own would be an additive change, left for a later decision.
- `agent_call_deadline_sec` at or above the client's abort, or a negative that was meant as a number,
  loads (it never refuses) but is a `doctor` finding and a startup warning.
- Not solved here: the capacity wait still ends on its own TTL rather than on this deadline; a
  producing job is still polled to its node ceiling when no deadline is set (the CLI); and the other
  doors that call the same engine (`agent_run` and `offload_ask` with a route, the review lane's fleet
  path) carry no deadline, because the plan names the two delegation doors, so a call through them can
  still outlive the client's abort.

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
- `internal/delegate/withdraw.go` (`withdrawBound`, the unwind bound the give-up's withdraw takes once the
  deadline has passed; the withdraw itself is ADR 0064's)
- `internal/delegate/processgate.go` (`pageIssueFailed`, which does not count a cut) and
  `internal/delegate/run.go` (`awaitCapacity`'s tick, `exhaustedSettled`, `replacementNode`'s read and
  `runOne`'s retry note, where the deadline meets ADR 0063's waits and re-placement)
- `internal/delegate/progress.go`, `internal/mcpserver/progress.go` (progress notifications)
- `internal/mcpserver/mcpserver.go` (`callDeadlineAt`, `handleAgentDelegate`, `handleResearch`,
  `delegateIsError`, `researchWire`)
- `internal/config/config.go` (`AgentCallDeadlineSec`, `CallDeadline`, `CallDeadlineFindings`)
- `internal/core/calldeadline.go` (`CallDeadlineReasonPrefix`), `internal/rig/rig.go` (the
  `call-deadline` axis)

## Related docs

- [MCP server](../../systems/mcp-server.md), [fleet node](../../systems/fleet-node.md),
  [operator guide](../../OPERATOR-GUIDE.md)
- ADR [0063](0063-placement-holds-instead-of-sleeping-or-refusing.md) (the capacity wait, the queue budget
  and the page cap the deadline composes with),
  ADR [0064](0064-a-delegator-takes-back-what-it-has-not-started.md) (the withdraw a give-up asks for),
  ADR [0055](0055-walls-are-ceilings-liveness-is-progress.md) (liveness ceilings),
  ADR [0028](0028-delegation-durability-is-a-push-side-intent-ledger.md) (the intent ledger),
  ADR [0030](0030-pull-queue-ships-dark.md) (route=queue)
- Register rows C-67 (this deadline) and C-75 (partial results and result order)
