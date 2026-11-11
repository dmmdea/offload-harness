---
status: Accepted
date: "2026-11-11"
---

# An accelerator answers through a GPU lease, and a shared tool falls back to the next device

## Context

Two rules kept accelerators idle while callers waited. Both were seen on 2026-11-11, the day the AX650-class
NPU card ([ADR 0101](0101-axera-ax650-npu-card-as-an-accelerator.md)) took four shared tools on the delegator.

**A reservation of the GPUs refused the NPU.** A text lease reserves a node's cards for a measurement, and
`/fleet/dispatch` refused every task type but an agent contract while one was held, because "the cards it
will use are chosen later, and unknown is every card". An `accel` job uses none of them: it runs on a TPU or
an NPU through a loopback sidecar, and on the NPU card in its own memory. For the hours a measurement held
the node's GPUs, every forwarded accelerator call was refused with `node leased`, with the device idle.

**A shared tool had one device and no second choice.** A shared tool name has one owner per box
([ADR 0037](0037-a-capability-name-has-one-owner-per-box.md)), and the operator may name it
([ADR 0068](0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md)). ADR 0068 also decided
that "nothing is chosen per call and nothing falls back at call time". With detection, classification,
embedding and semantic segmentation handed to the NPU card on one node, those four tools deferred whenever
that node was off or refusing, while a Coral and an RK3588 NPU that serve the same names were reachable and
idle. The operator's standing order is that the harness adapts to the work.

## Decision

**1. A GPU lease never refuses an `accel` job.** `fleetnode.textLeaseAgainst` answers "not held" for the
`accel` task type. Every other task type keeps its reading: an agent contract by the seats it could run on,
the rest by the whole node. The lease is still published in `/fleet/health` unchanged.

**2. The owner of a shared tool is the first choice, not the only one.** When the owner cannot take a call,
the call runs on the next device that serves the same name, in the listed order (local devices, then
`fleet_accelerators`), and the result says so. This supersedes ADR 0068's "nothing falls back at call time";
the rest of ADR 0068 and ADR 0037 stand: one registered tool per name, the owner's description, schema and
model, the same owner on both surfaces.

- **What "cannot take the call" means** (`agent.AccelCannotServe`). A defer: the node is off, no node
  advertises the device, the dispatch was refused, the sidecar did not start. Or a sidecar refusal that is
  about the device: a model that is missing, fails its digest or does not load, a disabled or absent NPU, an
  internal error. A refusal of what the caller sent (`bad_request`, `image_missing`, `image_unreadable`,
  `out_path_exists`, `out_path_unwritable`, `forbidden`, `not_found`, `unknown_model`) is an answer: another
  device would refuse the same input.
- **Each fallback device runs its own tool.** Its own table row gives the sidecar tool, the required
  argument and the schema. The caller sent the owner's arguments; only the ones that device's schema
  declares travel to it.
- **The result is attributable.** A fallback answer carries
  `fallback: {from, reason, served_by}`. When no device serves the call, the owner's answer comes back with
  `fallback_tried: [{device, reason}]`.
- **An embedding does not move silently.** A vector from another device is in another space, and nothing in
  the numbers says so. `offload_image_embed` and `offload_text_embed` fall back only when the caller passes
  `any_space: true`; without it the owner's defer comes back with `fallback_withheld`, naming the devices
  that could have answered. The argument is added to the tool's schema only on a box where a fallback device
  exists, and it never reaches a sidecar.
- **`out_path` is checked before it travels.** The owner applied its own rule to `out_path` (the NPU card's
  sidecar, or the caller-side landing of a fleet device). A fallback device's sidecar may apply none, and an
  agent seat can pass any path, so the fallback applies the lane's rule itself before any device runs:
  `out_path` is never the input image, its directory must exist, and an existing target is replaced only
  when it is a regular file that is already a PNG. Otherwise the owner's answer comes back with
  `fallback_withheld`. The Coral sidecar's `semantic_segment` gains the same rule at the sink, where it had
  none (found by the security pass on this change).
- **Nothing is remembered between calls.** There is no "owner is down" state and no health pre-check: every
  call tries the owner first. A node that is off costs the roster probe (about 2 s) before the fallback runs.
- **Where it lives.** At tool registration on both surfaces, through one function
  (`agent.AccelFallback`): `mcpserver.handleAccelChain` and `agent.accelLaneTools`. A node running a
  forwarded job calls its local lane directly (`pipeline.runAccelTask`), so a call cannot forward twice.
- **Status.** A device's `offload_status.accelerators` row gains `fallback_for`, the names it answers when
  their owner cannot. `serves` and `owns` are unchanged. A tool with a fallback says so in its description.

## Consequences

- A measurement lease on a node's GPUs and accelerator work on the same node run side by side. The lease
  holder's GPU numbers are untouched; the sidecar's pre- and post-processing does use host CPU for a few
  milliseconds per call.
- A fallback answer comes from another model: a different detector, classifier or segmenter, with that
  device's defaults for any argument the caller did not send. `fallback.served_by` and the result's `model`
  say which one ran, and a caller that needs the owner's model reads them.
- A box with one device, or with no shared name, advertises exactly the tools it did before: descriptions and
  schemas are extended as text and only where a fallback device exists.
- A call whose input is refused is not retried elsewhere; a call the owner fails for a device reason costs
  one more call per fallback device before it defers.

## Alternatives considered

- **Keep the refusal and let the operator release the lease.** That is the rule that failed: the lease was
  doing its job for the GPUs and had no claim on the NPU.
- **Scope the exemption to devices with their own memory.** The RK3588 NPU shares host RAM, but a lease
  reserves GPUs, not RAM, and its declared RAM figure is for the holder's own load. One rule for every
  accelerator is the one a reader can predict.
- **Route by health before the call.** It needs a second source of truth about the owner and goes stale
  between the probe and the call. Trying the owner first reads the same health through `pickNode` and fails
  in about 2 s.
- **Let an embedding fall back like the rest.** A similarity index built from two spaces is wrong without
  any error, and two of the three devices can share a dimension.
- **Let the caller name a device per call.** It would work, and it is the device-suffixed tool ADR 0037
  rejected: a caller would have to know the hardware to ask for a capability.

## Related code

- `internal/fleetnode/leases.go` (`textLeaseAgainst`), `internal/fleetnode/accel_task_test.go`
- `internal/agent/accelfallback.go`, `internal/agent/acceltools.go` (`accelLaneTools`)
- `internal/mcpserver/acceltools.go` (`accelFallbackPlan`, `handleAccelChain`, `accelStatus`),
  `internal/mcpserver/accelfallback_test.go`

## Related docs

- [systems/accelerators.md](../../systems/accelerators.md)
- [ADR 0037](0037-a-capability-name-has-one-owner-per-box.md),
  [ADR 0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md),
  [ADR 0068](0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md),
  [ADR 0101](0101-axera-ax650-npu-card-as-an-accelerator.md)
