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
