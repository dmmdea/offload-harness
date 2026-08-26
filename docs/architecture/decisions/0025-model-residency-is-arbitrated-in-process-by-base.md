---
status: Accepted
date: "2026-08-26"
---

# Model residency is arbitrated in process, keyed on the resolved base

## Context

llama-swap serializes model residency: one serving slot, one model at a time. Every text seat in
the default config points at the same endpoint —

| seat | model | endpoint |
|---|---|---|
| `agent_model` | `qwen3.8-27b` | `http://127.0.0.1:11436` |
| `model` | `gemma-4-e4b` | `http://127.0.0.1:11436` |
| `reasoning_model` | `gemma-4-26b` | `http://127.0.0.1:11436` |
| `escalation_model` | `gemma-4-12b` | `http://127.0.0.1:11436` |
| `triage_model` | `gemma-4-e2b` | `http://127.0.0.1:11436` |
| `vision_model` | `qwen3-vl-8b` | `http://127.0.0.1:11436` |

— so any two lanes that name different models at the same time force an evict-and-reload. The
harness had nothing that serialized by model. The full audit at the time of this decision:
`mediaSlot` is media-only and in-process, `gpulease`'s `ClassMedia` never covers interactive text,
`sttclient`'s `inferMu` guards a different process (whisper-server), `pipeline`'s `swapMu` guards a
timestamp map rather than requests, `delegate`'s `sem` caps fan-out width inside one `Run`, and
`internal/mcpserver` has no limiter at all — every `offload_*` text call went straight through.

Measured consequence: the agent seat degraded ~4x, from 72 s to 307 s per call, while a cascade text
call kept swapping the slot underneath it.

Neither the fleet queue (0.100.0) nor re-placement (0.101.0) addresses this. Both competing calls
were on the same box, and a queue that distributes work across machines has nothing to distribute
away from.

Two constraints shaped the answer:

- **A filesystem lease per text call is off the table.** ADR [0018](0018-machine-wide-fenced-gpu-lease.md)
  built exactly that for media and deliberately excluded ordinary interactive text: "thousands per
  day at ~46 ms, and leasing them is untenable. That asymmetry is a known limit, not an oversight."
  Nothing since has changed that arithmetic.
- **The harness never learns what is currently resident.** `llamaclient` marshals `{"model": alias,
  …}` and POSTs; llama-swap queues the request while it loads and there is no "model loading" status
  to detect (see the design note on `pipeline.breakerFailure`). So arbitration cannot be based on
  observing the server — it has to be based on what this process is asking for.

## Decision

**`internal/modelaffinity` — a process-local admission gate on every llama-swap generation request,
keyed on the RESOLVED base URL.**

- Requests naming the **same model on the same base** proceed concurrently. llama-swap already queues
  those harmlessly; the expensive event is the switch, not the overlap, and serialising them would be
  a pure regression.
- A request naming a **different model on a base that has in-flight requests** parks until they
  drain, then proceeds. N interleaved switches become one switch per batch.

**Keyed on the base, never on the model.** `llamaclient.resolveEndpoint` can return a different base
per model — a `seat_endpoints` pin, a busy-aware `cascade_remote_lanes` failover, or the default — and
two models served by two llama-swap instances do not contend at all. The gate consumes the base that
function already decided; it never re-decides it.

**Both lanes take it.** The gate is installed in `llamaclient.Generate` / `GenerateVision` /
`GenerateVisionInterleaved` *and* in `agent.LLMClient.Chat`, which POSTs to `/v1/chat/completions`
itself and never went through `llamaclient`. The agent seat is the lane the incident degraded, so
gating only the cascade side would have been a lock only one side takes — the shape ADR 0018 was
written to end.

**Bounded, with a named outcome.** A park is bounded by `(batches ahead + 1) × budget`, where budget
is the resolved `http.Client`'s own request timeout. The bound is wall-clock, fixed at park time and
never extended by progress. Exhaustion returns a `*modelaffinity.WaitError` carrying the base, the
model wanted, the model that held the slot, the in-flight count and the number of switches queued
ahead. The caller's `ctx` bounds the wait too and in production is usually the tighter of the two.

**No starvation.** Once someone parks, later arrivals naming the resident model park behind them
rather than joining the running batch, and promotion always takes the model at the head of the queue.

## Consequences

- The measured thrash between the agent seat and a cascade seat inside one harness process is gone;
  interleaved switches become one switch per batch.
- **Process-local is the limit, and it is a real one.** Two harness processes on one box — an MCP
  server plus a CLI invocation, or two MCP servers under two editors — still thrash each other
  exactly as before, because neither can see the other's in-flight set. Closing that would require
  the machine-wide, fenced, pid-recycle-safe state root of ADR 0018 on every text call, which is the
  cost that ADR refused for text. It is named here rather than built.
- Two llama-swap routes that can force a load stay outside the gate: `internal/agent`'s
  `/upstream/{model}/props` probes (`ProbeSeatPin` exists to *warm* a seat, so gating it would fight
  its purpose) and `internal/tokclient`'s `/upstream/{model}/tokenize`. Both are affine to their own
  caller's seat, neither is a burst source, and a tokenize is a separate admission from the
  generation that follows it — so gating it could not batch the two anyway.
- A request can now fail for a reason that is neither the model nor the network. The error type is
  distinct so this is diagnosable, and its wording deliberately carries the substring
  `pipeline.classifyErr` buckets congestion by, so the ledger files it as `timeout` rather than
  `other`.
- Hot-path cost: two uncontended mutex acquisitions and no allocation for an admission that does not
  block.

## Alternatives considered

- **A `gpulease` text class per request.** Rejected: ADR 0018 already weighed and refused this for
  interactive text, and nothing about the volume has changed.
- **Keying the gate on the model alone.** Rejected: it serialises models that live on different
  llama-swap instances and does not serialise the ones that share a slot. Pinned by a mutation test.
- **One global gate ignoring the base.** Rejected: it serialises independent endpoints. Also pinned
  by a mutation test.
- **Serialising all text calls on a base.** Rejected: same-model concurrency is free on llama-swap,
  so this would be a pure regression on the common path.
- **Detecting residency by probing llama-swap.** Rejected: there is no "model loading" status to
  detect, and a probe per call is both a cost and a race — the answer can be stale before the request
  it informs is sent.
- **Holding one admission across tokenize-then-generate.** Deferred, not rejected: it would close the
  `/upstream/{model}/tokenize` residual, but it is a larger change through `internal/pipeline` and
  belongs on its own.

## Related code

- `internal/modelaffinity/affinity.go` — the gate.
- `internal/llamaclient/client.go` — the cascade and vision seam (three generation methods).
- `internal/llamaclient/lanes.go` — `resolveEndpoint`, whose decision the gate consumes.
- `internal/agent/client.go` — the agent seat seam.
- `internal/gpulease/gpulease.go` — the text exclusion this decision works around.

## Related docs

- [ADR 0018](0018-machine-wide-fenced-gpu-lease.md) — the machine-wide fenced GPU lease and its text exclusion.
- [`docs/systems/offload-pipeline.md`](../../systems/offload-pipeline.md)
- [`docs/systems/coding-agent.md`](../../systems/coding-agent.md)
- [`docs/systems/gpu-lease.md`](../../systems/gpu-lease.md)
- [`docs/glossary.md`](../../glossary.md)
