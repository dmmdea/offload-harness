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
   looks at its tickets every 10 s +/- 20 % (a chosen transport constant) oldest first, and ends with the last one. A ticket made while it
   runs wakes it for an immediate scan; the ticket that starts it does not, because its first scan is the start (the lane reading the call
   made is reused by that scan and is not asked for again a moment later). A scan
   decides and never runs a job, so a local run that waits in the lease line does not stall the scan of another ticket or the
   touch that keeps each ticket's place alive. For a waiting ticket it: keeps the place; **starts the local run when the lane
   question says the ticket would be served now** (at most one ticket per scan, the oldest) **and could read the lane** (a lane
   the pipeline tried to read and could not, `core.LaneVerdict.Unjudged`: the card table that did not read in its 4 s bound nor in its one 10 s retry, the lease directory
   not opening, is not a free lane; the fleet is looked at in the same scan and the run here is the answer only when no node can
   take the call; a call the prober does not model at all still runs here); else asks the fleet with ADR 0082's
   own filter and ranking (`candidatesFor`, not forked), minus the nodes a job of the machine has a claim on (an image ticket's or a graph ticket's, decision 6) and the nodes an older waiting ticket of the machine matches (an older ticket that has not
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
   sent: the read and the write are one step, so two processes never send one single-job node two jobs. **A whole-node graph is a ticket (Amendment 1) and takes the same claim, under the same lock, before it sends**, so an image ticket is never sent a node a graph ticket is running on, a graph ticket is never sent a node an image ticket is running on, and two graph tickets of two processes do not stack on one node. A call forced to a node (`route` remote, or `auto` on a machine with no lane for it, image or graph) takes no claim and reads none, as before this decision: the node's own grant is what stands between it and a ticket's job. **A claim of the kind `graph`** is the one a 0.180.0 process wrote for the one-shot graph call that release made (decision 12, as it stood there): it has no ticket file behind it, it names a graph call, and it is judged by its owner alone (that call had no scanner to write a beat; it ended, and let the node go, when its job did), which makes it live while its process is, and, when the process is gone, only for the first 2 minutes after the claim was taken (counted from the claim, not from the death, since the call wrote no beat; after that the node's own health speaks for the job). This release writes none, and still reads and honours it, because the processes of one machine are not all restarted at a deploy: a 0.180.0 process that is running a graph on a node holds it by that claim alone. Every machine has the claim directory (decision 1): only a machine whose lease root is refused (a cloud-synced folder) keeps no queue, says so once in its log, and there nothing is a ticket: `auto` places neither an image nor a graph on the fleet, and the call waits in the machine's own line. The lock is an operating-system file lock
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
    overflows like any other, bringing its place along. All four were written for an image call; Amendment 1 extends them to a
    whole-node graph.
12. **Superseded by Amendment 1 below.** *The state this decision was in at 0.180.0, kept as the record, and told in the past tense
    because none of it is true of this release:* a whole-node graph was not a ticket. ADR 0082's Amendment 1 (a graph overflows to an
    idle single-card node that holds the files it names) went out together with this ADR. The ticket was the only busy-lane path of an
    IMAGE call; a graph kept the one-shot placement of the first cut (`tryGraphOverflow` and `placeGraph` in
    `internal/mediaremote/place.go`, reached from `RunWith` before the ticket door): it was offered once to the nodes that could take
    it, at most three, one graph call of a process per node, and when none admitted it, it waited in this machine's own queue. **It
    shared the nodes with the tickets by the tickets' own two rules.** It never took a node a job of this machine had a claim on, and it
    took the claim of the node it used (decision 6), so a ticket and an overflowing graph could not both be sent one node (a graph
    forced to a node took no claim; decision 6). And it yielded to the tickets: every ticket that was waiting was older than a graph that
    arrived then, so a node an older waiting ticket matched at its last scan was left to that ticket (the answer's row read `free ...
    ticket mq-... is older and first in line for it`), and so was every node while that ticket had not been scanned yet (its first scan
    comes within about one scan interval of its creation). The graph then waited in this machine's own queue, which was never a refusal.
    A lane the pipeline tried to read and could not (`Unjudged`, decision 5) was not a free lane for a graph either: the fleet was looked
    at first and the graph ran here when no node took it. **A graph did not wait in the cluster queue.** It had no ticket, so it kept no
    place in the cluster queue, did not start on a lane that freed after its one look at the fleet, and a repeat of the same graph was a
    second call. The reason it was left out was that a ticket's identity is the hash of the request's parameters (decision 2), and a
    graph's parameters name files on this machine (`graph_path`, `manifest_path`) rather than carry them, so what makes two graph calls
    the same call needed a decision of its own; Amendment 1 is that decision.

13. **A batch is a set of tickets when it must be and one warm session when it can be.** `generate-image --batch jobs.jsonl [--family F]
    [--route auto|local|remote] [--results r.jsonl] [--resume] [--jobs N]` (register F28) decides once, at the start, where its jobs run and never
    mixes. The whole batch goes through the warm session (`pipeline.RunImageBatchAs`: one ComfyUI, the checkpoint loads once) when the route is
    local, the machine has no fleet, no state dir or an inherited lease, its lane is free, or its lane is not free **and no roster node renders the
    recipe** (krea2 and the 2512 build exist on this machine's files only, so per-job tickets would be a cold start per picture on the one lane that
    can run them: slower than waiting for the lane once; that batch keeps its place in the lease line for the lane, up to the queue's 8 h ceiling and
    not the 90 s window, so the command blocks until it has the lane instead of ending `queued` with exit 0 to be re-run from the back of the line,
    and an interrupt ends the wait). Otherwise each job is one ticket of decisions 1 to 10, at most N in flight (default 1 for
    this machine's lane plus one per matching node, at most 8, chosen), so a node's claim (decision 6) is what keeps a batch from sending two jobs to
    one single-job node; where no claim protects a node (`--route remote`, a machine with no image lane: the single-render call picks a node without
    one) the jobs run one at a time. There is no ticket to hold such a job, so a held card or a fleet with no free node is asked again after a pause
    (5 s, 15 s, 1 min, then every 5 min; chosen) up to the same ceiling, and a job that finds no place is `queued`, never `failed` (nor is a ticket the
    queue ended at its ceiling, which is not sent again as a new ticket); once one job has waited the ceiling the jobs not yet sent are queued at once; a fleet
    that can never take the job as configured (every node left out for a configuration fact, none merely busy or out of reach: `core.ErrClassNoNodeConfigured`)
    is not waited for, and the job fails at once naming each node's miss; the node's
    recipe is not compared there, so a row carries the family and licence the node reports, and a family only the nodes define is carried by name. The jobs reach a node exactly as written (decision 3 fixes a seed the job did not give; `refine=false` only
    when the job says so), and each output is fetched, checked against the sha256 the node published, to its exact `out`. There is no mixed
    local/remote work stealing and no batch task on the fleet (v1). Each job ends in a row `{i, out, seed, ok, ms, error, node, family, sha256,
    verdict}` (`rendered`, `skipped-existing`, `failed`, `queued`), kept beside the results file (`<results>.partial`) as its job is final and written over it when the batch ends, so an earlier results file survives a refused or a dead batch (a run that reopens the name sets a dead batch's partial aside as `<results>.partial.prev`); a file the batch rejects (a node's file with no published sha256) is set aside as `<out>.unverified`, so a failed job leaves no file of its own at its path (a picture that was already there before the batch is not its to remove). `--resume` skips a job only when the file
    at its `out` is not empty, decodes whole as a PNG and has the size the job names; anything doubtful is rendered again (the 21 zero-byte PNGs of
    2026-10-09). A job whose `out` cannot be made fails alone before a lane is asked for it. Exit 0 every job has its picture or a busy lane deferred
    the whole warm batch (rows `queued`), 4 some job failed (also in a deferred batch), 1 it stopped, was interrupted or a job found no place (`queued`).

## Consequences

- The operator's "even if all PCs are full" holds for images and, by Amendment 1, for a whole-node graph: the job waits for the first matching lane that frees anywhere, in the
  order it arrived, with no caller loop, and the answer says per node why it is not running yet. Late binding costs up to one scan
  interval (12 s) plus however long the scan before it took (each waiting ticket costs one lane question of up to 14 s: the 4 s read and its 10 s retry, when the first ran out of time) between a
  lane freeing and the ticket noticing, and a freed node can be taken in that interval by a caller that is not a ticket (a video, animation or audio call placed on the fleet, or any call forced to a node); the job
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
- **A batch of krea2 or of the 2512 build never leaves this machine** (decision 13): no node renders them, so the batch keeps its place in line
  for the lane (up to the 8 h ceiling) and runs warm on it, and says which node does not match and why. A batch of a 2.1 family pays a ComfyUI cold start per job
  on a node (zero-warm, ADR 0009), which beats waiting only while this machine's lane is held by a long job and a node is idle.
- **An interrupted batch leaves its jobs on the nodes.** A job already on a node cannot be recalled (ADR 0064); the same command adopts it only if
  the jobs carry explicit seeds (the ticket's identity is the request, and a seed minted afresh is a new request). The verb runs both
  paths under one interrupt context, so on the warm session the first interrupt cancels the render too (its process tree is killed and
  the lease released); before, the warm session ran under `context.Background()` and an interrupt killed the whole process.
- **Measured and unmeasured.** Everything is proved against real fleet-node servers, a real lease manager and fakes (the S1
  pattern); nothing was run against a live lease, a live node or a render. The scan interval, its jitter, the pause steps, the
  owner and keep timings and the 8 h ceiling are chosen constants, not resource numbers (docs/systems/media-generation.md,
  "The ticket queue").

## Alternatives considered

- **Keep S1's synchronous placement beside the ticket.** Rejected for an image call: a first call that placed itself outside the
  ticket bypassed the claim, so two processes' first calls could still stack on one idle node, and a bounced first call still
  rejoined the local line late. It was kept for a whole-node graph in 0.180.0 (decision 12), where the 0.180.0 integration fix round made
  it read and take the ticket's claim and yield to older waiting tickets, so that it neither took a node a ticket was using nor jumped an
  older waiting ticket; a graph that no node took still rejoined the local line late, and a repeat was a second call. Amendment 1
  removes it.
- **Make a whole-node graph a ticket in the first cut.** Deferred there and in 0.180.0, not rejected: the identity of a graph call was
  undecided. Amendment 1 decides it (the content of the graph) and does it.
- **Park the job on the first node that will be free soonest.** Rejected: a lease end is a ceiling, not a promise, and a job parked
  on a node cannot be recalled; late binding starts it on whichever lane frees first.
- **Read the state files and then send.** Rejected for the in-use rule: that is exactly the window in which two processes both
  read a node idle.
- **Enable the pull queue (ADR 0030).** Still rejected for the reasons of ADR 0082: the nodes in question cannot pull.
- **Mixed local/remote work stealing for a batch** (the warm session on this machine plus tickets on the nodes for the rest). Rejected for v1:
  it needs a rule for which jobs go where, and nothing has measured the cold start a node pays against a warm job here, so the rule would be a guess.
- **A batch task on the fleet** (a node holds a warm session for N jobs). Rejected for v1: every candidate node streams the 2.1 text encoder from
  RAM on every render, so a warm remote session buys little today, and it would be a new fleet task every node must keep true.
- **Per-job tickets even when no node matches.** Rejected: one cold start per picture on one lane, to gain nothing.

## Amendment 1 (2026-10-10, P0 step S5; released in 0.181.0): a whole-node graph is a ticket

Decision 12 left a graph out of the queue because a ticket's identity is the hash of the request's parameters and a graph's
parameters name files on this machine instead of carrying them. The operator's order is the same for a graph as for an image
("route work to any PC in the cluster, even if all PCs are full"), and the one-shot placement that 0.180.0 shipped for a graph had
three defects that only the queue closes: it kept no place in the cluster queue (a graph that no node took joined this machine's own
line late, from when its fleet attempt was over), it did not start on a lane that freed after its one look at the fleet, and a repeat
of the same graph was a second call. (The 0.180.0 integration fix round had already made the one-shot call take the ticket's per-node
claim and yield to older waiting tickets, which closed two defects of the first cut: two graphs on one node from two processes, and a
graph that jumped an older ticket. Both guarantees stay, and on the ticket path they are the ticket's own, the one rule of decisions 5
and 6 for both kinds.) This amendment makes a busy whole-node `run_graph` a ticket and removes the one-shot placement
(`tryGraphOverflow`, `placeGraph`, `overflowOutcome`, the in-process hold, the bound of three nodes, and the writer of the graph-kind
claim). Decisions 1, 2, 4 to 11 hold for a graph as written, except where a rule below says otherwise; it supersedes decision 12.

**What changed for a graph between 0.180.0 and 0.181.0**, in one place. *Changed:* (a) a busy `auto` graph that names no `devices`,
on a machine that has the lane, a fleet and a queue, is a ticket: it starts on the first matching lane that frees anywhere, in order of
arrival, and the call answers with the ticket, its position and `cluster[]` instead of the local lane's queued answer with a `tk-`
token to re-send; (b) the bound of three nodes is gone (a ticket tries one node per scan and waits for the next lane); (c) a node's 503
for a file the graph names carries `refusal: "not_capable"`: the ticket marks that node not-capable for itself and puts no pause on
it (0.180.0 put the placer's 5 s pause on the node for every call); (d) a graph that reads input files is a local-only ticket with a
place and a position, and every answer carries `fleet_note` (0.180.0 annotated only a deferral); (e) a graph that carries a `tk-`
`waiter_token` no longer stays on this machine: its place comes into the ticket and it may go to a node, as an image's does (0.180.0
never moved it); (f) the deferral a graph ends in after a local failure no longer carries `cluster[]` (the per-node reasons live in
the queued answers, as for an image); (g) on a machine whose lease root is refused, so that it keeps no queue, a busy graph is no
longer offered to the fleet (an image never was); (h) the MCP tool takes `wait_s`, and `run-graph` blocks on its ticket. *Unchanged,
and held on the ticket path by tests of both kinds:* a graph and an image ticket share one per-node claim, taken under the directory
lock, and one order, in which an older waiting ticket is first in line; a lane the pipeline could not read is a ticket and the fleet is
looked at first; the queue lives under the lease's state root, so a config with no `state_dir` has it; a node's file check at its
door, the one-card rule, `min_vram_gb`, the release proof; a graph that names `devices`, or runs under a lease its process inherited,
stays on this machine and is no ticket.

1. **Identity is the graph's content.** The key is the sha256 of the task and the parameters without the `waiter_token`, with the
   graph and the manifest read as what they say (canonical JSON: keys sorted, insignificant whitespace dropped, numbers as written)
   in place of `graph_path` and `manifest_path`. The MCP door writes an inline `graph_json` to a new temp file on every call, so a
   key made of the path would never attach, and a graph file edited under the same path is another graph. `out_dir`,
   `reserve_vram`, `min_vram_gb` and `model_family` stay in the key: the same graph sent to another folder is another job. A graph
   that cannot be read keeps its path in the key, and the lane that runs it reports the contract. The ticket mints no seed and sends
   no recipe digest: the graph owns its own sampling, and decision 3 is for an image.
2. **The trigger and the place are the whole-node reading's.** A graph that names no devices asks `Pipeline.MediaLaneFree` (ADR 0082
   Amendment 1 rule 2); a free lane runs it here as it always did, a busy one makes the ticket, and so does a lane the pipeline
   tried to read and could not (the card table past its bound, the lease directory not opening: decision 5's `Unjudged`, which the
   graph probe sets for exactly those two reads and for nothing else), whose ticket looks at the fleet in its first scan. The place it keeps in this machine's
   line is the whole-node token the real admission leaves for a waiting graph (no card named, reason `run-graph`;
   `Pipeline.leaveGraphPlace`, which adds no admission logic), at the ticket's arrival time, resumed if the call already held one,
   touched like an image ticket's and spent by the final local run through `waiter_token`. A graph that names `devices`, one under an
   inherited lease and a host that leases no cards keep none (the first two are not tickets at all; the third waits without local
   seniority and says so).
3. **Who may take it is S4's rules, in the same scan.** A candidate is a node that serves `run-graph` with the route CONFIGURED, runs
   this release, holds no lease, has exactly one card and a card of the size `min_vram_gb` asks (Amendment 1 of ADR 0082, rules 1,
   3 and 4), minus the nodes a ticket of this machine has a job on (the claim, which a graph ticket takes like an image's) and
   minus the nodes an older waiting ticket matches, of either kind. One candidate is tried per scan, and a node that bounces or is
   refused is paused by the placer's memory as for an image. The bound of three nodes (ADR 0082 decision 6, retired for an image by
   decision 11 above) is gone for a graph with the call that needed it.
4. **The node decides identity at its door, and a refusal is about the ticket, not the node.** The 503 that answers a graph naming a
   file the node does not hold carries `refusal: "not_capable"` in its body (a full node's and a route-not-ready 503 do not).
   The delegator reads the word, not the sentence. The ticket marks that node **not-capable for itself**, quoting the node's
   words, puts **no pause on the node** (the 5 s the placer sets for a 503 that names no `Retry-After` is for a full node, and it hid
   the node from every image ticket and every other graph it could serve), does not ask that node again for 5 minutes (chosen, not
   measured: the longest pause the placer puts on any node, and about the interval at which an operator who has placed the missing
   file expects the ticket to find it) and tries the next candidate in the same breath. A node the ticket holds not-capable is not one
   it matches, so it holds it against nobody. The memory is in the owning process; the cluster row it writes is what the other
   processes read.
5. **A graph that reads input files is a local-only ticket.** A node can match an input file only by name, so a same-named file of
   other bytes would run in its place (ADR 0082 Amendment 1 rule 2). Such a graph is a ticket that is never offered to a node: it reads
   no roster, matches no node and runs when this machine's lane serves it. It has what any ticket has (one per request, a place and a
   position in this machine's line), and every answer carries `fleet_note`, and the same sentence in its reason, saying why the fleet
   was not asked. A result carries no note. Carrying a size and a hash of each input file to the node, which would lift the
   restriction, is a named seam, not built; a call that must leave this machine anyway (`route remote`, or `auto` on a machine with no
   graph lane) is placed on a node as before, checked by name only.
6. **What the ticket closes, and what it keeps.** Closed: the late local join, the lane that frees after the one look at the fleet,
   the repeat that was a second call, the bound of three nodes, the `tk-` token a caller had to re-send, and the abandoned job: a graph
   a dead process sent is adopted by the next identical call, which polls and fetches and never sends the graph again (one that had
   sent nothing is swept). Kept, as the ticket's own rules and no longer a second copy of them: the claim is taken under the directory
   lock, so two processes, or two calls of one process, never put two graphs on one node, and a graph is an ordinary member of the
   line, so a node an older waiting ticket matches is that ticket's. The placement text of a result that ran on a node names the lane
   that left (`graph`, not `image`).
7. **The doors.** The MCP `offload_run_graph` answers inside `wait_s` (else the machine's `gpu_wait_ms`) with the ticket, and the
   ticket goes on after the call returns; `run-graph` on the CLI blocks until the job is done, prints on stderr where the ticket stands,
   and the first interrupt cancels a ticket that sent nothing (decision 9). A graph's tool text no longer says that it is offered once.
8. **ADR 0082 changes** in decision 8 (a graph that no node admits is a ticket and carries `cluster[]` in its answer), decision 9 (a
   graph that carries a `waiter_token` brings its place into the ticket), decision 11 (a graph is no longer outside the four
   amendments), and Amendment 1 rule 2 and its named limits.
9. **The ticket is its bytes.** A ticket may wait for hours and the caller's graph file is the caller's: reused for the next shot,
   edited, removed. So the ticket writes the graph and the manifest exactly as the plan holds them into its queue directory
   (`body-<id>-graph.json`, `body-<id>-manifest.json`), takes its key from those bytes, and the node's payload, the local run, the
   need and an adopter all read them: whichever lane serves the ticket renders the same graph, and nothing the caller does to its own
   file afterwards changes it. They are the one request body the store keeps (an image's lives in the owning process), and like every
   file of the store they are owner-only (0600 on POSIX; the directory is 0700 where the queue creates it), because a graph carries its
   prompt text. They go with
   the ticket (finished, canceled, swept after an owner that died before sending, the loser of a create race), stay while a dead
   owner's placed ticket can still be adopted, and are never swept while a ticket may still be being made (younger than a minute).
   The ticket also stores a **path key** (the same paths and parameters, no file read): a `waiter_token` is accepted for a request
   with the same content or the same path key, so a result stays collectable by the token it was promised under after the file is
   gone or overwritten, and the call that names it adopts a placed job from the copies. A repeat WITHOUT a token attaches by content
   alone: a file overwritten with the next shot is a new job, and it does not inherit the arrival of an abandoned ticket it only
   shares paths with.

**Consequences.** A graph call that finds every lane busy no longer comes back as the local lane's own queued answer with a `tk-`
token the caller must re-send; it comes back with a ticket and does not loop, as an image does. A caller that obeyed the old text and
re-sends with its `tk-` token still gets a ticket, bringing its place. The one-shot path added `cluster[]` to the deferral the local
run ended in (a graph that, after its wait, failed here with a typed code); the final result of a ticket is the lane's own, as an image
ticket's is, and the per-node reasons live in the queued answers. A node that lacks a file is asked once per ticket per 5
minutes instead of being hidden for 5 s from everything. Every waiting graph ticket costs what an image ticket costs: one lane
question and one roster read per scan (a local-only ticket reads no roster). The graph is read once, when the ticket is made, and the
ticket keeps that copy (rule 9), so a file overwritten or removed meanwhile changes nothing and a repeat of the overwritten file is
another ticket. What the ticket does not keep is the input files a graph reads (rule 5): their bytes are no part of the key, so a
finished result nobody collected (kept up to six hours) answers a repeat of the same graph even when the caller has put other bytes
under the same input name; such a caller sets another `out_dir` (part of the key) or collects first. A waiting graph ticket's place
is the whole-node token, so every call that arrives after it queues behind it on this machine's own lane, an image call whose own
card is idle included (it can still go to an idle node), until the ticket runs, is canceled or ends (8 h at most); an image
ticket's place names its cards, and before this amendment a graph's place lapsed about ten minutes after its caller stopped
polling. That is the seniority the operator asked for and also what it costs. A lane that could not be read is a ticket, not a graph
that waits in the pipeline with a `tk-` token. A 0.180.0 process that is still running keeps the one-shot path until it restarts: it writes a graph call's claim (kind `graph`,
decision 6), which this release's scanners read and honour, and it reads this release's graph tickets as it reads any ticket (their
claims, and their place in the line), so a node is not sent two jobs across the two releases. Nothing here was run against a live lease, a live node or a render;
the live acceptance in the CHANGELOG entry of this change (A9) is the first live evidence.

## Related code

- `internal/mediaremote/ticket.go`, `queuefs.go`, `scan.go`, `door.go`, `place.go` (the placement both kinds of ticket share: the roster, the filter, the attempt and its refusals), `graph.go` (a graph's rules and the note for one that reads input files), `mediaremote.go` (`RunWith`), `batch.go` (`RunBatch`, decision 13)
- `internal/core/place.go` (`PlaceKeeper`), `internal/pipeline/medialane_place.go`, `medialane_graph.go` (`leaveGraphPlace`), `medialane.go`, `internal/gpualloc/queued.go`, `internal/fleetnode/graph_files.go` (the refusal word)
- `internal/mcpserver/mcpserver.go` (`wait_s`, the place forwarding), `status_section.go` (`media_queue`), `main.go` and `generate_image_batch.go` (`generate-image`, the batch verb, `run-graph`),
  `internal/pipeline/pipeline.go` (`RunImageBatchAs`, `NormalizeImageBatch`)

## Related docs

- [ADR 0082](0082-a-media-call-overflows-to-the-fleet.md), [ADR 0077](0077-a-fleet-token-holder-may-send-one-media-job-with-its-input-files.md), [ADR 0039](0039-a-held-card-is-a-place-in-line.md)
- [Media generation](../../systems/media-generation.md), [GPU lease](../../systems/gpu-lease.md), [Operator guide](../../OPERATOR-GUIDE.md)
