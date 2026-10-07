---
status: Accepted
date: "2026-09-08"
---

# 0038 — Accelerator work travels to the box that has the device, bytes included

Release: 0.115.0

## Context

ADR 0024 made a device beside the GPU an *additive* capability of the box that carries it, and
ADR 0037 settled which device owns a shared tool name on one box. What neither covered is the
box that has **no** device: on 2026-09-08 the Coral Edge TPU was fully served on `<node-c>`,
and from `<node-b>` — where the operator actually works — the only way to reach it was an
`agent_delegate` contract routed by hand to that node, naming an image path *on `<node-c>`'s
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
   *(0.145.0: [ADR 0068](0068-the-operator-may-name-the-owner-of-a-shared-accelerator-tool.md) lets
   `accelerator_tool_owners` give one shared name to a fleet device; every name without an entry
   still follows this rule.)*
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

- From `<node-b>`, `offload_classify_image(image_path=<a file on the calling node>)` answers from
  `<node-c>`'s Coral with a placement block; the same tool inside an `agent_run` on `<node-b>` does
  the same. `agent_delegate` contracts to `<node-c>` keep working unchanged.
- A box that lists nothing in `fleet_accelerators` is byte-identical to 0.114.x (pinned by
  test). A box that lists a device it also carries registers the local lane only.
- The fleet's device inventory is readable in one place: `NodeView.Accelerators`, decoded from
  health, absent = none.
- Not in this record: multi-node fan-out of accelerator calls, a savings ledger for accelerator
  work (still the recorded follow-up from ADR 0024), and video/stream inputs.

## Amendment 2026-10-01 (register E-08, the Hailo-8L is the one device that never travels)

Decisions 1 and 2 send accelerator work to the box that carries the device. One device is the
explicit exception: **the Hailo-8L stays local-only.** The standalone accelerator box that carries
it is, by operator decision (register J-13), never delegated to, never a fleet node and never a
sender; this amendment enforces the device-level half of that in the harness. ADR 0024's
`accelerators` list is still how the box registers the device's tools and agent-loop lanes for its
own callers. What changes is everything a fleet node would do with the id:

1. **Not published.** `/fleet/health` omits `hailo-8l` from `accelerators`, whichever source supplied
   the list (the installer manifest or the config fallback).
2. **No lane.** A node whose only device is the Hailo-8L does not advertise `accel` in
   `supported_task_types`, and the pull door does not claim it.
3. **No job.** An `accel` job naming `hailo-8l` is refused at admission — a 400 on the push door, a nack
   on the pull door — whether or not the node lists the device. The refusal prints only the devices the
   fleet may use, never the node's full list.
4. **No forward.** `fleet_accelerators: ["hailo-8l"]` is a `doctor` finding and a startup warning, because
   no node can answer it. `fleet-ui` refuses a listen port equal to `hailo_endpoint`'s on a box that lists
   the device (its documented default, 18813, is also the sidecar's); neither port moves.

The rule is per device: one list (`config.LocalOnlyAccelerator`) behind one filter
(`config.FleetVisibleAccelerators`). The Coral Edge TPU and the RK3588 NPU are still published, advertised
and accepted exactly as decisions 1 and 2 describe, and a device `profiles.json` declares without a
decision fails a test in `internal/tierseed`. Not in this amendment: refusing `fleet-serve` on a standalone
box, or knowing which other boxes name it in `delegate_remotes` — the harness cannot read another box's
config, so that half stays an operating rule.

## Amendment 2026-10-07 (a caller cannot name a node the roster does not list)

The standalone box is never delegated to, by operator decision (register J-13). The amendment above made the device
unpublishable and the node unaddressable on the fleet's own paths; one path stayed open by habit: a model-named
`remotes` list on `agent_delegate` REPLACED `delegate_remotes` after a check of each URL's SHAPE alone (loopback, the
tailnet's range or zone, a dotless name), so any tailnet host passed and was dialled, the fleet bearer riding its health
read. The list may now only NARROW the fleet: every entry must be in `delegate_remotes`, one outside it is refused with a
message that says so before anything is dialled, and a box that configures no roster accepts no list
(`delegate.CheckRosterRemotes`, applied by the door and again by the engine for a call that says its list came from a
model, `RunOptions.RosterOnly`). The operator's CLI verbs keep naming any tailnet node: `fleet-smoke --remote` is how a
node that has not joined the roster is tested, and an operator is not the caller this rule is about. What stays an
operating rule, as above, is never listing the standalone box in a delegator's `delegate_remotes`, and refusing
`fleet-serve` on it; there is still no discovery, and nothing here adds one.

## Related

- [ADR 0024](0024-accelerators-are-additive-to-the-gpu-tier.md) — accelerators are additive
- [ADR 0037](0037-a-capability-name-has-one-owner-per-box.md) — one owner per name per box
- [docs/systems/accelerators.md](../../systems/accelerators.md) — the operating doc
- [docs/systems/fleet-node.md](../../systems/fleet-node.md) — the task types and health payload
