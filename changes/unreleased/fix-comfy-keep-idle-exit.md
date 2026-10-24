### Changed - a harness render keeps the ComfyUI it launched, and that ComfyUI ends itself once it is idle (ADR 0097)

- **What it does.** A harness runner leaves the ComfyUI it launched running after a GOOD job, holds it for the whole job with
  `GET /offload/hold` (a streamed response, one byte every 2 s, ended by the runner's exit), and the idle pack exits the process
  with `os._exit(0)` after `COMFY_IDLE_EXIT_SEC` seconds (unset is 300) with no prompt queued or running and no runner holding it.
  The next render inside the bound reuses the instance and skips the launch. A job that FAILED stops the instance it used, launched
  or reused, so a sticky CUDA error is still cured by the next launch. `COMFY_IDLE_EXIT_SEC=0` or `off` restores the zero-warm
  lifecycle of ADR 0009 (kill what the runner launched after every job). An explicit `--keep-comfy` keeps its old meaning. A reused
  self-bounded instance that was launched for another shape (a warm session against `--cache-none`, another `--reserve-vram`) is
  restarted. `GET /offload/idle` also answers `exit_limit_sec`, `exit_idle_sec` and `holds`; the launch marker records
  `idleExitSec`. In Go, `comfyinst.StopForRelease` is `StopForLease` that spares an instance with `idleExitSec`, and the media
  pipeline's routine lease release uses it, so keyed per-card and multicard instances also survive between jobs. `gpu reserve` when
  its command ends, its detached holder and `gpu release` still stop them.
- **Why.** Every render relaunched ComfyUI. On the 8 GB laptop card (ComfyUI 0.42.5, FLUX 3 Image Klein) two renders back to back
  took 135 s and 66.5 s, of which the launch to the first card allocation was 89 s (cold file cache) and 31 s (warm), while
  ComfyUI's own prompt ran 22.78 s and ComfyUI-Manager's prestartup took 5.2 to 11.5 s of each launch. After the end-of-job `/free`
  the bare process held 239 MiB of the card. The operator ruled on 2026-10-24 that this should not happen. The idle pack had no
  self-exit on purpose, because a runner in a long step that is not ComfyUI's looks abandoned; the hold is what tells the two apart.
  The exit is the engine's own idle clock (the llama-swap `ttl` analogue), read as the operator's yes under plan I5: the harness
  runs no timer or watcher.
- **Not changed.** Models still leave the card after every prompt (`--disable-smart-memory` and the end-of-job `/free`), so
  zero-warm for models stands; the model free (`OFFLOAD_IDLE_FREE_SEC`, 300 s) is unchanged and a hold does not block it. A hand-started
  instance that loads the pack for its model free never exits. Expected costs, not defects: a survivor holds about 240 MiB of the card
  and, measured live on 2026-10-14 on the 16 GB reference box (keyed single-card instance, ComfyUI 0.38.0), about 5.0 to 5.2 GiB of
  host private bytes, for at most the bound; under an outer `gpu reserve` the wrapper's job object still ends an instance launched
  inside it.
- **Check.** FLUX 3 Image Klein renders back to back, then 330 s idle, run live on 2026-10-24 with this change. On the 8 GB
  laptop card (default instance) three renders took 68.8 s, 36.4 s (66.5 s before) and 36.3 s on one ComfyUI process, `holds` read 1 during
  each job and 0 between, and the process ended itself at 304 s idle with the card back to 0 MiB. On the 16 GB reference box
  (keyed multicard instance, card-scoped leases) they took 74.4 s and 50.0 s on one process, each lease release logged "left the
  ComfyUI instance ... running", and the process ended itself at 302 s. ADR 0097 records the decision and supersedes in part the
  per-job process teardown of ADR 0009; the live check for the process bound is case E in `docs/systems/media-generation.md`.
- **Known gap.** Host-RAM admission still declares a render's full need while a reused survivor already holds about 5 GiB of it,
  so near the host's limit a render can wait when it would fit. It never over-admits.
