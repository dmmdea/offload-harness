---
status: Accepted
date: "2026-09-30"
---

# A delegator takes back a job it has not started, and a node cleans up after a delegator that left

## Context

Every time a delegator gave up on a job a node had accepted, the job stayed on the node. The delegator's
give-ups are the queue deadline (the node accepted the job and never started it inside the patience), a
caller that cancels, and an owned poll deadline. Each one marked the job "orphanable" and walked away. The node
had no route to take a job back, so the job sat in its backlog, started as soon as a slot freed, ran to the end
or to a stall (4 to 64 minutes of seat time), held slots the live jobs needed, and then fed its finished wall
into the node's own Retry-After estimate: a longer wait sent callers away for longer, which produced more
abandoned jobs.

The abandonment was deliberate. The operator's Option A ([ADR 0028](0028-delegation-durability-is-a-push-side-intent-ledger.md))
keeps a node's running work recoverable: each delegator writes an intent ledger, and a later process polls the
open intents and collects the results. The recovery never happened. On 2026-09-29, 61 of the 415 acked
dispatches were closed by the recovery pass, 36 of them on an HTTP 401 (the recovering process held no or the
wrong `fleet_auth_token`) and 25 on a 404 (the node no longer held the job), and none was recovered. In the
same window 43 % and 59 % of two nodes' agent runs had no matching delegator result row (50 % and 73 % of
their run wall), and one node spent 96 % of its wall in one hour on jobs whose delegator had left.

Nothing in the delegator's ledger could show this. Rows were written when a job ended, so a hang or a ghost
was invisible until it ended; rows carried no door and no id a node's ledger shared; the reason was cut to 120
bytes; the joins that finally found the ghosts needed a guess on latency; and the intent ledger's `ok` events
carried no timestamp or process id.

## Decision

1. **A delegator can take back a job the node has not started.** `DELETE /fleet/jobs/{id}` asks the node to
   withdraw a job. Under the job store's mutex, the same one the scheduler claims under, an `accepted` job
   becomes terminal and can never start; a job that is running or finished is not touched. The answers:

   | Answer | Meaning |
   |---|---|
   | `200 {"state":"withdrawn","withdrawn":true}` | the job had not started; it never will (a repeat says the same) |
   | `409 {"state":"running"\|"done"\|"error","withdrawn":false}` | it had started; the node did not touch it |
   | `404` | the node does not hold that id |
   | `401` | the same bearer gate as the poll, checked before any state crosses the wire |
   | `405` | not an agent job (media and vision pollers are other clients) |

   A node that predates the route answers `405` (its pattern is GET-only), or `404`; a delegator reads both as
   "no withdraw here" and behaves exactly as it did.

2. **A withdrawn job is terminal, not deleted.** Its record becomes `error: "withdrawn: ..."`, its request
   payload and temp files are released once, and it ages out with every other terminal record. Terminal states
   stay write-once, a poller always reaches one, a duplicate dispatch of the id meets the known-job path (409,
   never a second run), and the jobs feed shows what became of it.

3. **The delegator asks once, best-effort, where it gives a job up.** The request has a 5 s bound and runs on a
   context the caller's cancel does not touch (the commonest reason to withdraw is that the caller was just
   canceled). Only a confirmation changes anything:
   - at the **queue deadline**, a confirmed withdrawal files the result as a capacity refusal, so the existing
     re-placement machinery offers the subtask to another node (the job never ran anywhere, so this cannot
     arrange a double run); the queued wait is credited back to the contract's budget, like every other span it
     spent waiting; and the intent closes as `withdrawn`. A `409 running` means the job left the backlog between
     the last poll and the withdraw: the delegator keeps polling it, and does not ask again;
   - on a **cancel** (both exits of the poll loop; one of them used to skip the orphanable mark and closed the
     intent as "terminal observed" for a job the node might still run) and at an **owned poll deadline**, a
     confirmed withdrawal closes the intent as `withdrawn`; a job last seen running is not asked;
   - every other answer (404, 405, 401, 5xx, a dropped connection, a timeout) leaves today's behaviour: a
     failure naming the deadline, not re-placed, the intent left open for recovery.

4. **A node reaps the ghosts of a delegator that left.** An `accepted` job that a delegator pushed and that
   nobody has polled for `fleet_poll_lease_sec` is skipped by the claim scan at once and reaped by a ticker
   (terminal, `error: "reaped: ..."`). Default 60 s; a negative value turns the rule off; a positive value under
   15 s is raised to 15, because the delegator's own gap between polls (a 12 s long poll plus a few seconds of
   sleep) must fit inside the lease. What counts as a poll: an authorized poll of the job, a duplicate dispatch
   of its id, and a long poll parked on it. What does not: the unauthenticated jobs feed the fleet overview
   reads, and a poll that failed the bearer gate. A running job is never reaped. The rule covers only agent jobs
   pushed by a dispatch: a job the pull queue claimed is admitted without a poller (its result travels by ack),
   and media and vision jobs are polled by other clients on cadences this node does not control.
   The claim scan skips a stale job so the ticker's interval is never a window in which a ghost starts. This
   protects a node from an older delegator that does not withdraw.

5. **An abandoned run's wall does not feed the Retry-After basis.** A run that finishes more than a lease
   after its poller last looked is a ghost's wall; `recent_agent_wall_sec` and the Retry-After built from it
   skip it. A look after the job finished never changes that, so a recovery pass an hour later cannot make a
   ghost's wall a sample.

6. **The recovery pass stops closing intents on a 401**, and every intent event says when and by whom. A 401 is
   a fact about the caller's credentials, so the intent stays open (the 48 h expiry still bounds it) and the
   pass logs it once, however many intents it covered. A terminal error a node writes for a job it never ran
   (`withdrawn: ...`, `reaped: ...`) has no result to file, so recovery closes that intent as `never started`,
   writes no envelope and does not count it as a recovery. Every intent event carries the unix second and the
   pid of the process that wrote it, stamped on the one append path.

7. **The ledger sees its own failure shapes.** On the delegator's `agent_delegate` row:
   - `door`: the contract's, else the engine's own name, on every row;
   - `fleet_job_id`: the id the node knows the job by, when it went to a node; the node's own `agent` row carries
     the same id (both doors a node admits work through stamp it), so the orphan join is one equality;
   - `reason_code`: a closed set (`ok`, `failed_verification`, `queue_full`, `queue_deadline`,
     `queue_withdrawn`, `poll_deadline`, `canceled`, `node_unreachable`, `job_lost`, `dispatch_refused`,
     `remote_error`, `capacity_wait`, `shed`, `no_eligible_node`, `seat_down`, the `stall_*` phases, the node's
     defer classes, `other`), set on every row by a total classifier and normalized by `Record`;
   - `reason`: stored whole (bounded at 4096 bytes on a rune boundary). `ShortReason` is the 120-byte display and
     grouping form the report readers use, so a class is not split by every job-specific number;
   - a `phase: "started"` marker row, written when a node acks the job or a local run begins, so a hang or a ghost
     is visible while it happens. A refused dispatch leaves no marker. The marker is never a job: every counter
     skips it, and `ParentJobIDs` skips it too, because a marker looks exactly like a parent row and an inner row
     whose real parent never landed would otherwise be dropped as the child of a marker.

   The C-62 rule is unchanged: inner rows carry `parent_job_id`, and an orphan inner row still counts as its job.

## Consequences

- The node half needs a node redeploy. Until a node is upgraded a new delegator gets `405` and behaves as it did;
  an upgraded node cleans up after an old delegator on its own (decision 4).
- The withdraw and the lease remove ghosts before they start. They do not touch a running job: work that has
  started still runs to its end, and a delegator that gave up on it leaves its intent open for the recovery pass.
- Whether a caller's cancel should also cancel a RUNNING job is an operator decision this change does not make.
- A delegator that resumes after more than the lease (a suspended host, a long partition) finds its accepted job
  reaped and reads the terminal `reaped` state as a remote job error. Re-placing on that stable text is possible
  and is not done here.
- Every job leaves one more ledger row (the marker). Any reader outside this repository that counts rows must
  skip `phase: "started"`; the ones in this repository already do.
- Rows written before this change keep their 120-byte reason and carry no code, no fleet id and no marker.
- Not addressed here: the delegator's queue patience (a fixed five minutes whatever the node's ETA), admission
  that counts queue positions which cannot start in time, and dealing by capacity. They are separate changes and
  are why a withdrawn job may find little execution budget left.

## Alternatives considered

- **Delete the record on withdraw.** A poller would then read a 404, which the delegator treats as a lost ack and
  answers by re-dispatching the same id: the withdraw could recreate the job it removed, and "pollers always reach
  a terminal state" would stop being true.
- **Cancel a running job when the delegator gives up.** Rejected: it discards the work Option A keeps recoverable,
  on a signal (a deadline) that says nothing about whether the answer is still wanted.
- **Reap every accepted job.** Rejected: the pull queue's claims and media renders have no poller this node can
  see, and reaping them would kill queued work on a timer.
- **Reap on a ticker alone.** Rejected: a slot can free at any instant, and the ticker's interval would be the
  window in which a ghost starts.
- **Carry the fleet job id in `Params`.** Rejected: `Params` feeds the result-cache key, so a job-unique id there
  would make every dispatched request miss.
- **Write the started marker to its own file, or under its own task name.** Rejected: the join to the finished
  row is the point, and one file with one schema keeps every existing reader's dedup rule in one place.
