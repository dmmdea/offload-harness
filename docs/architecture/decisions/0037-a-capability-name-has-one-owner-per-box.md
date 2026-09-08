---
status: Accepted
date: "2026-09-08"
---

# 0037 — A capability name has one owner per box: the first listed accelerator

Release: 0.114.0

## Context

ADR 0024 made accelerators additive: a box lists the devices that ride beside its GPU tier,
and each device's tools register only when it is listed. With one device that was the whole
rule. The Coral Edge TPU is the second, and two of its capabilities — `object_detect` and
`image_embed` — are names the Hailo-8L also owns. A box carrying both would try to register
`offload_object_detect` twice, and whichever registration won would be an accident of code
order, invisible to the operator and different between the MCP surface and the agent loop.

The design (`docs/superpowers/specs/2026-09-04-coral-edgetpu-accelerator-design.md`, D5)
considered device-suffixed names (`offload_object_detect_coral`): rejected, because a caller
then has to know the hardware to ask for a capability, which is the opposite of what an
accelerator lane is for.

## Decision

**A capability name has exactly one owner per box, and the owner is the FIRST accelerator in
`config.Accelerators` that owns it.** Registration walks the list in order; a later device's
same-named tool is skipped for that name and logged once at startup. Device-unique names
register regardless of order.

Both surfaces apply the rule with the same walk — `mcpserver.registerAccelTools` and
`agent.accelLaneTools` — and `hwdetect.DetectAllAccelerators` emits the ids in the same fixed
probe order (Hailo, then Coral), so a freshly detected box gets a deterministic owner and a
hand-edited list can reorder it deliberately.

Two consequences the rule carries on purpose:

- **A tool description always names the device that serves it**, and any embedding tool reports
  its `space`, so a shared name never silently changes the vector space a caller receives
  (1280-d EfficientNet vs 512-d TinyCLIP).
- **Distinct outputs get distinct names**: the Coral's DeepLab is semantic segmentation, the
  Hailo's YOLOv8-seg is instance segmentation, and they are `offload_semantic_segment` and
  `offload_segment` respectively rather than one name with two meanings.

## Consequences

- Today no box lists both devices, so the rule is a **tested invariant, not a live path**:
  `TestSharedNameRuleFirstListedOwnerWins` (MCP) and
  `TestAccelLanesSharedNameRuleFollowsOrder` (loop) assert it in both orders.
- Adding a third device means adding a table (both surfaces) and, if it shares a name, nothing
  else — the walk already decides.
- Phase B (routing a shared capability to a remote node's device) inherits the rule per node:
  each node's own list decides its owner; the delegator never has to.

## Related

- [ADR 0024](0024-accelerators-are-additive-to-the-gpu-tier.md) — accelerators are additive
- [systems/accelerators.md](../../systems/accelerators.md) — the tables and the deploy notes
