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

   > **Superseded in part by [ADR 0097](0097-a-harness-launched-comfyui-ends-itself-once-idle.md) (2026-10-24).** The kill in
   > this step no longer happens after a good job: the runner keeps the ComfyUI it launched, and the process ends itself after
   > 300 s idle. The `/free` and the lock release in this step stand.

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

**Extended 2026-10-01 (register C-87):** the memory stack is not CPU-only on every box. On the reference box it is served on the utility card, not the render card, so "CPU-only, zero GPU VRAM" in the freeing paragraph above, and "CPU-resident" in the "Unload every model" alternative below, no longer describe it there. The decision stands on the operator's rule that mem0 never yields to a lease, and it now holds on every path that clears cards for a GPU job: `gpu reserve --unload-seat` keeps the configured `memory_stack` resident, as the render side always did, and refuses its legacy total `GET /unload` fallback while a stack member is resident or `/running` cannot be read. See [GPU lease](../../systems/gpu-lease.md).

**Extended 2026-10-13 (register G9):** a warm session and a kept instance (`--keep-comfy`) outlive their runner's last job, so a teardown at the boundary leaves a gap. The harness launches ComfyUI with `--disable-smart-memory`, and with it ComfyUI unloads every model itself when a prompt ends, so a kept single-prompt instance is bare between jobs; what survives is a warm session's executor cache (the loader outputs, in host RAM) after a runner killed before its cleanup or a session that stalls, and a hand-started instance run without that flag (models in VRAM). Those are bounded by an engine-resident idle free, the llama-swap `ttl` analogue: the instance is launched with a harness-owned pack (`render/comfyui-ext/idle-nodes`) that, after 300 s with an empty queue, asks ComfyUI's own worker to unload the models and reset the executor cache. The harness runs no watcher and the pack acts on its own ComfyUI only. This is read from ComfyUI 0.38.0's code and checked against a fake queue; it was run in a live ComfyUI on 2026-10-14 (see [media generation](../../systems/media-generation.md)). The decision above is otherwise unchanged: a single render is launched exactly as before, and an instance is still torn down at the batch boundary or by its lease holder. The pack bounds models and caches, not the process: the Python process and its CUDA context live until the holder stops the instance. See [media generation](../../systems/media-generation.md).

**Superseded in part 2026-10-24 (ADR 0097):** the per-job process teardown. Measured on the 8 GB laptop card, two renders back to back each relaunched ComfyUI, and the launch to the first card allocation took 89 s (cold file cache) and 31 s (warm) of walls of 135 s and 66.5 s, while ComfyUI's own prompt ran 21.89 s and 22.78 s. The operator ruled that this should not happen. Now a harness runner keeps the ComfyUI it launched after a good job, holds it for the whole job through `GET /offload/hold`, and the idle pack ends the process after `COMFY_IDLE_EXIT_SEC` (default 300 s) with no prompt and no holder; `COMFY_IDLE_EXIT_SEC=0` restores the lifecycle above. A failed job still stops the instance it used. What stands: the lock, the llama-swap free, `--disable-smart-memory`, the end-of-job `/free` (models still leave the card after every prompt, so zero-warm for models stands), the preserved memory stack, and teardown of the instance by an explicit `gpu release`. What no longer holds: "kill the ComfyUI process" after every job, "a single render pays full cold-start cost" within the idle bound, and "the pack bounds models and caches, not the process" (the pack now ends the process too). A survivor holds about 240 MiB of the card until it exits. See [ADR 0097](0097-a-harness-launched-comfyui-ends-itself-once-idle.md).

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
