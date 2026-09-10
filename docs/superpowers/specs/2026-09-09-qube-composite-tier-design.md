# One box, three tiers: the Qube as a composite of blackwell-16 / 2x16 / 3x16 — design

Date: 2026-09-09 · Status: DRAFT, awaiting operator review · Path: architectural
(touches tier identity, placement, the delegate gate, health, status, the serving renderer,
docs, tier matrix)

## Why

Operator directive, 2026-09-08: *"qube now must be part of 3 tiers at the same time,
blackwell 16, blackwell 2x16 and blackwell 3x16, dynamically switching and routing models to
the optimal path depending on task needs."*

What is already true: the 3-card llama-swap config **serves all three shapes** — single-card
seats (the cascade rungs e2b/e4b/12b/26b on device 0; OCR vl-8b, STT, embed/rerank on
device 2), pair seats (`agent-pool` 27B vLLM with the Lenovo cache server, vl-32b, the 27B/31B
llama.cpp agents on 0+2) and the 3-card opt-ins (`qwen3.8-flash-next` 131k/262k,
`qwen3.8-27b-vllm-3card`). 21 models are served today.

What is not true, and what this spec changes:

1. **Identity is one id.** `installed.json`, health and the tier matrix say `blackwell-3x16`;
   nothing states that the same box is a complete `blackwell-16` and a complete
   `blackwell-2x16`, so fleet routing, the matrix and a collaborator install all reason from a
   single row.
2. **No context-aware seat choice.** The agent lane is always `agent-pool` (window 163,840).
   A contract that does not fit is refused as `defer_class: contract`
   (`internal/delegate/fit.go:137`) or trimmed (`pipeline.go:477`) — it is never escalated to
   the 3-card 262k seats, which are reachable only by naming the model.
3. **No saturation-aware layer switch.** When the pair seat is full (`seatload.Inflight`,
   `run.go:1762`) work queues or goes remote; the single-card layer that could take a second
   stream sits idle. The directive in the plan's section 1 — hardware must be USED — is not met.
4. **Placement is invisible.** A result does not say which cards or seat served it.

## What already exists and is reused unchanged

- The per-task logistic entry-tier router (`internal/router`) that picks a cascade rung for
  mechanical text — that IS the single layer's placement; untouched.
- `seatload.Inflight` (live in-flight count on a seat) and the delegate gate's capacity rows
  (`internal/delegate/gate.go`, `fit.go`: `EstTokens + specReserve <= AgentCtxTokens`).
- `vllm_seat.fallback_agent_model` (the pair's llama.cpp 27B) and the measured 3-card seats in
  `llama-swap.win-triple-blackwell.yaml`.
- The desktop floor and the "never while the operator is at the desk" rule for the 5070 Ti
  (ADR/notes for blackwell-3x16, `TestTripleBlackwellNeverSchedulesOntoTheDisplayCard`), and the
  existing operator-presence pause that already stops local-offload work when a game takes the
  machine.
- ADR 0037/0038 shape: a placement block in every result.

## Decisions (D1–D8)

### D1 — A tier may `compose` other tiers; the box keeps ONE installed id

`profiles.json` `blackwell-3x16` gains:

```json
"composes": ["blackwell-16", "blackwell-2x16"],
"layers": {
  "single": {"tier": "blackwell-16",   "devices": ["0", "2"], "role": "residents on 0, singles on 2"},
  "pair":   {"tier": "blackwell-2x16", "devices": ["0,2"],   "role": "agent-pool, vision, media pools"},
  "triple": {"tier": "blackwell-3x16", "devices": ["0,1,2"], "opt_in": true, "display_floor_gib": 4}
}
```

`installed.json` keeps `profile: blackwell-3x16` (a collaborator's 2-card box is unaffected; the
matrix keeps one row per tier). Health and `offload_status` advertise **`tiers:
["blackwell-16","blackwell-2x16","blackwell-3x16"]`** and the `layers` map with live occupancy.
*(Operator decision 3 — default taken: one id + `composes`. Override: a `tiers` list in
installed.json.)*

### D2 — Placement is a per-task decision among layers, recorded in the result

A new `internal/placement` package answers one question for every unit of work on a composite
box: **which layer**. Inputs: task class, estimated tokens (prompt + completion budget), the
contract's quality gate (has acceptance checks / output_schema), live layer occupancy, and the
display-card guards. Output: `{layer, seat, devices, reason}`, carried as `placement` on every
result (loop, MCP, fleet job), next to the existing node/seat fields.

| task class | default layer | escalates to | overflows to |
|---|---|---|---|
| mechanical single-shot text (summarize/classify/extract/triage) | single (router rung) | — | — (already the cheapest) |
| agent / delegation contract | pair (`agent-pool`) | **triple** when `est_tokens + max_tokens > pair window` | **single** (`agent_overflow_model`) when the pair is saturated AND the contract is best-effort |
| vision — `ocr` role | single (vl-8b, device 2) | — | — |
| vision — `vision` role | pair (vl-32b) | — | — |
| image / video / audio | pair pools (existing keys) | — | — |

Two new tier-seeded config keys carry the extra seats: `agent_long_model`
(`qwen3.8-flash-next-262k`, window 262,144, layer triple) and `agent_overflow_model`
(`gemma-4-26b-agent`, window 131,072, layer single). Boxes that seed neither have exactly one
layer and today's behaviour, byte-identical (pinned).

### D3 — Overflow to the single layer is for best-effort work only

A pair-saturated box (in-flight ≥ `max_num_seqs`, or `seatload` busy) sends a contract to the
single layer's 26B **only when the contract carries no acceptance checks** (an `agent_run`, a
mechanical digest); a contract with acceptance checks or an `output_schema` **waits for the
pair** (the existing placement wait, `agent_placement_wait_sec`) or goes remote per today's
gate. Quality-gated work never trades the 27B (15/15 D1) for the 26B (12/15) to save a queue.
*(Operator decision 1 — default taken: allowed for best-effort only. Override: never / always.)*

### D4 — The triple layer is entered on window overflow or explicit ask, never on load

The 3-card seats join **only** when the pair window cannot hold the contract, or when the
caller says `context_class: "long"`. Never because the pair is busy: the 5070 Ti is context/KV
only. Admission additionally requires (a) ≥ `display_floor_gib` free on device 1 **read live
from nvidia-smi at admission**, and (b) the operator-presence pause reports the machine idle
(the same signal that already pauses local work when a game takes the desk). Either guard
failing is a `defer` whose reason names the guard — never a silent fall-back to trimming.
*(Operator decision 2 — default taken: window overflow + explicit only.)*

### D5 — The composite render is structural, not hand-copied

`servingtmpl` reads `composes`: every seat the composed tiers declare must appear in the
composite render with the composite's device map (single → devices 0/2, pair → 0,2), and a
test fails the render if a composed tier's seat is missing or lands on a device the layer does
not own. This turns today's hand-maintained triple template (which twice shipped an unadapted
copy of the 2x16 media block) into a checked union. The 3-card opt-in seats stay declared in
the composite tier only.

### D6 — The fleet sees three capacity rows, not one

`NodeView` gains `Layers []LayerView{Name, Devices, Seat, CtxTokens, Inflight, Admissible}`
decoded from health; the delegate gate scores a composite node **per layer**: a pair-busy Qube
is still eligible for a mechanical contract on its single layer, and a 200k contract that fits
only the triple layer is placed there (subject to D4) instead of being refused as too big for
every node. Nodes without `layers` decode to one implicit layer (today's `AgentCtxTokens`).

### D7 — Status and UI

`offload_status.local` gains `tiers` and `layers` (devices, seat, window, in-flight, admissible
+ reason); the fleet overview page draws the Qube as three stacked layer cards. `placement.layer`
appears in `agent_delegate` / `agent_run` results and in the ledger, so the utilization
scoreboard can say how much work each layer took.

### D8 — Versioning, docs, matrix

0.116.0. New `docs/systems/composite-tier.md`; ADR 0039 *"A box is the union of its tiers, and
placement is a per-task decision"*; `docs/tiers` regenerated; tier matrix: the Qube's row
carries `composes` and the evidence sheet gets the three live gates below.

## Out of scope

- Pinning any tier seat to the 5070 Ti (operator rule, unchanged).
- Pipeline-parallel vLLM on three cards as the delegation lane (measured failure, 2026-09-07).
- Composites on single-card boxes (Lenovo, Aorus): one layer, nothing changes.
- Cross-box "layers" (that is the fleet, which exists).

## Testing

- **Unit:** the placement decision table (every row above, both guards, each override key);
  window-overflow escalation chooses `agent_long_model` and records `placement.layer=triple`;
  saturation + acceptance ⇒ wait, saturation + best-effort ⇒ single; D4 guards each produce a
  named defer; `composes` render pin (a composed seat missing or on the wrong device fails);
  `NodeView.Layers` decode + per-layer gate scoring; a box with no layers is byte-identical
  (tools/list, health, placement) — pinned.
- **Live gates on the Qube (the only check that counts):**
  1. a mechanical `summarize` while `agent-pool` holds 32 in-flight ⇒ `placement.layer=single`,
     no queue wait;
  2. two concurrent digest contracts, one with acceptance checks and one without, with the pair
     saturated ⇒ the gated one waits for the pair, the best-effort one lands on the 26B with
     `layer=single`, both succeed;
  3. a 200k-token contract with the operator away and ≥ 4 GiB free on device 1 ⇒
     `layer=triple` on `qwen3.8-flash-next-262k`, needle answered; the same contract with the
     operator at the desk ⇒ a defer naming the presence guard;
  4. `/fleet/health` from the Lenovo shows `tiers` (3) and `layers` (3) for the Qube; the
     delegator's `offload_status.fleet` shows the Qube as three capacity rows.

## Operator decisions carried as defaults (say the word to override)

1. Overflow to the single layer: **best-effort contracts only**; quality-gated work waits.
2. Triple layer: **window overflow or explicit `context_class: long` only**, under the floor and
   the presence guard; never because the pair is busy.
3. Identity: **one installed id (`blackwell-3x16`) + `composes`**, advertised as `tiers`/`layers`.
