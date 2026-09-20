---
status: Accepted
date: "2026-09-20"
---

# ADR 0055 — llama.cpp prompt-cache tiers: host-RAM cache per RAM tier, SSD slot save/restore driven by the harness

Status: ACCEPTED (Layer 1 shipped 0.131.3, 2026-09-20; Layer 2 planned — `plans/2026-09-20-llamacpp-prompt-cache-tiers.md`, operator-approved 2026-09-20)

## Context

Every llama-server in the fleet (b9934 on binxarn, b10964 on the Qube) carries an automatic
host-RAM prompt cache: `--cache-ram N` (MiB, default 8192, `0` off). When a slot's KV is evicted
by a new request the server parks it in RAM and restores it the moment a later request shares
the prefix. The agent loop's steps share a growing prefix and re-delegated context docs share
their whole prefix, so the harness already benefited — at one fixed figure on every box, from a
4 GB Vivobook to a 64 GB Lenovo, and with nothing surviving the 5-minute idle unload.

The same server exposes `--slot-save-path` plus `POST /slots/{id}?action=save|restore`, an
explicit save of one slot's KV to a file and back. The harness never called it. LMCache does
not attach to llama.cpp (it is a vLLM component; ADR 0045 covers the vLLM seats), so this is
the llama.cpp side of the same idea.

## Decision

**Layer 1 (this release):** the renderer emits `--cache-ram __CACHE_RAM__` on every llama.cpp
seat, resolved from a top-level `profiles.json` map `cache_ram_mib_by_ram_tier` keyed by the
`hwdetect` RAM tier (min / low / mid / high). Starting values 1024 / 2048 / 6144 / 12288 MiB.
An unresolvable tier renders the server default (8192) — the renderer never emits `0`, which
would disable the cache. vLLM proxy entries never carry the flag.

**Layer 2 (planned):** templates add `--slot-save-path <prefix>/kvslots/<seat>/`; the fleet
node gains `POST /fleet/kvslot/save|restore {seat, key}` wrapping the server's slot API, with
the key = `k1-` + sha256 over `model_file, ctx, kv_k, kv_v, build_family` + the byte-exact
system prompt and context docs in contract order, so a file is only ever restored into the
identical seat shape (a mismatch is a clean 409/404, never a crash); an LRU sweeper keeps the
directory under a per-tier cap. The delegator computes the key when it packs a contract,
restores before the first planner turn on a node that advertises `kvslot: true`, saves after
the first turn, and records `kvslot_restore: hit|miss|skip` on the ledger row. Both calls are
best-effort with a 2 s timeout; a contract never blocks on the cache.

## Consequences

- RAM tier now sizes a third thing (after the 26B placement and the RAM-gated seeds).
- The gains are measured, not assumed: the plan's Task 3 runs the grounded digest fixtures
  twice per node and revises the map from the numbers before the figures are called tuned.
- Files under `kvslots/` are portable between nodes serving the identical seat shape; pointing
  the Qube pair and the Lenovo at the existing kvcache share is a later task, after the
  single-node gain is on the record.
- `setup/render.tests.ps1` asserts the flag; changing the map is a profiles.json change and goes
  matrix-first like every tier change.
