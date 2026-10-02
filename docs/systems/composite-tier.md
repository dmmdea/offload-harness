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

## The layers on the `ampere-16` node

A composite box does not need more than one card. The `ampere-16` reference node (one NVIDIA A2,
16 GB) declares two layers on that single device (register A-100, 0.129.0), and since register A-113 the
tier table declares them too: a fresh install of the tier seeds the layers the reference node's
hand-edited config carries (see "How the tier table carries it" below).

| layer | tier | devices | seats | chosen when | state |
|---|---|---|---|---|---|
| `single` | `ampere-16` | 0 | agent `qwen38-27b-gsq-vllm` (32,768) | every agent contract the node takes, by row 5b — it is the planner default under a layer name | active |
| `fast` | `ampere-16` | 0 | agent `qwen36-35b-a3b-gsq-vllm` (32,768, `max_inflight` 8) | only when a contract NAMES it (`layer: "fast"`) or names the model | active |

`fast` is the tier's fast DIGEST layer, not a second general-purpose seat. Judged blind its coverage
is **4.65 against the `single` seat's 8.53** — faithful, no fabrications, but shallow — so it is never
the node's agent seat, free choice never lands on it (row 4b: a non-default layer is reached by name
only), and it is for digest-shaped contracts. It is not a swap for `single`, and a judgment or
coverage contract does not belong on it.

Every vLLM seat the tier declares — the 27B lane seat and the 35B — is **storeless by measured
declaration** (register B-01, re-measured 2026-09-18), so `doctor` prints a storeless-OK line per seat
instead of failing it for a missing cache-server binding (ADR
[0045](../architecture/decisions/0045-a-cache-server-binding-per-vllm-seat.md)): the MP server's own
~690 MiB CUDA context beside the resident embedder OOMs the engine at `util 0.90` on a 16 GB card.
(The 27B GSQ seat's cache path was also blocked, until 2026-09-18, by LMCache 0.5.4 × vLLM 0.29, register
D-117, which is the state [ADR 0049](../architecture/decisions/0049-ampere-16-vllm-seat-is-the-3bit-gsq-27b.md)
records. LMCache 0.5.5 carries the kv-layout fix, so that blocker is cleared and the VRAM cost above is the
reason that stands.) The reason each seat carries is its own `storeless_reason` in the tier table, seeded
verbatim into its binding. (The reference node also lists a third seat, the 4B `qwen3.5-4b-vllm` rollback unit, which
is hand-installed and not part of the tier: its binding is that node's own, and `audit-config` reports the
two roster keys as DIFFERENT against it until the operator declares or retires it.)

### How the tier table carries it

`setup/templates/profiles.json` declares, for `ampere-16`, the two layers above, the 27B as `vllm_seat`
(the agent lane) and the 35B as `extra_vllm_seats` (a seat served on demand beside the lane seat on the
same card, never the agent lane: [ADR 0048 Amendment 2](../architecture/decisions/0048-vllm-is-a-first-class-engine-on-every-tier.md)).
Each layer names its seat explicitly, so the values are the reference node's own (the ones
`internal/placement` and `internal/delegate` pin) and `audit-config` reports MATCH for `layers`, `tiers` and
`tier_profile` against a fixture that carries them (a live extract of that node redacts each layer seat's
`ctx_tokens`, so the 32,768 is the pinned value, not a live reading); a layer seat that names a vLLM seat must equal
that seat's `max_model_len` (and its `max_num_seqs` as `max_inflight`, when set), or the table is refused
at parse. What a box seeds and renders depends on which seats it can run, decided per seat by the same
prerequisite check (`vllmseat.Spec.Detect`: the hand-built venv plus that seat's own weights; for the 35B,
`DetectExtra` also wants the two wrapper scripts the operator installs, step 4 below):

| the box runs | `layers` | `vllm_seats` and `kv_cache_server` | llama-swap entries |
|---|---|---|---|
| the 27B and the 35B | `single`, `fast` | both seats, one storeless binding each | both, as alternatives |
| the 27B only | `single` | the 27B | the 27B |
| the 35B only | none | the 35B | the 35B |
| neither (a plain llama.cpp box) | none | none | neither |

A box that lacks the 35B's weights, or the wrapper scripts its entry runs, therefore never advertises the
`fast` layer or its seat, and a box that lacks the 27B's seeds no layers at all: `single` is the planner-default layer (row 5b below), and a
layer set that lost it would make the node ineligible for every contract it ran the day before, so the
box stays a plain box instead. `install render` refuses a layer that names a seat the rendered config
does not define, for any tier that declares layers.

The two seats cannot both be loaded (11.85 + 11.2 GB of weights against 15.4 GB usable; a warm swap
between them costs about 50-70 s), so the renderer emits every vLLM seat of a tier as an ALTERNATIVE of the
others inside the residents set — `emb & rer & (vagt | vagt2)` — never as co-resident members. A
matrix that called the pair a valid combination would have llama-swap load the second beside the first,
an out-of-memory at the engine's first allocation. The seats stay resident-class: an ordinary chat request
never evicts the agent lane, and asking for the 35B by name swaps it in.

Beside whichever seat is loaded the card keeps the memory stack's support models, the embedder (about 460 MiB) and the
reranker (306-378 MiB, by its batch size). The memory embedder has absolute priority (operator order, 2026-09-30) and the
reranker stays resident too, with about 1 GiB of the card free (operator order, 2026-10-01): the seats make room for
them. So each seat's `gpu_memory_utilization`, `max_num_seqs` and `max_num_batched_tokens` are capped at the point measured
to leave both room on the 15,356 MiB card — the 27B at util 0.84 with 4 sequences, a 2,048-token batch and a fixed 1.42 GiB KV
pool (`kv_cache_memory_bytes` 1,524,713,390), the 35B at util 0.855 with 8 sequences and a 4,096-token batch ([ADR 0049](../architecture/decisions/0049-ampere-16-vllm-seat-is-the-3bit-gsq-27b.md)
Amendments 4, 5 and 6) — and `ampere16_coresidency_test.go` fails when either rises above it. The first declaration (util 0.90,
32 sequences) ran the engine at 14,788 MiB and kept the embedder from loading for 35 minutes; the second (util 0.87, 8
sequences, 4,096 batched tokens) left 639 MiB, and the reranker failed to start beside the loaded seat 7 times (register A-122b).

The 27B's KV pool is pinned, not sized by utilization (Amendment 6, register A-129): vLLM measures a utilization-sized pool
during startup profiling, device-wide, so a cold compile cache or a memory-stack model loading in that window shrinks it below
one 32,768-token request and the start fails (`To serve at least one request...`). The linux-systemd run script renders the pin
as `--kv-cache-memory-bytes`, and vLLM then ignores `gpu_memory_utilization`, which stays declared as the point the engine's
workspace was measured at. Both seats run vLLM 0.30.0 on the reference box since 2026-10-02 (register A-119). The A2 throttles
under sustained decode (register A-130), so a tok/s declared for either seat is a cool-card figure, not a sustained one.

### What an operator still installs by hand on a fresh `ampere-16` box

The installer detects prerequisites and never builds them, and it does not render the 35B seat's unit:

1. **The vLLM venv** (vLLM 0.30.0; the 27B checkpoint also needs its shipped embedding patch) and the
   `--vllm-venv` / `--hf-home` flags naming it, passed to `install seed`, `install render` and
   `audit-config` alike (with `--vllm-user` and `--vllm-proxy-host` for the render, and `--vllm-seat-dir`
   to all three when the seat directory is not `<home>/seat`).
2. **The weights**: exactly one snapshot each under the HF home, `hub/models--ISTA-DASLab--Qwen3.8-27B-3Bit-GSQ`
   and `hub/models--ISTA-DASLab--Qwen3.6-35B-A3B-2Bit-GSQ`. A seat with no snapshot is skipped, with the
   reason printed, and so is its layer.
3. **The 27B seat**: `local-offload install vllm-seat --profile ampere-16 …` renders its unit, wrappers and
   polkit rule and prints the two root steps.
4. **The 35B seat's unit, wrapper scripts and polkit rule.** Its production launch line carries
   `--language-model-only`, which the shared `linux-systemd` run script cannot express, so `install vllm-seat`
   does not render it. Until its two wrapper scripts are in the seat directory, `install seed` and
   `install render` leave the 35B and the `fast` layer out and name the missing file: llama-swap does not
   check that an entry's `cmd` exists when it loads its config, so a seat advertised without them would be
   listed, bound and layered and would fail only when a contract asked for it. Copy the 27B's rendered
   files under the 35B's unit name and change exactly what differs: the unit is `vllm-35b-seat` (no
   `[Install]` section: llama-swap owns its lifetime); the wrappers are `vllm-35b-seat-run.sh`,
   `vllm-35b-seat-cmd.sh` and `vllm-35b-seat-cmdstop.sh` in the same seat directory (`cmd` and `cmdStop`
   start and stop `vllm-35b-seat.service`); the polkit rule is the 27B's with the unit name changed. In
   the run script, point `--model` at the 35B snapshot, set `--served-model-name
   qwen36-35b-a3b-gsq-vllm a2-pool-35b qwen36-35b-gsq`, `--max-num-seqs 8`, `--max-num-batched-tokens 4096` (the 27B renders 2,048 since ADR 0049 Amendment 5; the 35B
   was measured at 4,096), `--gpu-memory-utilization 0.855` and `--tool-call-parser qwen3_coder`, and add `--language-model-only`;
   **delete** the 27B's `--kv-cache-memory-bytes 1524713390`: that is the 27B's pool, vLLM would ignore the utilization beside it,
   and the 35B's pool would be the other model's measurement instead of its own (96,416 tokens at util 0.855). Keep the window the
   table records (32,768) and the utilization above (0.855 on vLLM 0.30.0, the point measured to leave the embedder and the
   reranker room; 0.85 lost 3 KV blocks there).
5. **Then** re-run `install seed` and `install render` with the same flags: `install render` writes both
   llama-swap entries and `install seed` writes the layers, the roster and both bindings. `local-offload doctor` prints a storeless-OK line per seat, `offload_status` lists both
   layers, and a contract with `layer: "fast"` lands on the 35B.

## The decision table

`internal/placement` is the only place a placement is decided. Its rows, in order:

1. **No layers** → the zero decision. The caller publishes nothing and behaves as before.
2. **Mechanical text** (summarize / classify / extract / triage) → the `single` layer's router
   rung. When the pair's agent seat is LOADED, the reason says the single layer time-shares
   the pair's cards and names what it displaces; if the display layer is awake and its guards
   pass, the rung is substituted onto the display twin instead and nothing is displaced. The
   note is documentary. What keeps a rung from actually evicting a loaded vLLM seat is the
   cascade seat guard, which runs on every box, composite or not
   ([offload-pipeline.md](offload-pipeline.md)). A display twin that the matrix runs beside the
   seat passes it untouched.
3. **ocr / vision** → the layer and role that declare them (documentary: media placement is
   not routed through this table).
4. **An explicit `context_class: long`** → the biggest long-context layer the box declares:
   a three-card layer's long seat where one exists, otherwise the pair's, under the same
   eviction rule as row 6.
4b. **A contract that names a layer** (`layer` on the delegation door, register A-100) → that
   layer's agent seat, under the window check and the layer's own guards. The pair keeps row
   5, and `single` on a box that also declares a pair keeps deferring (council R2: its agent
   seat is the one-card identity on the pair's own cards, never an agent placement); every
   other named layer resolves here — this is how a box's SECOND layer, whose agent
   seat is not the planner default (<node-c>, ampere-16: its `fast` layer = the 35B digest
   seat beside its 27B GSQ on the one card), is reachable at all. A seat another layer holds
   loaded on the same card is named as the displacement (recorded, never acted on; llama-swap
   serialises the swap behind the loaded seat's in-flight work). The delegator decides FOR the
   named layer too — on its own box (`runner.decide`) and over every remote's rows — so a node
   that does not declare it is ineligible for that contract, an idle local box that does not
   declare it does not keep it, the free choice never overwrites the caller's layer on the
   dispatched copy, and with no node declaring it the contract defers naming the layer.
   The delegator keeps that rule at every place it picks its own seat (register A-108):
   `route=spread` never deals such a contract the local slot of a box that does not declare
   the layer (it deals among the remotes that do, and the overflow waits in line for them),
   `route=auto` never takes the idle-local shortcut for it (the roster is read for it although
   nothing is busy), and neither the capacity wait, a re-placement's local last resort nor a
   verification retry hands it to that seat.
5. **An agent contract that fits the pair's agent window** → the pair's agent seat, always.
   A saturated pair (in flight ≥ max_num_seqs) is RECORDED in the reason and nothing is
   re-placed: no other layer can hold that contract beside a loaded pair.
5b. **A box that declares no pair** (one card) → the `single` layer's agent seat: the planner
   default under a layer name. Without this row a one-card node's first layer declaration made
   it ineligible for every contract it ran the day before (the delegator's gate is "the table
   places it"). The free choice never lands on a second layer — that one is by name only.
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
| `internal/tierseed/` | seeding `tier_profile`, `tiers`, `layers` (filling a bare agent seat, dropping a layer whose vLLM seat the box does not run: `ResolveLayers`) and the extra seats' roster and bindings |
| `layer_coverage_test.go` | the regression floor: a tier that stops declaring a layer, a layer role or an extra vLLM seat fails by name |
| `internal/fleetnode/server.go` | the health rows and the layer-aware job feed |
| `internal/delegate/` | the local decision, the pair-long wait, per-layer remote placement |
| `setup/templates/profiles.json` | `composes` and `layers` for `blackwell-3x16`; `layers`, `vllm_seat` and `extra_vllm_seats` for `ampere-16` |
| `setup/install.ps1` | the Windows parity copy of the composite seed |
| [ADR 0052](../architecture/decisions/0052-a-box-is-the-union-of-its-tiers-and-placement-is-a-per-task-decision.md) | the decision record |
