# Fleet job lifecycle

## Purpose

How a job travels from a dispatcher's decision to a finished result on one node — and, just as
importantly, what happens when the dispatcher asks twice.

## Trigger

`POST /fleet/dispatch` from a compute-fleet dispatcher that has read this node's
`GET /fleet/health` and decided the job fits.

## Participants

The dispatcher (separate repository), the node's HTTP server, the job store, and the media generation
stack that actually runs the work.

## Step-by-step flow

1. **The dispatcher reads health.** Node identity, GPU vendor and architecture, live total and free
   VRAM, supported task types, loadable model families, measured footprints, and queue depth. If the
   VRAM snapshot is missing or older than 30 seconds, health answers **503** and the dispatcher looks
   elsewhere — refusing to answer beats answering with numbers a placement decision would be built on.

2. **The dispatcher posts a job.** The envelope is parsed strictly: unknown fields are rejected, the
   body is size-capped, and a wrong content type is a `400`. Several contract-reserved fields are
   accepted and ignored so the contract can grow without lockstep node changes.

3. **The node acks `202`** with the job id and `status: accepted`, then runs the work asynchronously.
   The ack means "mine now", not "done".

4. **State advances** `accepted` → `running` → `done` | `error`. Terminal states are write-once; a
   late completion cannot overwrite a finished job.

5. **The dispatcher polls** `GET /fleet/jobs/{id}` and reads `{ job_id, state, data?, error? }`. The
   field is `state`, not `status`. An unknown or evicted id is a `404`.

6. **A successful render also records a footprint observation**, so the node's advertised costs
   improve with use.

## Duplicate dispatch

This is the part with fleet-wide consequences, and the asymmetry is deliberate.

| Existing state | Response | Why |
|---|---|---|
| `accepted` | `202` re-ack | Already mine; do not schedule a second copy |
| `running` | `202` re-ack | Same |
| `done` | `202` re-ack | Same — see below |
| `error` | `409` | I tried and failed; another node legitimately should try |

A duplicate never starts a second run — acceptance is guarded so exactly one render happens per id.

The reasoning behind `done` → `202`: the dispatcher treats any non-`202` as a refusal and may
re-dispatch elsewhere. If a completed job answered non-`202`, the dispatcher would buy a duplicate
render somewhere else in the fleet — paying twice for work already finished. A *failed* job answering
`409` is an explicit refusal, which is exactly the signal that should cause a retry elsewhere.

Two edges worth knowing: a duplicate refusal cleans up the second request's materialized temp files
before responding, and if acceptance is refused while the job store reports nothing, drain has begun
and the node answers `503`.

> After terminal-state TTL eviction, a re-dispatched id looks new and **will re-render**. Documented
> and accepted.

## Data and state changes

Jobs live in memory with TTL eviction swept by a periodic janitor. `queue_depth` counts only
non-terminal jobs. Footprints persist to disk atomically. Outputs land wherever the job specified.

## Success behavior

`GET /fleet/jobs/{id}` returns `state: "done"` with the result payload. Exactly one render occurred,
however many times the job was dispatched.

## Failure behavior

`state: "error"` with a message, and subsequent duplicate dispatches for that id answer `409`. A
Defer from the underlying work is carried through as job data — a deferred render is a completed job
with a deferred result, not a job error.

Drain marks non-terminal survivors as `error: "interrupted"`, and draining happens *before* the
listener closes so pollers can still read final state.

## External dependencies

`nvidia-smi` for live capacity, the media stack for execution, and a trusted network between
dispatcher and node.

## Invariants and assumptions

1. Exactly one render per job id, per node, within the TTL window.
2. Terminal states are write-once.
3. Only `error` answers non-`202` on duplicate dispatch.
4. Health answers 503 rather than serving a stale snapshot.
5. The node refuses to start without a working GPU probe.

## Security and privacy notes

The contract is unauthenticated and assumes a trusted network. Binding beyond loopback requires
`--listen-trusted-network`, and `:18811` with an empty host is refused as non-loopback
([ADR 0005](../architecture/decisions/0005-loopback-only-serve.md)). Node identity defaults to the
hostname — worth knowing before exposing a node on a shared network.

## Observability and debugging

`curl <node>/fleet/health` confirms the node is serving with fresh numbers. A `503` there is almost
always a stalled sampler, not a dead node. When a job is missing, check whether its id was evicted
(`404`) before assuming it was never received.

## Testing notes

`internal/fleetnode/server_test.go` covers the health golden shape and both 503 paths, the dispatch
rejection matrix, and both duplicate cases — including an assertion that the runner executed exactly
once. `jobs_test.go` covers the state machine, concurrent accept, eviction, and drain.

## Source map

- [`internal/fleetnode/server.go`](../../internal/fleetnode/server.go) — routes and duplicate
  semantics
- [`internal/fleetnode/jobs.go`](../../internal/fleetnode/jobs.go) — state machine, drain, eviction
- [`main.go`](../../main.go) — `fleet-serve` startup, GPU probe, drain

## Related docs

- [../systems/fleet-node.md](../systems/fleet-node.md)
- [../FLEET-NODE.md](../FLEET-NODE.md) — operator guide
- [../architecture/decisions/0008-pdh-primary-vram-sampling.md](../architecture/decisions/0008-pdh-primary-vram-sampling.md)
