# Zero-warm generation

## Purpose

The GPU lifecycle every media job runs inside: how the card is cleared, handed to the renderer, and
returned to a usable state. Getting this wrong makes the machine unusable for everything else the
harness does, so the sequence is fixed and shared by every generation path.

## Trigger

Any GPU-heavy job — `generate-image` (single or `--batch`), `inpaint-image`, `generate-video`,
`generate-audio`, `run-graph` — via CLI or MCP.

## Participants

The GPU lock, the llama-swap serving layer, a local ComfyUI instance, and the calling generation
script.

## Step-by-step flow

1. **Acquire the GPU slot.** The lock is a *directory*, because `mkdir` is atomic on every OS with no
   dependencies. Only one GPU-heavy job runs per machine at a time. A busy slot waits up to a bounded
   time and then fails rather than blocking forever; vision tasks that find it held defer with
   `gpu_busy` instead of queueing.

   A lock whose owning process is dead is reclaimed **immediately**. The one-hour TTL is only a
   fallback — an earlier version gated reclaim behind the TTL alone and left the single slot
   deadlocked for up to an hour after a crash.

2. **Free the llama-swap tiers**, giving the render the whole card. This enumerates loaded models and
   unloads them **individually, skipping a keep-set** — the always-loaded embedding and reranker
   models are CPU-only, hold zero GPU VRAM, and an earlier unload-everything implementation destroyed
   that memory stack on every job for no benefit.

3. **Cold-start ComfyUI** with `--disable-smart-memory`, `--cache-none`, and a VRAM reservation.
   If an instance is already running, the harness uses it and does **not** take ownership — it will
   not kill a server it did not start.

4. **Run the job.**

5. **Tear down**, guarded and idempotent: `POST /free` with `unload_models` and `free_memory`, kill
   the ComfyUI process (if we started it), release the lock. SIGINT, SIGTERM, and SIGBREAK run the
   same cleanup and then exit, so an interrupt does not leak the slot.

## The warm batch variant

`generate-image --batch` runs N renders inside **one** slot acquisition. The only behavioral
difference is that `--cache-none` is omitted, so the checkpoint loads once instead of per render.
Teardown is not special-cased — it is the same single teardown, now at the batch boundary. Zero-warm
moves from per-render to per-batch rather than being abandoned.

A failed render inside a batch is recorded and the loop continues; one JSONL result line is written
per job. **The default single-render path is byte-identical to its pre-batch behavior.**

## Data and state changes

The lock directory is created and removed. Model residency changes on the GPU. Outputs are written.
Successful renders record a VRAM footprint observation used by fleet advertisement.

## Success behavior

The job's outputs exist, the GPU is free, ComfyUI is stopped (if the harness started it), the lock is
released, and the CPU memory stack is still resident.

## Failure behavior

Teardown runs regardless — that is what the guard is for. A busy slot produces a distinct, actionable
defer reason rather than a generic failure. A render error propagates as a typed defer with detail.

## External dependencies

A local ComfyUI installation, the llama-swap serving endpoint, and the bound model files.

## Invariants and assumptions

1. **Nothing GPU-resident persists between jobs** (batch: between batches).
2. **The CPU memory stack is never unloaded.**
3. One GPU-heavy job at a time, per machine.
4. ComfyUI is only killed if the harness started it.
5. Teardown is idempotent and runs on signals.

## Security and privacy notes

Entirely local. The lock directory's presence reveals only that a job is running.

## Observability and debugging

**When jobs will not start, check the lock directory first.** A leaked lock blocks every subsequent
GPU job on the machine, and its contents identify the owning process — which is what makes
reclaim-on-dead-process possible.

A cold start taking unusually long is the ComfyUI startup budget at work; it is generous and
environment-tunable, because a first start after a plugin change legitimately takes minutes.

## Testing notes

`render/*.test.mjs` covers lock acquisition and reclaim, lifecycle ordering, and batch semantics —
including that the default single-render path is unchanged. Run with `node --test render/*.test.mjs`
from the repo root.

## Source map

- [`render/gpu-lock.mjs`](../../render/gpu-lock.mjs) — the slot, the free step, guarded teardown
- [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs) — cold start, warm flag, ownership
- [`render/comfy-generate.mjs`](../../render/comfy-generate.mjs) — single vs batch

## Related docs

- [../architecture/decisions/0009-zero-warm-gpu-lifecycle.md](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md)
- [../systems/media-generation.md](../systems/media-generation.md)
- [run-graph-manifest-satisfaction.md](run-graph-manifest-satisfaction.md)
