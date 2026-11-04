### Fixed - the run-graph deps step skips a ComfyUI folder that has no venv instead of killing the install

- **What it does.** Step 8b of `setup/install.ps1` (run-graph satisfier tooling, documented as best-effort)
  now runs only when BOTH the ComfyUI directory and its venv python (`COMFY_PY`, default
  `<comfy_dir>/.venv/Scripts/python.exe`) exist. A directory without the python prints a `SKIP` that
  names the missing file and the `COMFY_PY` override.
- **Why.** The step gated on the directory alone. A box with a bare or half-made `C:/ComfyUI` folder
  and no venv reached `Ensure-RunGraphDeps`, which called the missing `python.exe`; under
  `$ErrorActionPreference = 'Stop'` that `CommandNotFoundException` ended the install one step before the
  version manifest and the final verdict were written, so `installed.json` carried components but no
  `profile`. Found on the first fresh Windows install (an RTX 5080 desktop, 2026-11-04).
- **Check.** Re-running the installer on that box printed the `SKIP` line and went on to the manifest
  and the `{"installed":true,...}` verdict.
