### Fixed - the dev tooling and runtime refusals carry no version pins: ComfyUI node packs are admitted by repository, the forecast lane resolves at install time, CI reads its floors and tracks the newest releases

- **What changed.** ComfyUI node packs are admitted by repository (`render/node-packs.json` has no commit column);
  a node clones a pack it does not hold at the head of the default branch and records the commit beside the pack. A
  render request never moves an installed pack: updating one is the operator's (`git pull --ff-only` in its folder,
  or the fleet's update tooling), so a holder of the render bearer cannot make a node fetch an upstream's newest push.
  The dependency marker is keyed on the recorded commit, so a pack the operator moved is re-resolved. The forecast
  installer (`install forecast`) resolves the Hugging Face revision of `main` and the weights' digest and size from
  the publisher's API at install time, and installs the timestral package from the head of its default branch;
  python stays a floor (`>=3.13`). The browse sidecar takes `browser-harness>=0.1.13` and `jev-ultrafast` from its git
  default branch, installed with `uv sync --upgrade`, so every install run re-resolves to the newest whatever
  `uv.lock` the install folder already holds. CI takes the newest stable Go (`go-version: stable`, `check-latest`; `go.mod`'s `go` line stays the floor), tracks the
  current Node, installs `govulncheck@latest`, and no longer demands the compiler equal a `toolchain` line, which is
  gone from `go.mod`. The vendored llama-swap config schema is a refreshable snapshot
  (`scripts/refresh-llamaswap-schema.sh`), and `version drift` treats it as a floor: only a server older than the
  snapshot is drift. The vLLM and LMCache patch notes say the patches target the version they were written against and
  carry a re-base duty. `no_version_pins_test.go` now also scans `render/`, `install_*.go`, `scripts/openspec*`,
  `accelerators/`, the browse and HyperFrames manifests and the CI workflow.
- **Accelerators.** `accelerators/coral/models.json` and `accelerators/rknpu/models.json` carry no `sha256` and no
  version. Both `fetch-models.sh` scripts resolve at run time and verify against the publisher: Coral reads the git
  blob id of each file at the tip of `google-coral/test_data`; RKNPU takes the newest non-draft release of
  `airockchip/rknn-toolkit2` (runtime library and the wheel for the venv's python) and `airockchip/rknn_model_zoo`
  (labels, test images, the ONNX download links) that carries every file asked of it, and the timm weights' `lfs.oid`
  from the Hugging Face tree API at `main`. Each file that passes is recorded as `{file, digest, publisher_digest,
  size, source, fetched_at}` in `models.installed.json` beside the weights, and the sidecars refuse a model whose
  bytes differ from that record (`model_sha_mismatch`); a file with no record is served and listed under
  `models_unverified` in `/health`. `fetch-models.sh --dry-run` prints the resolved digests and downloads nothing. The
  conversion recipe's pip lines are floors.
- **OpenSpec.** `scripts/openspec-install.sh` installs `@fission-ai/openspec` at the registry's `latest` with
  `npm install --ignore-scripts`, then checks the installed package's recorded integrity against a second
  `npm view ... dist.integrity` and runs `npm audit signatures`; `OPENSPEC_VERSION`, `OPENSPEC_SHA256` and
  `scripts/openspec/package-lock.json` are gone, and the admitted-software table in `docs/systems/security.md`
  names a source instead of a version and hash.
- **ComfyUI rule files.** The two rule-file hashes `render/templates-catalog.mjs` compared against are no longer
  literals in the runner. `templates-catalog.mjs record-rules --comfy-dir <dir>` records them in
  `.offload-rule-files.json` in the ComfyUI folder when a checkout is registered or refreshed; readiness compares a
  node's current files with that record and says "not recorded" when there is none.
- **Why.** A pin freezes every install at one release and rots against the fleet; floors live in `setup/floors.json`
  and `go.mod`'s `go` line only (ADR 0099).
- **Upgrade notes.** A manifest that still names a `commit` for a node pack is accepted and the commit is not read.
  Re-run `fetch-models.sh` on each accelerator box once: a model with no record in `models.installed.json` is served as
  unverified until then, and a converted RKNPU model is recorded by `fetch-models.sh --convert` on the build host.
  `version drift --json` keeps its `cli_pinned_surface` field name for schema compatibility; `comfy features --json`
  renames `pinned_api_version` to `known_api_version`.
- **Docs.** No document describes a pin, a lockfile, a blessed version or an exact sha as policy any more. The accelerator
  page has a section on how the Coral and RKNPU artifacts are fetched and verified (resolve at fetch time, the publisher's
  digest, the `models.installed.json` record, `models_unverified`, `--dry-run`); the media-generation, operator, setup,
  fleet-node, run-graph and glossary pages say a node pack is admitted by repository and its commit is recorded, that
  HyperFrames is the newest npm release at or above the floor, and that the ComfyUI rule files are compared with the
  record `templates-catalog.mjs record-rules` writes (`.offload-rule-files.json`, "not recorded" when absent). The Coral
  design and the browse-lane plan carry a dated line saying their pins were replaced by resolve-at-fetch (ADR 0099); the
  ADRs that decided a pin (0007, 0023, 0059, 0060, 0062, 0081, 0090, 0098) keep their text and carry a
  `Superseded by ADR 0099 for the pinning mechanism` status line, mirrored in the ADR index.
