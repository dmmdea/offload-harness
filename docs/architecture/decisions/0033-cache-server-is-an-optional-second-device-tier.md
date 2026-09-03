---
status: Accepted
date: "2026-09-03"
---

# 0033 — A second device's RAM is an optional KV tier, scored on capacity at parity cost

- Deciders: operator (2026-09-02 16:10–16:30, "cache server, it's not actually remote"), harness session
- Related: [0023](0023-agent-lane-tailnet-auth-and-locality.md) (locality), [0032](0032-a-peer-held-seat-is-waited-for-not-deferred.md)

## Context

A vLLM seat's usable KV in VRAM is small on 16 GB cards, and on a Mamba-hybrid model the pool's
banner overstates it (align-mode checkpoint blocks share the pool). Contexts that leave VRAM are
recomputed. The 2026-09-01 measurement had concluded that an LMCache tier "recovers no time"; the
2026-09-02 research pass showed that run had sized the tier below the pool it backed, and the
re-measurement showed a 24k-token context coming back from same-box RAM in 0.50 s (49.7×) and from
the Lenovo's RAM over the LAN in 20.6 s against a 24.6 s recompute (1.2×) — and, with one store namespace
per stack generation, in 3.86 s against 24.68 s (6.4×, 2026-09-03).

The operator's metric for the second-device route is capacity, not speed: memory the serving PC
does not have to give, with the GPU free while the load streams, and contexts that survive a swap.
The repository's contract is that it installs on any PC.

## Decision

1. The second-device KV store is a **"cache server" tier** — that name, not "remote" — declared by an
   optional `kv_cache_server` block, **off by default**, never a dependency of the install or of any
   single-box seat. An install that never mentions it behaves exactly as before.
2. It is scored on capacity at parity-or-close cost. Parity is success.
3. The harness records and reports the wiring (`config.Load` validation by key name, `offload_status`
   block with a reachability fact); it does not run the store or the engine. Reference templates for
   the seat wrapper, the llama-swap entry and the store's systemd unit ship under
   `setup/templates/vllm-seat/`.
4. The store address must be private (LAN or tailnet); an `fs_native` address must be an absolute
   mounted path, never a URL or host:port. Bulk KV prefers the direct LAN.
5. Fidelity before hit counters: a seat is bound to the harness only after a planted needle is
   retrieved verbatim after eviction and after an engine restart.

## Consequences

- Capacity: ~175k tokens per 45 GB of store at ~255 KB/token, on top of VRAM, without the serving
  box's RAM.
- One namespace per stack generation: objects written under another engine layout, KV dtype or
  LMCache build are unreadable; the prefix must change or the store be flushed.
- Known limit (2026-09-03): LMCache's Valkey adapter cannot serve a three-stage pipeline seat of a
  64-layer / 16-attention model (stages hold 6/5/5 attention layers; reads for the odd rank fail on
  size). Two-card tensor-parallel seats use the store; three-card seats use the same-box RAM tier
  until an upstream fix or a validated `fs_native` path.
- fp8 KV does not buy a longer context on 16 GB cards for this model: the doubled block forces a
  larger batch whose activation memory eats the saving.

## Alternatives considered

- **Same-box RAM only (L1 32–48 GB).** Fastest (warm-in-VRAM latency) but consumes the serving PC's
  RAM, which the operator reserves for llama-swap seats, MoE experts and ComfyUI. Kept as the tier
  for three-card seats, not the default.
- **vLLM's native CPU/disk offload.** Pathological on the hybrid under WSL2 (decode collapse, worker
  hang); the upstream hybrid offload planner (vLLM PR #38261) is unmerged.
- **Making the tier part of the install.** Rejected: the repo must install on one PC; a second device
  is a bonus, declared when present.

## Related code

- `internal/config/kvcacheserver.go` — the block, defaults, validation.
- `internal/mcpserver/mcpserver.go` — `kvCacheServerView` in `offload_status`.
- `setup/templates/vllm-seat/` — seat wrapper, stop script, llama-swap entry, store unit.

## Related docs

- [systems/cache-server.md](../../systems/cache-server.md)
- [OPERATOR-GUIDE.md](../../OPERATOR-GUIDE.md) — "Cache server — an optional second device holding evicted KV (0.112.0)"
