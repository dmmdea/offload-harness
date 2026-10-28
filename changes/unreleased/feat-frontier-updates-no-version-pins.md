### Changed - the harness never pins a version: installers resolve the newest complete build from the publisher, floors live in one file, and a lint keeps it that way

- **What it does.** No release tag, commit, byte count or digest literal for a dependency lives in the repository any
  more. `setup/install.ps1` and `setup/install.sh` resolve each component (llama.cpp, llama-swap, whisper.cpp,
  stable-diffusion.cpp, the models) from its publisher at run time, take the newest complete build, and verify the
  publisher's own digest: the GitHub release `assets[].digest` (and `gh attestation verify` where a project publishes
  attestations), the Hugging Face `lfs.oid` and size, the npm `dist.integrity`. A mismatch deletes the partial file and
  retries (three attempts); a verified file is swapped in once, outside the retry loop, the old one kept as `.prev`. A fresh install lands on the newest build, a re-run over a newer
  build changes nothing, and a newer hand-placed build is kept, never downgraded.
- **Floors, not pins.** The minimum build a seat needs (llama.cpp b11452 for `gemma-embedding2`, stable-diffusion.cpp
  master-968 for FLUX 3) lives in exactly one file, `setup/floors.json`. A floor only rises; a ceiling or an exact
  version is unrepresentable there; a candidate under a floor is never selected. The placement pin of ADR 0078 (a model
  held to a card) is unrelated and stays.
- **Generations with rollback.** Per component a node keeps the current and the previous generation. The live path
  (`<home>/llama`, `<home>/sdcpp`, ...) becomes a junction or symlink to a write-once `<component>-versions/<id>`
  directory, so no rendered config, task or probe changes; a candidate is staged, verified and switched atomically, and
  a failed gate or first load points the live path back at the previous generation. An engine whose live directory
  a running process holds is staged, reported and recorded as `staged`, never swapped out from under it. `installed.json`
  (schema 2) records per component the version, the publisher's digest, the size, the source and the install time, `dest`
  for a model file and `staged` for a generation waiting for a restart, each written the moment its component is live;
  the update verb (its own change) adds `current`, `previous`, `rejected[]`, the ETag and the check time.
- **The lint.** `no_version_pins_test.go` fails when an installer, a render script, a dependency manifest or an
  accelerator recipe carries a release tag, a build number, a 40- or 64-hex digest or an exact dependency version, naming
  file and line; `setup/floors.json` is the one place a build number may appear. The repository `AGENTS.md` and
  `CLAUDE.md` carry the rule in one sentence each.
- **Docs.** ADR 0099 (`docs/architecture/decisions/0099-frontier-updates-floors-never-pins.md`) records the rule, the
  publisher-integrity source, the generations layout, the update verb and the lint, and reverses the 2026-08-27 "blessed
  tag, never the newest nightly" policy: a measured gate with automatic rollback replaces curation, and the `release`
  channel stays selectable per node. ADR 0081's second-build doctrine is superseded (its status reads Superseded by 0099;
  the body is kept as history). The setup runbook (Hard rule 2 and the integrity-mismatch rows), the system docs, the root
  docs, the glossary (Floor, Version pin, Placement pin, Generation), the maxwell-3 design and plan, the older specs and
  plans, the profile notes and the regenerated `docs/tiers/` pages no longer state pinning as policy; dated measurements
  ("measured on b11490 on 2026-10-28") stay as records. The frozen 2026-08-26 roadmap gains one line marking it a
  historical snapshot.
- **Check.** `go generate ./...` regenerated `docs/tiers/*.md` from `setup/templates/profiles.json`; `go run
  ./cmd/changelog check` and the tier-docs, operator-identity and changelog-tree tests pass; a search of the docs, the
  runbook, the profile notes and the root docs for the old pin wording returns nothing.
