---
status: Accepted
date: "2026-09-12"
---

# Vision work travels to a node with an idle card

## Context

The harness could place TEXT work on fleet nodes (`agent_delegate`, ADR 0023) but had no path
to place VISION work there: `offload_vqa`, `offload_assess_image` and `offload_ocr` always ran
against the local endpoint. On 2026-09-11 a 275-image render held all three of the workstation's
cards for a day while a fleet node (`lenovo-ampere16`, an NVIDIA A2 with a vision seat) sat at
0 % utilization — and the image QA the render needed could only queue behind the render on the
box that was rendering.

The first question was the credential `:8080` on that node wanted. It is `pzdash` (a game
dashboard behind Basic Auth), not an inference server; the node's llama-swap listens on loopback
only. So there was never a remote model endpoint to expose or to authenticate against — the only
trust boundary on the node is `:18811`, and the fleet token it already carries.

Two constraints shaped the wire. `/fleet/dispatch` caps its body at 1 MiB, and one image at the
configured `vision_max_image_bytes` (6 MB) is six times that. And the caller must never learn a
path on the node's disk: the accel lane (ADR 0038) had already settled that bytes travel with the
job.

## Decision

1. **A `vision` fleet task on its own route, `POST /fleet/vision`.** The route exists for ONE
   reason — its body is an image, so it is capped from the node's `vision_max_image_bytes`
   (`VisionBodyCap`) rather than dispatch's 1 MiB. After the body read it joins the same
   admission path as every other job (`Server.admit`: bearer auth, known-id re-ack, drain, lease,
   band, queue cap, `BuildRequest`, the job store). Nothing about admission is re-implemented.
2. **The image travels as a data URI inside the job**; the node's pipeline loads it through the
   same loader (`imageio.LoadImageB64`) and the same cap as a local call. The caller reads its
   file on its own box, under its own cap.
3. **The node stores the FULL `core.Result`** — defers included — as the done job's data, so a
   remote judgment and a local one have one shape (the agent lane's convention: a defer is a
   `done` job saying `deferred: true`).
4. **One predicate, both sides.** `VisionLaneAdmissible` (a bound `vision_model` and the agent
   lane's reachability rule — loopback, or a token) decides the advertisement (`vision` in
   `supported_task_types`, plus `vision_model` in health) AND the ack-time admission, exactly as
   `AgentLaneAdmissible` does for the agent lane. A lane advertised that dispatch would refuse is
   a mis-route by construction.
5. **Auth = the agent lane's rule, verbatim.** The vision lane is token-gated on dispatch and on
   poll (`JobView.Gated` — kept apart from the agent marker so the jobs feed never calls a vision
   job an agent run). Media lanes stay tokenless.
6. **The route parameter, quality-first.** `local` is the default and is byte-identical to before.
   `auto` considers a node ONLY while the machine-wide GPU lease is held (`delegate.LocalBusy`,
   the same trigger agent placement uses) and falls back to local when no node is eligible —
   queued-local beats ineligible-remote. `remote` forces a node and DEFERS (`defer_class`
   `capacity`, or `config` with no remotes) rather than touching the local card. Placement ranks
   eligible nodes (`delegate.PlaceVision`) with the agent lane's `betterRemote`, so a vision
   placement and an agent placement agree on which of two nodes is the less loaded one.

## Consequences

- Image QA can run on an idle fleet card while the local cards render; the local GPU is untouched
  under `route: remote` (verified live: `nvidia-smi` on the workstation shows no new process).
- The local result cache and ledger see only local runs; a remote run's cache and ledger live on
  the node that ran it. `meta.node` / `meta.placement` make the placement attributable.
- `core.Result` gains `defer_class`, `core.Meta` gains `node` and `placement` — all omitempty, so
  every existing result publishes byte-identically.
- A node one release behind never lists `vision`, so it is never a target; a delegator one release
  behind ignores the new health fields. Staggered deploys stay safe in both directions.
- The agent loop's own `offload_vqa` tool twin still runs on the executing node's seat — the loop
  runs ON the node, so there is nothing to forward.

## Alternatives considered

- **Expose the node's llama-swap (`:11436`) beyond loopback, or proxy `/v1/chat/completions`
  through `:18811`.** Rejected: the node would then run a caller-shaped raw prompt on its seat
  with no task envelope, no cap, no admission, and the caller would need the node's model alias,
  grammar and template rules — every piece of the vision pipeline re-implemented client-side.
- **A `vision` `task_type` on `/fleet/dispatch` with a per-task body cap.** Rejected: the cap must
  be applied before the body is decoded, and the task type is inside the body. A route is the
  honest way to say "this body is an image".
- **Forwarding through the accel lane** (`task_type: accel`). Rejected: accel runs a sidecar tool
  on a device the caller lacks; vision runs the harness's own pipeline on a seat every box may
  have. Different admission (accel is uncapped), different result shape.
- **Auto-routing by remote GPU utilization instead of the local lease.** Rejected: the operator's
  rule is quality-first — an idle local box always runs the work; remote utilization is a
  tie-breaker in ranking, never the trigger (the same ordering ADR 0023's gate settled).

## Related code

- `internal/fleetnode/vision_task.go` — payload, `VisionBodyCap`, `VisionLaneAdmissible`,
  `buildVision`, `visionJobData`; `server.go` — `handleVision`, `admit`, the `Gated` poll gate.
- `internal/visionremote/visionremote.go` — `Run`, `Call`, `PlaceVision` wiring.
- `internal/delegate/gate.go` — `PlaceVision`; `nodeview.go` — `Tasks`, `VisionModel`.
- `internal/mcpserver/mcpserver.go` — `visionRun`, the `route` schema; `main.go` — `--route`.

## Related docs

- [FLEET-NODE.md — The vision task](../../FLEET-NODE.md#the-vision-task-post-fleetvision)
- [systems/fleet-node.md](../../systems/fleet-node.md)
- [ADR 0023](0023-agent-lane-tailnet-auth-and-locality.md) — the agent lane's auth and locality
- [ADR 0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md) — bytes travel with the job
