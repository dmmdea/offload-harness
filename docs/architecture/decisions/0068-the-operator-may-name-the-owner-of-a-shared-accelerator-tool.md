---
status: Accepted
date: "2026-09-30"
---

# ADR 0068 — The operator may name the owner of a shared accelerator tool, one tool at a time

## Context

ADR 0037 gives a shared accelerator tool name (`offload_object_detect`, `offload_image_embed`,
`offload_classify_image`) to the first listed device that owns it. When it was written no box listed two
devices, so the rule was an invariant rather than a live path. It is live now: the delegator carries a Coral
Edge TPU locally and reaches the RK3588 NPU over the fleet (`fleet_accelerators`, ADR 0038), and local devices
are walked first, so the Coral owns every shared name and the NPU's copies are skipped.

Order is the wrong granularity for that box. The two devices are not better or worse as a whole; they are
better at different tools. Reordering hands every shared name to one device at once, and a local device cannot
be listed after a fleet device at all. The operator needs to say "detection goes to the NPU, classification
stays on the Coral" and have both surfaces honour it.

Device-suffixed names (`offload_object_detect_rknpu`) stay rejected for the reason ADR 0037 gives: a caller
would have to know the hardware to ask for a capability.

## Decision

**`accelerator_tool_owners` maps a shared tool name to the device that serves it:
`{"offload_object_detect": "rknpu"}`.** The entry takes that one name before the ADR 0037 walk; every name
without an entry is decided exactly as before.

- **An entry applies only when it can.** The named device must be listed in `accelerators` or
  `fleet_accelerators`, and its tool table must have that name. Otherwise the entry is logged at startup
  and ignored, and the first-listed rule decides the name, so a typo or a stale entry never removes a tool.
- **A fleet device may take a name from a local one.** That is the case that motivated the key, and the
  forwarded tool keeps its `[FLEET: …]` description and `placement` result.
- **Both surfaces read one decision.** `mcpserver.accelOwnerPlan` computes the owner of every name without
  registering anything; MCP registration and the status block read it. The agent loop's lanes carry their
  claims (`AccelLane.Claims`, filled by `pipeline.NewLoopAccel` from `config.ToolOwnerClaims`), and
  `agent.accelLaneTools` applies them the same way. `TestToolOwnersLoopMatchesMCP` checks that a loop tool
  routes to the device the MCP plan names, in every order of three devices, for entries that apply and for
  entries that do not.
- **Status shows the result.** Each device's status entry adds `serves`, the tools it actually registered;
  `owns` stays the list of capabilities it could serve.

## Consequences

- One config line moves one capability, and removing the line restores ADR 0037's behaviour exactly.
- The key names tools, not devices' capability ids, because the tool name is what callers and the status
  block show; the same spelling works on both surfaces.
- Vectors from different devices are still never comparable: `offload_image_embed` keeps reporting `space`,
  so moving it changes the space a caller receives, and the tool description says which device serves it.
- Nothing is chosen per call and nothing falls back at call time. If the named device's node is down, the
  call defers (ADR 0038) instead of running on the other device, which matches every other accelerator
  lane.

## Related

- [ADR 0037](0037-a-capability-name-has-one-owner-per-box.md) — the first-listed rule this refines
- [ADR 0038](0038-accelerator-work-travels-to-the-box-that-has-the-device.md) — fleet accelerators
- [ADR 0062](0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md) — the RK3588 tier
- [systems/accelerators.md](../../systems/accelerators.md) — the tables and the deploy notes
