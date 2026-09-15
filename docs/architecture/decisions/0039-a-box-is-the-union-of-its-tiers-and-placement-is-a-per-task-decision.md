---
status: Accepted
date: "2026-09-09"
---

# 0039 — A box is the union of its tiers, and placement is a per-task decision

Release: 0.120.0

Provenance: the operator approved the design spec and said to implement it autonomously
("implement autonomously", 2026-09-09), which is why an agent set this record to Accepted
rather than leaving it Proposed — the same rule ADR 0036 records.

## Context

A hardware tier in this repo has always been ONE row: a box installs as `blackwell-2x16` or
as `blackwell-3x16`, and everything downstream — the served window, the KV type, the template,
the agent seat, the fleet's capacity view — reads that single row. The reference workstation
breaks the assumption. It is three 16 GB Blackwell cards, of which two are a measured pair
(devices 0 and 2, `-sm tensor`, the vLLM agent seat, 163,840 tokens) and one is the display
card (device 1, an RTX 5070 Ti driving the desktop). It is simultaneously:

- a complete `blackwell-16` (either 5060 Ti alone runs the cascade, the 26B agent seat, the
  OCR and STT seats),
- a complete `blackwell-2x16` (the pair runs the vLLM agent seat and the 262k long twin),
- and the installed `blackwell-3x16`.

Filing it as one tier makes the fleet read one capacity row where three exist, and makes every
placement decision a fixed binding instead of a choice. It also left the display card in a
policy vacuum: it is the only idle VRAM on the box while the pair is busy, and also the card
whose starvation cost a reboot on 2026-09-04 (a three-card engine at util 0.87–0.90 left it
under 1 GB, Windows fell to a 720p-class mode, clean event log — starvation, not a crash).

## Decision

**D1 — A tier may COMPOSE other tiers.** `profiles.json` gains `composes` and `layers`;
`tierseed` seeds `tier_profile`, `tiers` and `layers` into `config.json`, so at runtime every
surface reads the identity from CONFIG rather than re-reading the install. A box that seeds no
layers has ONE implicit layer and is byte-identical on every result, health, status and ledger
surface.

**D2 — Placement is a per-task decision, made by ONE table.** `internal/placement` turns
(task class, token need, quality gate, `context_class`, budget, live occupancy, live guards)
into a `Decision`. The same table serves the local box, a remote node's advertised layers, and
the node's own re-check at admission. There is no second "fit" heuristic anywhere.

**D3 — Saturation is RECORDED, never ACTED on** (council R2). The live matrix makes every
single-card seat mutually exclusive with the pair seat, and the pair pins its KV pool from all
free memory minus 0.5 GiB, so "overflow to the single layer" can never fire on this box.
Treating a saturated pair as "local busy" would hand quality-gated contracts to a 4B seat —
the exact trade the design forbids — so a saturated pair is written into `placed.reason`
("pair at 32/32 in flight — queued in the seat") and nothing is re-placed.

**D4 — The display card is fenced by guards that fail CLOSED** (council R6). `display_floor`
is `free(display card) − seat.display_footprint_gib ≥ floor` (the free-VRAM check it replaced
would have admitted a 10.5 GB load onto a card with 9 GB free); `host_ram` bounds a seat that
holds tens of GB of experts in RAM; `presence` reads the console session (LOCKED ⇒ away, else
last-input idle ≥ threshold AND the shell not busy/fullscreen ⇒ away). Any probe failure
refuses. `operator_presence` defaults to `present`, so the display card is closed until the
operator has read the probe's own readings in `offload_status` and opened it.

**D5 — A composite render is a CHECKED UNION.** `servingtmpl.CheckComposite` reads the
rendered config — what llama-swap loads — and refuses one in which a layer seat is undefined,
a seat's rendered `CUDA_VISIBLE_DEVICES` differs from its declared pin, a router `model_map`
target is missing, or a composed tier's capability is absent. `install render` runs it before
writing the file.

**D6 — The fleet sees layers, and the node decides for its own cards** (council R5). A
composite node advertises `tiers` and `layers` in health (lane-gated, from cached reads); the
delegator rebuilds them with `placement.FromRows` and runs the SAME `Decide`; the dispatched
contract carries `layer`; the node re-runs `DecideOnLayer` with its own live readers, because
the display-card guards can only be read where the card is.

**D7 — Every result says where it ran.** `placed` {tier, layer, role, seat, devices,
ctx_tokens, reason, guard, evicts} rides every agent result, the cascade result and the ledger
row. `results[].placement` stays the string it always was.

**D8 — The window-overflow seat is the PAIR's** (council R1, amended by the operator rule of
2026-09-10). Overflow escalates to the pair's `qwen3.8-27b-262k` (prefill 1,197 t/s measured)
and evicts the pair's agent seat only when that seat is idle or cold; a mid-flight agent seat
is WAITED for and named in `placed.evicts`. The three-card `triple` layer, whose only seats
parked 28–32 expert layers in host RAM, was removed by the operator rule that RAM is overflow
only (0.115.4), so the tier declares no triple layer and `context_class: long` resolves to the
pair's long seat. The table still serves a three-card layer for the day a seat fits inside
VRAM — the guards, the feasibility check and the fixtures are all still exercised.

## Alternatives considered

- **Carve KV headroom out of the pair seat for a co-resident small rung.** Measured dead: at
  11.22 GiB non-KV per worker, the 163,840 window needs ≈2.8 GiB of a ≈3.4 GiB pool, so no
  rung fits beside it.
- **Treat a saturated pair as "busy" and re-place elsewhere.** Rejected under D3: it hands
  quality-gated work to a 4B seat and defers best-effort work that today queues in the seat
  and completes.
- **Two flat seat keys instead of layers.** Rejected: the guards, the device pins and the
  window all belong to the LAYER, and flattening them puts the display-floor arithmetic in
  three places.
- **A display-card overflow enabled by default.** Refused: it is the operator's card and the
  operator's decision. The display layer ships `dormant: true`; enabling it is one config
  edit, after a measurement (G1b) taken while the operator is away.
- **Keep the three-card Flash-Next arms as the long layer.** Removed by operator rule: the
  cards do the inference and RAM is overflow only.

## Consequences

- A box with no `layers` is byte-identical on `/fleet/health`, `offload_status`, every agent
  result row, the cascade result and the ledger row. Every new field is `omitempty`.
- The two agent doors' InputSchema gain `context_class` on EVERY box — the one `tools/list`
  change in this release.
- The per-contract context cap becomes the box's: 256 KiB on a plain box,
  `min(2 MiB, largest layer window × 3)` on a composite one. Without it a composite box's own
  262k seat is unreachable through its own front door (at chars/3 a 256 KiB contract estimates
  ~87k tokens).
- Held for a later record: the fleet-overview layer cards, `top` sub-rows and a ledger
  `by_layer` summary. The ledger keeps a `layer` column so the figure can be summed later.
- Not in this record: no installer path downloads three-card weights; the display layer's
  twins render but stay dormant; and the live-gate measurements (G1b in particular) are taken
  on the reference box with the operator away, not by any automated path.

## Related

- [ADR 0035](0035-persistent-vllm-seat-behind-llama-swap.md) — the pair's vLLM agent seat
- [ADR 0036](0036-the-agent-lane-is-a-harnessed-environment.md) — the contract's own setup, and the
  provenance rule this record follows
- [docs/systems/composite-tier.md](../../systems/composite-tier.md) — the operating doc
- [docs/systems/fleet-node.md](../../systems/fleet-node.md) — the health payload and task types
- [docs/tiers/blackwell-3x16.md](../../tiers/blackwell-3x16.md) — the generated tier page
