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
| `internal/delegate/run.go` | the three call sites: `runRemote` (queued, running), `runLocal` (running), `attempt().finish` (terminal); `runner.pair` |
| `internal/ledger/ledger.go` | `Ledger.Observe`, called after every durable `Record` |
| `internal/config/config.go` | `PairWorkloadsEnabled`, `PairWorkloadsEndpoint` |
| `main.go` | attaches the ledger observer where the CLI/MCP ledger is opened |
| `internal/pairworkloads/seatwatch.go` | the seat watcher (0.133.0): direct traffic on this box's vLLM seats as cards, run by fleet-serve |
| `internal/seatinflight/seatinflight.go` | the machine-wide register of the harness's own seat requests, written by `modelaffinity.Admit` and the fleet chat lane; the watcher subtracts it |
| `internal/pairworkloads/orphans.go` | the open-card register (0.133.1): one marker per in-flight card under `<state root>/pair-open/`, and the sweep that closes the cards of a dead process (`SweepOrphans`, `RunOrphanSweeper`) |
| `internal/pairworkloads/pairworkloads_test.go`, `internal/delegate/pair_events_test.go`, `internal/pairworkloads/seatwatch_test.go`, `internal/pairworkloads/orphans_test.go` | the contract tests |

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
| `model` | the seat / model tier (`qwen3.5-9b-agent`, `gemma-4-e4b`, …); for an NPU call the device (`coral-edgetpu`, `hailo-8l`) — a forwarded call's ledger row is `<node>:<device>` and `FromLedger` splits it so the card runs on that node (`scheduledOn`) and shows the device |
| `engine` | the real engine, with the identifiers PAIR's upstream engine PRs use: `llamacpp`, `vllm` (seat name contains `vllm`, or — for a seat behind this box's own endpoint — a name the box declares in `vllm_seats` directly or through the alias the llama-swap roster resolves it to: the Qube's `agent-pool` is an alias of `qwen3.8-27b-vllm-3card`; `Emitter.LocalEngine`, 0.132.7. Remote placements keep the name-based label, since a node's aliases live in its own roster), `whispercpp` (transcribe / whisper seats), `comfyui` (image, video, audio generation and editing, `run_graph`), and the accelerator itself for an NPU call (`coral-edgetpu`, `hailo-8l`) — never `llamacpp` for work no llama.cpp seat did |
| `state` | `queued` → `workload:submitted`, `running` → `workload:started`, `completed` → `workload:completed`, `failed` → `workload:errored` |
| `originatedFrom` | this box's PAIR UUID, read from PAIR's `node-id.json` |
| `scheduledOn` | the PAIR UUID of the node the job runs on, resolved by name from PAIR's `cluster/members.json`: for a remote placement's in-flight frames the name is the **host of the dispatch URL** (the tailnet name in `delegate_remotes`, which is the hostname PAIR's members carry — never the fleet node id, which PAIR cannot resolve; 0.126.1), and for the terminal frame the name the node reported; the local UUID for local work. **Since 0.131.2 a node is resolved from every name it goes by** (`Event.NodeAliases`: dispatch host, fleet node id, wire name) against member names *and* member addresses; the emitter remembers per job what the in-flight frame resolved to, so the terminal frame keeps the card where the job ran; a remote run none of the names resolves is stamped `null` — PAIR draws no line and names no node — never the delegator (before 0.131.2 every terminal frame of a node that reports its fleet id re-pointed the card at the delegator). **A local run whose config `endpoint` is another box's engine** (a bench config aimed at the Lenovo's arm) carries that endpoint's host as the name, so the card lands on the box whose engine did the work, never on the delegator's (register C-58; `modelaffinity.EndpointHost`) |
| `createdAt`, `startedAt`, `completedAt` | epoch ms; the last two `null` until known |
| `error` | the defer reason / wire error / first failed acceptance check, on `failed` only |
| `requesterId` | `offload-harness/<session>` (the session the ledger already stamps), or `offload-harness` |

Prompts, contexts and outputs are **never** part of the frame — PAIR's contract forbids them,
and the harness has nothing to gain by sending them.

## The two sources

1. **Delegations** (`internal/delegate`, `pairevents.go`). A remote placement emits `queued`
   before the dispatch and `running` when the node acks; a local placement emits `running`
   when it is handed to the runner. `attempt()`'s `finish` emits the terminal frame with the
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
   the box that asked), cache hits (no GPU work).

The emitter (`internal/pairworkloads.Emitter`) is fire-and-forget: a goroutine per frame with
a 2 s timeout, one warning per process on the first failure, nothing ever changes a harness
result. PAIR being absent (no `node-id.json`) or down is normal.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `pair_workloads_enabled` | `false` | opt in. Enable on **delegator** boxes (the ones whose MCP sessions place work). A fleet-only node that also reported the delegation it serves would show the same job twice, once per origin |
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
  lease use — holding the frame's workloadInfo, the writer's pid and its process start identity.
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
- **Who sweeps.** Every emitter once, on its first `Emit` (so every harness process that reports
  anything closes what a dead one left open; the process's `Wait` covers it), and fleet-serve
  every 45 s when `pair_workloads_enabled` or `pair_seat_activity_enabled` is on.
- **Racing sweepers.** A claim is an O_EXCL `<marker>.lock`; the winner re-checks the marker,
  posts, removes the marker, then the lock — so one frame per orphan. Rename-to-claim does not
  work on Windows: two sweepers that opened the marker before either renamed it both succeed. A
  failed post (PAIR down) releases the lock and keeps the marker for the next sweep; a marker PAIR
  has not accepted for 48 h is dropped. A lock whose sweeper died, or older than 5 min, is removed
  by a later pass.
- **Seat-watch cards** go through the same `Emit`, so a fleet-serve killed with a direct-traffic
  card open leaves a marker the next sweep closes. A clean stop still completes open cards
  (`closeAll`).
- A disabled emitter (key off, or PAIR not installed) writes and sweeps nothing.

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
- **Nothing appears**: the key is off, PAIR is not installed (no `node-id.json`), or the
  stock worker is back after a PAIR update (`curl` returns connection refused on 14324).
  The first failed send logs one `pairworkloads:` line.

**Delivery before exit (0.132.8).** Frames go out on background goroutines; `delegate.RunWith` waits
for its emitter before returning and the one-shot CLI cleanup waits for the ledger emitter, so a
short-lived process never exits with a card's terminal frame still in flight.
