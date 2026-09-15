# Composite tier

## Purpose

Some boxes are more than one hardware tier at once. This document describes how such a box
declares that, and how the harness decides — per task — which of its device LAYERS runs the
work and on which seat.

"Tier" here means a **hardware tier** (`blackwell-2x16`, `ampere-8`: the install profile in
`setup/templates/profiles.json`), never the cascade's model tiers (workhorse / triage /
escalation). The glossary keeps both.

A box with no `layers` in its config is not composite, decides nothing, and every surface
below behaves exactly as it did before this existed.

## The layers on the reference box

The reference workstation installs as `blackwell-3x16`: three 16 GB Blackwell cards where
devices 0 and 2 are a measured 5060 Ti pair and device 1 is the RTX 5070 Ti driving the
desktop. It declares itself a complete instance of three tiers, and three layers:

| layer | tier | devices | seats | chosen when | state |
|---|---|---|---|---|---|
| `single` | `blackwell-16` | 0, 2 | router (the cascade's rungs), agent `gemma-4-26b-agent` (131,072), ocr `qwen3-vl-8b`, stt `whisper-stt` | mechanical text; the ocr and stt roles | active |
| `pair` | `blackwell-2x16` | 0,2 | agent = the tier's vLLM seat (163,840, 32 in flight), long `qwen3.8-27b-262k` (262,144, prefill 1,197 t/s), vision `qwen3-vl-32b` | every agent contract that fits; window overflow and `context_class: long` take the long seat | active |
| `display` | `blackwell-16` | 1 | router twins `gemma-4-e4b-display` / `gemma-4-e2b-display` | a mechanical call while the pair holds its cards — once the operator enables it | **dormant** |

There is no three-card layer: its only seats parked 28–32 expert layers in host RAM, and the
operator rule of 2026-09-10 is that the cards do the inference and RAM is overflow only
(0.115.4 removed them). The placement table still serves a three-card layer — a box that
declares one gets it, under its guards — so the day a three-card seat fits inside VRAM the
tier declares it again and nothing else changes.

## The decision table

`internal/placement` is the only place a placement is decided. Its rows, in order:

1. **No layers** → the zero decision. The caller publishes nothing and behaves as before.
2. **Mechanical text** (summarize / classify / extract / triage) → the `single` layer's router
   rung. When the pair's agent seat is LOADED, the reason says the single layer time-shares
   the pair's cards and names what it displaces; if the display layer is awake and its guards
   pass, the rung is substituted onto the display twin instead and nothing is displaced.
3. **ocr / vision** → the layer and role that declare them (documentary: media placement is
   not routed through this table).
4. **An explicit `context_class: long`** → the biggest long-context layer the box declares:
   a three-card layer's long seat where one exists, otherwise the pair's, under the same
   eviction rule as row 6.
5. **An agent contract that fits the pair's agent window** → the pair's agent seat, always.
   A saturated pair (in flight ≥ max_num_seqs) is RECORDED in the reason and nothing is
   re-placed: no other layer can hold that contract beside a loaded pair.
6. **Window overflow** → the pair's long seat. If the pair's agent seat is mid-flight, the
   decision asks the caller to WAIT and names the seat it would evict; if it is idle or cold,
   it is displaced with a note.
7. **Nothing fits** → a contract defer naming the largest window considered.

A long-seat placement also passes a **prefill feasibility check**: `tokens ÷ prefill_tps` must
fit the contract's budget. This is what keeps a seat that cannot finish inside any contract
budget from being chosen at all.

## Guards

A guard is evaluated where the card is, and every one of them fails CLOSED — a reader that is
missing, a card the probe cannot see, a presence the OS cannot report all REFUSE.

- **`display_floor`** — `free(display device) − seat.display_footprint_gib ≥ display_floor_gib`.
  The subtraction is the point: the free-VRAM check it replaced would have admitted a 10.5 GB
  load onto a card with 9 GB free. A seat pinned to the display device that declares no
  footprint is refused before any arithmetic (config validation refuses it at load, too).
  A display device pinned by GPU-UUID (the reference box pins it that way, because the board
  reorders CUDA indices on power loss) is resolved to an index through the probe; unresolvable
  or ambiguous refuses.
- **`host_ram`** — free host RAM ≥ the seat's declared `host_ram_gib`.
- **`presence`** — `operator_presence` is `present` (never), `away` (always), or `auto`:
  console session LOCKED ⇒ away; else last input idle ≥ `operator_idle_sec` (default 900)
  AND the shell not in a busy/fullscreen/presentation state ⇒ away.

**`operator_presence` defaults to `present`.** The display card is closed until the operator
has read the probe's own readings in `offload_status` and set `auto` or `away`.

## What every result carries

Every agent result, the cascade result and the ledger row carry `placed` on a composite box
and nothing at all on a plain one:

```json
"placed": {
  "tier": "blackwell-2x16", "layer": "pair", "role": "long",
  "seat": "qwen3.8-27b-262k", "devices": ["0", "2"], "ctx_tokens": 262144,
  "reason": "window overflow (need ~180000 > 163840); the pair's long seat holds it …",
  "evicts": "agent-pool"
}
```

`guard` is set instead when a guard refused. `devices` is always the SEAT's own pin, never the
layer's list of alternatives. The older `results[].placement` string is untouched.

## What the fleet sees

A composite node advertises `tiers` and `layers` in `/fleet/health` (lane-gated, built from
cached reads — the roster the residency refresh already fetched, the VRAM snapshot the sampler
already holds — so it costs no probe and never blocks). Each row carries the layer spec, each
seat's `served` flag, and the node's OWN admissibility verdict.

The delegator rebuilds those rows (`placement.FromRows`) and runs the SAME table over them.
The dispatched contract carries `layer`, and the node re-decides for that layer with its own
live readers before it runs: a verdict that travelled can only stand in where the delegator
has no reader of its own, and a live reading always wins.

`offload_status.local` carries `tier_profile`, `tiers` and `layers` for the box you are on;
`fleet.nodes[].layers` carries what each node publishes.

## What did not change

- A box with no `layers` publishes not one new key on `/fleet/health`, `offload_status`, any
  agent result, the cascade result or the ledger row.
- `results[].placement` is still a string.
- The only `tools/list` change is `context_class` on the two agent doors, on every box.

## Physics the design respects

- The pair seat pins its KV pool from all free memory minus 0.5 GiB, and the live matrix makes
  every single-card seat mutually exclusive with it. Nothing else fits beside it — measured:
  at 11.22 GiB non-KV per worker the 163,840 window needs ≈2.8 GiB of a ≈3.4 GiB pool.
- The display card starves at scale: 2026-09-04, a three-card engine at util 0.87–0.90 left it
  under 1 GB, Windows fell to a 720p-class mode and the box needed a reboot (clean event log —
  starvation, not a crash). Hence the floor, the presence guard and the dormant default.
- The pair's long seat prefills at 1,197 t/s, which is what makes window overflow finishable
  inside a contract budget.
- A contract's size is estimated at chars/3 — a deliberate over-estimate, never a tokenizer.
  A 256 KiB contract estimates ~87k tokens, which is why a composite box raises its own
  contract cap to `min(2 MiB, largest layer window × 3)`.

## Operator decisions surfaced

- **The dormant display layer.** It renders (two twins pinned to device 1, a matrix set that
  runs them beside the pair seat) and stays closed. Enabling it is one edit — `dormant: false`
  on that layer in `config.json` — and it is the operator's, after reading the G1b measurement
  (does the desktop hold its floor, and does the small call get faster than time-sharing the
  pair's cards?).
- **`operator_presence`.** Until it is set to `auto` or `away`, no placement touches the
  display card.
- **No installer path downloads three-card weights.** Any three-card seat is opt-in and
  operator-installed.

## Source map

| path | what lives there |
|---|---|
| `internal/placement/` | the decision table, the guards, the presence probe, the live snapshot, the health rows |
| `internal/config/layers.go` | `LayerSpec` / `LayerSeat`, the five config keys, `ValidateLayers`, `AgentContextCapBytes` |
| `internal/gpuprobe/` | the nvidia-smi parser and runner, host free RAM, index/UUID resolution |
| `internal/core/placed.go` | `core.Placed`, the block every result publishes |
| `internal/servingtmpl/composite.go` | the checked union (`CheckComposite`) and the display-twin fences |
| `internal/tierseed/` | seeding `tier_profile`, `tiers`, `layers` (and filling the bare agent seat) |
| `internal/fleetnode/server.go` | the health rows and the layer-aware job feed |
| `internal/delegate/` | the local decision, the pair-long wait, per-layer remote placement |
| `setup/templates/profiles.json` | `composes` and `layers` for `blackwell-3x16` |
| `setup/install.ps1` | the Windows parity copy of the composite seed |
| [ADR 0039](../architecture/decisions/0039-a-box-is-the-union-of-its-tiers-and-placement-is-a-per-task-decision.md) | the decision record |
