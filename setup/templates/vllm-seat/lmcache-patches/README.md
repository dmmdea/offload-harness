# LMCache overlay for vLLM seats

A vLLM seat loads LMCache through `SEAT_LMCACHE_PYTHONPATH` (see `../seat_fg.sh`): a directory that holds a full copy of
the installed `lmcache` package with the patches below applied. The engine and its LMCache MP server both import from
it, the venv's own package is never edited, and removing the overlay is one env line.

The renderer (`local-offload install vllm-seat`) refuses to render an fp8-KV seat without an overlay, because stock
LMCache restores fp8 pages corrupt on hybrid models and reports success while doing it (patch 01).

## Patches (base: LMCache 0.5.5)

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
VENV=/root/g7/vllm-env OVERLAY=/root/g7/lmcache-overlay bash lmcache-patches/repatch-lmcache-overlay.sh
```

The script builds `$OVERLAY.new`, applies the patches in order, checks one import marker per patch plus the smoke test,
writes `.overlay-provenance` (base version, and each patch's name and sha256), and swaps the new tree in only while no
running MP server has the old one loaded. The previous tree is kept at `$OVERLAY.prev`; rollback is
`mv $OVERLAY.prev $OVERLAY` with the seat stopped. Rebuild after every `lmcache` upgrade in the venv.

To build a second overlay beside a live one (a new vLLM/LMCache venv under test), give both variables new paths:
`VENV=/root/g7/vllm-env-new OVERLAY=/root/g7/lmcache-overlay-new`. The live seat is not touched.

## After a version change

Point the seat at a **new** L2 `base_path`. A version change can alter the bytes of a page without changing its size,
and `fs_native` does not check file sizes on read, so a store shared across versions can serve wrong KV.
