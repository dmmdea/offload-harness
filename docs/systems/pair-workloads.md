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

## Source map

| Path | Role |
|---|---|
| `internal/pairworkloads/pairworkloads.go` | the emitter: PAIR identity from `node-id.json` / `cluster/members.json`, `EngineFor`, `MethodFor`, frame building, `Send` / `Emit`, the ledger observer (`AttachLedger`, `FromLedger`) |
| `internal/delegate/pairevents.go` | `pairInflight` / `pairTerminal`: the delegation frames and the card identity pinned on `PlacedResult` |
| `internal/delegate/run.go` | the call sites: `runRemote` (queued; running from the poll loop), `runLocal` (queued; running through `pairStartGate`), `attempt().finish` (terminal); `runner.pair` |
| `gpu_leasecard.go` | the lease card: `leaseCardIdentity`, `newLeaseCard`, `running`, `finish`; wired into `runGPUReserve` (`gpu_cmd.go`) |
| `internal/core/workmark.go` | `WithWorkingMark` / `MarkWorking`: the lane's "my work started" signal a call card turns running on |
| `internal/pairworkloads/calls.go` | running cards for long tool calls: `Begin` (returns the call id), the per-task open-card queue the ledger observer `claim`s from (by call id first) |
| `internal/pipeline/pipeline.go` | `CallTracker`, `SetCallTracker`, the `Begin` at the top of `Run` (stamps the call id on `core.Meta`) |
| `internal/ledger/ledger.go` | `Ledger.Observe`, called after every durable `Record` |
| `internal/config/config.go` | `PairWorkloadsEnabled`, `PairWorkloadsEndpoint` |
| `main.go` | attaches the ledger observer where the CLI/MCP ledger is opened |
| `internal/pairworkloads/seatwatch.go` | the seat watcher (0.133.0): direct traffic on this box's vLLM seats as cards, run by fleet-serve |
| `internal/seatinflight/seatinflight.go` | the machine-wide register of the harness's own seat requests, written by `modelaffinity.Admit` and the fleet chat lane; the watcher subtracts it |
| `internal/pairworkloads/orphans.go` | the open-card register (0.133.1): one marker per in-flight card under `<state root>/pair-open/`, and the sweep that closes the cards of a dead process (`SweepOrphans`, `RunOrphanSweeper`) |
| `internal/pairworkloads/remote.go` | `RemoteCall`: the one card and the one asker ledger row of a call routed to a fleet node (source 5 below) |
| `internal/pairworkloads/wire.go` | the attribution headers an asker sends (`SetWireHeaders`, `WireHeadersFor`), `AskerName`, and `NodeName` (the dispatch host a node card is reported under) |
| `internal/core/remoteattr.go` | `RemoteAttribution` / `RemoteAttributor`: the seam the remote lanes report through; `*pipeline.Pipeline` implements it (`internal/pipeline/remoteattr.go`) |
| `internal/fleetnode/nodecard.go` | the serving node's own card for a job whose asker will not card it (*The node's fallback card* below) |
| `internal/pairworkloads/pairworkloads_test.go`, `internal/delegate/pair_events_test.go`, `internal/pairworkloads/seatwatch_test.go`, `internal/pairworkloads/orphans_test.go` | the contract tests |
| `internal/pairworkloads/attribution_test.go`, `internal/pairworkloads/remote_test.go`, `internal/{composeremote,visionremote,textremote}/attribution_test.go`, `internal/fleetnode/attribution_test.go`, `internal/delegate/attribution_test.go` | the remote-call attribution tests: one card per call, the headers, the node card, view-only resolution |

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
| `state` | `queued` → `workload:submitted`, `running` → `workload:started`, `completed` → `workload:completed`, `failed` → `workload:errored` |
| `originatedFrom` | this box's PAIR UUID, read from PAIR's `node-id.json` |
| `scheduledOn` | the PAIR UUID of the node the job runs on, resolved by name from PAIR's `cluster/members.json`: for a remote placement's in-flight frames the name is the **host of the dispatch URL** (the tailnet name in `delegate_remotes`, which is the hostname PAIR's members carry — never the fleet node id, which PAIR cannot resolve; 0.126.1), and for the terminal frame the name the node reported; the local UUID for local work. **Since 0.131.2 a node is resolved from every name it goes by** (`Event.NodeAliases`: dispatch host, fleet node id, wire name) against member names *and* member addresses; the emitter remembers per job what the in-flight frame resolved to, so the terminal frame keeps the card where the job ran; a remote run none of the names resolves is stamped `null` — PAIR draws no line and names no node — never the delegator (before 0.131.2 every terminal frame of a node that reports its fleet id re-pointed the card at the delegator). **A view-only node resolves too** (unreleased): the names are matched against `<appdir>/configs/view-only-nodes.json` — the list of nodes the desktop shows that are not cluster members, `[{name, address, port, nodeUuid}]` — by name or address, case-insensitively, AFTER the members, so a member always wins; the file is cached and reloaded exactly as `members.json` is, and a missing or malformed file only means no view-only nodes (256 cards of a view-only node read `scheduledOn` null before) **A local run whose config `endpoint` is another box's engine** (a bench config aimed at <node-c>'s arm) carries that endpoint's host as the name, so the card lands on the box whose engine did the work, never on the delegator's (register C-58; `modelaffinity.EndpointHost`) |
| `createdAt`, `startedAt`, `completedAt` | epoch ms; the last two `null` until known |
| `error` | the defer reason / wire error / first failed acceptance check, on `failed` only |
| `requesterId` | `offload-harness/<session>` (the session the ledger already stamps), or `offload-harness` |

Prompts, contexts and outputs are **never** part of the frame — PAIR's contract forbids them,
and the harness has nothing to gain by sending them.

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
   outcome. A call that wrote no row (a cache hit) is closed when `Run` returns (`closeCall`; a
   panic closes it failed). **A row names its call.** `Begin` returns the call id (the card's own
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
   auto-spilled `compose_video`, vision (`vqa`, `ocr`, `assess_image`, ...) or text (`classify`,
   `extract`) call never goes through `Pipeline.Run` on the asking box (`composeremote.Run`,
   `visionremote.Run` and `textremote.Run` send the job and poll it), so before this the asker wrote
   neither a ledger row nor a card for work it had spilled, and a plain row would have carded the
   asker itself (`ledger.Entry` named no node). The lanes now report through
   `core.RemoteAttribution`, which the `Runner` they receive (`*pipeline.Pipeline`) provides:
   - **One card per dispatched call**, on the node that serves it: `Event.Node` is the host of the
     dispatch URL (`pairworkloads.NodeName`, the same name a delegation reports under) and
     `NodeAliases` is the fleet node id from its health; `id` is the fleet job id; the engine is fixed
     when the card opens (`hyperframes` for a composition, otherwise the task's own); the model is the
     task until the node's result names the seat. `queued` when the call is dispatched, `running`
     when the node's job state turns `running`, then `completed`, or `failed` with the result's
     reason (a node that answered counts as started, so a finished card never reads "never started").
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
| `X-Offload-Asker` | the asker's PAIR member name when its emitter is enabled and the box is a member (lowercased), else its short lowercase hostname | every asker, on every request that creates work: delegation dispatch and queue submit, compose (template and project), vision, text, accelerator forwards |
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
| env `OFFLOAD_PAIR_APPDIR` | platform default | PAIR's app-data dir when it is not at `%LOCALAPPDATA%\Nvidia Corporation\Personal AI Router` (Windows) / `~/.config/Nvidia Corporation/Personal AI Router` (Linux); tests use it |

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
  `<state root>/pair-open/<pid>-<job id>.json` — the machine-wide root `seat-inflight/` and the GPU
  lease use — holding the frame's workloadInfo, the writer's pid, its process start identity and
  the **endpoint** (the ingress URL) the card was posted to.
  The terminal frame removes it once delivered. A terminal frame that could **not** be delivered
  (PAIR restarting, an answer slower than 2 s) replaces the marker as a *pending* terminal frame,
  which the next sweep resends as it is — the job's real verdict — without waiting for the
  producer to exit (dropping it would lose the only record of a card PAIR still shows running;
  keeping the in-flight marker would later close a finished job as `failed`). The marker is written atomically
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
  the marker and ends the pass — every later marker would fail the same way — and the next sweep
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
- **A remote call has no card on its asker**: the asker's emitter is not enabled (the node then cards it,
  as `Requested from fleet:<asker>`, only when the node's own emitter is enabled), or no node was chosen
  (a placement defer writes a row and no card), or the node is an older build that does not read the
  signal. A card with no node line: the dispatch host and the fleet node id are in neither
  `cluster/members.json` nor `configs/view-only-nodes.json`; the first failure logs one
  `pairworkloads: no PAIR member is named ...` line.
- **Nothing appears**: the key is off, PAIR is not installed (no `node-id.json`), or the
  stock worker is back after a PAIR update (`curl` returns connection refused on 14324).
  The first failed send logs one `pairworkloads:` line.

**Delivery before exit (0.132.8).** Frames go out on background goroutines; `delegate.RunWith` waits
for its emitter before returning and the one-shot CLI cleanup waits for the ledger emitter, so a
short-lived process never exits with a card's terminal frame still in flight.
