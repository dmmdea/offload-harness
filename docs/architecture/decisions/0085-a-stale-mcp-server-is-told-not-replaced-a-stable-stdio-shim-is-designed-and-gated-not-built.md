---
status: Proposed
date: "2026-10-13"
---

# A stale MCP server is told, not replaced: a stable stdio shim is designed and gated, not built yet

## Context

An MCP server over stdio is spawned once per client and lives as long as the client does. A process keeps the executable
image it started from, so a rename-swap deploy replaces the installed file while every server already running goes on
running the old one. On the reference fleet 13 of 18 MCP processes were older than the deployed build, and one day's 24
lost subtasks were traced to delegators that predated the fix that would have saved them (0.184.0, D-36).

Detection has shipped in steps. 0.184.0: the `process` block of `offload_status`, `gpu doctor`'s older-than-installed
marking, and a `stale_binary` string on three doors (`agent_delegate`, `offload_research`, `offload_status`). This change:
the same note as a second text block on the result of every other door, on every call; a read of the installed image that
cannot block a rename of it; and a line in the operator's deploy tooling that lists the MCP servers still on an older
image after each Windows leg. None of that replaces a stale server. It tells the session, which then reconnects itself.

The server cannot replace itself. It must not exit or re-exec during a call (that would cut the jobs it holds), and on
Windows there is no exec: a new process id breaks the stdio pipe the client holds. Per the Claude Code MCP documentation
as read when this was written, a stdio server is not reconnected by the client on its own (`/mcp`, Reconnect, does it by
hand), while a remote HTTP or SSE server is reconnected automatically (up to five attempts, exponential backoff from one
second).

The question: should the harness build a stable shim, a frozen process the client spawns once that spawns and swaps the
real server behind it, so that a deploy reaches a running session with no reconnect?

## Decision

**Not built yet.** The shim is XL, not one item: it is a fleet-wide single point of failure that by definition cannot be
hot-fixed, and proving it needs live Claude Code (editor and terminal) and Codex, a fake-worker conformance suite and
Windows job-object tests. What ships now is the notice and the report; the session reconnects itself.

**The gate for building anything further.** Build the next option only if, after the notice on every door and the deploy
report have been in the field for two consecutive deploys, the deploy records still list sessions on an old image for more
than a day despite the notice. Then compare the two options below in this order: the loopback HTTP endpoint first (it needs
no frozen binary, the client already reconnects it), the shim second.

## What Windows does with a running image (the facts any design here is bound by)

- A running executable can be renamed but not deleted or overwritten while a process runs from it. node-swap renames the
  installed file aside (`<exe>.bak-<stamp>`) and puts the new file at the path.
- A process's image follows the rename. After a swap the older processes report the renamed backup's path as their image
  (`gpu doctor` lists them at the `.bak-` path). A lookup of "who holds the installed path" by image path, which is how
  node-swap diagnoses a blocked rename, finds only the new processes. A stale server is invisible to it and the answer is
  "no process holds it", while the server holds the file under another name.
- A rename or a delete of a file needs `FILE_SHARE_DELETE` on every handle already open against it, and Go's `os.Open` on
  Windows grants only `FILE_SHARE_READ|FILE_SHARE_WRITE` (`src/syscall/syscall_windows.go`, go1.27.2). So any plain read of
  the installed image or a backup of it (a hash, a build-marker read, a shim's stat-and-read of the worker image) can make a
  rename fail on a sharing violation. This change reads the installed image through `gpulease.OpenShared` (`CreateFile` with
  share-delete), and `internal/gpulease/openshared_test.go` shows the rename succeeding with it and failing without it.
  A shim must open every file it reads that way, and must never hold the worker image open without delete sharing.
- The shim's own image is the one file no deploy may replace while any client runs it. It needs its own install path, apart
  from the swapped one, and it updates only by a client restart.
- A job object with kill-on-close ends every worker when the shim dies. Without it a crashed shim orphans workers that hold
  cards and leases; with it a swap must put the new worker in the job before it takes a request.
- A deploy tool that selects MCP servers by command line (the operator's script matches ` mcp( |$)`) would not match a worker
  named `mcp-worker`, while node-swap's substring rule (` mcp`) would. Both have to change together if a worker is renamed.

## The options

1. **Status quo plus the notice and the report (decided, shipped).** The session is told on its next call and the operator
   is told which sessions are stranded. Costs nothing at run time. Its weakness is the one the gate watches: a session that
   never calls a tool, or a person who ignores the block, stays on the old image.
2. **A loopback streamable-HTTP MCP endpoint.** The client reconnects a remote server by itself, so a deploy that restarts
   the endpoint reaches every session. Costs to settle before it is a candidate: per-session identity (today
   `CLAUDE_CODE_SESSION_ID` reaches a stdio server through its environment and the session registry keys on the process id;
   over HTTP it becomes a header, or is lost), per-process state that is "one process, one tenant" today (the tenant, the ask
   cache, quarantine, pins, accelerator sidecars), the loopback-and-auth rule of [ADR 0005](0005-loopback-only-serve.md), and
   a listener to bind under the port discipline. A restart of the endpoint also ends the calls in flight unless it drains.
3. **A frozen stdio shim.** A separate tiny binary (its own package, no imports from `internal/`), registered in the client
   configuration instead of `local-offload mcp`. It spawns the real server as a worker and proxies newline-delimited
   JSON-RPC; records the client's `initialize` request and `initialized` notification; before each request stats the worker
   path and, when the file moved and its bytes differ, spawns a new worker, replays `initialize` with the recorded params,
   requires the same negotiated protocol version, compares capabilities and `tools/list` (sending
   `notifications/tools/list_changed` when they differ) and on any failure keeps the old worker and says so on stderr.
   New requests go to the new worker; the old one lives until its in-flight calls end (an id-to-worker map routes results and
   `notifications/cancelled`; progress tokens are client-chosen, so outbound notifications merge under one output lock), then
   its stdin is closed. Workers sit in a job object with kill-on-close, stdin end-of-file propagates, and every file is
   opened with share-delete. The server sends only progress notifications and issues no sampling, roots or elicitation
   requests, which removes server-to-client request routing from the problem.

   Hazards that live in the server and not in the proxy: per-process state is lost or duplicated at a swap (the tenant, the
   ask cache, quarantine, pins, accelerator sidecars); the in-process GPU lease holder is the worker, so the old worker must
   live until its render ends, possibly hours; and one session then has two registered owner entries for a while (supported by
   the registry, but it doubles every count that reads it). The version the client shows stays the first handshake's.

## Acceptance matrix (for whichever of 2 and 3 is built)

| Case | Passes when |
|---|---|
| Swap while idle | the next request is served by the new image, the client saw no error and no reconnect |
| Swap during a multi-hour call | the call finishes on the old worker, the next call runs on the new one, and neither worker is killed |
| Worker crashes | the next request starts a worker, or fails loudly with the reason; nothing hangs |
| New worker fails its handshake | the old worker keeps serving and the failure is named once |
| Client cancels during a swap | the cancellation reaches the worker that holds the call |
| Client closes stdin | every worker ends, including one that is draining, and the shim's job closes |
| Rename-swap while a worker drains | the rename succeeds (no handle without share-delete), and a rollback can follow it |

## Consequences

The stale server is told on every call, by the same note the status door carries, and the operator is told which servers are
stranded. Nothing is interrupted and nothing is killed. The cost of leaving the gap open is bounded by the gate above: the
deploy records show whether the notice is enough.

A server that started before this change has no middleware, so the notice reaches it only after a reconnect; the first
real proof needs two deploys (one to put the middleware on a running session, one to see it speak).

## Alternatives considered

The shim and the HTTP endpoint above. A watcher that restarts MCP servers after a deploy was not considered: the harness does
not kill sessions' processes on its own, and the deploy script never stops another session's server.

## Related code

- `internal/mcpserver/stale_notice.go`, `internal/mcpserver/stalebinary.go`, `internal/mcpserver/stale_image.go`
- `internal/gpulease/openshared_windows.go`, `internal/gpulease/audit.go`
- `internal/nodeswap/nodeswap.go` (the holder diagnosis that cannot see a stale server)

## Related docs

- [mcp-server.md](../../systems/mcp-server.md) (the stale-binary bullet and the rollout-counting rule)
- [gpu-lease.md](../../systems/gpu-lease.md) (the older-than-installed marking)
- [ADR 0005](0005-loopback-only-serve.md), [ADR 0018](0018-machine-wide-fenced-gpu-lease.md)
