# Local-Offload — ROADMAP (pointer)

> **The plan of record is plan v3 (2026-09-18), not this file.** This file used to be a second master plan (T1–T6, the stack closeout, the nightshift and round-2 sections, the Flash-Next park, the monthly update process) and drifted from the program register. Since 2026-09-18 it only points; the 2026-08-26 body is kept verbatim in [ROADMAP-2026-08-26-frozen.md](ROADMAP-2026-08-26-frozen.md) for the verdicts it records (FLUX no-go, ik_llama reject, the qwythos bench, the Flash-Next measurements). Register row I-19 closed the reconciliation.

## Where the plan lives

Plan v3 is kept in the operator's ecosystem workspace, outside this repo (the drive letter is per machine: `G:` on the Qube, `D:` on the laptop):

| file | holds |
|---|---|
| `plans/offload-harness/README.md` | law (INV-1…20 with their gates), standing authorizations, the lane board (L1–L6), the session protocol (P-1…P-8) |
| `plans/offload-harness/STATE.md` | the current verified state, one line per surface, rewritten at every session start |
| `plans/offload-harness/lanes/L1…L6-*.md` | the live register rows, six columns, one file per lane |
| `plans/offload-harness/CLOSED.md` | every CLOSED / DROPPED / reference row, verbatim |
| `plans/2026-09-17-harness-scheduling-overhaul.md` | this week's append-only ledger |

Design detail stays where it always was: ADRs under [architecture/decisions/](architecture/decisions/), system docs under [systems/](systems/), the tier docs under [tiers/](tiers/), and `CHANGELOG.md` for what shipped.

## Where the old sections went

| frozen section | plan v3 home |
|---|---|
| T1 Flash-Next (measured, parked) and the three re-eval triggers (MTP head in llama.cpp, a smaller quant that keeps parity, a card-informed differential tier that beats 0.46× decode) | L3 (seats, tiers, measurement): the Flash-Next / big-MoE rows and the kept-for-re-measure weights disposition (A-14, A-97) |
| T1-adjacent NVFP4-MTP agent-seat A/B | L3, row A-14 |
| T2 security + toolchain | L4 (gates), rows H-26 / H-27 |
| T3 cu130 chain, T4 media models, T5 llama.cpp + llama-swap pass, T6 media lease TTL | closed 2026-08-27 (frozen body §STACK UPDATE); leftovers are L3 / L6 rows |
| Stack closeout, round 2, nightshift 2026-08-27 | closed; their verdicts stay in the frozen body and in `CHANGELOG.md` |
| 48 GB gates + 3-card optimization | closed 2026-09-01; the standing gates are L4 rows H-01…H-04, H-24 |
| STANDING PROCESS — monthly whole-system update & upgrade | L6 (integrations, parity): the fleet-update lane; the memory rule `no-version-pinning-frontier-updatable` |
| Historical build order (2026-06 → 2026-07), Parked, Source briefs | frozen body only |

## Rules this file still carries

A tier is a hardware class, never a Windows class; deployment state is never hardened into a spec; quality outranks speed on every tier (INV-5); every idle model unloads at five minutes (INV-2); inference runs on the cards, RAM is overflow only (INV-1). The full text and the gates behind each rule are in plan v3 Part A.
