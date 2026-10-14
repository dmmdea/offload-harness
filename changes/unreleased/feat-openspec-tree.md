### Added - the openspec/ tree, a forked harness-change schema with an optional ADR artifact, and the pinned install and validate scripts

- **What it does.** `openspec/` holds the spec and plan artifacts of a change for the pinned OpenSpec CLI (1.14.1). `config.yaml`
  names the project-local schema `harness-change`, carries the repository's standing invariants as the `context` every artifact
  is written under (quality outranks speed with no wall-time term in a pass rule, no CPU inference, idle models unload at 300 s,
  no daemon or scheduler, public-repo privacy with node letters only, one changelog fragment per change, ADR for a decision,
  red-first tests, the leak gate), and adds per-artifact `rules` (a design names the ADR it needs, tasks start with a failing
  test and name their verifying command). The schema is a fork of `spec-driven` that keeps proposal, specs, design and tasks and
  adds `adr`, an ADR draft that requires `design` and that nothing requires, so it never blocks tasks or apply. The OpenSpec
  artifact schema has no `optional` key (an unknown key is accepted and ignored), so the optionality is the shape of the graph,
  pinned by a test. `scripts/openspec-install.sh <prefix>` fetches the tarball with `npm pack @fission-ai/openspec@1.14.1
  --ignore-scripts`, refuses on any sha256 other than the admitted one, installs from that file with `--ignore-scripts`, keeps
  npm's cache and temp files under the prefix and prints the four environment exports (telemetry off, update check off, config
  home). `scripts/openspec-validate.sh` sets the three switches and runs `openspec validate --all --strict --no-interactive`
  plus `openspec schema validate` for each local schema, which `validate --all` never reads. `docs/systems/openspec.md` is the
  new system page, linked from the admitted-software row in `docs/systems/security.md` and from the docs index.
- **Why.** Specs and plans for a change had no fixed shape and no validator. The pilot gives them both without touching how a
  change is executed or shipped: OpenSpec owns spec and plan artifacts only, and no other workstream depends on it.
- **Tests.** Red first (the tests were committed before the tree and scripts existed). The structural tests need no CLI:
  the config names a schema that exists, the spine is intact and `adr` is optional by the graph, rules name real artifacts, the
  context keeps each invariant. The script tests run both scripts against stand-in `openspec` and `npm` programs: the switches
  are forced whatever the caller exported, the strict arguments are exact, a failing validator fails the gate, a tarball with the
  wrong hash is refused before any install and leaves no scratch directory, and the script's pins equal the admitted row.
  `TestOpenSpecTreeValidates` runs the real CLI where `OPENSPEC_BIN` names it and skips with the reason elsewhere. The scripts
  were also run for real against a scratch prefix: the pinned tarball's hash matched and the installed package set equals the
  existing install.
