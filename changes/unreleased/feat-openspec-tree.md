### Added - the openspec/ tree, a forked harness-change schema with an optional ADR artifact, and the pinned, lockfile-backed install and validate scripts

- **What it does.** `openspec/` holds the spec and plan artifacts of a change for the pinned OpenSpec CLI (1.14.1). `config.yaml`
  names the project-local schema `harness-change`, carries the repository's standing invariants as the `context` every artifact
  is written under (quality outranks speed with no wall-time term in a pass rule, no CPU inference, idle models unload at 300 s,
  no daemon or scheduler, public-repo privacy with node letters only, one changelog fragment per change, ADR for a decision,
  red-first tests, the leak gate), and adds per-artifact `rules` (a design names the ADR it needs, tasks start with a failing
  test and name their verifying command). The schema is a fork of `spec-driven` that keeps proposal, specs, design and tasks and
  adds `adr`, an ADR draft that requires `design` and that nothing requires, so it never blocks tasks or apply. The OpenSpec
  artifact schema has no `optional` key (an unknown key is accepted and ignored), so the optionality is the shape of the graph,
  pinned by a test; the generated archive skill still asks for confirmation when a change has no `adr.md`, and the system page
  says to confirm and move on. `scripts/openspec-install.sh <prefix>` fetches the tarball with `npm pack
  @fission-ai/openspec@1.14.1 --ignore-scripts`, refuses on any sha256 other than the admitted one, stages the committed
  `scripts/openspec/package.json` and `package-lock.json` beside that verified file and runs `npm ci --ignore-scripts` there,
  so the CLI and all 69 dependencies are exact versions under sha512 integrity values and none can drift between installs. It
  checks the staged CLI's version before moving `node_modules` into the prefix, replaces an earlier install of this CLI there
  and refuses a `node_modules` that is something else's, keeps npm's cache, temp files and the CLI's config home under the
  prefix, and prints the four environment exports (telemetry off, update check off, config home).
  `scripts/openspec-validate.sh` sets the three switches and runs `openspec validate --all --strict --no-interactive` plus
  `openspec schema validate` for each local schema, which `validate --all` never reads. `docs/systems/openspec.md` is the new
  system page, linked from the admitted-software row in `docs/systems/security.md` and from the docs index; CONTRIBUTING's
  pre-pull-request list now names the validate step for a change under `openspec/`, because `go test ./...` runs the real
  validator only where `OPENSPEC_BIN` is set.
- **Why.** Specs and plans for a change had no fixed shape and no validator. The pilot gives them both without touching how a
  change is executed or shipped: OpenSpec owns spec and plan artifacts only, and no other workstream depends on it. Pinning the
  tarball alone left the dependency set to the registry at install time; the lockfile closes that.
- **Tests.** Red first (the tests were committed before the tree, the scripts and the lockfile existed). The structural tests
  need no CLI: the config names a schema that exists, the spine is intact and `adr` requires exactly `design` and is optional
  by the graph, rules name real artifacts, the context keeps each invariant (including the no-cloud-fallback line), and the
  lockfile holds exact versions, sha512 integrity values and registry URLs for every package. The script tests run both scripts
  against stand-in `openspec` and `npm` programs: the switches are forced whatever the caller exported, the strict arguments
  are exact, a failing validator fails the gate, a tarball with the wrong hash is refused before any install and leaves no
  scratch directory, the script's pins equal the admitted row, and a matching tarball installs through `npm pack` then
  `npm ci` in a staged copy of the committed lockfile project with npm's cache and temp folders under the prefix even when the
  caller exported others, a wrong installed version installs nothing, an earlier install is replaced and a foreign
  `node_modules` is left alone. `TestOpenSpecInstallScriptInstallsTheRealPackage` runs the script for real (npm and the
  network) when `OPENSPEC_INSTALL_TEST=1`, with every home, data and temp folder pointed at an empty folder beside the prefix,
  and fails if any gains a file or if the installed packages differ from the lockfile; it skips with the reason elsewhere.
  `TestOpenSpecTreeValidates` runs the real validator where `OPENSPEC_BIN` names it and skips with the reason elsewhere.
