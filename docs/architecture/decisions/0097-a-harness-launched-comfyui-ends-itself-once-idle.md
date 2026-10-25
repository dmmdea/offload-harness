---
status: Accepted
date: "2026-10-24"
---

# A harness-launched ComfyUI ends itself once idle

## Context

ADR 0009 made media generation zero-warm: every render cold-starts ComfyUI, runs, frees the models with `/free`, kills the
process and releases the lock. It chose that because a ComfyUI instance that stays warm makes a consumer card useless for
everything else. It accepted the price: a single render pays the whole cold start.

The price was measured on 2026-10-24 on the 8 GB laptop card (RTX 3070 Laptop, ComfyUI 0.42.5, FLUX 3 Image Klein). Two
renders ran back to back, and each one relaunched ComfyUI:

| Render | Wall | Launch to first card allocation | ComfyUI's own prompt |
|---|---|---|---|
| First (cold file cache) | 135 s | 89 s (66 percent of the wall) | 21.89 s |
| Second (warm file cache) | 66.5 s | 31 s (47 percent of the wall) | 22.78 s |

ComfyUI-Manager's prestartup took 5.2 to 11.5 s of each launch. After the end-of-job `/free`, the bare process held 239 MiB of
the card. So the relaunch cost more than the render, and what the kill returned to the card was a context of about 240 MiB.

The operator's ruling on 2026-10-24 on seeing this was "that should not be happening".

Two things already existed that point at the answer:

- A kept instance (`--keep-comfy`) survives its runner, and the idle pack (`render/comfyui-ext/idle-nodes/offload_idle`, 0.186)
  frees its models and caches after 300 s with an empty queue. The pack ran in a live ComfyUI on 2026-10-14. It was written with
  no self-exit on purpose. A runner in a long step that is not ComfyUI's (ffmpeg, a QA pass, an LLM call) shows an empty queue,
  exactly like an abandoned instance. A process that exits under such a runner breaks its next prompt with a refused connection,
  where a model free only costs a reload. A bound with no way to tell the two apart would have been wrong.
- A kept instance's lifetime was the GPU lease's: the holder stopped it at release. That is a bound set from outside, by a
  command that may never run again (a runner killed before its cleanup, a holder that died).

The missing piece was a way for a runner to say "I am still here" that costs the runner nothing and is withdrawn by itself when
the runner dies. A held connection does that.

## Decision

**1. A harness runner keeps the ComfyUI it launches after a good job.** `withGpuSlot` (`render/gpu-lock.mjs`) leaves the
instance running when the job succeeded and the idle pack reported that the process is bound. The next job on the same card
reuses it and skips the launch. The end-of-job `/free` still runs on every job, so models still leave the card after each
prompt; what survives is the bare process (about 240 MiB of the card).

**2. The process ends itself.** The idle pack gains a process bound, `OFFLOAD_IDLE_EXIT_SEC`. After that many seconds with no
prompt queued or running and no runner holding the instance, the pack waits a 2 s grace, checks again under the queue mutex and
exits the process with `os._exit(0)`.
- The exit clock starts when the pack loads, so an instance nobody ever uses ends too.
- Unset, empty or 0 means no process bound. A hand-started instance that loads the pack for its model free never exits.
- The model free (`OFFLOAD_IDLE_FREE_SEC`, 300 s) is unchanged, and a hold does not block it.
- The harness runs no timer and no watcher. The clock is the engine's own, the analogue of llama-swap's `ttl`.

**3. A runner holds the instance for the whole job.** `GET /offload/hold` is a streamed response that writes one byte every 2 s.
The pack counts it as a holder until a heartbeat write fails or the transport closes. A runner opens it after the launch (or
after reusing a kept instance) and closes it in its teardown. A runner that is killed drops the connection, so the hold ends on
its own and the bound resumes. While a runner is in a long step that is not ComfyUI's, the bound does not fire.
`GET /offload/idle` also answers `exit_limit_sec`, `exit_idle_sec` and `holds`.

**4. A failed job stops the instance it used.** If the job failed, the runner stops the instance it launched, or a reused
instance that bounds itself (after proving it is the harness's own again), so a sticky CUDA error is still cured by the next
launch. Only a good job leaves an instance for reuse (an explicit `--keep-comfy` aside). A reused instance that another runner
also holds (`holds` above the failed runner's own) is left running: that runner's job is live, and it stops the instance itself if
its job fails too. The queue is not asked here, because a hung prompt of the failed job's own looks like anyone's, and a hung
instance never reaches its process bound.

**5. The setting is `COMFY_IDLE_EXIT_SEC`, on the runner.** Unset means 300. `0` or `off` restores the ADR 0009 lifecycle
exactly: no process bound, the runner kills what it launched after every job. A whole number of seconds from 5 up is that
bound. Anything else is 300 with one `COMFY-IDLE-WARN` line. It reaches the pack as `OFFLOAD_IDLE_EXIT_SEC`, written into the
launch marker as `idleExitSec`, and is fixed when an instance is launched. An explicit `--keep-comfy` keeps its old meaning: the
instance is kept whatever happens and carries no process bound from this setting.

**6. A reused instance must match the call.** A self-bounded instance that was launched for another shape (a warm session versus
`--cache-none`, another `--reserve-vram`) is restarted (`launchShapeGap`), unless the runner that launched it is still working
on it, or any client holds it or has a prompt queued or running on it (`instanceInUse`); then the call runs on it as it is,
with a `COMFY-KEEP-WARN` line. A family change still frees the weights, as before.

**7. The pipeline's routine release leaves such an instance alone.** `internal/comfyinst` gains `StopForRelease`, which is
`StopForLease` except that it spares an instance whose marker has `idleExitSec` set (the outcome says `Spared`). The pipeline's
media-lease release (`stopKeptInstances`) uses it, so keyed per-card and multicard instances survive between jobs too. `gpu
reserve` when its command ends, its detached holder and `gpu release` still use `StopForLease` and stop them.

**8. The operator's yes.** Plan invariant I5 and `openspec/config.yaml` forbid a new daemon, scheduler, listener or watchdog
without the operator's explicit yes. The 2026-10-24 ruling, "that should not be happening", is read as that yes for this one
mechanism: the engine ends its own process once idle. It is not a harness watcher. The harness starts nothing, polls nothing and
signals no process it did not launch. The pack acts only on the ComfyUI it was loaded into.

## Consequences

- A second render within the bound skips the launch: 31 to 89 s on the 8 GB laptop card. A render that follows a gap longer than
  the bound pays a launch again, as before. Measured live on 2026-10-24 with this change:

  | Node | Render 1 | Render 2 | Same process | Ended itself |
  |---|---|---|---|---|
  | 8 GB laptop card, default instance | 68.8 s (launch) | 36.4 s (was 66.5 s); a third, 36.3 s | yes, one pid pair for all three | at 304 s idle; card back to 0 |
  | 16 GB reference box, keyed multicard instance | 74.4 s (launch) | 50.0 s | yes; each release logged "left the ComfyUI instance ... running" | at 302 s idle |

  Between prompts the survivor held 239 MiB on the laptop card and 344 MiB and 156 MiB on the multicard pair, and a chat seat
  loaded beside it on the compute card. The process held 4.1 to 5.3 GiB of host private bytes until it ended.
- **Known gap: host-RAM admission counts a reused survivor twice.** The media admission still declares the render's full
  host-RAM need (19.3 GiB for FLUX 3 Image Klein on the multicard pair) while the reused survivor already holds about 5 GiB of it
  in the commit charge. The error is toward waiting, never toward over-admitting (the calibrated declaration only raises), and
  on 2026-10-24 the second render was admitted at once with about 4.5 GiB to spare. Near the host's limit it can make a render
  wait that would fit. Subtracting a reused survivor's private bytes from the declaration is future work.
- A survivor holds about 240 MiB of the card. On the 16 GB reference box a kept keyed single-card instance (ComfyUI 0.38.0) held
  about 5.0 to 5.2 GiB of host private bytes, measured live on 2026-10-14. It now holds that for at most the bound (300 s by
  default) and then the process ends. This ADR bounds the "kept idle instance holds about 5 GB that nothing returns" residual
  by ending the process instead of freeing its memory piecemeal.
- A job that needs the card with nothing on it can stop the instance by hand (see the operator guide), or the runner can be
  started with `COMFY_IDLE_EXIT_SEC=0`.
- A `--reserve-vram` change or a switch between warm and single pays one restart. A family change frees the weights, as before.
- Under an outer `gpu reserve`, the wrapper's job object still ends an instance launched inside it when the wrapped command ends.
  The reserve is the explicit hold, so that is the existing rule, not a regression.
- An explicit `--keep-comfy` is unchanged. It has no process bound and is stopped by its lease's holder or by hand.
- A hand-started instance is not touched. The harness does not inject into an instance it does not own (ADR 0009). One that
  loads the pack for its model free by hand never exits.
- A runner that dies drops its hold with the connection. The bound then counts from the moment no prompt and no hold remain.
  A job on an instance whose runner opened no hold (a client that is not this harness's runner) is not protected: the bound can
  end the instance under it if it sends no prompt for longer than the bound.

## Alternatives considered

- **Keep ComfyUI warm forever.** Rejected. INV-2 says every idle model unloads at five minutes, and an instance that never ends
  holds a CUDA context and gigabytes of host memory for nobody. It also repeats the reason ADR 0009 gave.
- **A watcher or reaper in `fleet-serve` that ends idle instances.** Rejected. Plan I5 forbids a new watchdog, and a reaper has
  to guess when a runner is abandoned, which is the problem the hold solves. The engine's own clock needs no guess and no extra
  process.
- **A lazy sweep at the next admission** (the next job finds old instances and stops them). Rejected. An instance nobody reuses
  would never end, which is the case that matters: the card keeps its context and the host keeps its memory until somebody asks
  for the card.
- **Make the launch faster and still kill per job** (drop ComfyUI-Manager's prestartup, avoid the double CUDA initialisation).
  Not enough. It would save part of the 5.2 to 11.5 s of prestartup, and every render would still pay the rest of the launch.
  It can still be done later; it does not replace this.
- **No hold; use the lease's epoch as the proof of life.** Rejected. Reusing an instance re-stamps its marker to the new epoch
  while the child keeps the epoch it was launched under, so the pack cannot read it reliably (media-generation.md).

## Related code

- [`render/gpu-lock.mjs`](../../../render/gpu-lock.mjs): `withGpuSlot` keeps, holds, and stops a failed job's instance.
- [`render/comfy-lifecycle.mjs`](../../../render/comfy-lifecycle.mjs): `ensureComfy` (kept launches carry the pack and
  `OFFLOAD_IDLE_EXIT_SEC`), `resolveIdleExit`, `openComfyHold`, `stopHarnessInstance`, `launchShapeGap`.
- [`render/comfy-ownership.mjs`](../../../render/comfy-ownership.mjs): the launch marker records `idleExitSec`.
- [`render/comfyui-ext/idle-nodes/offload_idle/__init__.py`](../../../render/comfyui-ext/idle-nodes/offload_idle/__init__.py): the
  process bound, `/offload/hold`, `/offload/idle`.
- [`internal/comfyinst/comfyinst.go`](../../../internal/comfyinst/comfyinst.go): `Marker.IdleExitSec`, `StopForRelease`,
  `Outcome.Spared`.
- [`internal/pipeline/mediaadmit.go`](../../../internal/pipeline/mediaadmit.go): `stopKeptInstances` uses `StopForRelease`.

## Related docs

- [ADR 0009](0009-zero-warm-gpu-lifecycle.md): the lifecycle this changes in part. Its per-job process teardown is superseded;
  models still leave the card after every prompt and zero-warm for models stands.
- [media generation](../../systems/media-generation.md): the kept instance, the idle pack, its settings and the live check.
- [GPU lease](../../systems/gpu-lease.md): who stops a kept instance, and the new routine release.
- [operator guide](../../OPERATOR-GUIDE.md): `COMFY_IDLE_EXIT_SEC`, the hold route, stopping an instance by hand.
- [zero-warm generation](../../flows/zero-warm-generation.md): the flow.
