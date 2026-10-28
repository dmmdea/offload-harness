### Fixed - the HyperFrames lane no longer refuses to run unless one exact version is installed: the harness installs the newest npm release and the runner requires only a floor

- **What was wrong.** The composition lane was welded to one HyperFrames release. The runner
  (`render/compose-hyperframes.mjs`) threw `CLI_MISSING` on any version but its exact-version constant, the
  Go installer (`internal/hfinstall`) embedded a committed `package-lock.json` and compared the runner's
  pin, the lock and `package.json` for equality, `local-offload install hyperframes` compared the
  chrome-headless-shell build, and capability status read `BOUND-BUT-MISSING` whenever the install
  differed from the pin. A newer upstream release was therefore refused by every layer until someone
  edited five files in step, and a node that fell one release behind answered `CLI_MISSING` to every
  composition. Nothing about a newer release is wrong in itself.
- **The fix.** The installed HyperFrames is a record with a floor. `setup/hyperframes/package.json`
  depends on `latest` and the lockfile is gone; the installers run `npm install --ignore-scripts` in a
  staging copy (npm verifies each tarball against the registry's integrity field), run
  `npm audit signatures`, read the staged `node_modules/hyperframes/package.json`, check its version
  against the `hyperframes` floor in `setup/floors.json`, and swap it in with the previous tree kept as
  `node_modules.prev`. Every swap is a whole directory rename: PowerShell's `Move-Item` is not one (when a
  render holds a file it moves what it can and splits the tree), so `setup/install.ps1` renames through
  `Move-DirAtomic`, which also carries the engine installer's legacy-folder migration and staged-generation
  move. A refused swap changes nothing; one that fails after the old tree moved aside puts it back. A lockfile an
  older installer left is retired to `package-lock.json.prev`, and a rollback restores only the `.prev` files its
  own swap wrote. An install at or above the registry's newest is kept, never downgraded. The
  version and time are recorded in `installed.json` under `components.hyperframes.current`
  (`{id, version, installed_at}`; a flat schema-1 entry is replaced by the record). The runner's
  `assertInstalled` throws `CLI_MISSING` only when the CLI is absent or below the floor, and capability
  status (`BOUND-BUT-MISSING`) agrees. The exact-version constant, its assertion, the drift, pin-match and lock-pin helpers and the
  chrome-headless-shell comparison are removed: HyperFrames fetches its own browser and
  the harness only reports where it put it. The `version` op now answers `{version, floor, at_floor}`.
- **What an operator sees.** `local-offload install hyperframes` moves a node to the newest release (or
  says it already is), prints the browser path, and `installed.json` names the version. A release that
  changes a flag the runner relies on is absorbed in the runner's argument builders, not pinned away.
