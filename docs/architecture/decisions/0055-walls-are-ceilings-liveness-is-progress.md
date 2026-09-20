---
status: Accepted
date: "2026-09-20"
---

# Walls are ceilings; liveness is progress

## Context

Until 0.130.x a delegation contract ran under ONE number sized before the run — D-03's auto wall:
cold load + think block + steps × (tokens ÷ rate + overhead) + final + re-pack, clamped to
`[300, 900]` s — enforced as a `context.WithTimeout` over the whole node-side run. The delegator
polled until `timeout_sec + 60 s` (re-anchored on the node's published `wall_sec`, D-116).

The ledger on 2026-09-20 showed what that does to a slow but healthy seat. Every wall timeout
that day was a **producing** job:

| time | seat | steps | tokens out | seat tokens in | latency | reason |
|---|---|---|---|---|---|---|
| 11:48 | lenovo `qwen38-27b-gsq-vllm` | 11 | 949 | 213,442 | 984 s | wall timeout after 900s |
| 12:07 | lenovo `qwen38-27b-gsq-vllm` | 11 | 544 | 169,515 | 900 s | wall timeout after 900s |
| 16:09 | Qube `agent-pool` | 11 | 6,236 | 549,594 | 769 s | wall timeout after 600s |

The 16:09 row's `tok_per_s` read **0** on the `agent_delegate` row — the one field that should
have proved the job alive was unfilled on the row the operator reads.

The code could not tell a live job from a hung one because it never looked: the loop's seat call
was **non-streaming** (`Stream: false`), so inside a step the node had no telemetry at all — a
550 k-token prefill and a dead engine were indistinguishable until the clock ran out. The run
registry's heartbeat (`HeartbeatMs`) was the holder *process* ticking, not the job (register
C-32). The operator's standing order — "smart, dynamic, and with the correct telemetry to
determine if a job is truly still going" — was not met by any of the three surfaces.

## Decision

1. **Every seat completion streams** (`stream: true`, `stream_options.include_usage: true`).
   The client decodes the SSE stream into the same `wireResp` a JSON answer produces, so
   nothing after the decode changed; each delta (content, reasoning or tool-call arguments) is a
   progress event delivered through a `ProgressFunc` carried in the call's context. A seat that
   answers JSON anyway (a proxy that ignores `stream`) is decoded as before.

2. **A run is ended by a STALL or by a CEILING — never by its expectation expiring.** The
   contract's wall (`timeout_sec`, or the node's auto wall) is now the **expectation**: still
   reported as `wall_sec`, still what the delegator sizes and anchors from, no longer a kill.
   `agent.Monitor` owns the run's liveness:
   - **Stall** = no progress event inside the phase's allowance. The allowance is dynamic:
     admission → the admission budget; **prefill** → `pending prompt tokens ÷ the seat's
     measured prefill rate × 1.5 + 30 s` (400 tok/s assumed until measured); **decoding** →
     `20 deltas ÷ decode rate`; **tool** → the tool's own cap + 30 s (an uncapped tool: 1 h);
     **re-pack** → its chat timeout; all floored at 60 s. A stall is filed
     `stalled: no progress for Xs in <phase> (allowed Ys: <arithmetic>)` as
     **`DeferClassInfrastructure`** — the seat's health, never the budget signal.
   - **Ceiling** = `max(3 × estimate, 2 × wall, 1800 s)`, capped at 4 h
     (`AgentCeilingSecCap`). A run still producing when it passes is filed
     `ceiling Ns reached while producing (T tok at R tok/s)` as **`DeferClassBudget`** —
     the sizing signal. The context's `Deadline()` reports the ceiling so every existing
     reader (the re-pack floor, the seat-wait budget) keeps working; the cause is typed
     (`context.Cause`), so no arm guesses from the clock.
   - `wall timeout after Ns` is retired from the node's own vocabulary; it survives only as the
     text for a **caller's** deadline expiring, and says so.

3. **Telemetry on every surface the operator reads.** The run registry carries job liveness
   *beside* the process heartbeat: `last_progress_ms`, `tok_s`, `live_phase`, `allowance_ms`,
   rendered as one shared line ("producing 3.4 tok/s, last token 2s ago (allowed 60s in
   decoding)" / "silent 187s of 214s allowed in prefill") in `gpu status` and
   `offload_status.gpu_lease.activity.runs[]`. The fleet node publishes the same as `progress`
   on `/fleet/jobs/{id}` with `stall_allowance_sec` and `ceiling_sec` twinned at the top level.
   The wire result carries `ceiling_sec`, `stall_allowance_sec`, `last_progress_ms`. The
   ledger's `agent_delegate` row now fills `tok_per_s`.

4. **The delegator polls while the node reports progress.** Its deadline holds while the node's
   `last_progress_ms + allowance + grace` is in the future, bounded by the node's ceiling. A node
   that reports no progress is polled exactly as before; a node whose report goes stale is not
   extended, and the deadline reason says when it last moved and what it was allowed.

5. The seat-rates store records the seat's **prefill rate** (`prefill_tok_s`) from the loop's
   prefill accounting, so the prefill allowance is measured after the first run, not assumed.

## Consequences

- A 27B seat at 1 tok/s finishes its contract. A seat that dies mid-stream (an engine error
  frame, a socket that stops) is a stall within one allowance, filed as infrastructure, with the
  arithmetic in the reason.
- A seat that answers 429 past the contract's busy-seat budget ends the re-pack as a transport
  verdict on that attempt — under the wall the clock cut that wait; now the budget decides, and
  no chat-lane attempt is spent on a seat already given up on. A counted 429 wait is itself a
  liveness touch (the seat answered), never a stall.
- The client's own `Timeout` no longer applies to a call under liveness: with streaming it
  would cover the whole body read and cut a long answer mid-stream. The context owns the
  deadline there; the CLI doors and probes keep the client timeout.
- Contract sizing (`timeout_sec`, `wall_estimate_sec`, `wall_sec`) and the delegator's
  anchoring (D-116) are untouched. `AgentTimeoutSecCap = 900` still caps the *declared*
  value; it no longer ends anything.
- New stable reason prefixes: `stalled: ` and `ceiling `. Readers keyed on
  `wall timeout after` see it only for a caller-imposed deadline.
- Deliberately out of scope: the media lease's reclaim rule (C-32's other half) still has no
  job-liveness input — it is a different holder (ComfyUI) with a different progress signal.
- Tests never sleep real seconds for liveness: the pipeline's `livenessFloor`, `livenessSlack`
  and `ceilingFloorSec` are package vars a test compresses, as `pollSecond` already was.

Register: plan `plans/2026-09-20-liveness-walls.md` (AI Ecosystem), rows C-32 (partial) and
the D-row this ADR is filed under. Supersedes the enforcement half of D-03; D-03's sizing stands.

## Alternatives considered

- **Raise the cap** (900 s → 3,600 s). Rejected: it moves the cliff, it does not remove it — a
  1 tok/s seat on a 12-step contract still dies producing, and a hung engine now holds the seat
  four times longer. The defect is enforcing a pre-run number, not its value.
- **Per-step request timeouts instead of streaming.** Rejected: a step is one blocking POST, so a
  per-step timeout is the same blindness at a finer grain — a 214 k-token prefill is minutes of
  legitimate silence inside one step, and only a streamed first delta ends it.
- **Reuse the registry heartbeat as job liveness.** Rejected: it tracks the holder process (C-32's
  finding); folding job progress into it would hide a hung job behind a live process again.
- **Poll the engine's own metrics** (`/metrics`, `/slots`). Rejected for the node path: the
  engine's counters are per seat, not per run, and vLLM/llama.cpp expose them differently; the
  stream is per run and identical on both.

## Related code

- `internal/agent/client_stream.go`, `internal/agent/client.go` — the SSE decoder and the streamed `Chat`.
- `internal/agent/progress.go`, `internal/agent/liveness.go` — the progress callback, `StallPolicy`, `Monitor`.
- `internal/agent/loop.go` — phases and per-delta progress from the loop.
- `internal/pipeline/liveness.go`, `internal/pipeline/agenttask.go` — the policy, the ceiling, the stall/ceiling arms, the fleet progress report.
- `internal/gpuactivity/registry.go` — job liveness beside the heartbeat; `Run.Liveness`.
- `internal/fleetnode/jobs.go`, `internal/fleetnode/server.go` — `progress` on `/fleet/jobs/{id}`.
- `internal/delegate/run.go`, `internal/delegate/intent.go` — polling on progress; `tok_per_s` on the delegate row.
- `internal/seatrate/seatrate.go` — `prefill_tok_s`.

## Related docs

- [OPERATOR-GUIDE.md — The timeout chain](../../OPERATOR-GUIDE.md) (rewritten for 0.131.0).
- ADR 0041 (the drain waits for runs inside the queue) — the run registry this builds on.
- Register D-03 (wall sizing, unchanged), D-116 (the delegator anchors on `wall_sec`, unchanged), C-32 (job liveness, the media half still open).
