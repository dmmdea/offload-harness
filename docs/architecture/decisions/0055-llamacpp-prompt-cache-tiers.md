---
status: Accepted
date: "2026-09-20"
---

# ADR 0055 — llama.cpp prompt-cache tiers: host-RAM cache per RAM tier, SSD slot save/restore driven by the harness

Status: ACCEPTED (Layer 1 shipped 0.131.3, 2026-09-20; Layer 2 node side shipped 0.132.0, 2026-09-21; its delegator side planned — `plans/2026-09-20-llamacpp-prompt-cache-tiers.md`, operator-approved 2026-09-20)

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

**Layer 2, node side (0.132.0):** templates add `--slot-save-path <prefix>/kvslots/<seat>/`; the fleet
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

## Layer 2 as shipped on the node (0.132.0) — and the measurement that stops it there

The endpoints are built, tested and correct against the real API. The capability underneath
them is INERT on the build the fleet runs, and this section is the measurement that says so,
taken on binxarn (llama.cpp b9934, Qwen3.5-4B UD-Q4_K_XL, 32k window, Vulkan) 2026-09-21.

The call shape is right — measured, not assumed:

    save    {"id_slot":0,"filename":"…","n_saved":3231,"n_written":158642712,"timings":{"save_ms":255}}
    restore {"id_slot":0,"filename":"…","n_restored":3231,"n_read":158642712,"timings":{"restore_ms":25}}

and a server started without `--slot-save-path` refuses with
`501 "This server does not support slots action"`, which is why the flag is rendered per seat
and why the node's own 501 means "older render, fall through".

**The restore buys nothing.** One prompt of 3,230 tokens, every arm on a freshly started seat:

| arm | prompt_ms | tokens actually processed |
|---|---|---|
| cold, no restore (the baseline) | 27,218 | 3,230 |
| restore, then the same prompt | 27,217 | 3,230 |
| restore, then the same prompt pinned with `id_slot: 0` | 27,716 | 3,230 |
| restore, then the same prompt through the raw `/completion` endpoint | 27,198 | 3,218 |
| **control — same process, same prompt twice (the Layer 1 RAM cache)** | **27,237 → 4,551** | **3,230 → 516** |

Three independent call shapes, one null result; the control in the same run shows the RAM
cache doing exactly what Layer 1 promises (6x, 84 % of the prefill skipped). `GET /slots` names
the mechanism: after a normal request slot 0 carries `n_prompt_tokens`,
`n_prompt_tokens_processed` and `n_prompt_tokens_cache`; after a restore of the same 3,231
tokens it carries **none of them**. The file restores the KV cells and not the bookkeeping the
prefix matcher reads, so the next request re-prefills from zero.

This is upstream, not ours: ggml-org/llama.cpp issue #25913 ("/slots save/restore silently
loses all prompt reuse — checkpoints are never persisted"; the in-memory cache stores a whole
`server_prompt` WITH checkpoints, which is precisely why RAM reuse works and disk reuse does
not) with an open fix in PR #26004, and issue #24746 ("explicit slot requests bypass prompt
cache restore"), which is why the `id_slot` arm is the slowest of the three.

**Consequences, decided:**
- The node lane ships as built: correct, bearer-gated, 501-safe, and called by nobody.
  Reverting it would throw away verified work and guarantee we rediscover all of this.
- **The delegator side of Layer 2 is BLOCKED**, not deferred. Building key computation,
  restore-before-first-turn and the ledger fields on top of a capability measured at zero
  would be work that cannot pay, and a `kvslot_restore: hit` on a ledger row would be a lie.
- The unblock condition is explicit: a llama.cpp build carrying PR #26004 (or equivalent),
  re-run the table above on binxarn, and only a row where the restore arm beats the baseline
  reopens the delegator work.
- Layer 1 is unaffected and is where the measured win lives today.

What the renderer does NOT do: the whisper, embedding and reranker entries take neither flag
(they build their own command lines and have no chat KV worth saving); only the chat and agent
seats and their CPU twins carry them. On a tier whose render knows no install home the
slot flag is empty, so an older caller renders exactly what it rendered before.

One operational number for whoever unblocks this: a slot file is roughly 50 MB fixed plus
33 KB per cached token — 61 MB for 344 tokens, 151 MB for 3,231, and about 1.1 GB for a full
32k window. The 8 GiB default cap therefore holds ~7 full-window slots, and the per-tier caps
in the plan need to be set from that arithmetic, not guessed.
