### Added - `local-offload acceptance --render`: one real output per bound route

- **What it does.** `acceptance --render [--only a,b] [--list] [--out-dir DIR] [--json]` runs each bound route once through this binary's real verb and checks the output against a known answer: summarize, classify, embed, vision, stt, agent, image, video, animate, voice, music, compose, edit, media, inpaint, upscale. An unbound route is a named SKIP, and a deferred run is a FAIL, because a route that did not run proves nothing. The gate runs with the result cache off, because a cached answer proves nothing either. Outputs stay in the output folder.
- **Why.** A node counted as installed once `doctor` was green, and `doctor` only reads configuration. A bound route could still fail on its first real job.

### Added - tier amd-rdna3 binds video, animate, voice and music on 64 GB boxes

- **What it does.** The mid and high RAM bands bind FastWan video, VACE animate with depth, Chatterbox voice and ACE-Step music from `config_seed_ram_mid_high`. The Windows installer fetches every weight the seed names, about 28 GB, and builds what has no usable Windows binary.
- **Builds.** `setup/build-engine-vulkan.ps1` builds whisper-server, da3-cli and a patched audiocpp_cli on the box with a portable MSYS2 under `<home>\toolchain`. It takes no admin rights, runs at below-normal priority on half the cores, and refuses a binary with no Vulkan link. A failed build warns and the install goes on: the route it feeds reads BOUND-BUT-MISSING and the render gate fails it by name.
- **Voice reference.** audio.cpp's Chatterbox has no voice of its own, so a voice call with no reference failed while loading. The runner now says so before the lease, and on this tier the installer makes a default reference from the box's own Windows voice.
- **Limits.** The timeouts, token caps and sizes are amd-gcn's, measured on a Vega 7 under Linux, so they stay projected until a 780M has run them. `local-offload update` has no audio.cpp or depth-anything row yet, on this tier or on amd-gcn.

### Added - upscale and inpaint with no ComfyUI

- **What it does.** `upscale_image` and `inpaint_image` run through stable-diffusion.cpp's `sd-cli` behind the new engine keys `upscale_engine` and `inpaint_engine` (`sdcpp`). Tier amd-rdna3 seeds both from its base `config_seed`, so a 780M box serves them with no ComfyUI.
- **Upscale.** `sd-cli -M upscale` with 4xNomos2_hq_mosr (CC-BY-4.0, commercial use allowed with attribution, 8.6 MB, fetched by the installer to `models/upscale/`). The scale comes from the model, so a `scale`, `width` or `height` that differs is met by an ffmpeg lanczos resample to the exact size. A failed upscaler load saves the un-upscaled image and exits 0 in sd-cli, so the runner checks the output really is larger and fails `MODEL_INCOMPATIBLE` otherwise. A per-request `model` is refused on this engine.
- **Inpaint.** `sd-cli -i --mask --strength` on the image lane's own Z-Image Turbo files, with no weight of its own. The runner grows the mask by `grow_mask` (default 16), renders, and merges the render back over the source through the feathered mask, so the pixels outside the seam are exact. The image lane's `--vae-on-cpu` is not forwarded. Removal is clean. A large repaint that adds a new object can show the mask's rectangle, because Z-Image is not an inpaint model.
- **Guard.** `createLogGuard` has an upscale stage: the upscaler prints a small host buffer before its device buffer, and the default stage would kill that run and demand a diffusion buffer an upscale never has. The default stage is unchanged.
- **Limits.** Measured on a Vulkan GPU of another class (upscale 512 to 2048 in 8 s, inpaint clean), so the tier stays PROJECTED until a 780M has run them. `edit_image_generative` and `run_graph` still need ComfyUI.

### Fixed - a fresh Windows Vulkan install reaches a working stack on a data drive

- **Install home.** `install.ps1` no longer defaults to the OS drive. It picks a fixed NTFS or ReFS volume with at least 20 GiB free, never the OS drive, USB or a cloud drive: a drive already holding an install first, otherwise the one with the most free space. It records `OFFLOAD_HOME` for the user. An existing profile install stays where it is. `OFFLOAD_ALLOW_OS_DATA=1` keeps the profile. A box with no other disk fails loud and names both overrides.
- **Tool caches.** When the home is off the OS drive, the Go, npm, uv and pip caches and the build temp folders move under `<home>\cache`. Values the operator set stay, except TEMP and TMP.
- **Config shadow.** A leftover `LOCAL_OFFLOAD_CONFIG` stops the install with the exact fix, because it silently outranks the file the installer writes. `OFFLOAD_ALLOW_CONFIG_ENV=1` keeps it. `doctor` reports the same shadow as a warning.
- **Re-render.** Step 6 reads `install render --state`: a stale serving config is re-rendered, a hand-edited one is kept with a warning, and `OFFLOAD_RERENDER=1` replaces it. The old file is kept beside it.
- **Seat weights.** The installer now fetches what a tier's `media_seats` bind (whisper, Silero VAD, mmproj) and every file its seed layers name, and `agent_ctx_tokens` reaches the installed config.
- **Serving config path.** Both installers now write `serving_config_path`. Without it `doctor` read capacity as UNKNOWN on every fresh install.
- **Host tools.** The Windows installer gets ffmpeg through winget, and makes an edit venv with Pillow when no Python on the box has it. Without them the compose, edit and media routes could not run. On sd.cpp tiers `edit_image` now finds its script: the route read CONFIGURED while every call deferred.
- **A ComfyUI that is not ours.** A tier that blanks `run_graph_script` no longer installs anything into a ComfyUI it finds on the box. Elsewhere, pip's stderr no longer kills the step.
- **PowerShell 7 launches.** Started through `Start-Process` from PowerShell 7, Windows PowerShell inherited module paths that hide `Get-FileHash`, and every download failed its hash check. `install.ps1` and the engine build now repair the module path first.
- **Smaller fixes.** The selftest's `$(if` expression and the fleet launcher's `harness/` path. A 64 GB APU with an 8 GB carve-out shows 55.8 GiB, which the installer read as 56 and the harness as 55, so they disagreed on the 26B's RAM gate. Both now round to nearest.

### Fixed - the selftest measures what it says on Windows

- **Port wait.** A force-killed server's port stays listed long after its process is gone, measured 15 s and 74 s with a model on the card, against the 8 s the next probe waited. The next canary then read "port busy" as a failed load.
- **Exit codes.** Windows PowerShell reads `ExitCode` as empty unless the process handle was taken while it ran, so a finished render read as a failure.
- **Warm numbers.** The micro-bench warms the tier first. It read 0.3 tok/s from a cold load.
- **Agent smoke.** `local-agent --serve` cold-loads its planner seat before it listens, for up to 10 minutes, and the smoke allowed 30 s. A 26B seat on an iGPU would have failed it every time.
- **Other people's servers.** The selftest counted and killed servers by install path, which would have killed an owner's live seats. It now acts only on children of its own swap.
- **Real lanes.** The whisper and new vision canaries run the real lanes through `acceptance --only vision,stt`. Promotions the canaries earn are recorded in the receipt's `tuned` block, and the receipt is saved under `<home>\receipts`.

### Changed - tier amd-rdna3 binds the agent lane on 64 GB boxes

- **What it does.** The mid and high RAM bands bind `gemma-4-26b-agent` from `config_seed_ram_mid_high` and opt the node into remote agent work with one worker. Below that band the lane stays on the workhorse and the node stays out of remote agent work.
- **Why.** The seat serves `--parallel 1`, and the same RAM gate renders it. The tier has no ComfyUI, so its script keys are blanked and `upscale_model` removed: the shipped defaults read as bound routes in `doctor`.
- **Check.** A full installer rehearsal for this tier on Vulkan in a sandbox profile, then one real call per lane. It ran on a discrete card, so its numbers are not 780M measurements.

### Changed - the unified-memory tiers cap the host prompt cache at 8192 MiB on 64 GB boxes

- **What it does.** A tier may carry its own `cache_ram_mib_by_ram_tier`; a band it names wins over the top-level map. `amd-rdna3` and `amd-gcn` name `mid: 8192`, so their seats render `--cache-ram 8192` on a 56 to 119 GiB box. Every other tier and band renders what it did.
- **Why.** On a unified-memory box the memory of the card is the RAM of the host. The 26B seat takes about 17 GiB of it, and the top-level 16384 MiB cache on top would reach about 33 GiB of a 56 GiB machine its owner also works on. 8192 is the server default, so the floor rule of ADR 0056 holds.
- **Check.** `install render -profile amd-rdna3 -ram-tier mid` writes `--cache-ram 8192` (it wrote 16384), `-ram-tier high` still writes 24576, `amd-gcn` mid writes 8192 and `blackwell-16` mid still writes 16384. The figure is chosen, not measured against a contract stream.

### Fixed - the merged branch satisfies the guards main gained

- **What it does.** The leak gate walks a WAV the way it walks a PNG: it checks the RIFF/WAVE container and each chunk length, scans every chunk that can carry text (LIST/INFO, bext, iXML, id3, cue, labl, note, and any id it does not know) and any bytes after the RIFF, and treats only the fmt and data payloads as opaque; a .wav that is not a valid RIFF/WAVE, or is truncated, fails closed. `acceptance/assets/jfk.wav` is rewritten without its encoder tag (same format, same samples). The six literal refusal and bad-argument reasons of the upscale and inpaint doors get the codes their kind already has (config for a refused binding or a missing sdcpp file, contract for an out-of-range inpaint argument). The compact-description keep entry for the inpaint failure sentence names both engines.
- **Why.** Main gained the leak gate's unknown-binary rule, the defer-reason code walk and the compact-description ledger after this branch was cut; the branch's speech sample, its new reasons and its reworded inpaint description met all three.
- **Check.** The leak gate, ledger and mcpserver packages pass, and the tracked-tree gate passes with the rewritten WAV (the PCM samples hash the same through ffmpeg before and after).
