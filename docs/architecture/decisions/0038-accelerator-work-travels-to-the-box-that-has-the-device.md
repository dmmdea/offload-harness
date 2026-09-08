---
status: Accepted
date: "2026-09-08"
---

# 0038 — Accelerator work travels to the box that has the device, bytes included

Release: 0.115.0

## Context

ADR 0024 made a device beside the GPU an *additive* capability of the box that carries it, and
ADR 0037 settled which device owns a shared tool name on one box. What neither covered is the
box that has **no** device: on 2026-09-08 the Coral Edge TPU was fully served on the Lenovo,
and from the Qube — where the operator actually works — the only way to reach it was an
`agent_delegate` contract routed by hand to that node, naming an image path *on the Lenovo's
disk*. The operator's verdict on both: "ship it" (routing) and "fix it" (the path). The Coral
design's Phase B had already drawn the seams; this record fixes them as policy.

## Decision

1. **A fleet task type `accel`** runs one accelerator tool on the node that lists the device.
   Its payload is `{accelerator, tool, args, image_b64?, image_name?}`: the image travels as
   bytes (cap 8 MiB), lands in a job-scoped directory that lives exactly as long as the job,
   and `args.image_path` is rewritten to it. A file the tool writes there (the Coral's semantic
   mask) comes back as `mask_b64`. The node runs only its **local** lanes for this task, so a
   forwarded call can never forward again — hop limit 1 by construction, not by counter.
2. **A box opts in to reaching a device it lacks** with `fleet_accelerators: [<id>]`. That
   id's tool table registers locally — on the MCP surface and in the agent loop alike, so
   `tools/list` parity between the two holds — and every call forwards through
   `internal/accelremote` to the **first `delegate_remotes` node whose `/fleet/health` lists
   the id**. The probe is per call (2 s), because the fleet's device list is a fact about
   other machines and must be read at its source.
3. **The image is read where the caller is.** `image_path` names a file on the calling box;
   its bytes ride in the job. A caller-side `out_path` is dropped (the node's default mask,
   beside the shipped image, is what comes back). This is the whole of the "path on the
   other box" fix: no shared filesystem, no mount, no second config.
4. **A local device always wins a shared name.** `config.Accelerators` is walked before
   `config.FleetAccelerators`, on both surfaces, so ADR 0037's first-listed-owner rule extends
   across the fleet without a new rule: local first, then fleet, in config order.
5. **Placement is in the result.** Every forwarded result carries
   `placement{node, base, accelerator, job_id, wall_ms, remote:true}`, so a slow call or a
   defer is attributable from the result alone.
6. **`accel` is exempt from the fleet concurrency cap.** The cap protects the shared llama-swap
   text endpoint; a 3 ms TPU call never touches it, and parking one behind a five-minute digest
   contract would be the cap protecting nothing (the same reasoning that exempts media and STT).

## Alternatives considered

- **Mount the caller's files on the node** (SMB/NFS). Rejected: it makes routing depend on a
  filesystem topology the harness does not own, breaks for any box outside the tailnet share,
  and turns "which node" into "which mount".
- **Register the remote tools implicitly whenever a remote advertises a device.** Rejected:
  `tools/list` is pinned byte-identical for a box that declares nothing (the Hailo and Coral
  tests hold that line); an implicit surface that appears and disappears with a remote's
  health would break every session's tool set silently.
- **Route the whole agent contract to the device's node and let the seat call the tool.**
  That is Phase A and stays available — it is the right shape when the *reasoning* should
  happen next to the device. It does not answer the operator's ask, which was the tool from
  the box they sit at.
- **Carry the image as a context doc of an `agent_delegate` contract.** Context docs are text
  by contract (`{name, text}`); widening them to bytes changes every node's materialisation
  and lint for a case the `accel` task serves directly.

## Consequences

- From the Qube, `offload_classify_image(image_path=<a file on the Qube>)` answers from the
  Lenovo's Coral with a placement block; the same tool inside an `agent_run` on the Qube does
  the same. `agent_delegate` contracts to the Lenovo keep working unchanged.
- A box that lists nothing in `fleet_accelerators` is byte-identical to 0.114.x (pinned by
  test). A box that lists a device it also carries registers the local lane only.
- The fleet's device inventory is readable in one place: `NodeView.Accelerators`, decoded from
  health, absent = none.
- Not in this record: multi-node fan-out of accelerator calls, a savings ledger for accelerator
  work (still the recorded follow-up from ADR 0024), and video/stream inputs.

## Related

- [ADR 0024](0024-accelerators-are-additive-to-the-gpu-tier.md) — accelerators are additive
- [ADR 0037](0037-a-capability-name-has-one-owner-per-box.md) — one owner per name per box
- [docs/systems/accelerators.md](../../systems/accelerators.md) — the operating doc
- [docs/systems/fleet-node.md](../../systems/fleet-node.md) — the task types and health payload
