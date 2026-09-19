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
| `internal/pairworkloads/pairworkloads_test.go`, `internal/delegate/pair_events_test.go` | the contract tests |

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
| `model` | the seat / model tier (`qwen3.5-9b-agent`, `gemma-4-e4b`, …) |
| `engine` | the real engine, with the identifiers PAIR's upstream engine PRs use: `llamacpp`, `vllm` (seat name contains `vllm`), `whispercpp` (transcribe / whisper seats), `comfyui` (image, video, audio generation and editing, `run_graph`) |
| `state` | `queued` → `workload:submitted`, `running` → `workload:started`, `completed` → `workload:completed`, `failed` → `workload:errored` |
| `originatedFrom` | this box's PAIR UUID, read from PAIR's `node-id.json` |
| `scheduledOn` | the PAIR UUID of the node the job runs on, resolved by name from PAIR's `cluster/members.json`: for a remote placement's in-flight frames the name is the **host of the dispatch URL** (the tailnet name in `delegate_remotes`, which is the hostname PAIR's members carry — never the fleet node id, which PAIR cannot resolve; 0.126.1), and for the terminal frame the name the node reported; the local UUID for local work or an unknown name. **A local run whose config `endpoint` is another box's engine** (a bench config aimed at the Lenovo's arm) carries that endpoint's host as the name, so the card lands on the box whose engine did the work, never on the delegator's (register C-58; `modelaffinity.EndpointHost`) |
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
| env `OFFLOAD_PAIR_APPDIR` | platform default | PAIR's app-data dir when it is not at `%LOCALAPPDATA%\Nvidia Corporation\Personal AI Router` (Windows) / `~/.config/Nvidia Corporation/Personal AI Router` (Linux); tests use it |

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
- **A card stuck `running`**: the terminal frame was never sent (process died) — PAIR's
  staleness sweep fails it after its origin goes silent; nothing to do.
- **Nothing appears**: the key is off, PAIR is not installed (no `node-id.json`), or the
  stock worker is back after a PAIR update (`curl` returns connection refused on 14324).
  The first failed send logs one `pairworkloads:` line.
