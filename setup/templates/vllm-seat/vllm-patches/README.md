# vLLM patches for a seat on WSL2

## `house-kv-split-0.32.0.patch` (HOUSE-KV-SPLIT)

**The problem.** vLLM 0.29 and later back all of a worker's KV cache with one `torch.zeros` allocation. LMCache's MP
connector exports each layer's storage through CUDA IPC. On WSL2 the dxg layer refuses to make an allocation that large
resident for export, at about 3.3 GB or more. The seat then dies at start with a CUDA out-of-memory error, and the guest's
kernel log shows `dxgkio_make_resident: Ioctl failed: -12`. vLLM 0.28 gave each KV tensor its own allocation (about
548 MiB) and exported fine.

**What the patch does.** With `HOUSE_KV_SPLIT=1`, the patch restores that shape. It cuts the pool into one allocation per
region, where a region is the union of the overlapping layer slots of the cache groups. Every layer view is rebuilt with
the dtype, shape and strides of the single-buffer path, so the byte layout and the KV pool size do not change.
- Unset, or `0`, leaves vLLM's behaviour unchanged.
- It raises at start, instead of quietly keeping one buffer, when the config cannot be cut: layers interleaved block by
  block, a spec that takes the raw backing, or ROCm.
- It does not support SimpleCPUOffload or the NIXL / MoRIIO / Mooncake single-storage fast paths. The LMCache MP connector
  works with either shape.

**Measured on a tp2 seat** (two 16 GB Blackwell cards, fp8 KV, a GDN hybrid, `--max-model-len 163840`, KV pool pinned
with `--kv-cache-memory-bytes`):
- 16 regions of 217.4 MiB per rank;
- 0 new `make_resident` failures;
- the same 209,597-token pool the seat had on vLLM 0.28. Without the split, vLLM 0.32 did not start: 30 `make_resident`
  failures.

The patch is written against vLLM 0.32.0 (`vllm/v1/worker/utils.py`). Re-port it for any other version: it is checked to
apply cleanly to 0.32.0 only.

### Apply

With the seat stopped, against the seat venv:

```sh
SP="$("$VENV/bin/python" -c 'import os, vllm; print(os.path.dirname(os.path.dirname(vllm.__file__)))')"
patch -p1 -d "$SP" --forward --dry-run < vllm-patches/house-kv-split-0.32.0.patch \
  && patch -p1 -d "$SP" --forward < vllm-patches/house-kv-split-0.32.0.patch
grep -c HOUSE-KV-SPLIT "$SP/vllm/v1/worker/utils.py"     # non-zero once applied
```

Then add `export HOUSE_KV_SPLIT=1` to the seat env. The renderer does not emit it yet.

At the next start, every tensor-parallel rank logs:

```
HOUSE_KV_SPLIT=1: split the <N> MiB KV cache into <k> region(s) of <a>-<b> MiB.
```

Reinstalling or upgrading vLLM removes the patch, so re-apply it and check for that line before the seat serves.

The same seat also needs LMCache's copy-engine direct path off on LMCache 0.5.6: see the `wsl2-0.5.6` set in
`../lmcache-patches/README.md`.
