### Fixed - `gpu reserve` for a ComfyUI run-graph declares what the graph loads, and a graph with no weight files declares the ComfyUI process floor, not the largest family's size

- **The gap.** `gpu reserve --class media -- node render/comfy-run-graph.mjs --graph g --manifest m` was not recognised as a
  render helper (the helper table left `comfy-run-graph.mjs` out, with a comment that it takes the graph positionally; it
  parses `--graph`, `--manifest` and `--template` as flags). The call fell to the media class default, the largest ComfyUI
  family the box binds, 32.9 GiB for a node that binds Wan 2.2, and a small graph was refused on a box whose commit charge was
  high unless the operator passed `--ram`. The sizing itself also gave up on a graph that names no weight file
  (`ForGraph`, `ForManifestModels`), so the run-graph lane's own admission fell to the same default.
- **What changed.** `comfy-run-graph.mjs` is recognised (route `run-graph`): `--graph FILE` is sized by the weight files its loaders
  name, and `--template NAME --manifest FILE` by the manifest's model files. A graph, or a manifest with an empty `models` list,
  that names no weight file is the measured ComfyUI process floor: 3 GiB touched and 4 GiB of commit (a small VHS GIF graph
  measured 2.63 GiB private and 3.64 GiB of system commit, rounded up), source `measured`. A graph with a loader node that names no
  file, a graph with no node, a weight under another extension and a graph whose weights are not on disk yet keep the class default.
- **Commit is stated.** The floor's 4 GiB is a stated commit (`Need.CommitGiB`, `Need.CommitStated`): the grant admits it against
  the OS commit limit like a `--ram-commit`, not against physical RAM less the headroom as an unstated commit is. `gpu reserve`
  (`resolveReserveHostLoad`), the pipeline's media admission, the whole-node acquire and the run-graph lane probe all carry it; an
  operator's `--ram` or `--ram-commit` still wins. The class-default hint now names `comfy-run-graph.mjs` and no longer says a graph
  that names no weight file takes the class default. Described in `docs/systems/gpu-lease.md` (what a lease declares).
