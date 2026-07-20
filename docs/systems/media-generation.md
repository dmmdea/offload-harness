# Media generation

## Purpose

Image, video, audio, and SVG generation plus image editing — everything that drives a local ComfyUI
instance or a generation script, under a GPU lifecycle that leaves the machine usable afterwards.

## Questions this doc answers

- What happens to the GPU during a render, and what state is the machine left in?
- Which models are bound, and where is that configured?
- What are the image-editing operations, and how are they invoked?
- When is batching worth it?
- Why is FLUX not an option?

## Scope

The generation verbs and MCP tools, the GPU lock and zero-warm lifecycle, warm batch mode, the
inpainting route, the edit-operation pack, and per-machine model bindings.

## Non-scope

- Arbitrary caller-supplied graphs and node provisioning → the run-graph flow at
  [../flows/run-graph-manifest-satisfaction.md](../flows/run-graph-manifest-satisfaction.md)
- Serving text tiers → [setup-installer.md](setup-installer.md)
- VRAM footprint measurement for fleet dispatch → [fleet-node.md](fleet-node.md)

## Key concepts

**GPU Lock** — a single-slot, cross-process lock; only one GPU-heavy job runs at a time per machine.
**Zero-Warm** — no GPU residency persists between jobs. **Warm Batch** — an opt-in session where the
checkpoint loads once for N renders. **Op** — one image-editing operation inside `edit-image`.

## How the system works

Every GPU-heavy job runs inside `withGpuSlot`, which owns the whole lifecycle: acquire the lock, free
the llama-swap tiers, cold-start ComfyUI, run the job, then tear down — `/free`, kill the ComfyUI
process, release the lock. Teardown is idempotent and also runs on SIGINT/SIGTERM, so an interrupt
does not leak the slot.

The lock is a **directory**, because `mkdir` is atomic everywhere with no dependencies. A lock whose
owning process is dead is reclaimed **immediately**; the one-hour TTL is only a fallback. An earlier
version gated reclaim behind the TTL alone and left the single GPU slot deadlocked for up to an hour
after a crash.

Two details of the free step are easy to get wrong:

- **It frees per model, not everything.** The always-loaded embedding and reranker models are
  CPU-only and hold zero GPU VRAM. An earlier unload-all implementation tore that memory stack down
  on every generation job for no VRAM benefit; the keep-set now protects it.
- **ComfyUI is only killed if the harness started it.** An already-running instance is left alone.

Full rationale in [ADR 0009](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md).

**Warm batch.** `generate-image --batch` takes a jobs file and runs N renders in one session. The
only behavioral change is omitting ComfyUI's `--cache-none`, so the checkpoint loads once; teardown
still happens exactly once, at the batch boundary. A failed render is recorded and the batch
continues, one JSONL result line per job. **The default single-render path is unchanged.**

**Image editing** is one verb (`edit-image` / `offload_edit_image`) carrying an `ops` list, not a
family of verbs. The full op set (validated in `mediaops.ValidateOps`) is `crop`, `resize`,
`convert`, `composite`, `text`, `mask_boxes`, `grade`, `lut_cube`, `perspective_composite`, `finish`,
`flatten_design`, and `instantiate_design` (the last two drive GIMP). `finish` is delivery sharpening
and should come **last** — sharpening before a resize is undone by the resampling — but this is a
caller convention that the validator documents, not an ordering it enforces (mask and rendition
chains may legitimately follow). `renditions` is a top-level parameter, not an op: it re-runs the
pipeline once per export target.

**Inpainting** (`inpaint-image` / `offload_inpaint_image`) takes a mask, or builds one from
`mask_boxes`. `--auto-text` localizes rendered-text regions with the vision model and inpaints them.
It is **active**, not gated: the always-defer gate was removed on 2026-07-17 after a grounding
evaluation passed 3/3. Its safety envelope is validation rather than a gate — an unparseable answer,
no boxes, or absurd boxes covering more than 60% of the image all error out so the caller defers,
with the manual `mask_boxes` workflow named. It never silently repaints unverified regions.

## Data and state

Rendered outputs land in the configured media directory or a caller-supplied `out_dir`. Footprint
observations are recorded as a side effect of successful renders — see
[fleet-node.md](fleet-node.md).

## Interfaces and entry points

CLI verbs `generate-image`, `inpaint-image`, `generate-video`, `generate-audio`, `generate-svg`,
`edit-image`, `media`, `run-graph`; the matching `offload_*` MCP tools.

## Dependencies

A local ComfyUI installation (`comfy_dir`), the model files named by the bindings below, GIMP for the
design ops, and the Node renderer scripts under `render/`.

## Downstream effects

The GPU lock is machine-wide: a long render blocks vision tasks, which defer with `gpu_busy` rather
than queueing. Consumers of the media outputs — notably the creative pipeline workflows — depend on
the output envelope shape.

## Invariants and assumptions

1. **Zero-warm by default.** Nothing GPU-resident survives a job.
2. **The CPU memory stack is never unloaded** by the free step.
3. Only one GPU-heavy job at a time, per machine.
4. **No FLUX-family model is ever added** — see
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md). The binding reason
   is the non-commercial licence, not VRAM; a bigger card does not reopen it.

## Model bindings

Bound per machine through flat config keys, so the same code serves different hardware:

| Concern | Keys |
|---|---|
| Image | `imagegen_family`, `imagegen_ckpt`, `imagegen_vae`, `imagegen_steps/cfg/sampler/scheduler` |
| Inpaint | `inpaint_ckpt`, `inpaint_vae`, `inpaint_steps/cfg/sampler/scheduler` |
| Video | `videogen_unet_high`, `videogen_unet_low`, `videogen_text_encoder`, `videogen_upscale_model` |
| Audio | `voicegen_*`, `musicgen_script` |
| ComfyUI | `comfy_dir`, per-task `*_script` and `*_timeout_sec` |

Hardware profiles seed these. Tiers at 16 GB and above bind **HiDream-O1 bf16** via
`imagegen_family` — the official graph for that DiT, never the generic SDXL graph — and **Wan 2.2
Q8_0** experts with an fp16 text encoder. **RealVisXL** is the SDXL-class inpainting default. The 8 GB
tiers stay SDXL-class for image generation until O1 on 8 GB is verified on real hardware.

The recommended **≥16 GB image-*edit* primitive is Qwen-Image-Edit-2511** (Apache-2.0). It is a
model-matrix *designation*, not a config binding — image editing at that tier runs through
[run-graph](../flows/run-graph-manifest-satisfaction.md) with the model set declared in the caller's
node manifest (e.g. the creative-marketing-pipelines scene-swap), so no edit checkpoint is seeded into
`config.json`. **Pin a `_1` GGUF quant (`Q4_1`/`Q5_1`), never a `_K_` one:** 2511 K-quants fail
`UnetLoaderGGUF` with `cannot reshape array` even on byte-perfect files (city96/ComfyUI-GGUF #247).
Measured on `ampere-16` 2026-07-19: Q5_1 (15.4 GB) + fp8 encoder fits 16 GB with block-swap, composite
peak 15,757 MiB. FLUX-family models remain prohibited
([ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md)).

## Error handling

Failures return typed Defers rather than crashing: a busy GPU lock defers with a distinct reason, a
render error defers with detail. The batch path records per-job failures and continues.

## Security and privacy notes

Generation runs local. `run-graph` executes caller-supplied graphs and provisions caller-specified
node packs, which is a trusted-caller interface by design — see
[ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md) for what
protects the environment from it.

## Observability and debugging

Look at the lock directory first when jobs will not start — a leaked lock blocks everything on the
machine. ComfyUI's own logs cover render failures. `fleet-measure` prints observed VRAM peaks per
task.

## Testing notes

`render/*.test.mjs` (run with `node --test` from the repo root) covers the lock, lifecycle, batch
semantics, and output parsing. Go-side coverage sits in `internal/pipeline/` for the media dispatch
and defer paths.

## Common pitfalls

- Assuming the free step unloads everything — it deliberately preserves the CPU memory stack.
- Treating `grade` or `finish` as verbs. They are ops inside `edit-image`.
- Using `perspective` — the op is `perspective_composite`.
- Assuming the pipeline reorders ops for you — it does not; `finish` should be placed last by the
  caller, and the validator does not enforce it.
- Expecting `--auto-text` to defer always. That gate was removed after its evaluation passed.
- Expecting concurrency on one machine. Concurrency is a fleet concern.

## Source map

- [`render/gpu-lock.mjs`](../../render/gpu-lock.mjs) — slot, free step, teardown
- [`render/comfy-lifecycle.mjs`](../../render/comfy-lifecycle.mjs) — cold start, warm flag
- [`render/comfy-generate.mjs`](../../render/comfy-generate.mjs) — single and batch render
- [`render/edit_image.py`](../../render/edit_image.py) — the edit ops
- [`internal/pipeline/inpaint_autotext.go`](../../internal/pipeline/inpaint_autotext.go) — auto-text
  localization and its validation envelope
- [`internal/imagegen/`](../../internal/imagegen/), [`internal/gpugen/`](../../internal/gpugen/)

## Related docs

- [../flows/zero-warm-generation.md](../flows/zero-warm-generation.md)
- [../architecture/decisions/0009-zero-warm-gpu-lifecycle.md](../architecture/decisions/0009-zero-warm-gpu-lifecycle.md)
- [../architecture/decisions/0011-flux-family-license-prohibition.md](../architecture/decisions/0011-flux-family-license-prohibition.md)
