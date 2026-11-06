### Added - `local-offload acceptance --render`: one real output per bound route

- **What it does.** `acceptance --render [--only a,b] [--list] [--out-dir DIR] [--json]` runs each bound route once through this binary's real verb and checks the output against a known answer: summarize, classify, embed, vision, stt, agent, image, video, animate, voice, music, compose, edit, media, inpaint, upscale. An unbound route is a named SKIP, and a deferred run is a FAIL, because a route that did not run proves nothing. Outputs stay in the output folder.
- **Why.** A node counted as installed once `doctor` was green, and `doctor` only reads configuration. A bound route could still fail on its first real job.

### Added - tier amd-rdna3 binds video, animate, voice and music on 64 GB boxes

- **What it does.** The mid and high RAM bands bind FastWan video, VACE animate with depth, Chatterbox voice and ACE-Step music from `config_seed_ram_mid_high`. The Windows installer fetches every weight the seed names, about 28 GB, and builds what has no usable Windows binary.
- **Builds.** `setup/build-engine-vulkan.ps1` builds whisper-server, da3-cli and a patched audiocpp_cli on the box with a portable MSYS2 under `<home>\toolchain`. It takes no admin rights, runs at below-normal priority on half the cores, and refuses a binary with no Vulkan link. A failed build warns and the install goes on: the route it feeds reads BOUND-BUT-MISSING and the render gate fails it by name.
- **Limits.** The timeouts, token caps and sizes are amd-gcn's, measured on a Vega 7 under Linux, so they stay projected until a 780M has run them. `local-offload update` has no audio.cpp or depth-anything row yet, on this tier or on amd-gcn.

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
