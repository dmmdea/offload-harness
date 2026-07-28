---
status: Accepted
date: "2026-07-28"
---

# Alias-backed media is declared per tier, and the binding is derived from the seat

## Context

The harness reaches two media capabilities by llama-swap ALIAS rather than by spawning a
process: vision (`vision_model`, an llama-server seat carrying an `--mmproj`) and speech-to-text
(`stt_model`, a whisper.cpp `whisper-server` seat). Everything else media — image, video, music —
is a spawn-per-job subprocess arbitrated by the machine-wide lease of ADR 0018.

Those two aliases were the last capability in the system still floating free of any producer.
`internal/mediacap` derives every FILE-backed route from config plus the files it names, and
records why: *"a declared capability map is not merely stale, it is worse than none."* It
deliberately excludes alias routes, delegating their reachability to a live `/v1/models` diff.
That delegation was sound. What was missing is that **nothing rendered the seat the alias needed.**

Measured on this repo at 0.34.0, not inferred:

| observation | value |
|---|---|
| serving templates defining a whisper seat | **0 of 6** |
| `config.Default().STTModel` | `"whisper-stt"` |
| (tier, OS) pairs whose rendered config could not serve their own binding | **20 of 20** |
| bindings other than `stt_model` that failed closure | **0** |

So every freshly rendered node advertised `stt` to the fleet dispatcher — `internal/fleetnode`
gates that task on `STTModel != ""` — while `local-offload acceptance` failed the same node on
the same alias. It lied to the fleet and failed its own gate at once, out of the box, on every
tier, with no media seat involved anywhere. `vision_model` had already been fixed to `""` with
the comment *"opt-in … no phantom"*; `stt_model` was simply missed in that pass.

The fix cannot be "always template a whisper seat": that forces a whisper.cpp build and a
large-v3-class download onto tiers that never asked, including `cpu` and `amd-gcn`. Media
capability is a property of the hardware class, so it belongs in the hardware class's row.

## Decision

**A tier declares its alias-backed media seats, and the seat is the sole writer of the binding
that routes to it.**

1. `media_seats` in `setup/templates/profiles.json` carries typed seats (`kind: vision|stt`).
   `internal/mediaseat` owns the type, the validation, and the derivation of the config binding.
2. `internal/servingtmpl` renders each seat into the models map **and** into a group. Both,
   always: llama-swap rejects a config whose group names an undefined model, and a model in no
   group silently joins the implicit default group, which swaps and evicts.
3. A seat declares a residency **ROLE** (`swappable` / `resident`), never a group name. Group
   names are a per-template namespace — `heavy`/`support` here, `offload-family` in three win-*
   templates, `architect`/`editor` in win-dual-cuda, none at all in win-cuda-resident — while a
   tier is a hardware class that renders into several of them. Each template maps roles onto its
   own groups with an `# offload-seats:` directive.
4. A tier declaring seats against a template with no such directive is **refused by name**. It is
   never a silent omission: silent capability loss across an OS boundary is the exact failure this
   workstream exists to end.
5. `stt_model` defaults to `""`, matching `vision_model`. A tier earns the binding by declaring
   the seat; a node with a hand-provisioned upstream sets it explicitly.

## Consequences

- The state "bound to a seat that does not exist" is no longer representable from the tier table.
  `TestEveryBoundAliasIsServed` holds the invariant across every tier × every OS it renders for,
  and went red on 20 of 20 pairs before this change.
- `config_seed` may no longer write `vision_model` or `stt_model`; it is refused by name. Two
  writers is how the seat and its binding drifted apart.
- **A node that relied on the old default loses its STT binding on upgrade unless its config sets
  it explicitly.** This is the migration cost of the fix and it is deliberate: the reference 8 GB
  node was silently depending on a default whose seat it happened to have hand-provisioned.
- Windows tiers can declare seats but no win-* template maps roles yet, so those pairs refuse.
  That gap is now LOUD and enumerable instead of invisible.
- Image/video/music stay out of this schema. There is no `sd-server` client in the repo, and
  hosting one inside llama-swap would put its TTL/group machinery and the ADR 0018 lease in
  charge of the same card simultaneously.

## Alternatives rejected

- **Template every seat.** Forces builds and multi-GB downloads onto tiers that cannot use them.
- **Per-tier group names.** Not expressible: one tier renders into templates with disjoint group
  namespaces, so any name it picked would be simultaneously valid and invalid for itself.
- **A tier-level `swap_groups` block.** Hands a tier author the ability to set `exclusive: true`
  on a media group — the shape measured to return 502s for a full TTL after any render. Group
  FLAGS are measured invariants and stay in the reviewed template; the tier declares membership.
- **Leave `stt_model` and rely on doctor's alias diff.** The diff already reports it. The problem
  is that it reports it on every node, so the signal had been trained into noise.
