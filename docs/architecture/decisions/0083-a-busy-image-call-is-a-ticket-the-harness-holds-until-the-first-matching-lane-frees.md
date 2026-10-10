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
   `<state_dir>/media-queue/mq-<id>.json`, written atomically, that any process of the machine can read, and the call is only a
   way of waiting for it. The file holds what a process other than the owner needs: the owner (pid and start time) and its
   beat, the request's key, the arrival time, the seed, the place in the local line, the state (`waiting`, `local`, `placing`,
   `placed`, `fetching`, `done`, `failed`), the node and the node's job id once there is one, the nodes whose recipe matched at
   the last scan, and the last answer. The request body stays in the owning process; the file keeps no prompt.
2. **Identity is content.** The key is the sha256 of the task, the prompt and the parameters as sent without the `waiter_token`
   (numbers canonicalised through JSON). Any identical re-send attaches to the live ticket, with or without a token, and a
   result is handed over once: the call that takes it removes the file, and a later identical request is a new job. A
   `waiter_token` of the form `mq-...` collects that ticket (a token of a different request is refused by name; one that names
   nothing is a new arrival; an `mq-` token never reaches the lease line). A `tk-...` place of the local line is brought into
   the ticket. This is what ends the loop: the tenth identical request is the first one's ticket.
3. **The ticket fixes one seed.** When the caller gave none, the ticket mints one (1 to 1e9, the pipeline's own range) once, so
   a bounce, a re-placement, an adoption and a local run all render the same composition. The key is taken before the seed is
   fixed, so a re-send without a seed attaches and one with another seed is another job.
4. **The ticket keeps its place in the machine's own line.** `core.PlaceKeeper` (implemented by the pipeline, forwarded by the
   MCP runner) wraps the lease manager's tokens and adds no admission logic: a place is left at creation with the ticket's
   arrival time on the cards the real admission would queue the call on, touched on every scan, left again with the same arrival
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
   question says the ticket would be served now** (at most one ticket per scan, the oldest); else asks the fleet with ADR 0082's
   own filter and ranking (`candidatesFor`, not forked), minus the nodes a ticket of the machine has a job on and the nodes an
   older waiting ticket of the machine matches (an older ticket that has not been scanned yet is first in line for every node).
   Nothing is sent to a node until a lane is free there: **late binding**, no job parked anywhere. One node is tried per scan; a
   node that refuses or bounces the job goes back to waiting and is paused by the placer's memory (a full node that names no
   `Retry-After` for 5 s, a bounce for 60 s and then 300 s; a node that does not answer for 5, 15, 60 and then 300 s, with one
   line on the process's stderr per step and none per scan); a local run that lost the race to another job goes back to waiting
   with its place. A ticket never gives up on a lane (the S1 bound of three nodes is gone); it ends after 8 h without one.
6. **A node a ticket has a job on is in use for every scanner on the machine.** A claim file per node is written, together with
   the ticket's `placing` state, under a directory lock, before the job is sent: the read and the write are one step, so two
   processes never send one single-job node two jobs. A claim is live while its ticket has a job in flight on that node and
   its owner is alive, or died less than 2 minutes ago (chosen: the job it sent keeps running on the node, which shows its lease
   only once the job is admitted, and in those seconds the node would read idle); after that the node's own health speaks for it.
7. **Owners die; tickets are adopted or swept.** An owner is dead when its process is gone or when it has not written its beat
   for 10 minutes. A `waiting` ticket of a dead owner, and a `local` one (its render died with its process), are swept by the next
   scan or call of any process, and their places let go; the identical call that finds one starts a new ticket that keeps its
   arrival time. A `placed` or `fetching` ticket is kept for 6 hours and **adopted by the next identical call**: the new owner
   polls the recorded node job and fetches its outputs and never sends the request again, so the job renders once. The adopted
   call writes its one ledger row and opens no card (the card the ended process opened is closed by the sweep that closes a dead
   process's cards). A finished ticket nobody collected stays 10 minutes.
8. **A scan writes no card, no row and no waiter.** A scan reads the lane, the roster and the files, writes its own ticket file
   (beat, matches, answer) and touches its place. The call's one ledger row and one PAIR card are opened at dispatch: the node's
   acceptance for a remote job (`core.RemoteAttribution`, as in ADR 0082), the pipeline's own run for a local one. A local run
   that loses a race writes the pipeline's own deferred row.
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
    overflows like any other, bringing its place along.

## Consequences

- The operator's "even if all PCs are full" holds for images: the job waits for the first matching lane that frees anywhere, in the
  order it arrived, with no caller loop, and the answer says per node why it is not running yet. Late binding costs up to one scan
  interval (12 s) between a lane freeing and the ticket noticing, and a freed node can be taken by a caller that is not a ticket in
  that interval; the job then costs one bounce (a 5 s pause for a full node, 60 s for a bounce).
- **One first-in-first-out line per delegating machine.** Two machines each hold their own queue and meet only at a node's own
  grant; a holder node's queue (ADR 0030) is still the named upgrade, not built. The order among a machine's tickets is arrival
  time; the local lane's order is the lease line's.
- **A repeat of an identical request within the life of an uncollected result returns that result.** A caller that wants a second
  picture of the same prompt must differ in a parameter (a seed); one that sends the identical request while the first is still
  waiting or running gets the same ticket. This is the price of ending the loop.
- A CLI ticket dies with the CLI: a waiting one is swept, a placed one is adopted by the next identical command.
- **Cost.** Each waiting ticket pays a lane question (one bounded `nvidia-smi` read) and a roster read per scan; the creation
  reading is reused by the first scan for 2 s. N waiting tickets are N reads per interval; a roster-first short circuit and a
  shared table read are named follow-ups.
- **Measured and unmeasured.** Everything is proved against real fleet-node servers, a real lease manager and fakes (the S1
  pattern); nothing was run against a live lease, a live node or a render. The scan interval, its jitter, the pause steps, the
  owner and keep timings and the 8 h ceiling are chosen constants, not resource numbers (docs/systems/media-generation.md,
  "The ticket queue").

## Alternatives considered

- **Keep S1's synchronous placement beside the ticket.** Rejected: a first call that placed itself outside the ticket bypassed
  the claim, so two processes' first calls could still stack on one idle node, and a bounced first call still rejoined the local
  line late.
- **Park the job on the first node that will be free soonest.** Rejected: a lease end is a ceiling, not a promise, and a job parked
  on a node cannot be recalled; late binding starts it on whichever lane frees first.
- **Read the state files and then send.** Rejected for the in-use rule: that is exactly the window in which two processes both
  read a node idle.
- **Enable the pull queue (ADR 0030).** Still rejected for the reasons of ADR 0082: the nodes in question cannot pull.

## Related code

- `internal/mediaremote/ticket.go`, `queuefs.go`, `scan.go`, `door.go`, `place.go`, `mediaremote.go` (`RunWith`)
- `internal/core/place.go` (`PlaceKeeper`), `internal/pipeline/medialane_place.go`, `medialane.go`, `internal/gpualloc/queued.go`
- `internal/mcpserver/mcpserver.go` (`wait_s`, the place forwarding), `status_section.go` (`media_queue`), `main.go` (`generate-image`)

## Related docs

- [ADR 0082](0082-a-media-call-overflows-to-the-fleet-when-its-own-lane-cannot-take-it-and-a-family-is-identified-by-its-recipe.md), [ADR 0077](0077-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md), [ADR 0039](0039-a-held-card-is-a-place-in-line.md)
- [Media generation](../../systems/media-generation.md), [GPU lease](../../systems/gpu-lease.md), [Operator guide](../../OPERATOR-GUIDE.md)
