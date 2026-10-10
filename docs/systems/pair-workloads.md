# PAIR workloads — harness jobs in NVIDIA Personal AI Router's Jobs list

## Purpose

**Since 0.126.0.** Every job the harness runs or delegates can appear as a card in NVIDIA
Personal AI Router's (PAIR) Jobs list on every cluster member, and count in PAIR's scheduler as
pending work on the node that runs it. Off by default; two config keys turn it on.

## Questions this doc answers

- Why does PAIR not show harness work by itself, and what had to change on PAIR's side?
- What exactly does one workload frame carry, and where does each field come from?
- Which harness events produce frames, and why do delegations and plain tool calls use
  different sources?
- How is it enabled, on which boxes, and how is it verified or diagnosed?
- Why does a box whose harness runs as a different OS user than PAIR still get cards (the identity
  fallback), and when does it apply?
- How does a box that is not a PAIR member at all (a view-only box, a thin client) get cards (the card relay), and how is a
  relayed card kept from being closed by the wrong pid check?

## Source map

| Path | Role |
|---|---|
| `internal/pairworkloads/pairworkloads.go` | the emitter: PAIR identity from `node-id.json` / `cluster/members.json`, `EngineFor`, `MethodFor`, frame building, `Send` / `Emit` / `EmitSync` (the post that returns when it has been attempted), the ledger observer (`AttachLedger`, `FromLedger`) |
| `internal/delegate/pairevents.go` | `pairInflight` / `pairTerminal`: the delegation frames and the card identity pinned on `PlacedResult` |
| `internal/delegate/run.go` | the call sites: `runRemote` (queued; running from the poll loop), `runLocal` (queued; running through `pairStartGate`), `attempt().finish` (terminal); `runner.pair` |
| `gpu_leasecard.go` | the lease card: `leaseCardIdentity`, `newLeaseCard`, `running`, `finish`; wired into `runGPUReserve` (`gpu_cmd.go`) |
| `internal/core/workmark.go` | `WithWorkingMark` / `MarkWorking`: the lane's "my work started" signal a call card turns running on |
| `internal/pairworkloads/calls.go` | running cards for long tool calls: `Begin` (returns the call id), the per-task open-card queue the ledger observer `claim`s from (by call id first), and `CardOutcome`, the one place a finished call becomes a terminal state (*A held card is not a failure*) |
| `internal/pipeline/pipeline.go` | `CallTracker`, `SetCallTracker`, the `Begin` at the top of `Run` (stamps the call id on `core.Meta`), `closeCall` (the one deferred close: every return and a panic) |
| `internal/ledger/ledger.go` | `Ledger.Observe`, called after every durable `Record` |
| `internal/config/config.go` | `PairWorkloadsEnabled`, `PairWorkloadsEndpoint` |
| `main.go` | attaches the ledger observer where the CLI/MCP ledger is opened |
| `internal/pairworkloads/seatwatch.go` | the seat watcher (0.133.0): direct traffic on this box's vLLM seats as cards, run by fleet-serve |
| `internal/seatinflight/seatinflight.go` | the machine-wide register of the harness's own seat requests, written by `modelaffinity.Admit` and the fleet chat lane; the watcher subtracts it |
| `internal/pairworkloads/relay.go` | the card relay (D26): `RelayConfig`, relay mode of the emitter (`relayRoute`, `buildRelay`, `postRelay`, `Mode`, `LocalIdentity`) and the member's decode (`ParseRelay`, `RelayJobID`, `RelayRequester`, `RelayLimiter`) |
| `internal/fleetnode/pair_relay.go` | `POST /fleet/pair-relay`: the door (token gate, `PairRelayAdmissible`, asker, rate limit, body cap) |
| `internal/pairworkloads/nodeinfo.go` | the identity fallback: this node's UUID from PAIR's loopback node-info when `node-id.json` is missing or unreadable, gated on the ingress answering (*Identity when the harness user is not PAIR's user*) |
| `internal/pairworkloads/orphans.go` | the open-card register (0.133.1): one marker per in-flight card under `<state root>/pair-open/`, and the sweep that closes the cards of a dead process (`SweepOrphans`, `RunOrphanSweeper`); a terminal frame parks its verdict in the marker before it is posted (`parkTerminal`) |
| `internal/pairworkloads/remote.go` | `RemoteCall`: the one card and the one asker ledger row of a call routed to a fleet node (source 5 below) |
| `internal/pairworkloads/wire.go` | the attribution headers an asker sends (`SetWireHeaders`, `WireHeadersFor`), `AskerName`, and `NodeName` (the dispatch host a node card is reported under) |
| `internal/core/remoteattr.go` | `RemoteAttribution` / `RemoteAttributor`: the seam the remote lanes report through; `*pipeline.Pipeline` implements it (`internal/pipeline/remoteattr.go`) |
| `internal/fleetnode/nodecard.go` | the serving node's own card for a job whose asker will not card it (*The node's fallback card* below) |
| `internal/pairworkloads/relay_test.go`, `internal/fleetnode/pair_relay_test.go` | the relay's tests: the member's decode and resolution, the remote marker, the rate limit, relay mode, the route's token gate |
| `internal/pairworkloads/pairworkloads_test.go`, `internal/delegate/pair_events_test.go`, `internal/pairworkloads/seatwatch_test.go`, `internal/pairworkloads/orphans_test.go` | the contract tests |
| `internal/pairworkloads/attribution_test.go`, `internal/pairworkloads/remote_test.go`, `internal/{composeremote,visionremote,textremote,sttremote,mediaremote}/attribution_test.go`, `internal/fleetnode/attribution_test.go`, `internal/delegate/attribution_test.go` | the remote-call attribution tests: one card per call, the headers, the node card, view-only resolution |

## What problem this solves

PAIR's Jobs list is a stream of workload lifecycle frames that only PAIR's own two proxies
produce (its Ollama-compatible and LM Studio-compatible proxies). The harness routes around
those proxies — llama-swap on `:11436`, the fleet nodes over the tailnet — so nothing it ran was
visible in PAIR, and PAIR's scheduler could route its own traffic onto a card the harness was
already using. PAIR's workload manager now accepts the same frames on a **loopback ingress**
(our patch, see *The PAIR side* below); this package posts them.

## The contract (one frame per lifecycle transition)

`POST http://127.0.0.1:14324/v1/workloads/events`, a JSON-RPC 2.0 notification:

| Field (`params.workloadInfo`) | Value the harness sends |
|---|---|
| `id`, `runId` | the harness job id (`agd-…` for delegations; `led-<ts>-<n>` for ledger rows). Both the same, so PAIR's store key `(originatedFrom, engine, runId, id)` is unique per job |
| `model` | the seat / model tier (`qwen3.5-9b-agent`, `gemma-4-e4b`, …); for an NPU call the device (`coral-edgetpu`, `hailo-8l`, `rknpu`) — a forwarded call's ledger row is `<node>:<device>` and `FromLedger` splits it so the card runs on that node (`scheduledOn`) and shows the device |
| `engine` | the real engine, with the identifiers PAIR's upstream engine PRs use: `llamacpp`, `vllm` (seat name contains `vllm`, or — for a seat behind this box's own endpoint — a name the box declares in `vllm_seats` directly or through the alias the llama-swap roster resolves it to: <node-b>'s `agent-pool` is an alias of `qwen3.8-27b-vllm-3card`; `Emitter.LocalEngine`, 0.132.7. Remote placements keep the name-based label, since a node's aliases live in its own roster), `whispercpp` (transcribe / whisper seats), `comfyui` (image, video, audio generation and editing, `run_graph`), and the accelerator itself for an NPU call (`coral-edgetpu`, `hailo-8l`, `rknpu`) — never `llamacpp` for work no llama.cpp seat did |
| `state` | `queued` → `workload:submitted`, `running` → `workload:started`, `completed` → `workload:completed`, `failed` → `workload:errored`. These are PAIR's whole lifecycle: its workload manager defines no cancelled or skipped state, so a call that never ran closes `completed` or `failed` (*A held card is not a failure*) |
| `originatedFrom` | this box's PAIR UUID, read from PAIR's `node-id.json` |
| `scheduledOn` | the PAIR UUID of the node the job runs on, resolved by name from PAIR's `cluster/members.json`: for a remote placement's in-flight frames the name is the **host of the dispatch URL** (the tailnet name in `delegate_remotes`, which is the hostname PAIR's members carry — never the fleet node id, which PAIR cannot resolve; 0.126.1), and for the terminal frame the name the node reported; the local UUID for local work. **Since 0.131.2 a node is resolved from every name it goes by** (`Event.NodeAliases`: dispatch host, fleet node id, wire name) against member names *and* member addresses; the emitter remembers per job what the in-flight frame resolved to, so the terminal frame keeps the card where the job ran; a remote run none of the names resolves is stamped `null` — PAIR draws no line and names no node — never the delegator (before 0.131.2 every terminal frame of a node that reports its fleet id re-pointed the card at the delegator). **A view-only node resolves too** (unreleased): the names are matched against `<appdir>/configs/view-only-nodes.json` — the list of nodes the desktop shows that are not cluster members, `[{name, address, port, nodeUuid}]` — by name or address, case-insensitively, AFTER the members, so a member always wins; the file is cached and reloaded exactly as `members.json` is, and a missing or malformed file only means no view-only nodes (256 cards of a view-only node read `scheduledOn` null before) **A local run whose config `endpoint` is another box's engine** (a bench config aimed at <node-c>'s arm) carries that endpoint's host as the name, so the card lands on the box whose engine did the work, never on the delegator's (register C-58; `modelaffinity.EndpointHost`) |
| `createdAt`, `startedAt`, `completedAt` | epoch ms; the last two `null` until known |
| `error` | the defer reason / wire error / first failed acceptance check, on `failed`; and the reason a call was held back, on the quiet `completed` close of a call that never got its card (*A held card is not a failure*) |
| `requesterId` | `offload-harness/<session>` (the session the ledger already stamps), or `offload-harness` |

Prompts, contexts and outputs are **never** part of the frame — PAIR's contract forbids them,
and the harness has nothing to gain by sending them.

### A held card is not a failure (unreleased)

A media call that waited its window and found the card it needed held by another job does not fail:
it either leaves a place in line and answers with a `waiter_token` (`err_class` `gpu_queued`) or, from a
door that cannot resume, answers `gpu busy` (`gpu_busy`). A composition that waited its window for the
process's one compose slot and found another composition holding it (`compose_busy`; the slot is no
GPU card, a composition is CPU-class, but nothing ran and the answer says to call again) is the same
case. "A busy card is a place in line" (register C-89, plan P13). Until 2026-10-09 such a call's card
closed `failed`, red, with the reason; PAIR has no cancelled state to say "did not run", so the
harness uses the two it has:

| Call ended | Card closes | `startedAt` | `error` |
|---|---|---|---|
| succeeded | `completed` | when the lane held the engine | null |
| **held back by another job's hold on what it needed** (`core.CardHeld`: `err_class` `gpu_queued`, `gpu_busy` or `compose_busy`) and its lane never held the engine | `completed`, a quiet card | null (never started) | the defer reason |
| anything else that did not succeed (a render that broke, a configuration fault such as `gpu_lease_unavailable`, a deferral with no held class) | `failed`, red | when the lane held the engine, else null | the reason |
| the lane panicked (a remote lane included: `core.CloseOnPanic`) | `failed` | as above | `panic: <value>`, posted before the panic goes on |

`pairworkloads.CardOutcome` is the one function that decides this, and every writer of a call's closing
frame goes through it, so the card is the same whichever of them gets there first: `Begin`'s `end`, the
ledger row (`FromLedger`), `RemoteCall.Finish` and the node's fallback card (`nodeCard.finish`). The
class decides, never the reason text. A **remote** card (`RemoteCall`, `nodeCard`) turns `running` when
the node admits the job, which says nothing about the card, so its held close ignores the running mark
and keeps whatever start the card showed.

**The class has to reach the asker.** A call sent to a node closes its card on the asker's box, from the
`Result` the lane rebuilt from the node's poll. The text, vision and stt lanes return the whole `Result` in
the job's `data`, so they always carried the class. The media and compose lanes did not: a failed media job's
poll published `error` (the reason) alone, and the lane rebuilt the deferral with an empty class, so a node
whose card was held read, on its asker, as a red card that had started (found by the 2026-10-09 review of
this fix, reproduced against the real node server). The node now files the class with the failure: the run
closure returns a `classedError`, `Jobs.execute` stores it on the job record, and the poll publishes it as
`err_class` beside `error` (`fleetnode.JobView.ErrClass`, `jobWire.err_class`); `mediaremote` and
`composeremote` read it into `Meta.ErrClass`, so a deferral the node returned comes back to the caller with
the class it was filed under, as a local one always did. The field is additive and `omitempty`, like
`wall_sec` and `progress`: a failure with no class and a job that did not fail publish the keys they always
did, an asker that does not know the field ignores it, and a node that does not publish it leaves the class
empty, so its asker closes the card `failed` as it always did (update the node to get the quiet close).

Why `completed` and not `workloads:remove`, the one other thing a producer can say: removal is outside the
lifecycle the card relay carries (`ParseRelay` refuses it), so a relay-mode box could not use it, and it
would drop the reason; the fork's local ingress does accept it, so on a box that reports to a local PAIR it
remains an option. What `completed` costs, read from the fork's desktop source (the live dashboard was not
looked at): the desktop paints a `completed` card gray, prints `error` only on a `failed` card, and labels
every card that is not `running` "Ran on <node>" with a "Completed at" time. A held card therefore reads
as a call that ran and finished, with the reason not on its face; it did not run, and the frame, PAIR's
history, the harness ledger row (`gpu_queued`, `gpu_busy`, `compose_busy`) and the call's own reply all say
so. Whether a quiet `completed` card or no card is the better reading is the operator's decision; either
is this one function, plus the relay's method list for the second.

Where the rule reaches, and where it stops. A text or vision call opens no `Begin` card (PAIR keys a
card on its engine, and they learn it only as they run), so its only card is the ledger row's terminal
one, and a `gpu_busy` row of such a call (a vision call skipped because a render held the card) now
closes quiet through the same function; a door killed right after such a call loses that terminal-only
card instead of leaving one open, which was already so and is benign (no false red, no orphan). Not
switched, on purpose: the delegation card (`pairTerminal`) closes `failed` for any deferral. An
agent contract's deferral carries a `defer_class`, not an `err_class`, and a placement that could not
get a slot emits no card at all, so there is no held-card deferral on a delegation card to quiet. The
relay member's terminal path for a card another box opened (`pl.remote`) is parked before its post like
any other while this member still holds the card's in-flight marker in memory (the producer's frames
arrive within one member process); a member that restarted since the in-flight frame finds the marker by
name and writes the pending verdict only if the post fails, and a relayed card whose producer died closes
by its terminal relayed frame or the age cap, as before.

## The two sources

1. **Delegations** (`internal/delegate`, `pairevents.go`). A placement emits `queued` before
   the dispatch (remote) or when it is handed to the runner (local), and `running` only once the
   seat is WORKING on it (0.140.5, `seatWorking`): a streamed token, a decode, a tool call, a
   re-pack, or a prefill older than `pairPrefillGrace` (8 s; the seat probe names a load within
   5 s). Admission and cold load stay `queued`, so a card no longer reads "Running" over a seat
   that is still loading. A remote run reads it from the poll's `progress`, a local run from its
   own progress reports (`core.WithProgressReport`, `pairStartGate`); a run that reports no
   progress at all turns `running` after `pairNoProgressGrace` (30 s). A run that never worked
   closes with `startedAt` null. `attempt()`'s `finish` emits the terminal frame with the
   verdict the ledger row carries (a wire failure, a defer or a failed acceptance check is
   `failed`). The model and engine are **fixed at the first frame and carried on the
   `PlacedResult`** (`pairModel`, `pairEngine`, `pairCreated`, `pairStarted`): the node may end
   up on a different seat than the one intended, and two identities would be two cards, one
   stuck `running` until PAIR's staleness sweep failed it. A subtask that never placed
   (refused before placement, a capacity settle) emits nothing — it touched no card.
2. **Every other tool call** (`internal/pairworkloads.AttachLedger`, wired in `main.go` where
   the CLI/MCP ledger is opened). A `ledger.Ledger` observer (`Observe`) turns each written
   row into one terminal frame: `completedAt` = the row's timestamp, `createdAt` =
   `startedAt` = that minus the latency. Skipped on purpose: `agent_delegate` rows (source 1
   owns them), `agent` rows (this box serving someone else's delegation, already reported by
   the box that asked), cache hits (no GPU work), and **inner rows** (below).
3. **Long tool calls while they run** (0.140.5; queued-then-running 0.140.6,
   `internal/pairworkloads/calls.go`). A row is written when a call ends, so source 2 alone left
   a ten-minute render with no card until it finished. `Pipeline.Run` calls
   `Emitter.Begin(task, door)` (the pipeline's `CallTracker`, wired in `openPipeline`), which emits
   a `queued` frame (`id` = `call-<ms>-<n>`) for the tasks whose engine the task alone names: the
   `comfyui` lanes, `compose_video` (`hyperframes`) and `transcribe` (`whispercpp`). The card turns
   `running` when the lane marks its work started (`core.MarkWorking`): the media lanes do it the
   moment `acquireMediaLease` hands them the GPU, `compose_video` when it takes its slot. A render
   waiting behind another job's lease therefore reads "queued", not "Running". `transcribe` never
   marks (its wait is a whisper load inside llama-swap, which the lane cannot see), so its card
   stays queued until it ends. The call's own ledger row then closes THAT card (`claim`): same id,
   engine and creation, the start the mark recorded (none if it never started), the row's model and
   outcome. A call that wrote no row (a cache hit, or a ledger another process holds) is closed when
   `Run` returns (`closeCall`, deferred at the `Begin`, so no return can skip it; a panic closes it
   failed). **The close is on the wire before `Run` returns** (unreleased; the 2026-10-09 incident). A door
   answers the moment its call returns, and a client that opens a fresh stdio door per attempt closes
   stdin and kills the process right after the reply. The close used to be a background `Emit` (the
   row's, from the observer's goroutine; `end`'s own), so a killed door left the card's `queued`
   marker in the register and the orphan sweep closed the card "harness process exited before the job
   finished" for a call that had answered cleanly: two red cards on the dashboard for two attempts
   that each got a clean `gpu queued`. Now `end` posts its own close inline (`EmitSync`, bounded at
   2 s), and when the row claimed the card `end` waits (`awaitRowClose`, at most 4 s) for the observer's
   frame, posted inline on its own goroutine, to land. A PAIR that hangs on the POST therefore costs a
   call that bound once and never its answer, and the terminal verdict is already parked in the marker
   (below), so the sweep sends the true outcome even then. A hang EARLIER, in planning the frame (a
   relay's health probe, a cold identity read), is not covered: the in-flight marker is then all the
   register holds, and the sweep closes the card as an orphan. **A row names its call.** `Begin` returns the call id (the card's own
   job id), `Run` stamps it on `core.Meta.CallID`, the ledger row carries it as `call_id`, and
   `claim(task, callID)` closes exactly that card. Matching the oldest open card of the task
   instead (the rule before this change) let overlapping calls of one task trade cards: concurrent
   `transcribe` calls do not serialize on the media slot (transcribe never takes the GPU lease, and
   the whisper POSTs queue only inside one process), so a later call can finish first, and the
   shorter call's row closed the OLDER call's card, its own
   `End` then closed its own card with no row data, and when the older row finally landed the queue
   was empty and it opened a third card (11 surplus cards in the retained history, all on 2026-10-01).
   Now a row with a `call_id` whose card is not open (already
   closed, or another process's) claims nothing and gets its own card; it never closes someone
   else's. Only a row with no `call_id` (a writer that stamps none) falls back to first in, first
   out. `End` never closes a card its own row already closed, and closes its own card when the row
   never comes. Text and vision calls open nothing: PAIR keys a card on its engine, and they learn
   llamacpp vs vllm only as they run.

**Inner rows: one call is one card.** A call that writes several ledger rows names its own row
as the call and marks every other row `parent_job_id = <the call's job_id>` (the register C-62
inner-row rule, extended from `agent` rows to every multi-row call). `AttachLedger` skips any
row with a `parent_job_id`, so the call's own row is its one card; the job counters
(`ledger.JobRows`, `SummarizeFile`) already count the call's own row and skip its inner rows,
and an orphan inner row (the call's own row never landed) still counts as a call. Paths that
write several rows and how each follows the rule:

| Call | Rows | Call's own row | Inner rows |
|---|---|---|---|
| `video_watch` | one per window, plus one | the summary (or the all-deferred defer), `job_id` minted per call | every window row |
| `video_describe` | one per context-overflow retry (frame width halved), plus the last attempt | the attempt that ends the loop; the id is minted only when an attempt is retried, so the usual one-attempt call writes one plain row | each overflowed attempt that was retried |
| text cascade (`summarize`, `classify`, `extract`, `triage`) | an escalating attempt writes its own row (D-127), the climbed tier another, a final all-fail defer a third | the row of the tier that answered, or the final defer | each escalating attempt; minted on the first climb, so a call that never climbs writes one plain row |
| `extract_image` | the `ocr` and `extract` sub-calls | a row the composite writes for itself on every exit (task `extract_image`) | the two sub-call rows, marked through `core.Request.ParentJobID` (in-process only, never on the wire) |
| `inpaint_image` with `auto_text` | the vqa box-detector sub-call | the inpaint row, which every exit writes | the detector's vqa row |

Left alone on purpose: `RunImageBatch` writes one row per item by design (each item is its own
job), `agent_delegate` already follows the rule with its own `agd-` card, and every other media call
writes exactly one row. Token accounting follows the call's own row so no token is counted twice:
an inner row keeps the prompt tokens it processed as savings, and the call's own row carries only
what no inner row carries (the synthesis prompt, a cache-hit window's stored figure) plus the whole
output, with `cards_tokens` set to the work the cards actually did (an inner row records 0; a
cascade call's row adds the card work of its climbing attempts, carried in `core.Meta.CardsCarried`,
and an `extract_image` row adds the same carried work of its sub-calls; only the call's own row does,
never the entry tier's correctness-label snapshot, whose `cards_tokens` is the 0 it always carried). A
`video_watch` result still reports the whole call's tokens to the caller.
4. **Jobs under the GPU lease** (0.140.6, `gpu_leasecard.go`). `gpu reserve -- <cmd>` is how every
   bench, render and measurement runs on every node, and none of it reached PAIR: on 2026-09-23 the
   <node-a> ran a seat bench and a ComfyUI diagnostic and <node-c> a Wan 2.2 smoke render, each at
   84-100 % on its card, while the Jobs list showed only <node-b>'s delegations. The wrapper now
   owns one card (`id` = `lease-<ms>-<pid>`): `queued` from the moment it joins the lease queue and
   through the drain, `running` once the command starts, `completed` on exit 0 or `failed` with
   `exit status N` (plus `(interrupted)` / `(lease lost)`). The model is the harness verb
   (`generate-video`, on that verb's engine) or the script an interpreter runs (`seatbench.ps1`,
   `acestep_diag.py`) or the program (`llama-bench`), on the engine `gpu-lease`; the requester is
   `--origin` when given. A ONE-SHOT harness verb run directly under the lease
   (`gpu reserve -- local-offload generate-video …`) gets `OFFLOAD_PAIR_UNDER_LEASE=1`, which turns
   the emitter off in it (`FromConfig`), so it adds no second card (`silencesWrapped`). A shell, an
   interpreter or a long-running harness verb (`mcp`, `fleet-serve`, the documented
   `gpu reserve … -- <session>` form) never gets it: the flag would be inherited by everything
   that process starts, for as long as it runs, and silence a whole session. The
   `--detach` form holds a card for nobody's command and opens none.

5. **Calls routed to a fleet node** (unreleased, `internal/pairworkloads/remote.go`). A remote or
   auto-spilled `compose_video`, vision (`vqa`, `ocr`, `assess_image`, ...), text (`classify`,
   `extract`), media (`generate_image`, `generate_video`, `animate_character`, `generate_audio`, `run_graph`) or `transcribe` call (the card's engine is `whispercpp`) never goes through `Pipeline.Run` on the asking box (`composeremote.Run`,
   `visionremote.Run`, `textremote.Run`, `mediaremote.Run` and, since 0.164.0, `sttremote.Run` send the job and poll it), so before this the asker wrote
   neither a ledger row nor a card for work it had spilled, and a plain row would have carded the
   asker itself (`ledger.Entry` named no node). The lanes now report through
   `core.RemoteAttribution`, which the `Runner` they receive (`*pipeline.Pipeline`) provides:
   - **One card per dispatched call**, on the node that serves it: `Event.Node` is the host of the
     dispatch URL (`pairworkloads.NodeName`, the same name a delegation reports under) and
     `NodeAliases` is the fleet node id from its health; `id` is the fleet job id; the engine is fixed
     when the card opens (`hyperframes` for a composition, otherwise the task's own); the model is the
     task until the node's result names the seat. `queued` when the call is dispatched, `running`
     when the node's job state turns `running`, then `completed`, or `failed` with the result's
     reason (a node that answered counts as started, so a finished card never reads "never started";
     except a node that answered that another job held its card, which ran nothing and closes the card
     quiet, *A held card is not a failure*; for the media and compose lanes the class arrives on the
     node's poll, as `err_class`). The terminal frame is posted inline before the lane
     returns its result (`Finish` uses `EmitSync`): the door answers at once and may be killed right
     after, as for a local call. The asker's ledger row is recorded BEFORE that post (it is local file
     I/O, and the post can take its whole bound when PAIR hangs), so a door killed inside the bound loses
     the card's close, which the register has parked, and never the call's audit and savings row. Every
     lane defers `core.CloseOnPanic(h)` right after `BeginRemote` (stt, text and vision on both routes):
     a panic after the card opened (in the dispatch, the poll or the fetch) finishes the call as a deferred
     infrastructure failure carrying the panic's text, so the card closes `failed` and the row is written,
     and the panic goes on, as `closeCall` does for a local call. On a normal return the guard does
     nothing. On the auto route's fallback to a local run, `Discard` closes the
     attempt that reached a node the same way, so a PAIR that hangs delays the local run by the
     post's bound (a refused connection is instant).
   - **One asker ledger row** (door, route, placement, `node`, `node_id`, `fleet_job_id`, latency,
     the deferred and error fields), written for every remote call. It carries `card_by_caller`,
     which `AttachLedger` skips, so the observer never cards it a second time.
   - **No card when no node was chosen**: a placement defer before any dispatch (no eligible node, a
     bundle the node would refuse, no `delegate_remotes`) writes its row and opens nothing.
   - **The auto route falling back to the local seat stays as it was**: an attempt that reached no node
     leaves nothing behind (the local run writes its own row through the pipeline); one that did reach
     a node (refused at dispatch, poll failures) closes its card `failed` and keeps one deferred row.
   - The local route, and an auto call that runs here, never touch any of this.

   A `*pipeline.Pipeline` only opens the card when `SetPairEmitter` was called (`openPipeline` does); the
   row is written either way.

**Served work is the asker's card** (0.140.6): a fleet node stamps work it runs FOR another box
with the door `fleet` (`pairworkloads.FleetDoor`); `Begin` opens nothing for it and the ledger
observer skips its rows, exactly as it always skipped `agent` rows. That is what makes
`pair_workloads_enabled` safe on EVERY box, fleet nodes included: their own work (a CLI render
started over ssh, a lease job) reports, the delegations and dispatches they serve do not.

**The node's fallback card** (unreleased, `internal/fleetnode/nodecard.go`). "The asker's card" fails
when the asker reports nothing: a thin client, a scratch `install client` config, a box that is not a
PAIR member. Two request headers (`core.AskerHeader`, `core.PairCardHeader`, beside `X-Offload-Tenant`)
carry the attribution from the asker to the node:

| Header | Value | Sent by |
|---|---|---|
| `X-Offload-Asker` | the asker's PAIR member name when its emitter is enabled and the box is a member (lowercased), else its short lowercase hostname | every asker, on every request that creates work: delegation dispatch and queue submit, compose (template and project), media (dispatch and media-job), vision, text, accelerator forwards |
| `X-Offload-Pair-Card` | `node` | ONLY an asker whose emitter is not enabled (key off, PAIR not installed): nothing on that box will card the job |

The signal is inverted on purpose, for a staggered rollout: an older asker sends neither header, so it
keeps today's behaviour (it cards the job, or nobody does) and can never be carded twice. The node
reads both headers in `admit` (and, for a pulled job, from the queued job: `fleetqueue.Job.Asker` /
`PairCard`, stored by the holder's submit handler). The asker name is untrusted, so it is reduced to
printable text of at most 64 characters (`core.SanitizeAsker`) before it is used.

- **The asker's name is recorded whenever the header is present**: the node's ledger row carries it as
  `requester` (`core.Request.Requester` → `Meta.Requester` → `ledger.Entry.Requester`).
- **The card is emitted only on the signal.** From the node's own emitter (`fleetnode.Options.Pair`,
  `fleet-serve` builds it from the node's config, so an in-flight card of a killed `fleet-serve` still
  has an orphan marker and is closed): `queued` before the job is admitted (not after: the job can finish
  on its own worker before `Admit` returns, and a queued frame emitted after a terminal one would
  re-create the marker the terminal frame removed), `running` when the job starts, then `completed` or
  `failed` with the result's reason. A job dropped before it started (withdrawn, or the node drained)
  closes failed through `OnDropped`; a refused admission closes failed too. `id` is the fleet job id, the
  node is this box, the engine comes from the harness task (`EngineFor`), the model from the job (the
  result's model replaces the admission-time guess on the terminal frame), and the requester reads
  `offload-harness/fleet:<asker>`. A node whose emitter is not enabled drops the frames.
- **`AttachLedger` keeps skipping `door=fleet` rows**, so the node's card and its ledger row never
  double. A pulled (claim-loop) job now gets `door=fleet` exactly like a pushed one — it never had it, so
  the node's pipeline could card a long pulled job as its own work while its asker carded it as well.
- **A claimed job this node already holds opens no card.** A lease-expiry re-claim of its own job is a
  duplicate admission: the claim loop looks the id up first (as `handleDispatch` does), so a second
  `queued` frame can never reopen a card the terminal frame closed or regress a running one. A claim a
  draining node refuses closes its card `failed` ("node draining"); the lease requeues the job.
  The pushed path (`handleDispatch`) has the same guarantee against a **racing** duplicate: two
  dispatches of one id both pass the handler's first lookup, so the id is looked up again and the
  `queued` frame and `Admit` run under one lock (`Server.cardAdmitMu`). Of two racing duplicates one
  is admitted and carded; the other finds the winner's job, emits no frame (a `queued` frame cannot be
  recalled once posted, and one landing after the winner's terminal frame reopens its card) and gets the
  idempotent 202.
- **`fleet-serve` waits for the node emitter on shutdown** (`fleetServeDrain`: `DrainAndStop`, then
  `Emitter.Wait`, each post bounded at 2 s) on both ways out of `fleetServeAwait`: an interrupt, and a
  `Serve` that returns an error on its own (the listener failed; the process exits with that error and
  the same jobs in flight), so the terminal frames of the jobs that finished during the
  drain, and the failed frames of the ones it dropped, are posted before the process exits instead of
  being left to the next process's orphan sweep (which would close a completed card `failed`).

The emitter (`internal/pairworkloads.Emitter`) is fire-and-forget: a goroutine per frame with
a 2 s timeout, one warning per process on the first failure, nothing ever changes a harness
result. PAIR being absent (no `node-id.json`) or down is normal.

**Frames of one job are not ordered on the wire.** Each frame is posted on its own goroutine, so
a run that ends within milliseconds of its queued frame (a defer, a local run's error, a node's
refusal of the dispatch) can land its terminal frame first. A subtask refused before placement
sent no queued frame (source 1 above), so it has no order to lose. That is by design: the
open-card register's markers are written on the caller's goroutine, in frame order, and PAIR's
broker store and workload manager merge a job's frames by lifecycle rank (queued < running <
terminal) and drop a non-terminal frame for a card they already hold terminal, so the card
converges whatever the arrival order. A test of delegation frames therefore asserts the frames
present and the card they describe, never the position the scheduler happened to give them
(`TestFailedLocalPlacementReportsErrored` forces the terminal frame first, with an ingress that
holds the queued one, and asserts that it landed so; register C-77).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `pair_workloads_enabled` | `false` (`install client` seeds `true`; it is inert where PAIR's `node-id.json` is absent) | opt in. Enable on **every** box with PAIR installed (0.140.6). Work a fleet node serves for another box (`agent` rows, the `fleet` door) is skipped, so a job never shows twice; before 0.140.6 this was delegator-only, and every fleet node's own work was invisible |
| `pair_workloads_endpoint` | `http://127.0.0.1:14324/v1/workloads/events` | the ingress URL |
| `pair_seat_activity_enabled` | `false` | fleet-serve reports DIRECT traffic on this box's vLLM seats (see *Seat activity* below). Enable on every box that **serves** a vLLM seat; independent of `pair_workloads_enabled` |
| `pair_node_info_url` | `""` = `http://127.0.0.1:14318/v1/node-info`, except where `OFFLOAD_PAIR_APPDIR` is set | PAIR's loopback node-info, read for this node's UUID only when `node-id.json` is missing or unreadable (*Identity when the harness user is not PAIR's user*). Loopback only: any other host fails the config load naming the key |
| `pair_workloads_relay` | absent = `auto` | where this box's frames go when it has **no** PAIR identity (*The card relay*): `auto` / absent = every `delegate_remotes` base whose health advertises `pair_relay`; `"off"` = no relay; other entries = explicit member base URLs. Each entry is validated like `delegate_remotes`. The bearer is `fleet_auth_token` |
| env `OFFLOAD_PAIR_APPDIR` | platform default | PAIR's app-data dir when it is not at `%LOCALAPPDATA%\Nvidia Corporation\Personal AI Router` (Windows) / `~/.config/Nvidia Corporation/Personal AI Router` (Linux); tests use it |

## Identity when the harness user is not PAIR's user

The emitter's identity is PAIR's `node-id.json`, and PAIR rewrites that file `0600`. On a box where the
harness runs as a **different OS user** than PAIR (a small ARM node: the fleet node runs as its own
service user, PAIR as the logged-in one) the file is unreadable, or the harness user's own default app dir
does not exist, so the emitter stayed disabled forever although PAIR's worker was up and its ingress
answered. PAIR's own node-info service listens on loopback without a login and reports the same UUID as
`hostUuid` (fork `services/nvpair-node-info`, `GET /v1/node-info`, plaintext HTTP on `:14318`), so:

- **When it applies.** Only when `node-id.json` cannot be **read** (missing, or any read error such as a
  permission denial). A readable `node-id.json` is the primary path and never touches node-info or the
  ingress probe; a readable file that names no UUID is a broken PAIR, not a permission problem, and
  stays disabled as before.
- **What it reads.** `hostUuid` from node-info, with a 1 s timeout, no redirect followed, and accepted
  only as a canonical UUID (8-4-4-4-12 hex). `pair_node_info_url` overrides the URL and must be a
  loopback address (127.0.0.0/8, `::1`, `localhost`): any other host fails the config load naming the
  key, and the emitter refuses it again at run time.
- **Only while the ingress answers.** The fallback identity is accepted only when the configured
  ingress answers HTTP at all: one `POST {}` with `Content-Type: application/json`, which PAIR refuses
  with a 4xx (it is not a frame, so it opens no card); any HTTP answer, whatever its status, proves a
  listener. A box that has node-info but no ingress (a view-only node, or a PAIR whose worker has no
  ingress) therefore stays disabled instead of posting cards nobody receives.
- **Known cost.** The probes run inside the identity reload, which holds the identity lock: on a box
  where `:14318` is filtered (packets dropped, not refused) every identity reader waits the 1 s probe
  timeouts once per 60 s. A refused connection, the common case, fails at once.
- **Cached.** A successful node-info answer and a successful ingress probe are each trusted for 10 min,
  and the identity reload that asks is itself throttled to 60 s, so nothing is probed per call. A
  failed probe is retried on the 60 s reload. The first call on a cold process waits at most the two
  1 s timeouts; a refused connection (PAIR not installed, the common case) fails at once.
- **Members.** `cluster/members.json` is read exactly as on the primary path. Where it is unreadable
  only this node resolves: a card for any other node carries `scheduledOn` null, as for any node PAIR
  does not know.
- **The asker's wire headers follow the same identity** (`WireHeadersFor` builds its emitter with the
  same node-info URL): an asker enabled through the fallback does not ask the serving node to card the
  job, so one job is still one card.
- **The default URL is not applied where `OFFLOAD_PAIR_APPDIR` is set.** That variable names the PAIR
  data dir on purpose (a portable install, a test fixture), and a missing `node-id.json` there means
  "PAIR is not installed here", not "ask the default port": a test that points the app dir at a scratch
  directory can never reach a live PAIR. Name `pair_node_info_url` explicitly to use the fallback on
  such a box. An emitter built from a bare `pairworkloads.Config` (no `NodeInfoURL`) has no fallback.
- **A clustered node still answers.** PAIR's broker spawns node-info with `--node-id` and deliberately
  without `--cluster-dir` (fork `services/nvpair-ui-broker/broker.go`, `spawnNodeInfo`): it stays plain
  HTTP on the fixed `:14318` even when the node is a cluster member, reports the broker's own resolved
  UUID (the value in `node-id.json`), and gates callers only in the standalone `--cluster-dir` mode the
  broker does not enable. So the fallback works on a cluster member; a node-info started by hand with
  `--cluster-dir` would answer `403` and the emitter would stay disabled.

## The card relay: a box that is not a PAIR member (D26)

A box with no PAIR identity of its own (no readable `node-id.json`, and no node-info and ingress on loopback: a
view-only box, a thin client built by `install client` where PAIR is not installed) could never card its own work. The
operator's desktop showed that box's view-only card idle while its `gpu reserve` leases, CLI calls and fleet-served jobs
stayed invisible. A **relay** closes the gap with the same frames: a fleet-serve on a PAIR member posts them for it.

**The member's side** (`internal/fleetnode/pair_relay.go`, `pairworkloads.ParseRelay`):

- `POST /fleet/pair-relay` takes ONE workload lifecycle frame, exactly the JSON-RPC notification this package builds
  (`workload:submitted|started|completed|errored`, `params.workloadInfo` with the documented keys), plus an optional
  top-level `node` (a node name hint) and `node_aliases` (other names of the same node) **beside** the frame, never inside
  `workloadInfo`. The decode is strict: an unknown key at any level, a method that disagrees with the state, a bad type,
  a control character or a body over 64 KiB (`RelayBodyMax`) is refused (`400`, `413` for the size).
- The route is **token-gated** like every other gated lane (`tokenGated`; the bearer is checked before the body is read; a
  tokenless node beyond loopback answers `403`, a tokenless loopback node stays open) and **advertised in health as
  `pair_relay: true` exactly when it would admit** (`PairRelayAdmissible`: the node has a PAIR identity of its own, via
  `Emitter.LocalIdentity`, and the reachability rule holds). A node that itself reports through a relay never relays for
  another. The identity read behind the advertisement is the emitter's cached one (re-read at most once a minute; its cost
  is the node-info probes of *Identity when the harness user is not PAIR's user* on a box whose `node-id.json` is
  unreadable).
- The relaying box names itself in `X-Offload-Asker` (**required**, sanitized and bounded to 64 printable characters as
  H2's requester is). A token bucket per asker (5 frames/s, burst 60) and a global one (50/s, burst 200) answer `429` with
  `Retry-After` **before the body is read**; the asker name is a header the caller chooses, so the global bucket is what a
  rotating name meets. The rate bounds the calls, not the cards left open, so a second bound counts the open relayed cards:
  at most 128 per asker and 512 over all askers (`RelayOpenPerAsker`, `RelayOpenGlobal`). A frame that would open a NEW card
  past either cap is a `429` with `Retry-After`; the next frame of a card already open only refreshes it, and a terminal frame
  is always admitted and frees its slot (refusing it would strand the card it ends). An entry ages out after
  `RelayOpenMaxAge` (24 h), the age at which the member's sweep closes the marker it mirrors. The count is in memory, so a
  member restart empties it; the markers a restart leaves are the sweep's, so a flood is bounded by the caps plus what one
  restart forgets, never unbounded.
- What the member posts, through its own emitter (so its orphan register covers the in-flight card):

  | Field | Value |
  |---|---|
  | `id`, `runId` | `relay-<asker>-<id>` bounded to 160 characters, with 8 hex of a digest of the exact (asker, id) pair appended, so two different pairs never share a card whatever the concatenation or the bound |
  | `originatedFrom` | the member's own UUID. A value in the body is checked for shape and ignored: the relay never chooses an origin |
  | `requesterId` | `offload-harness/fleet:<asker>`, plus `/` and the relayed requester's own suffix (its session) when it has one, bounded |
  | `scheduledOn` | resolved **by the member**, by its own resolver (`members.json`, then `view-only-nodes.json`), from the `node` hint and its aliases; with no hint, from the asker's own name (a view-only box's name resolves there); otherwise `null`. Never the member itself for a job that did not run there. The body's `scheduledOn` is ignored |
  | the rest | `model`, `engine` (a lower-case name), `state`, the three timestamps and `error` as relayed, `error` cut to the card's one line |

- **A relayed in-flight marker belongs to a remote producer.** The member cannot see whether the producer's process, on
  another box, is alive, so its marker carries `pid` 0 and `remote: true`, is **never judged by the member's pid table**,
  and closes only by its terminal relayed frame or by the age cap `RelayOpenMaxAge` (24 h, the register's leak cap: a lease
  card legitimately runs for hours). The terminal frame finds the marker **by name** (`0-<job id>.remote`), so it still
  closes the card after the member restarted since the in-flight frame. A terminal frame PAIR could not take is kept as a
  pending frame of the same remote kind.
- **A relayed marker is not a `.json` file** (`0-<job id>.remote`, `remoteSuffix`). A harness built before the relay sweeps
  the same `pair-open/` directory on the same box (a long-lived MCP server or CLI the upgrade did not restart), reads every
  `.json` marker with `pid <= 0` as orphaned, and ignores the unknown `remote` field, so it would post `failed` for a relayed
  job that is still running. Those binaries skip every file that is not `.json`; this build's sweep reads both.
- **A late in-flight frame is ignored.** Frames of a job are not ordered on the wire (above), so a `queued` or `running` frame
  can reach the member after the job's terminal frame. PAIR drops it, but the member would write a fresh remote marker for it
  that nothing closes until the age cap and count it as an open card. The member's limiter remembers the cards a terminal
  frame closed for 10 minutes (`relayClosedTTL`, at most 4096) and answers a later in-flight frame of one `200` without posting
  it (a refusal would make the relaying box demote a healthy relay). A producer that reuses a job id after that window opens a
  card again. Known cost: a relaying box that dies leaves its card "Running" on the desktop until
  it is closed by the box's own sweep (below) or the cap.

**The relaying side** (`internal/pairworkloads/relay.go`):

- It is used **only when the emitter has no local identity** (the primary `node-id.json` and the node-info fallback both
  fail); a box with either keeps reporting to PAIR's loopback ingress and never relays. In relay mode `Enabled()` is true,
  and a frame is a relay body (the same workloadInfo keys with `originatedFrom` and `scheduledOn` null, plus the hint) sent to
  the **first healthy member** with the fleet bearer and `X-Offload-Asker` (this box's short name). An event with no node
  (this box's own work) sends this box's own name as the hint; a delegation's event sends the dispatch host and the fleet node
  id as the hint and its alias.
- `pair_workloads_relay` selects the members: absent or empty is `auto` (every `delegate_remotes` base whose `/fleet/health`
  advertises `pair_relay`, probed over `netguard.SafeTransport` with a 1 s bound, outside the identity lock; a member never
  probed is probed on the first call, which pays the bound once, and afterwards a verdict is answered at once and refreshed
  in the background when it is 60 s old (30 s after a failed probe), so no call waits on a member that went offline),
  `"auto"` says so outright, `"off"` turns the relay off, and any other entry is an
  explicit member base URL (a box with no `delegate_remotes` sets it by hand; explicit members are used without a probe). The
  bearer is `fleet_auth_token`. `install client` seeds nothing for it: `auto` covers a client that has `delegate_remotes`.
  The probe is a small health reader in this package, not `delegate.FetchNodeView`, because `internal/delegate` imports this
  package.
- **One job, one card.** `WireHeadersFor` stops sending `X-Offload-Pair-Card: node` in relay mode (the emitter is enabled),
  so the serving node does not card a job the asker cards through its relay.
- **The orphan register records the relay.** A relay marker's `endpoint` is the relay's **route URL** (`<base>/fleet/pair-relay`),
  and it keeps the node hint, so the relaying box's own sweeper closes only the cards it opened through a relay (H1's endpoint
  scoping keeps these apart from local-ingress markers), through that relay, as an `errored` relay frame carrying the hint.
  A job's terminal frame goes to the relay its in-flight frames went to, even when the first healthy member changed since.
  The pin is confirmed by the first post a relay takes: a job's first frame that FAILS on its relay drops the pin, so the
  next frame picks a relay not known to be down (a relay that never took a frame of the job holds no card), while a
  confirmed pin stays through a later failure (the card is open there; its terminal frame waits as a pending marker).
  A relay that fails a post goes behind the others for 30 s. A **terminal frame through a relay with no marker of its own**
  (its in-flight frames never went out) is kept as a pending frame when the relay does not take it, so the sweep delivers it
  once the relay answers instead of the card never appearing.
- **Overlong text is shortened, never refused.** The member's strict decode refuses a `model`, `requesterId` or id over 128
  bytes with a `400`, which the sender treats as permanent, so a lease card named for a long script file name would never
  exist. The relaying side cuts `model` and `requesterId` to 128 bytes on a rune boundary (non-printing characters become
  spaces) and an id longer than that to a prefix plus 8 hex of a digest of the whole id, the same on every frame of a job.
- **One dead relay does not stall the sweep.** A sweep pass that fails a post to one endpoint skips that endpoint's remaining
  markers for the pass and goes on to the markers of the other relays and of the local ingress; the skipped ones are retried
  by the next sweep (before this, the first failed post ended the pass for every marker behind it, until the 48 h give-up).
- `offload_status` has a `pair` block (absent unless `pair_workloads_enabled` is on): `mode` is `local ingress`,
  `node-info fallback`, `relay` (with the route URL) or `off` (with the reason).

Trust: a holder of the fleet token can make this member post a card named for any asker, on any node PAIR knows. The frame
carries no prompt, context or output, and the member checks its shape, namespaces its id, bounds its rate and the cards it leaves open; the token is
the same one that already lets its holder run renders and agent contracts on the node.

## Seat activity: traffic that bypasses the harness (0.133.0)

The two sources above only see work the harness does. A client that calls llama-swap
directly — a curl soak, opencode's own chat model, codex pointed at `:11436` — loaded the
cards for hours on 2026-09-22 with an empty Jobs list. With `pair_seat_activity_enabled`,
fleet-serve's **seat watcher** closes that gap:

- Every 2 s it reads llama-swap `/running` and, for each ready seat declared in `vllm_seats`, the
  seat's own `/metrics` at the `proxy` address `/running` reports for it: live load =
  `vllm:num_requests_running` + `vllm:num_requests_waiting`. Never `/upstream/<seat>/metrics`:
  llama-swap counts every `/upstream` request as activity, so a 2 s poll there kept every seat
  loaded past its 300 s idle unload. A llama-swap whose `/running` carries no `proxy` leaves the
  seat unread (no card), and so does a seat that binds 127.0.0.1 behind a llama-swap on another
  machine — the gauge is readable only on llama-swap's own box (`seatload.SeatURL`).
- It subtracts the harness's own requests on that seat. Every on-box admission
  (`modelaffinity.Admit`, i.e. every cascade, repack and agent-loop call) and every fleet
  chat-lane proxy writes one marker file under `<state root>/seat-inflight/` for as long as
  the request is held (`internal/seatinflight`); marker names are resolved through the
  llama-swap roster, so `agent-pool` counts against `qwen3.8-27b-vllm-3card`. **A harness job
  is never shown twice** — it already has its delegation or ledger card.
- What is left is direct traffic. Two agreeing polls open a card (`seat-<seat>-<ms>`, model =
  the seat, engine `vllm`, requester `llama-swap/direct`, on this box); two idle polls complete
  it at the last busy poll. The seat leaving `/running` with a card open fails it ("seat exited
  while serving direct traffic"), as do metrics unreadable for a minute. Stopping fleet-serve
  completes open cards.
- One card per busy **stretch** of a seat, not per request: a seat's gauge counts requests, it
  does not name them.
- vLLM seats only. The llama.cpp seats here are cascade rungs and the mem0 embedder, whose
  direct callers would flood the list.
- A marker whose process died is removed on the next read; any marker older than an hour is
  ignored. A leak can only hide direct traffic, never invent a card.
- **A poll that cannot attribute a marker reports nothing for that seat** — no card opens, and
  an open one neither closes nor fails. A marker names the alias the request used, so resolving
  it needs the roster; an unresolved alias would subtract zero and publish a harness request as
  direct traffic. The last roster read is kept across a failed refresh (aliases do not change
  while llama-swap is merely busy), so only a box whose roster never answered goes quiet.
- **The marker register is armed only where the key is on** (`config.Load`): `modelaffinity.Admit`
  is the gate every text call passes, and a box that runs no watcher must not pay a file create
  and remove per request. Set the key in the config **all** the box's harness processes read —
  the MCP servers write the markers, fleet-serve reads them.
- Marker removal retries in the background: on Windows the watcher's own read holds the file
  without delete sharing, and one dropped removal would hide that seat's direct traffic for an
  hour.

## Orphaned cards: a producer that dies mid-job (0.133.1)

A card opens with an in-flight frame and closes with the terminal frame from the **same
process**. A process killed in between (2026-09-22: a `local-offload delegate` CLI run killed by
its parent after its running frame) leaves the card "Running" until PAIR restarts: PAIR's workload
manager keeps local-ingress records with no expiry and re-asserts them on its anti-entropy
heartbeat, and the broker's staleness sweep exempts records of its own origin. Nothing on PAIR's
side can know the producer died, so the harness retires its own orphans:

- **Register.** Every in-flight frame (`queued`, `running`) writes one marker
  `<state root>/pair-open/<pid>-<job id>.json` (a relayed card's marker is `0-<job id>.remote`, *The card relay*) — the machine-wide root `seat-inflight/` and the GPU
  lease use — holding the frame's workloadInfo, the writer's pid, its process start identity and
  the **endpoint** (the ingress URL) the card was posted to.
  The terminal frame removes it once delivered. A terminal frame that could **not** be delivered
  (PAIR restarting, an answer slower than 2 s) replaces the marker as a *pending* terminal frame,
  which the next sweep resends as it is — the job's real verdict — without waiting for the
  producer to exit (dropping it would lose the only record of a card PAIR still shows running;
  keeping the in-flight marker would later close a finished job as `failed`). **The pending marker is
  written before the post, not after it** (unreleased; `track` → `parkTerminal`, on the caller's
  goroutine): a process killed while its terminal post is in flight, or before a background post has
  started, leaves the verdict itself in the register, and the sweep sends that instead of "harness
  process exited before the job finished" for a job whose outcome it knew. The one cost is a window of
  milliseconds in which a LIVE producer's marker is already pending: a sweeper that reads it then
  resends an identical terminal frame, which PAIR merges as an equal-rank no-op, and the producer's
  own removal of the marker after its post is idempotent. The other cost is one more marshal and
  atomic write (with Windows' bounded rename retry) on the caller's goroutine for every terminal frame
  that has an open marker: delegations, lease cards, node cards and seat-watch `closeAll` pay it too,
  not only the door path. The marker is written atomically
  (temp + rename) on the caller's goroutine, so a job's markers follow its frames in order; every
  error is swallowed (a marker that cannot be written only means a card that cannot be closed after
  a crash — never a failed or slowed job).
- **Sweep.** A marker is an orphan when its pid is dead (`gpulease.PIDAlive`), its pid now belongs
  to a different process (`gpulease.ProcessStart` differs), or it is older than 24 h
  (`OpenMaxAge`, the leak cap). The sweep sends the terminal frame the producer never sent — state
  `failed`, error "harness process exited before the job finished", the in-flight frame's id,
  origin, node, engine, requester and timestamps unchanged (the same card), `completedAt` = now —
  then deletes the marker.
- **Endpoint scoping.** A sweep closes only the markers of **its own endpoint**: the marker's
  `endpoint` and the sweeper's `pair_workloads_endpoint` must name the same ingress (compared as
  scheme, case-insensitive host, port — a scheme's default port is the same as none — and path). A
  marker with no `endpoint` was written before the field existed and counts as the default ingress
  (`http://127.0.0.1:14324/v1/workloads/events`). A foreign marker is left exactly as it is: not
  locked, not posted, not deleted, not even dropped at the age caps. Closing means "post to my
  endpoint, delete on success", so a sweeper that took another ingress's marker would close nothing
  real and destroy the only record of the card.
- **Who sweeps.** Every emitter once, on its first `Emit` (so every harness process that reports
  anything closes what a dead one left open; the process's `Wait` covers it), and fleet-serve
  every 45 s when `pair_workloads_enabled` or `pair_seat_activity_enabled` is on.
- **Racing sweepers.** A claim is an O_EXCL `<marker>.lock`; the winner re-checks the marker,
  posts, removes the marker, then the lock — so one frame per orphan. Rename-to-claim does not
  work on Windows: two sweepers that opened the marker before either renamed it both succeed. A
  post to an **unreachable** PAIR (transport error, HTTP 5xx, or any 4xx that does not judge the frame: 401, 403, 404, 405, 408, 429 ...) releases the lock, keeps
  the marker and skips every later marker of the same endpoint for the pass — they would fail the same way; other endpoints' markers go on — and the next sweep
  retries; a marker PAIR has not accepted for 48 h is dropped. A post PAIR **rejects** (HTTP 400, 413 or 422:
  it will never accept that frame; 401/403/404/405 describe the route, not the frame, so a PAIR
  mid-deploy or a wrong endpoint cannot make the sweeper delete every marker) drops that one marker and its lock, logs one line
  naming the job id and the status, and the pass goes on to the next marker; before this rule one
  rejected marker starved every later one until the 48 h give-up. The same rule applies to a live
  producer's terminal frame: rejected, it is dropped with a log line instead of being rewritten as a
  pending marker; unreachable, it stays pending. A marker whose closing frame cannot even be **built**
  (`sweepFrame` returns an error) is the same permanent verdict, because no PAIR can accept a frame that
  does not exist: the sweep logs the job id and the cause, drops the marker and its lock, and goes on,
  instead of reading it as an unreachable PAIR and ending the pass at that marker every sweep until the
  48 h give-up. A lock whose sweeper died, or older than 5 min, is
  removed by a later pass.
- **Seat-watch cards** go through the same `Emit`, so a fleet-serve killed with a direct-traffic
  card open leaves a marker the next sweep closes. A clean stop still completes open cards
  (`closeAll`).
- A disabled emitter (key off, or PAIR not installed) writes and sweeps nothing.
- **Tests must isolate their state root.** Every test that builds an **enabled** emitter (or runs
  anything that does: a lease card, a delegation) must give it its own state root (`StateDir` or
  `OpenDir` set to `t.TempDir()`, or `LOCAL_OFFLOAD_STATE_DIR` set by the package's `TestMain`),
  because an emitter with no root resolves to the machine-wide `pair-open` directory and its first
  sweep reads the operator's real markers. The lease-card test once did: it closed real orphan
  markers into its own httptest ingress and deleted them, leaving the real cards "Running" for 31.9 h
  (the endpoint scoping above now stops that second-hand, but the isolation is still the rule). The
  root, `internal/delegate`, `internal/mcpserver`, `internal/pipeline` and `internal/pairworkloads`
  packages each carry a `TestMain` that points the variable at a throwaway directory and fails closed:
  when that directory or the variable cannot be set up it exits 1 without running a test, instead of
  falling back to the machine-wide root.

## The PAIR side (what has to be true on the box)

PAIR 0.1.1's stock workload manager has no ingress for a third-party producer: its only
listener is the cluster-mTLS peer port `:14320`, pinned peers only. The patched worker adds
the loopback ingress:

- Source: fork `dmmdea/Personal-AI-Router`, branch `feat/workload-local-ingress`
  (`nvpair-workload-manager` 0.14.0; product 0.92.0). `main` mirrors upstream; the feature
  branch is the unit submitted upstream as a PR; `deploy` is upstream `main` with the feature
  branches rebased on top. An upstream release = fetch, rebase, rebuild, redeploy.
- Enabling: `<appdir>/workload-ingress.json` with `{"listen": "127.0.0.1:14324"}` (or the
  worker flag `--local-ingress`). A non-loopback address is refused at startup.
- Deploy on a Windows node: stage `nvpair-workload-manager.exe` + `swap-workload-manager.ps1`
  under `~/pair-stage`, run the script from an SSH session (elevated on these boxes): it
  renames the live worker aside as `.0.13.3.bak`, copies the new one in, kills the worker,
  and the broker's supervisor relaunches it — the desktop app never restarts. On a Linux node
  the binary lives in `/opt/PAIR/resources/cli-bin/` (root-owned; `sudo mv` + `cp`), and
  killing the worker lets the TUI-owned broker relaunch it.
- PAIR's own auto-updater overwrites the worker on an upstream release; re-run the swap.
- Port `14324/tcp` loopback is in every node's port file.

## Verifying

```bash
# 1. the ingress answers (200) and refuses a producer mistake (400)
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:14324/v1/workloads/events \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"check-1","model":"m","engine":"llamacpp","runId":"check-1","state":"running","createdAt":1,"startedAt":1,"completedAt":null,"error":null,"requesterId":"check"}}}'
# 2. the broker applied it (Jobs list + history) and peers received it
grep -c check-1 "$LOCALAPPDATA/NVIDIA Corporation/Personal AI Router/workloads-history.json"
# 3. clean up
curl -s -X POST http://127.0.0.1:14324/v1/workloads/events -d '{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"check-1"}}'
```

A real check is one `agent_delegate` from a fresh MCP session with the key on: the card
appears in PAIR's Jobs list on this box **and** on the node that ran it, reading
"Requested from <this box> / Ran on <node>" with the seat as the model name.

## Failure modes

- **Two cards for one job**: the in-flight and terminal frames named different
  models/engines. Source 1 pins the identity on the `PlacedResult` for exactly this reason;
  a new emit site must reuse `pairInflight` / `pairTerminal`, never build its own frame.
- **A card stuck `running`**: the terminal frame was never sent because the process died. PAIR
  does **not** clean this up for a local-ingress card (see *Orphaned cards* below); the harness's
  sweep closes it as `failed` within one fleet-serve sweep interval (45 s), or on the next emit of
  any harness process on the box. A card still stuck: check `<state root>/pair-open/` for its
  marker (`<pid>-<job id>.json`) and whether a harness process on the box has either key on.
- **A red card "harness process exited before the job finished" for a call that answered**: before
  this change (unreleased) a long call's close was a background post, and a client that kills its door the
  moment it has the reply took the close with it. Now the close is on the wire before the call returns
  and the verdict is parked before any post; seeing this on a current build means the process really
  was killed mid-call (a crash, a killed session), which is what the text says.
- **A held-card call shows `Completed`, not red**: that is *A held card is not a failure*. The reason
  is in the frame's `error` (PAIR's `workloads-history.json`), not on the card face; the harness
  ledger row says `gpu_queued`, `gpu_busy` or `compose_busy`.
- **A remote media or compose call whose node's card was held still shows red and started**: the
  node is older than this change and does not publish `err_class` on its poll, so its asker cannot tell
  a held card from a render that broke and closes the card `failed`. Update the node. The text, vision and
  stt lanes never had the gap.
- **A remote call has no card on its asker**: the asker's emitter is not enabled (the node then cards it,
  as `Requested from fleet:<asker>`, only when the node's own emitter is enabled), or no node was chosen
  (a placement defer writes a row and no card), or the node is an older build that does not read the
  signal. A card with no node line: the dispatch host and the fleet node id are in neither
  `cluster/members.json` nor `configs/view-only-nodes.json`; the first failure logs one
  `pairworkloads: no PAIR member is named ...` line.
- **A box with no PAIR gets no card**: `offload_status` `pair` says why (`off` with the reason). In `auto` no `delegate_remotes`
  member advertises `pair_relay` (it needs a PAIR identity of its own and a `fleet_auth_token`, or a loopback listener), or
  the box has no `delegate_remotes` and no explicit `pair_workloads_relay`. A card Running long after its box went away is
  a relayed card whose producer died: the relaying box's sweep closes it through the relay when that box comes back, and
  the member's age cap (24 h) otherwise.
- **Nothing appears**: the key is off, PAIR is not installed (no `node-id.json`, and on a box
  where the harness user is not PAIR's user, node-info and the ingress are not both answering on
  loopback: `curl http://127.0.0.1:14318/v1/node-info` must show a `hostUuid`), or the
  stock worker is back after a PAIR update (`curl` returns connection refused on 14324).
  The first failed send logs one `pairworkloads:` line.

**Delivery before exit (0.132.8).** Frames go out on background goroutines; `delegate.RunWith` waits
for its emitter before returning and the one-shot CLI cleanup waits for the ledger emitter, so a
short-lived process never exits with a card's terminal frame still in flight. That covered a process
that exits on its own; it did not cover a long-lived MCP door that a client kills right after a reply
(2026-10-09, unreleased): a long call's close, and a remote call's, are now posted inline before the call
returns (source 3 and 5), and the verdict is parked in the register before any post (*Orphaned cards*).
**A delegation's abandoned subtask** is the last card that could outlive its answer: a seat that ignores
its context is abandoned at the call deadline and the door answers a budget defer for it, but the
goroutine's own terminal frame is dropped once the run is shut. `RunWith` therefore keeps an account of
the cards it opened (`notePair`) and `shutPair` closes the ones still open, `failed`, under the
identity their in-flight frames named, with the deadline's words ("call deadline reached: the subtask
had not stopped when the call returned"), before `pair.Wait()` delivers them. Before this such a card
stayed `queued` or `running` until the door's process died and the orphan sweep closed it. The verdict
is the call's, not the node's: a subtask placed on a fleet node closes `failed` here while the node may
still be running the job (the published budget defer says as much: it cannot be recalled), exactly as a
cooperating subtask the deadline cancelled already did.
