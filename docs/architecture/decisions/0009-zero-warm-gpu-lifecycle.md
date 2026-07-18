---
status: Accepted
date: "2026-07-18"
---

# Zero-warm GPU lifecycle for media generation

## Context

Media generation and text inference compete for the same GPU. A ComfyUI instance holding a
checkpoint resident can occupy most of an 8 GB card; the llama-swap tiers that serve offload tasks
want that memory too. Running both warm on a consumer card means one of them fails, usually with an
out-of-memory error partway through the expensive one.

Keeping the renderer warm is tempting because cold-starting ComfyUI and loading a checkpoint is the
dominant cost of a single render. But a permanently warm renderer makes the machine useless for
everything else the harness does.

## Decision

Media generation is **zero-warm by default**: nothing GPU-resident persists between jobs.

Every GPU-heavy job runs inside a single-slot, cross-process lock, and the lifecycle is fixed:

1. **Acquire the GPU slot.** The lock is a directory, because `mkdir` is atomic on every OS with no
   dependencies. A held slot fails fast rather than queueing forever, and a stale lock whose owning
   process is dead is reclaimed immediately — the long TTL is only a fallback, after an earlier
   version left the single slot deadlocked for up to an hour following a crash.
2. **Free the llama-swap tiers**, giving the render the whole card.
3. **Cold-start ComfyUI** with `--disable-smart-memory` and `--cache-none`.
4. **Run the job.**
5. **Tear down**, guarded and idempotent: `/free` with `unload_models` and `free_memory`, kill the
   ComfyUI process, release the lock. Signal handlers run the same teardown, so an interrupt does not
   leak the slot.

Two refinements matter as much as the sequence:

**Freeing is per-model, and the CPU memory stack is preserved.** An earlier unload-everything
implementation tore down the always-loaded embedding and reranker models on every generation job.
Those are CPU-only and hold zero GPU VRAM, so unloading them bought nothing and needlessly destroyed
the memory stack each time. The free step now enumerates loaded models and skips a configurable
keep-set.

**ComfyUI is only killed if we started it.** If an instance is already running, the lifecycle leaves
it alone — the harness does not manage a server it does not own.

**Warm batch is the one opt-in exception.** `generate-image --batch` sets a warm session, whose only
effect is omitting `--cache-none` so the checkpoint loads once for N renders. Teardown still happens
exactly once, at the batch boundary — zero-warm moves from per-render to per-batch rather than being
abandoned. The default single-render path is unchanged.

## Consequences

- The machine returns to a known state after every job. Text inference works immediately afterward
  without manual intervention.
- A single render pays full cold-start cost. This is the deliberate trade, and it is why `--batch`
  exists for the case where the cost is amortizable.
- Only one GPU-heavy job runs at a time per machine. Concurrency is a fleet-level concern, solved by
  distributing across nodes rather than packing one card.
- The memory stack stays resident across generation jobs, which is what makes it dependable.
- Teardown correctness is load-bearing. A leaked lock blocks every subsequent job on that machine,
  which is why reclaim-on-dead-pid is immediate and why signal handlers share the cleanup path.

## Alternatives considered

- **Keep ComfyUI warm permanently.** Rejected: it makes the GPU unavailable for text inference on
  exactly the consumer cards this targets.
- **A process-level mutex instead of a lock directory.** Rejected: the lock must hold across separate
  processes and survive inspection by an operator. A directory is atomic, visible, and dependency-free.
- **Unload every model before a render.** Tried and reverted — it destroyed the CPU-resident memory
  stack for no VRAM gain.
- **Queueing on a busy slot instead of failing.** Partially adopted: there is a bounded wait, after
  which the job fails rather than blocking indefinitely.

## Related code

- [`render/gpu-lock.mjs`](../../../render/gpu-lock.mjs) — the slot, the free step, guarded teardown
- [`render/comfy-lifecycle.mjs`](../../../render/comfy-lifecycle.mjs) — cold start, warm flag
- [`render/comfy-generate.mjs`](../../../render/comfy-generate.mjs) — batch vs single render

## Related docs

- [../../flows/zero-warm-generation.md](../../flows/zero-warm-generation.md)
- [../../systems/media-generation.md](../../systems/media-generation.md)
