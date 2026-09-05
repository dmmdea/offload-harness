# Cache server (optional second-device KV tier)

## Purpose

Give a vLLM seat more memory than its cards hold, from a second machine's RAM, without loading the
serving PC. A context that fell out of VRAM comes back from the store at parity cost with recomputing
it, the GPU stays free while the load streams, and the context survives a seat swap. It is a
capacity tier, scored on what it adds, not on speedup (operator directive, 2026-09-02).

## Questions this doc answers

- What is the "cache server" and when does the harness care about it?
- What is measured, on which hardware, and what does it not do?
- How is it declared, and what happens on a box that never declares it?
- Which engine layouts can use a store, and which cannot (and why)?

## Scope

The `kv_cache_server` config block, its validation, the `offload_status` report, and the reference
seat templates under `setup/templates/vllm-seat/`. The harness does **not** run the store or the
engine: those are the operator's llama-swap seat and a store process on the second device.

## Non-scope

llama.cpp seats (no external KV connector exists for them); the fleet node protocol (a fleet node
runs contracts, a cache server holds KV pages — different roles, possibly the same machine); vLLM's
native CPU/disk offloading (measured unusable on the Mamba-hybrid 27B under WSL2, 2026-09-01).

## Key concepts

- **Seat** — a llama-swap model entry; here a vLLM engine with the LMCache MP connector.
- **L1 staging** — LMCache MP's pinned host buffer beside the engine (default 8 GB). A staging
  area, not the tier.
- **Store (L2)** — the second device's memory behind LMCache's L2 adapter: Valkey (Redis protocol,
  measured) or a filesystem export (`fs_native`).
- **Chunk** — LMCache's unit of KV, in tokens; must equal the engine's unified block size for the
  model (784 for Qwen3.8-27B with fp16 KV; fp8 KV doubles it to 1568).
- **Key prefix** — the store namespace for one stack generation (engine layout, KV dtype, LMCache
  build). Objects written under another generation are unreadable, not merely stale.

## How the system works

1. The operator declares the block (`enabled: true`, store, address, sizes, seat) in `config.json`.
   Absent or `enabled: false` is the default and changes nothing; a single-box install never
   depends on it.
2. `config.Load` refuses a block that cannot work, by key name: an unknown store, a missing or
   malformed `host:port`, a public address (KV pages are unauthenticated bulk memory — the store
   lives on the LAN or the tailnet only), an `fs_native` address that is a URL or host:port instead of
   an absolute mounted path, negative sizes, and an enabled block with neither `key_prefix` nor
   `seat` (no shared namespace by accident). `address` is trimmed at load.
3. `offload_status` reports the block in both states: `{"enabled": false, "declared": …}` when off;
   when on, the wiring plus a 1 s TCP reachability fact for a Valkey store named by an IP literal
   (a hostname is reported as unprobed; `fs_native` as "no port"), so an agent reading status sees
   "declared but down" before its first contract waits on it. A block the load refused is reported
   as `invalid` and never dialed. The block is declarative: the seat wrapper runs what its own
   `seat.env` says, and the status note reminds the operator to keep the two in agreement.
4. `fs_native` over a network share is the measured transport of choice (Lenovo tmpfs over SMB 3.1.1: a
   23.7k-token prefix back in 2.6–2.9 s at fp16 and 0.80 s at fp8 KV, vs 3.8 / 0.92 s through Valkey;
   `--l2-prefetch-policy` / `--l2-store-policy` variants gained nothing; the legacy `fs` adapter was slower).
   The seat wrapper mounts the share before the MP server starts when `SEAT_L2_MOUNT_SRC` / `SEAT_L2_MOUNT_DIR`
   (and optionally `SEAT_L2_MOUNT_OPTS`, `SEAT_L2_MOUNT_TYPE`, default `cifs`) are set, and REFUSES to start when
   the mount fails — an unmounted base_path is a local directory the adapter writes to, so the seat would look
   healthy while the cache server held nothing. The share is named by a hostname the box resolves (tailnet
   MagicDNS or static DNS), never a DHCP address — a vanished lease refused every seat start for hours on
   2026-09-04 — and the refusal message says why (does not resolve / port unreachable / share refused).
   `SEAT_L2_MIN_MBPS` (default off) is a write floor measured with a 64 MiB fsync probe after the mount: a path
   that crawls (4.6 MB/s over a Wi-Fi hop, measured) makes the tier slower than recompute, so the seat refuses
   rather than serving a useless tier; the same-box tier is the fallback. A three-stage pipeline seat gets
   nothing from any L2 (Valkey or fs_native): keep it on the same-box tier.
4b. The seat wrapper refuses to start when its port is already bound (a foreign listener would otherwise pass
   llama-swap's health check and serve the seat's traffic — measured 2026-09-03), and names its MP server unit
   from `SEAT_MP_UNIT` (default `lmcache-mp`). A benchmark or scratch engine in the same box must run on its own
   port, its own MP unit/port and its own served model names; it must never reuse the seat's.
4d. `seat_stop.sh` captures the API server's process tree before killing it and reaps the engine children
   (`VLLM::EngineCore`, `VllmWorker-N`) that outlive it, plus any parentless engine process; it then reads the
   seat devices back and warns, naming the holders, when they still hold VRAM. A stop that arrives mid-request
   left an EngineCore alive for 8 minutes on 2026-09-05 with the port free — llama-swap saw a clean unload.
4c. Before the engine starts, the wrapper waits (`SEAT_VRAM_WAIT_SEC`, default 60) for every seat device to
   fall below `SEAT_VRAM_FLOOR_MIB` (default 1024) and names the holders if they do not — a start a few
   seconds after a swap-out found the cards still holding the previous engine and vLLM refused the KV pool
   (two cold loads in a row, 2026-09-04). It warns; the engine's own error stays the final word.
4a. The seat wrapper (`setup/templates/vllm-seat/seat_fg.sh`) starts the LMCache MP server with the
   L1 size, chunk and L2 adapter, then the engine in the foreground of the llama-swap client, so a
   swap-out reaps the engine while the store keeps the pages.

## Important flows

- **Evict-then-reuse** — a prefix leaves VRAM (flooded out or swapped out); the next request's
  lookup finds it in L1 or L2 and vLLM skips the prefill for those tokens. Proof of use is vLLM's own
  `vllm:external_prefix_cache_hits` counter, never time-to-first-token alone.
- **Swap-in** — llama-swap restarts the seat; the store still holds the pages; the first requests
  refill VRAM at parity cost instead of recomputing.

## Data and state

The store holds ~255 KB per token as measured (Qwen3.8-27B, fp16 KV, two-rank layout); a 45 GB
Valkey holds ~175k tokens. No persistence: a store restart empties the tier, which is safe (pages
are recomputed on demand). The key prefix is the only namespace; flush the store or change the
prefix whenever the engine layout, the KV dtype or the LMCache build changes.

## Interfaces and entry points

- `config.json` → `kv_cache_server` (see `internal/config/kvcacheserver.go` for every field).
- `offload_status` → `kv_cache_server` block.
- `setup/templates/vllm-seat/` → reference seat wrapper, stop script, llama-swap entry, and the
  second device's `kv-cache-server.service`.

## Dependencies

vLLM ≥ 0.26 with `LMCacheMPConnector`; LMCache ≥ 0.5.4 (MP mode, `--separate-object-groups` for
Mamba hybrids); a Valkey 8 store or a filesystem export on the second device; a direct LAN between
the boxes (MagicDNS/WireGuard measured 6.6× slower for bulk KV).

## Downstream effects

None on tools/list or on any llama.cpp seat. The seat that uses it gains capacity; `agent_model`
may point at that seat once the fidelity gate is green (a planted needle retrieved verbatim after
eviction and after a restart — hit counters alone do not prove fidelity).

## Invariants and assumptions

- Optional, off by default, never a dependency of the install.
- The store address is private (LAN/tailnet); public addresses are refused at load.
- Chunk = engine unified block size; a mismatch fails engine registration loudly.
- One namespace per stack generation.
- **Layout constraint (measured 2026-09-03):** LMCache's Valkey adapter sizes L2 reads from one
  layout per model. A pipeline-parallel seat whose stages hold different numbers of full-attention
  layers (three stages of a 64-layer model with attention every 4th layer: 6/5/5 in any split)
  fails L2 reads with `value size exceeds buffer capacity` for the odd rank and the tier serves
  nothing. Tensor-parallel (uniform ranks) works; three-stage pipeline seats can use the
  L1-only (same-box RAM) tier until this is fixed upstream or the `fs_native` adapter is validated.

## Error handling

Load-time refusals name the key (`kv_cache_server.address: …`). A store that is down reports
`reachable: false` in status; the engine treats lookups as misses (no request fails on the tier).
A size mismatch in the store is logged by LMCache as a warning and treated as a miss — the seat
keeps serving, the tier just stops paying, which is why the counters must be read after any change.

## Measurements (2026-09-02/03, Qwen3.8-27B INT4, vLLM 0.28.0, LMCache 0.5.4)

| layout | tier | 24k-token context restored | vs recompute | tokens from tier |
|---|---|---|---|---|
| 2 cards (tp2) | same-box RAM L1 32 GB | 0.50 s | 49.7× | 23,520 / 23,520 |
| 2 cards (tp2), 65k | Lenovo Valkey over 10 GbE, L1 8 GB, one namespace per generation | **3.86 s** | **6.4×** | 23,520 / 23,520 |
| 2 cards (tp2) | same route, store shared across earlier layouts (afternoon run) | 20.6 s | 1.2× | 23,520 / 23,520 |
| 2 cards (tp2) | Valkey with 8 io-threads + 16 workers | 32.8 s | 0.75× | 4,704 (timeouts) |
| 3 cards (pp3 26/26/12), 131k | same-box RAM L1 32 GB | 0.53 s | 26× | 23,520 / 23,520 |
| 3 cards (pp3 26/26/12) | Lenovo Valkey | 13.85 s (recomputed) | 1.0× | 0 (size mismatch, see invariants) |

Raw link: ~980 MB/s Qube→Lenovo, ~700 MB/s Lenovo→Qube (iperf3); the Valkey path used ~300 MB/s.

## Source map

| concern | where |
|---|---|
| the block, defaults, validation (`validateKVCacheServer`, `privateHost`) | `internal/config/kvcacheserver.go`, wired in `internal/config/config.go` (`Config.KVCacheServer`, `load`) |
| status report (`kvCacheServerView`) | `internal/mcpserver/mcpserver.go` (`handleStatus`) |
| tests | `internal/config/kvcacheserver_test.go`, `internal/mcpserver/status_test.go` (`TestStatusReportsKVCacheServer`) |
| reference seat: wrapper, stop, llama-swap entry, store unit | `setup/templates/vllm-seat/` |
| measurements and the layout constraint | `docs/architecture/decisions/0033-cache-server-is-an-optional-second-device-tier.md` |

## Related docs

- [ADR 0033](../architecture/decisions/0033-cache-server-is-an-optional-second-device-tier.md)
- [OPERATOR-GUIDE.md](../OPERATOR-GUIDE.md) — "Cache server — an optional second device holding evicted KV"
- [fleet-node.md](fleet-node.md) — a fleet node runs contracts; a cache server holds KV pages (different roles)

