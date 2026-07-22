# Run-graph manifest satisfaction

## Purpose

How `offload_run_graph` takes a caller-supplied ComfyUI graph plus a Node Manifest, makes the
environment real (node packs at pinned commits, model files present), runs the graph, and returns
node-addressed outputs — or a typed Defer explaining precisely what it could not satisfy.

This is the generic primitive that lets a workflow repository own graph authoring while the harness
owns execution.

## Trigger

`offload_run_graph` (MCP) or `run-graph` (CLI), with `graph_path` or `graph_json`, `manifest_path` or
`manifest_json`, an `out_dir`, and an optional VRAM reservation.

## Participants

The Go entry (`internal/rungraph`), the Node runner (`render/comfy-run-graph.mjs`), the satisfier
(`render/manifest-satisfy.mjs`), the manifest parser (`render/manifest.mjs`), preflight
(`render/preflight-graph-file.mjs`), the GPU lock, and a local ComfyUI installation with its virtual
environment.

## Step-by-step flow

1. **Parse the manifest.** Normalized to `schema_version`, `workflow`, `comfyui_min_version`,
   `node_packs[{name, repo, commit}]`, and `models[{path, source_url, sha256}]`. `repo` and `commit`
   are required per pack; `name` is derived from the repository when absent. Only `path` is required
   per model — `sha256` defaults to null.

2. **Check tooling.** The satisfier's hard requirement is `uv` alongside the ComfyUI virtual
   environment's Python. Missing → `SATISFIER_UNAVAILABLE`. Skipped entirely for models-only or empty
   manifests.

3. **Check the ComfyUI version** against `comfyui_min_version` → `COMFY_VERSION_BELOW_MIN`.

4. **Satisfy models.** Presence is resolved across ComfyUI's **full model search path** — the
   canonical `<comfy_dir>/<model.path>` first, then every directory registered for that model class
   in `<comfy_dir>/extra_model_paths.yaml`. This matters on machines that keep the model tree off the
   OS drive (e.g. a `V:` Optane tree): checking `comfy_dir` alone made such a model read as *missing*
   and re-download tens of GB per run. The YAML is parsed by a minimal, dependency-free reader that
   understands `base_path` plus `category: dir` entries and block scalars (`unet: |` listing several
   dirs); it is fail-safe — anything unparseable yields no extra roots, i.e. the old comfyDir-only
   behavior.

   Given the file is found: no declared sha256 → trust it. A `.sha-ok` sentinel → already verified,
   skip. **Present with a declared sha but no sentinel** (the hand- or `curl`-provisioned case) → hash
   it **once** and, if it matches, *adopt* it by writing the sentinel beside the file that was
   actually found. If it mismatches and a `source_url` exists the file is replaced; with no
   `source_url` it defers `MODEL_SHA_MISMATCH` — naming the real problem rather than the misleading
   "missing on disk". Otherwise download from `source_url`, verify, and write the sentinel. No
   `source_url` → `MODEL_DOWNLOAD_FAILED`. Models downloaded without a declared hash are reported as
   `unverified_models` in the success envelope rather than silently trusted.

5. **Clone or check out each node pack** at its pinned commit. A pack is considered changed by
   comparing git HEAD before and after — a same-commit checkout is correctly a no-op. Packs are placed
   *before* dependency resolution, because the unified resolve reads their on-disk requirements.

6. **Resolve and install dependencies once**, for all packs together, under host-pin constraints that
   prevent anything moving `torch`, `torchvision`, `torchaudio`, or `numpy`. The resolve runs on
   every satisfy EXCEPT the proven re-run: it is skipped only when no checkout moved AND a
   **persisted marker** proves this exact pin-set was already resolved+checked successfully.
   `uv` resolves; `python -m pip` installs. The marker (`<venv>/.offload-deps-satisfied` — it
   attests venv state, so it lives inside the venv and dies with it; holding the pin-set
   key `name@commit|…`), written ONLY after a fully successful resolve+check — "git didn't move this
   run" alone proves nothing: a prior run that checked packs out and failed before installing must
   still resolve. A tripwire re-checks the pins afterward with full local versions; the cheap
   `pip check` coherence gate always runs. Failure → `VENV_INCOHERENT`. See
   [ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md).

   **Spawn failures are classified apart** (live report 2026-07-20: a transient `spawn UNKNOWN`
   after a long batch was misreported as venv incoherence): if a satisfier subprocess (git, uv,
   pip) fails to *start*, it is retried once after 500 ms, then defers **`SATISFIER_SPAWN_FAILED`**
   — never `VENV_INCOHERENT`, because "the check could not run" says nothing about the venv. One
   deliberate, narrow fail-open: when the pin-set is unchanged AND the satisfied-marker proves it
   was previously resolved+checked, a coherence check whose subprocess fails to spawn yields a
   `warning` (stderr `SATISFY WARN`) instead of a defer. Anything unproven fails closed. The git
   checkout stage is deliberately NOT retried — a retry would recompute its HEAD-before against its
   own surviving side effects and misreport `changed=false`; a git spawn failure defers typed and
   the caller retries the whole satisfy, which is idempotent at that level.

7. **Start ComfyUI** through the zero-warm GPU lifecycle. An externally managed instance that lacks
   the required packs → `EXTERNAL_COMFY_NEEDS_PACKS`. Failure to start → `COMFY_START_FAILED`.

8. **Preflight the graph** against the running server's `/object_info`, per node class. Missing class
   → `NODE_CLASS_MISSING`; missing required inputs → `PREFLIGHT_MISSING_INPUTS`. The node-class check
   deliberately lives *here*, after ComfyUI is up with the packs installed — not in the satisfier,
   where the classes would not yet be loadable.

9. **Execute, poll, and fetch outputs**, then tear down through the same GPU lifecycle.

## Data and state changes

Node packs cloned into ComfyUI's custom-nodes directory at pinned commits; Python packages installed
into its virtual environment (additively); model files and `.sha-ok` sentinels written under the
ComfyUI tree; outputs written to `out_dir`. Successful renders also record a VRAM footprint
observation.

## Success behavior

```json
{ "outputs": { "<node_id>": [ { "path": …, "type": …, "kind": …, "width": …, "height": … } ] },
  "image_path": "…", "unverified_models": [] }
```

`outputs` is keyed by ComfyUI node id, so a caller addresses results by the node that produced them
rather than guessing from filenames. `kind` is one of `image`, `gif`, `video`, `audio`.
`image_path` is a convenience alias for the first image. `width`/`height` are read from PNG headers
and are therefore `0` for non-PNG outputs.

## Failure behavior

A typed Defer: `{ "deferred": true, "code": …, "ref": …, "detail": … }`. Codes emitted by this path:
`MODEL_DOWNLOAD_FAILED`, `MODEL_SHA_MISMATCH`, `SATISFIER_UNAVAILABLE`, `COMFY_VERSION_BELOW_MIN`,
`VENV_INCOHERENT`, `SATISFIER_SPAWN_FAILED`, `EXTERNAL_COMFY_NEEDS_PACKS`, `COMFY_START_FAILED`, `NODE_CLASS_MISSING`,
`PREFLIGHT_MISSING_INPUTS`, `RUN_ERROR`. A failure on the Go side of the call (rather than in the
runner) defers with a free-form `run-graph failed: …` reason and a lowercase `err_class`, not a typed
code — the `GPU_BUSY` and `TIMEOUT` tokens in the MCP tool description are advisory text, not codes
this path emits.

### Resolved defects

All four defects previously recorded here are fixed; they are kept as a short history because each
one shaped the current design.

1. **`require is not defined` in the model leg** — fixed in v0.22.3. Three `require()` calls sat
   inside an ESM module, so a present-but-unsentinelled model reported `MODEL_DOWNLOAD_FAILED` with
   detail `require is not defined`, and a successful fresh download threw *outside* the try/catch,
   escaping as an untyped process exit. Replaced with ESM imports and the sentinel write brought
   inside the guard. The `sha256: null` workaround this forced on callers is no longer needed.
2. **Caller-supplied `out_dir` not created** — fixed in v0.22.4 (`resolveOutDir` now creates it).
3. **Models over ~2GB could never be satisfied** — fixed in v0.22.9. The download buffered whole
   files via `Buffer.from(await r.arrayBuffer())` and hit Node's ArrayBuffer cap; it now streams the
   body to disk, which has no size limit.
4. **Models outside `comfy_dir` re-downloaded, and pre-provisioned files re-fetched** — fixed in
   v0.22.11. Presence checked only `comfy_dir`, so a model held on a secondary tree registered in
   `extra_model_paths.yaml` read as missing and re-downloaded tens of GB per run; and the skip gate
   trusted the `.sha-ok` sidecar alone, so a byte-correct hand-provisioned file fell into the
   download branch. Presence now spans the full search path, and a present file with a pinned sha is
   hashed once and adopted. This closes the old note that "there is no code path that hashes an
   already-present file" — there is one now, and it is the adoption path.

The historical root cause behind several of these was that `defaultSatisfyDeps` — the entire
production glue block — was imported by no test, so the suite stayed green while the path was broken.
It is covered now.

## External dependencies

A local ComfyUI installation with `uv` in its virtual environment, git for pack checkout, and network
access for pack clones and model downloads.

## Invariants and assumptions

1. Provisioning is additive and never moves the protected host packages.
2. Packs are placed before dependency resolution.
3. The node-class gate runs after ComfyUI is up, not before.
4. Failures are typed Defers, not crashes.
   Model presence is resolved across ComfyUI's full search path, so the harness and ComfyUI agree on
   whether a model exists; a file the graph can load is never re-downloaded.
5. The harness never adds models of its own; manifests are the caller's responsibility — which is
   also the boundary for
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md).

## Security and privacy notes

This is a **trusted-caller interface**: it executes caller-supplied graphs and installs
caller-specified code from caller-specified repositories at caller-specified commits. Pinning to exact
commits is what makes it auditable; it is not a sandbox. Do not expose it to an untrusted caller.

## Observability and debugging

The defer `code` identifies the stage; `ref` identifies the offending pack, model, or node class.

> **Diagnosability (fixed in stages):** since v0.22.5 the defer detail names the actual problem —
> host-pin drift reports *which* pinned package moved (expected vs observed), a genuine conflict
> reports pip's own message. Since v0.22.13 a subprocess that failed to *spawn* is a separate code
> entirely (`SATISFIER_SPAWN_FAILED`) rather than masquerading as incoherence.

A caller-supplied `out_dir` is **not** created for you — a missing directory produces an ENOENT at
first output write, surfacing as `RUN_ERROR`. Create it first. (The default output directory, used
when `out_dir` is empty, is created.)

## Testing notes

`render/manifest-satisfy.test.mjs` covers the models leg, defer codes, command builders, argument
injection rejection, and the host-constraint helpers. `render/comfy-run-graph.test.mjs` covers the
defer envelope and preflight paths. Go-side: `internal/rungraph/`, `internal/pipeline/pipeline_rungraph_test.go`.
Run the Node suites with `node --test render/*.test.mjs` from the repo root.

## Source map

- [`render/manifest-satisfy.mjs`](../../render/manifest-satisfy.mjs) — satisfier, host pins, tripwire
- [`render/manifest.mjs`](../../render/manifest.mjs) — schema normalization and hashing
- [`render/comfy-run-graph.mjs`](../../render/comfy-run-graph.mjs) — orchestration and envelope
- [`render/preflight-graph-file.mjs`](../../render/preflight-graph-file.mjs) — node-class gate
- [`internal/rungraph/`](../../internal/rungraph/) — Go entry and result types

## Related docs

- [../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md)
- [zero-warm-generation.md](zero-warm-generation.md)
- [../systems/media-generation.md](../systems/media-generation.md)
