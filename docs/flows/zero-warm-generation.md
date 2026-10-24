# Zero-warm generation

## Purpose

The GPU lifecycle every media job runs inside: how the card is cleared, handed to the renderer, and
returned to a usable state. Getting this wrong makes the machine unusable for everything else the
harness does, so the sequence is fixed and shared by every generation path.

## Trigger

Any GPU-heavy job — `generate-image` (single or `--batch`), `inpaint-image`, the generative
instruction edit (`offload_edit_image_generative`), `generate-video`, `generate-audio`,
`run-graph` — via CLI or MCP.

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
   models are small and, on the reference box, pinned to the utility card, not the render card, and an earlier unload-everything implementation destroyed
   that memory stack on every job for no benefit. Under a lease on SOME cards the helper also leaves the models positively placed on the other cards (by `layers`
   or by the serving config's device pin) resident; it unloads the list the lease hands it, which includes a model nothing places.

3. **Start ComfyUI** (cold-start it) with `--disable-smart-memory`, `--cache-none`, and a VRAM reservation.
   If an instance is already running, the harness uses it and does **not** take ownership — it will
   not kill a server it did not start. Since ADR 0097 (2026-10-24) the instance a harness runner launches
   carries the idle pack with a process bound, so a second job inside the bound finds it running, **reuses**
   it and skips the launch (31 to 89 s on the 8 GB laptop card). A reused instance launched for another
   shape (warm versus `--cache-none`, another `--reserve-vram`) is restarted. The runner then holds the
   instance for the whole job (`GET /offload/hold`) so the bound cannot fire under it.

4. **Run the job.**

5. **Tear down**, guarded and idempotent: `POST /free` with `unload_models` and `free_memory`, release
   the hold, then settle the ComfyUI process (if we started it), and release the lock. After a **good**
   job the process is left running: it holds about 240 MiB of the card and ends itself after
   `COMFY_IDLE_EXIT_SEC` (300 s unless set) with no prompt and no holder. After a **failed** job the
   runner stops the instance it used, so a sticky CUDA error is cured by the next launch.
   `COMFY_IDLE_EXIT_SEC=0` (or `off`) restores the old lifecycle: kill the process after every job.
   SIGINT, SIGTERM, and SIGBREAK run the same cleanup and then exit, so an interrupt does not leak
   the slot.

## The warm batch variant

`generate-image --batch` runs N renders inside **one** slot acquisition. The only behavioral
difference is that `--cache-none` is omitted, so the checkpoint loads once instead of per render.
Teardown is not special-cased — it is the same single teardown, now at the batch boundary. Zero-warm
moves from per-render to per-batch rather than being abandoned. When this machine's lane is held and a
roster node renders the recipe, the batch is instead one ticket per job (ADR 0083, decision 13): each job
is a node's own render, which reuses the node's kept ComfyUI inside the idle bound (ADR 0097) and cold-starts one otherwise, and the warm session exists only here.

A failed render inside a batch is recorded and the loop continues (unless ComfyUI itself became unusable, or the output disk filled: the batch then stops at that job, the later jobs get `not run:` rows and the batch exits non-zero, register C-83, 0.158.1; the full disk 0.178.0); one JSONL result line is written
per job. A batch that ran every job and had a failed one exits 4 (`render/comfy-generate.mjs`, `render/comfy-inpaint.mjs` and `local-offload generate-image --batch`; 0 is every job rendered, 1 a stop, 2 usage), and the Go caller reads the runner's 4 as a finished batch and takes the per-job rows. **The default single-render path is byte-identical to its pre-batch behavior.**

Since 0.178.0 every render, single or batch, writes its output to a staged sibling file and renames it into place (`render/atomic-out.mjs`), so a failed render, a full disk included, leaves no file at its path and keeps a good file already there.

## Data and state changes

The lock directory is created and removed. Model residency changes on the GPU. Outputs are written.
Successful renders record a VRAM footprint observation used by fleet advertisement.

## Success behavior

The job's outputs exist, no model is resident, the lock is released, and the memory stack is still resident.
ComfyUI is stopped if `COMFY_IDLE_EXIT_SEC=0`; otherwise a bare process (about 240 MiB of the card) is left
for the next job and ends itself after the idle bound (ADR 0097).

## Failure behavior

Teardown runs regardless — that is what the guard is for. A busy slot produces a distinct, actionable
defer reason rather than a generic failure. A render error propagates as a typed defer with detail.

## External dependencies

A local ComfyUI installation, the llama-swap serving endpoint, and the bound model files.

## Invariants and assumptions

1. **No model stays GPU-resident between jobs** (batch: between batches). A bare ComfyUI process, about 240 MiB, may
   persist for at most the idle bound (ADR 0097).
2. **The memory stack is never unloaded** — not by the free step and not by `gpu reserve --unload-seat` (register C-87); it is an exception to item 1.
3. One GPU-heavy job at a time, per machine.
4. ComfyUI is only killed or left to end itself if the harness started it.
5. Teardown is idempotent and runs on signals.

## Security and privacy notes

Entirely local. The lock directory's presence reveals only that a job is running.

## Observability and debugging

**When jobs will not start, check the lock directory first.** A leaked lock blocks every subsequent
GPU job on the machine, and its contents identify the owning process — which is what makes
reclaim-on-dead-process possible.

A cold start taking unusually long is the ComfyUI startup budget at work; it is generous and
environment-tunable, because a first start after a plugin change legitimately takes minutes.

A ComfyUI process still running after a job is the kept instance of ADR 0097, not a leak. `GET /offload/idle` on its port
says how long it has been idle against its bound (`exit_idle_sec`, `exit_limit_sec`) and how many runners hold it (`holds`).

## Testing notes

`render/*.test.mjs` covers lock acquisition and reclaim, lifecycle ordering, and batch semantics —
including that the default single-render path is unchanged. Run with `node --test render/*.test.mjs`
from the repo root.

## Source map

- [`render/gpu-lock.mjs`](../../render/gpu-lock.mjs) — the slot, the free step, guarded teardown
- [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs) — cold start, warm flag, ownership
- [`render/comfy-generate.mjs`](../../render/comfy-generate.mjs) — single vs batch
- [`render/batch-jobs.mjs`](../../render/batch-jobs.mjs) — the batch loop, the stop conditions and the exit codes
- [`render/atomic-out.mjs`](../../render/atomic-out.mjs) — delivers an output: a staged sibling, then a rename

## Related docs

- [../architecture/decisions/0009-zero-warm-gpu-lifecycle.md](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md)
- [../systems/media-generation.md](../systems/media-generation.md)
- [run-graph-manifest-satisfaction.md](run-graph-manifest-satisfaction.md)
