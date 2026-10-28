# LMCache overlay for vLLM seats

A vLLM seat loads LMCache through `SEAT_LMCACHE_PYTHONPATH` (see `../seat_fg.sh`): a directory that holds a full copy of
the installed `lmcache` package with the patches below applied. The engine and its LMCache MP server both import from
it, the venv's own package is never edited, and removing the overlay is one env line.

The renderer (`local-offload install vllm-seat`) refuses to render an fp8-KV seat without an overlay, because stock
LMCache restores fp8 pages corrupt on hybrid models and reports success while doing it (patch 01).

## Patches (written against LMCache 0.5.5)

These patches target the LMCache version they were written against; that is the version they are known to apply to,
not a pin. They carry a re-base duty: the update path re-applies them on the newest LMCache and fails loud when they do
not apply, keeping the previous generation serving, and a patch that no longer applies is re-ported or dropped.

| Patch | What it fixes | Needed by |
|---|---|---|
| `01-pr4253.diff` | LMCache PR #4253 (issue #4247): fp8-KV / FlashInfer stores on hybrid (Mamba + attention) models | every fp8-KV seat |
| `02-house-pp-layout.diff` | Per-rank L2→L1 buffer layouts. The layout registry is keyed by (model, world size) and kept the last rank's layout; pipeline stages hold different layer mixes (7/3/6 full-attention layers on a 28,13,23 split of a 64-layer model), so every L2 read of a differing rank failed and the seat got 0 L2 hits. | pipeline-parallel seats |
| `04-house-pp-register-bind.diff` | Binds each rank's layouts when the engine registers its KV caches. With 02 alone the binding waited for the first store, so the first request after every MP server start got 0 L2 hits and recomputed. The store-time bind stays as the fallback and the consistency check. | pipeline-parallel seats |
| `05-backport-pr4709.diff` | LMCache PR #4709 (vLLM adapter hunk): a failed retrieve's blocks are reported to vLLM instead of being served. | every seat; see the policy below |
| `06-backport-pr5249.diff` | LMCache PR #5249: with `--mamba-cache-mode align`/`all`, the last prompt token is excluded from the lookup range | hybrid models |
| `smoke-overlay.py` | CPU-only test of every patch (no server, model or GPU), run on the new tree before it is swapped in | — |

The kv-layout fix for vLLM ≥ 0.29 that LMCache 0.5.4 needed is upstream in 0.5.5 and is no longer shipped. A patch
whose change is already in the installed base is detected (`patch -R --dry-run`), skipped and recorded, never forced.

**Engine and server must run the same set.** An engine carrying 04 sends an 8-frame register message; a server without
04 drops it, and the engine's register times out after `mq_timeout`.

## Failed-load policy (patch 05)

With 05, vLLM decides what happens to blocks whose L2 load failed: `SEAT_KV_LOAD_FAILURE_POLICY=recompute` in the seat
env recomputes them; vLLM's default (`fail`) fails the request. On a hybrid model vLLM before 0.30 raises in the
scheduler on any flagged block whatever the policy (vLLM #50388), so use this set with vLLM ≥ 0.30. The rebuild script
refuses to swap an overlay that carries 05 into place while a seat env that names it does not set `recompute`
(`REPATCH_ALLOW_FAIL_POLICY=1` accepts `fail` explicitly).

## Build and deploy

Copy this directory into the seat directory inside the distro (the same place as `seat_fg.sh`), then, with the seat
stopped:

```sh
VENV=<seat-dir>/vllm-env OVERLAY=<seat-dir>/lmcache-overlay bash lmcache-patches/repatch-lmcache-overlay.sh
```

When the script runs from `lmcache-patches/` inside the seat directory, `VENV`, `OVERLAY`, `SEAT_FG` and `SEAT_ENVS`
default to paths under that directory; set `SEAT_DIR` to name it when the script runs from anywhere else.

The script builds `$OVERLAY.new`, applies the patches in order, checks one import marker per patch plus the smoke test,
writes `.overlay-provenance` (base version, and each patch's name and sha256), and swaps the new tree in only while no
running MP server has the old one loaded. The previous tree is kept at `$OVERLAY.prev`; rollback is
`mv $OVERLAY.prev $OVERLAY` with the seat stopped. Rebuild after every `lmcache` upgrade in the venv.

To build a second overlay beside a live one (a new vLLM/LMCache venv under test), give both variables new paths:
`VENV=<seat-dir>/vllm-env-new OVERLAY=<seat-dir>/lmcache-overlay-new`. The live seat is not touched.

## LMCache 0.5.6 on WSL2 (`wsl2-0.5.6/`)

The set in `wsl2-0.5.6/` is for a tensor-parallel seat on WSL2 running LMCache 0.5.6 (with vLLM 0.32.0). It holds one
patch, `07-house-wsl2-no-direct-copy.diff`.

**Why.** LMCache 0.5.6 turns on a copy-engine direct path by default (PR #5308). It hands the pinned L1 pool's host
addresses to `cudaMemcpyBatchAsync`. That is valid only where registered host memory is device-addressable at the same
address, which is the device attribute `cudaDevAttrCanUseHostPointerForRegisteredMem`.

`batch_memcpy_supported()` checks that attribute on ROCm only, not on CUDA. On WSL2, with three Blackwell cards, driver
CUDA 13.4 and runtime 13.0:
- every device reports the attribute as 0;
- `cudaHostGetDevicePointer` returns an address different from the host pointer, whether the memory is registered
  Mapped or Mapped|Portable, on every registering and current device.

**What fails without it.** The MP server's first store logs success, a `~CUDAEvent` illegal-memory-access warning follows,
and every later store or retrieve fails in `stage_block_ids` while requests wait Deferred. Registering the pool Portable
does not help.

**What the patch does.** It sets `_HAS_BATCH_MEMCPY_ASYNC = False` in `v1/multiprocess/object_group_transfer.py`, so
every transfer takes the staged path. It logs `HOUSE-WSL2-NO-DIRECT-COPY` once per server start, with what native support
would have been.

**Measured on a tp2 seat** (two 16 GB Blackwell cards, fp8 KV, a GDN hybrid, 163,840-token window):
- a 23,764-token needle prompt came back from the L2 store after eviction (23,520 external hit tokens, TTFT 3.0 s against
  24.1 s cold);
- it came back again after a full engine and MP server restart (3.6 s);
- 0 illegal-access lines.

**Status of the other patches on 0.5.6:**
- 01 (#4253), 05 (#4709) and 06 (#5249) are upstream in 0.5.6 under other names. They neither apply nor reverse-apply, so
  this set leaves them out.
- 02 and 04 (pipeline-parallel layouts) do not apply to 0.5.6 and are not ported yet. A pipeline seat stays on a 0.5.5
  overlay.

```sh
PATCHES=<seat-dir>/lmcache-patches/wsl2-0.5.6 VENV=<seat-dir>/vllm-env-032 OVERLAY=<seat-dir>/lmcache-overlay-056 \
  bash lmcache-patches/repatch-lmcache-overlay.sh
```

The build checks the `no_direct_copy` marker, read from the source because without a GPU the stock flag is False too.
This set has no smoke test, because `smoke-overlay.py` tests 0.5.5 internals.

The same seat also needs vLLM's KV allocation split into regions, or it does not start on WSL2: see
`../vllm-patches/README.md`.

## After a version change

Point the seat at a **new** L2 `base_path`. A version change can alter the bytes of a page without changing its size,
and `fs_native` does not check file sizes on read, so a store shared across versions can serve wrong KV.
