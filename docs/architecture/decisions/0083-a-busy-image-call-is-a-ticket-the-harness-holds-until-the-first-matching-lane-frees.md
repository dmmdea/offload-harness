---
status: Proposed
date: "2026-10-10"
---

# ADR 0083 — A busy image call is a ticket the harness holds until the first matching lane frees, anywhere, with one answer and no caller loop

## Context

ADR 0082 made a busy local lane overflow to an idle node that renders the same recipe. It left the operator's other half of the
order open: route media work to any node of the cluster **even when every node is full**, queue and never refuse, and do not
make the caller retry. Three things were still wrong once every lane was busy.

- **A call with no idle match fell back to the local line and joined it late.** Each lane question, roster read and bounce (up to
  a node's `gpu_wait_ms`, three nodes at most) was time the local queue did not count, so a caller that arrived meanwhile went
  ahead of it. A caller that looped on a queued answer re-sent the same request until a lane freed (85 answers in one recorded
  loop), and a call carrying a `waiter_token` never overflowed at all, so the loop stayed in the local line even when a node
  was idle.
- **Two processes of one machine could both send an idle node a job.** The only memory of "a job of mine is there" was the
  placer's package variable, which is per process.
- **A freed lane was found by the caller's next try.** Nothing started a waiting job when a lane freed, and an MCP session
  never reloads its config, so a node added to `delegate_remotes` since the session began joined it only after a restart.

## Decision

1. **A busy image call is a ticket.** An `auto` image call on a machine that has the lane and a fleet asks the pipeline whether
   the lane is free (ADR 0082). A free lane runs the call here as it always did. A busy one becomes a ticket, one file
   `<state root>/media-queue/mq-<id>.json`, written atomically, that any process of the machine can read, and the call is only a
   way of waiting for it. That root is the one the GPU lease uses (`state_dir`, else `LOCAL_OFFLOAD_STATE_DIR`, else `%ProgramData%\local-offload` on Windows or `/var/lib/local-offload` elsewhere; no config key is needed): the tickets, the node claims and the lease's place tokens of one
   machine share one root, through the one function that resolves it (`gpulease.ResolveStateRoot`), and a config that sets no
   `state_dir`, which is the usual one, has the queue. (`gpu_lock_path` moves only the lease directory, not the queue.) The file holds what a process other than the owner needs: the owner (pid and start time) and its
   beat, the request's key, the arrival time, the seed, the place in the local line, the state (`waiting`, `local`, `placing`,
   `placed`, `fetching`, `done`, `failed`), the node and the node's job id once there is one, the nodes whose recipe matched at
   the last scan, and the last answer. The request body stays in the owning process; the file keeps no prompt.
2. **Identity is content.** The key is the sha256 of the task, the prompt and the parameters as sent without the `waiter_token`
   (numbers canonicalised through JSON). Any identical re-send attaches to the live ticket, with or without a token, and a
   result is handed over once per request: the call that takes it marks the file collected, and the file stays 2 minutes (chosen)
   so the other calls waiting for the same ticket, in this process or another, receive the result too and nothing renders twice; a
   later request that only repeats the content is a new job. A
   `waiter_token` of the form `mq-...` collects that ticket (a token of a different request is refused by name; one that names
   nothing is a new arrival; an `mq-` token never reaches the lease line). A `tk-...` place of the local line is brought into
   the ticket. This is what ends the loop: the tenth identical request is the first one's ticket.
3. **The ticket fixes one seed.** When the caller gave none, the ticket mints one (1 to 1e9, the pipeline's own range) once, so
   a bounce, a re-placement, an adoption and a local run all render the same composition. The key is taken before the seed is
   fixed, so a re-send without a seed attaches and one with another seed is another job.
4. **The ticket keeps its place in the machine's own line.** `core.PlaceKeeper` (implemented by the pipeline, forwarded by the
   MCP runner) wraps the lease manager's tokens and adds no admission logic: a place is left at creation with the ticket's
   arrival time on the cards the real admission would queue the call on, touched on its own rhythm (a keeper per scanner, every
   scan interval, that does not wait for the scan: a scan visits its tickets one after another and each visit asks one bounded
   lane question, so a place touched only at its ticket's turn would lapse once enough tickets wait), left again with the same arrival
   time if it lapsed, let go when the ticket ends, and spent by the final local run through the `waiter_token` path every resumed
   call already takes. A caller that arrives after the ticket queues behind it. A host that does not lease cards keeps no
   places; the ticket then waits without local seniority and its answer says so. The lane question for a call that resumes a
   place is ordered by arrival on every plan (`gpualloc.QueuedClaimsBefore`): the lease queue serves a resumed place by arrival
   time, so a caller behind it never holds the card against it. Without this two tickets on one idle card each read the other's
   place as in its way and neither would start.
5. **One scanner per process decides; a goroutine per ticket in flight acts.** The scanner starts with the process's first ticket,
   looks at its tickets every 10 s +/- 20 % (a chosen transport constant) oldest first, and ends with the last one. A scan
   decides and never runs a job, so a local run that waits in the lease line does not stall the scan of another ticket or the
   touch that keeps each ticket's place alive. For a waiting ticket it: keeps the place; **starts the local run when the lane
   question says the ticket would be served now** (at most one ticket per scan, the oldest) **and could read the lane** (a lane
   the pipeline tried to read and could not, `core.LaneVerdict.Unjudged`: the card table past its 4 s bound, the lease directory
   not opening, is not a free lane; the fleet is looked at in the same scan and the run here is the answer only when no node can
   take the call; a call the prober does not model at all still runs here); else asks the fleet with ADR 0082's
   own filter and ranking (`candidatesFor`, not forked), minus the nodes a job of the machine has a claim on (a ticket's, or a
   whole-node graph call's, decision 6) and the nodes an older waiting ticket of the machine matches (an older ticket that has not
   been scanned yet is first in line for every node).
   Nothing is sent to a node until a lane is free there: **late binding**, no job parked anywhere. One node is tried per scan; a
   node that refuses or bounces the job goes back to waiting and is paused by the placer's memory (a full node that names no
   `Retry-After` for 5 s, a bounce for 60 s and then 300 s; a node that does not answer for 5, 15, 60 and then 300 s, with one
   line on the process's stderr per step and none per scan); a local run that the lane refused at once, or that lost the race to
   another job, goes back to waiting with its place and **pauses the lane for that ticket** for 5, 15, 60 and then 300 s by
   consecutive refusal (the same steps; a reading of busy resets them), so a lane that reads free and refuses at once (`gpu_wait_ms`
   0, a younger place on the card that a call without a wait cannot pass) is not asked in a loop, each run writing the pipeline's
   own deferred row; the fleet is looked at meanwhile. A ticket never gives up on a lane (the S1 bound of three nodes is gone); it
   ends after 8 h without one.
6. **A node a job of this machine has a claim on is in use for every scanner on the machine.** A claim file per node is written,
   together with the ticket's `placing` state and the id of the job about to be sent, under a directory lock, before the job is
   sent: the read and the write are one step, so two processes never send one single-job node two jobs. **A whole-node graph call
   (decision 12) takes the same claim, under the same lock, before it sends**, so a ticket is never sent a node a graph is running
   on, a graph is never sent a node a ticket is running on, and two graph calls of two processes do not stack on one node. A
   graph's claim has no ticket file behind it: it names a graph call, and it is judged by its owner alone (a graph call has no
   scanner to write a beat; its call ends, and lets the node go, when its job does), which makes it live while its process is, and,
   when the process is gone, only for the first 2 minutes after the claim was taken (counted from the claim, not from the death, since
   the call writes no beat; after that the node's own health speaks for the job). Every machine has the claim directory (decision 1): only a machine whose lease root is refused (a
   cloud-synced folder) keeps no queue, says so once in its log, and there only the in-process hold of decision 12 stands between
   two graph calls of one process, and two processes can still stack two graphs. The lock is an operating-system file lock
   (`flock`, `LockFileEx`), never a file whose age decides it is stale: every way of breaking a stale file lock has a gap in which
   a slow contender removes the fresh lock of the one that broke it (two holders inside at once, 24 of 300 contended rounds on the
   first design), while the system frees an OS lock when its holder's process ends, however it ends. The lock file stays for good.
   A cancel that lands while the claim waits for the lock is seen under the ticket's lock after the claim returns: the claim is let
   go and nothing is sent. A ticket's claim is live while its ticket has a job in flight on that node and
   its owner is alive, or died less than 2 minutes ago (chosen: the job it sent keeps running on the node, which shows its lease
   only once the job is admitted, and in those seconds the node would read idle); after that the node's own health speaks for it.
7. **Owners die; tickets are adopted or swept.** An owner is dead when its process is gone or when it has not written its beat
   for 10 minutes. A `waiting` ticket of a dead owner, and a `local` one (its render died with its process), are swept by the next
   scan or call of any process, and their places let go; the identical call that finds one starts a new ticket that keeps its
   arrival time. A `placed` or `fetching` ticket is kept for 6 hours and **adopted by the next identical call**: the new owner
   polls the recorded node job and fetches its outputs and never sends the request again, so the job renders once. The adopted
   call writes its one ledger row and opens no card (the card the ended process opened is closed by the sweep that closes a dead
   process's cards). A `placing` ticket (the owner ended while the job was being sent) names its job, minted before the claim; the
   adopter asks the node once, and a node that denies the job was never given it, so the ticket goes back in line with its arrival;
   a ticket with no job id (a file of an older build) is not polled and the answer says the job may be on the node. A job the node
   accepted and passed back (another job held its card) goes back in line as for a live owner, and the node is left alone for a
   while. A finished ticket nobody collected stays 6 hours when its job succeeded (chosen: the caller was told to call once and come
   back for it) and 10 minutes when it failed (a repeat of the request should try again).
8. **A scan writes no card, no row and no waiter.** A scan reads the lane, the roster and the files, writes its own ticket file
   (beat, matches, answer) and touches its place. The call's one ledger row and one PAIR card are opened at dispatch: the node's
   acceptance for a remote job (`core.RemoteAttribution`, as in ADR 0082), the pipeline's own run for a local one. A local run
   that is refused writes the pipeline's own deferred row (one per refusal, spaced by the pause above).
9. **One answer says where it stands and why.** A call whose window ends before its job is done answers, as a capacity deferral
   of class `gpu_queued`, `{queued, scope: "cluster", ticket, waiter_token, position, local, cluster[], next}`: the position among
   the machine's waiting tickets, this machine's lane (state, holder, callers ahead), every roster node in config order with its
   state (`free` for an idle node another ticket is first in line for, `busy` and the holder's class, reason and time left,
   `draining`, `not-capable` and the keys that differ, `unreachable`, `in-use` and whose ticket, `bounced`, `refused`,
   `skipped`) and since when, and `next`: the harness holds the job and starts it on the first matching lane that frees; do not
   call again or resend; a repeat attaches to this ticket; call with `waiter_token=mq-...` only to collect the result. The
   window is `wait_s` of the MCP door, else the machine's `gpu_wait_ms`; the ticket goes on after the call returns. The CLI
   (`generate-image`) blocks until the job is done, says on stderr where the ticket stands, and the first interrupt cancels a
   ticket that sent nothing; a job already on a node cannot be recalled (ADR 0064), so the ticket stays for the next identical
   command to adopt. `offload_status` gains `media_queue[]`.
10. **The roster is re-read by mtime.** A ticket re-reads `delegate_remotes` from the config file the session runs on (the MCP
    server passes it; the CLI passes it too) whenever the file's mtime or size changes. Only that key moves; a file that does not
    parse leaves the roster as it was.
11. **ADR 0082 changes in four places.** Decision 2: a call that resumes a place is ordered by arrival on every plan (the test that
    pinned the other rule is renamed). Decision 6: no bound of three nodes; the ticket waits instead. Decision 8: when nothing
    admits, the call is a ticket, not a local run that joins the line late. Decision 9: a call that carries a `waiter_token`
    overflows like any other, bringing its place along. All four are for an image call (see 12).
12. **A whole-node graph is not a ticket in 0.180.0.** ADR 0082's Amendment 1 (a graph overflows to an idle single-card node that
    holds the files it names) is released together with this ADR. The ticket is the only busy-lane path of an IMAGE call; a graph
    keeps the one-shot placement of the first cut (`tryGraphOverflow` and `placeGraph` in `internal/mediaremote/place.go`, reached
    from `RunWith` before the ticket door): it is offered once to the nodes that can take it, at most three, one graph call of a
    process per node, and when none admits it, it waits in this machine's own queue. **It shares the nodes with the tickets by the
    tickets' own two rules.** It never takes a node a job of this machine has a claim on, and it takes the claim of the node it
    uses (decision 6), so a ticket and a graph cannot both be sent one node. And it yields to the tickets: every ticket that is
    waiting is older than a graph that arrives now, so a node an older waiting ticket matched at its last scan is left to that
    ticket (the answer's row reads `free ... ticket mq-... is older and first in line for it`), and so is every node while that
    ticket has not been scanned yet (its first scan comes within about one scan interval of its creation). The graph then waits in this machine's own queue, which is
    never a refusal. A lane the pipeline tried to read and could not (`Unjudged`, decision 5) is not a free lane for a graph either:
    the fleet is looked at first and the graph runs here when no node takes it. **A graph does not wait in the cluster queue yet.**
    It has no ticket, so it keeps no place in the cluster queue, does not start on a lane that frees after its one look at the
    fleet, and a repeat of the same graph is a second call. The reason it is left out is that a ticket's identity is the hash of the request's parameters
    (decision 2), and a graph's parameters name files on this machine (`graph_path`, `manifest_path`) rather than carry them, so what
    makes two graph calls the same call needs a decision of its own; making the graph a ticket is the follow-up.

## Consequences

- The operator's "even if all PCs are full" holds for images: the job waits for the first matching lane that frees anywhere, in the
  order it arrived, with no caller loop, and the answer says per node why it is not running yet. Late binding costs up to one scan
  interval (12 s) plus however long the scan before it took (each waiting ticket costs one lane question of up to 4 s) between a
  lane freeing and the ticket noticing, and a freed node can be taken in that interval by a caller that is neither a ticket nor a
  whole-node graph (a video, animation or audio call placed on the fleet, or any call forced to a node); the job
  then costs one bounce (a 5 s pause for a full node, 60 s for a bounce).
- **One first-in-first-out line per delegating machine.** Two machines each hold their own queue and meet only at a node's own
  grant; a holder node's queue (ADR 0030) is still the named upgrade, not built. The order among a machine's tickets is arrival
  time; the local lane's order is the lease line's.
- **A repeat of an identical request within the life of an uncollected result returns that result** (6 hours for a success, 10
  minutes for a failure). A caller that wants a second picture of the same prompt must differ in a parameter (a seed); one that sends
  the identical request while the first is still waiting or running gets the same ticket. This is the price of ending the loop.
- A CLI ticket dies with the CLI: a waiting one is swept, a placed one is adopted by the next identical command.
- **A process that is suspended for longer than 10 minutes reads as dead to the others** (the owner is judged by its beat) until it
  writes the beat again: its waiting tickets can be swept meanwhile, an identical call can then make a second ticket, and a placed
  ticket is adopted rather than re-sent (the job is then polled by two processes). A suspend guard (the pid must also be gone) is the
  named follow-up.
- **Cost.** Each waiting ticket pays a lane question (one bounded `nvidia-smi` read) and a roster read per scan; the creation
  reading is reused by the first scan for 2 s. N waiting tickets are N reads per interval; a roster-first short circuit and a
  shared table read are named follow-ups.
- **Measured and unmeasured.** Everything is proved against real fleet-node servers, a real lease manager and fakes (the S1
  pattern); nothing was run against a live lease, a live node or a render. The scan interval, its jitter, the pause steps, the
  owner and keep timings and the 8 h ceiling are chosen constants, not resource numbers (docs/systems/media-generation.md,
  "The ticket queue").

## Alternatives considered

- **Keep S1's synchronous placement beside the ticket.** Rejected for an image call: a first call that placed itself outside the
  ticket bypassed the claim, so two processes' first calls could still stack on one idle node, and a bounced first call still
  rejoined the local line late. It is kept for a whole-node graph only, which is not a ticket in this release (decision 12), and
  that path takes the ticket's claim and honours the ticket's order, so that it neither takes a node a ticket is using nor jumps an
  older waiting ticket.
- **Make a whole-node graph a ticket in the same release.** Deferred, not rejected: the identity of a graph call (decision 12) is
  undecided, and the graph overflow (ADR 0082 Amendment 1) was built and tested against the one-shot placement.
- **Park the job on the first node that will be free soonest.** Rejected: a lease end is a ceiling, not a promise, and a job parked
  on a node cannot be recalled; late binding starts it on whichever lane frees first.
- **Read the state files and then send.** Rejected for the in-use rule: that is exactly the window in which two processes both
  read a node idle.
- **Enable the pull queue (ADR 0030).** Still rejected for the reasons of ADR 0082: the nodes in question cannot pull.

## Related code

- `internal/mediaremote/ticket.go`, `queuefs.go`, `scan.go`, `door.go`, `place.go` (the placement both paths share; `tryGraphOverflow` and `placeGraph` are the graph's), `mediaremote.go` (`RunWith`)
- `internal/core/place.go` (`PlaceKeeper`), `internal/pipeline/medialane_place.go`, `medialane.go`, `internal/gpualloc/queued.go`
- `internal/mcpserver/mcpserver.go` (`wait_s`, the place forwarding), `status_section.go` (`media_queue`), `main.go` (`generate-image`)

## Related docs

- [ADR 0082](0082-a-media-call-overflows-to-the-fleet-when-its-own-lane-cannot-take-it-and-a-family-is-identified-by-its-recipe.md), [ADR 0077](0077-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md), [ADR 0039](0039-a-held-card-is-a-place-in-line.md)
- [Media generation](../../systems/media-generation.md), [GPU lease](../../systems/gpu-lease.md), [Operator guide](../../OPERATOR-GUIDE.md)
