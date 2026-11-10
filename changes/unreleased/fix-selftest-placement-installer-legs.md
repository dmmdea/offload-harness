### Fixed - the selftest probes load the 26B at the tier's shipped expert placement

- **What it does.** On a tier whose profile says `moe_26b: n_cpu_moe` (blackwell-16, N=14) the ctx probe, the cold-swap timing, the `fa_q8kv` and `ctx_sweep` loads and the `bench` run now start the 26B with `--n-cpu-moe N -ngl 999`, the placement the installer renders for that tier, instead of full offload. Before, a display card either ran out of memory and the probe printed a false "apply ctx 16384" recommendation, or spilled and reported a slow number. Tiers with `moe_26b: gpu` and every other model load exactly as before.

### Fixed - the selftest and installer wording about expert placement is true again

- **What it does.** The serving-template header, `SETUP-AGENT.md`, `OPERATOR-GUIDE.md`, the setup-installer doc and the selftest's own comments, receipt strings and step titles no longer say that a 16 GB tier places experts on the card or that the installer places none in host RAM, and no longer list the retired `moe_full_offload` canary as "not run". The blackwell-16 report states its margin correctly: 1794 MiB under the heaviest desktop seen (16303 - 9109 - 5400 MiB), 2.6 GB at the 4546 MiB desktop.

### Fixed - `local-offload install forecast` builds in the install root by default

- **What it does.** With no `--home` the verb resolves the install root the way `update` does (`$OFFLOAD_HOME`, else the nearest `installed.json`) and only then falls back to `~/.local-offload`, so the venv lands where the forecast seat looks for it. The first progress line names the home it chose and where that came from; the `--home` help and `SETUP-AGENT.md` say so, and the checkpoint size is stated with its unit (837 MB on disk, decimal; the 804 MiB in `profiles.json` is the weights on the card).

### Added - `OFFLOAD_WITH_FORECAST=1` builds the forecast runtime during a Windows install

- **What it does.** After the harness is built, `install.ps1` runs `install forecast --home <root> --models <root>\models` for a profile that declares a `forecast` seat, and prints a one-line SKIP when `uv` or `git` is missing or the profile has no seat. Without the switch, when the seat is bound and `<root>\forecast\venv` is absent, the closing summary prints one line with the exact command. `install.sh` has no such leg.

### Fixed - `install.ps1 -RenderOnly` without `-RenderOut` no longer deletes the live `llama-swap.yaml`

- **What it does.** The render goes to a file beside the live yaml and replaces it only after the render succeeded, keeping the previous file as `llama-swap.yaml.prev-<stamp>`. With `-RenderOut` the behaviour is unchanged.

### Fixed - the standalone `detect.ps1` does not call the profile drive the install target

- **What it does.** With `OFFLOAD_HOME` unset, `detect.ps1` measures free disk on the profile drive and its line now says that this is a fallback and that `install.ps1` picks a data volume; with the variable set it still says "install target". `setup-installer.md` also records how an upgrade picks up `--slot-save-path` (the Step 6 `--state` check re-renders an unedited yaml; a hand-edited or unstamped one is kept until the operator replaces it).

### Fixed - the render gate calls the generative-edit and run-graph doors

- **What was wrong.** `acceptance --render` listed `gen-edit` and `run-graph` as a permanent SKIP even when their routes were bound. It said generative edit has no CLI verb (`edit-image-generative` exists) and that run-graph has no sensible embedded graph, so a node could pass the gate with both doors never called.
- **What it does.** When `edit_image_generative` is bound, `gen-edit` runs `edit-image-generative` on the embedded sign (`make the circle green`, seed 7) with the `gen_edit_timeout_sec` limit. When `run_graph` is bound, `run-graph` writes a two-node ComfyUI core graph (an empty 64x64 image and a SaveImage) into the gate's out dir and runs `run-graph --graph <file> --out-dir <subfolder>`. Both stay a SKIP with the route reason when the route is not bound. `docs/systems/fleet-node.md` lists them.
- **Checks.** `gen-edit` passes when the output PNG decodes; `run-graph` passes when the answer's `image_path` is a PNG that decodes to 64x64. `acceptance --list` prints both commands.

### Fixed - the installer's run-graph step reads the ComfyUI folder from the existing config

- **What was wrong.** Step 8b chose `COMFY_DIR`, else `C:/ComfyUI`. A node whose config already said `"comfy_dir": "D:/ComfyUI"` printed `SKIP run-graph deps (no ComfyUI at C:/ComfyUI ...)` and its run-graph tooling was never ensured.
- **What it does.** The folder is now `COMFY_DIR`, else the `comfy_dir` of the existing harness config, else `C:/ComfyUI`. The config is read once for this and for the `run_graph_script` check. Every other branch of the step is unchanged.
- **Checks.** A replay of the chosen lines against a temp config gave `E:/FromEnv` with the variable set, `D:/ComfyUI` with only the config, and `C:/ComfyUI` with neither.

### Fixed - the installer rebuilds the binaries when the checkout moved, not only when VERSION changed

- **What was wrong.** A re-run of `install.ps1` from a newer commit with the same `VERSION` kept the old `local-offload.exe` and `local-agent.exe`, because the build steps decided "up to date" from the version alone.
- **What it does.** After each build the installer writes the checkout's `git rev-parse HEAD` to `<exe>.built-from` beside the binary. The build is skipped only when the version matches and that file holds the current HEAD. A missing file, a missing `git` or a tree that is not a git checkout means the commit is unknown, so it rebuilds (go's build cache keeps that quick). `install.sh` installs a binary it is given and builds nothing, so it is unchanged.
- **Checks.** A replay of the decision: same commit skips; a different commit, no record and a non-git folder all rebuild.
