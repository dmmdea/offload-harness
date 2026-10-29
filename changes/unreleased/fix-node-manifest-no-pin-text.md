### Fixed - node packs carry no pin text, the run-graph cache notices a moved pack, and the install record says where a pack came from

- **The gap.** The 0.204.0 entry says no document describes a pin as policy any more, but `README.md` and the
  `offload_run_graph` tool description and `manifest_path` schema (shown to every MCP client) still read "custom node
  packs @ pinned commits via cm-cli". Nothing is pinned and cm-cli is not used: a pack is cloned from an allowlisted
  repository at the head of its default branch, and the satisfier drives git and uv directly. Those texts, the allowlist
  identifiers (`PINNED_NODE_PACKS`, `loadPinnedPacks`, `NodePackPin` and the like, now `ALLOWED_NODE_PACKS`,
  `loadAllowedPacks`, `AllowedNodePack`), the "pin-set" and "host-pin drift" wording, and the dead `cmCli` argument now say
  what the code does. The `VENV_INCOHERENT` detail for a moved host package now begins "host constraint drift"; callers
  branch on `code`.
- **The cache ignored a moved pack.** The run-graph cache skipped provisioning whenever the `.offload-owned.json` manifest
  hash matched, and since 0.204.0 that hash holds no commit, so a pack an operator moved, replaced or deleted was never
  resolved again while the marker matched, against `docs/flows/run-graph-manifest-satisfaction.md`. The marker now also
  holds the sorted `name@commit` key of the packs on disk, read with one `git rev-parse HEAD` per named pack; the cache
  applies only when the hash and the key both match. The first request after the upgrade provisions once.
- **The install record.** `custom_nodes/<name>.installed.json` is now schema 2: `{schema, repo, commit, installed_at,
  checked_at, previous, rejected}`, where `repo` is the origin the checkout reports, `previous` is the commit and time
  the pack held before, and `rejected` lists commits an install failed on. It is a record of what is installed, not an
  instruction. When the resolve, `pip check` or the host-constraint check fails in the same satisfy that cloned a pack,
  that directory is removed again (a pack that was already there is never touched) and its commit is added to
  `rejected`; a later request that finds the repository's newest commit there defers `NODE_PACK_REJECTED` instead of
  cloning and failing again, and a newer commit is tried afresh. The pack leg now beats the lease progress file around
  checkout, resolve and `pip check`.
- **Installer.** `install.ps1` no longer clones ComfyUI-Manager or installs GitPython for run-graph, which uses neither; a
  Manager clone failure used to abort the installer. `uv` stays required and `comfy-cli` stays optional.
- **Docs.** The setup, operator, fleet-node, glossary, manifest-flow and media-generation pages say the same; the
  MultiGPU archival note says the mirror's default branch carries the Windows/WDDM P2P fix.
- **Tests.** The pack-checkout tests in `manifest-satisfy.test.mjs` wrote their record into the real ComfyUI directory of
  the machine running them; they now use a temporary directory.
- **Fewer false rejections.** A fresh pack whose install failed on the network, a timeout, a busy card or a subprocess that would not start is removed again but its commit is not added to `rejected` (the defer is retryable); `pip check` is read before the clone and a fresh pack is rejected only when it gains lines, and a `pip check` failure whose lines all predate the satisfy no longer defers the run: it goes on and the result carries `warnings` naming them (a conflict between two packs' own requirements, such as albumentations 2.0.8 against albumentationsx, used to defer every graph on the box); a `rejected` entry carries a short sha256 of the venv's `pip freeze` and is ignored once that has changed.
