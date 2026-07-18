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

4. **Satisfy models.** For each model, if the file exists *and* (has no declared sha256, or has a
   `.sha-ok` sentinel sidecar), skip it. Otherwise download from `source_url`, verify the hash, and
   write the sentinel. No `source_url` → `MODEL_DOWNLOAD_FAILED`. Hash mismatch →
   `MODEL_SHA_MISMATCH`. Models downloaded without a declared hash are reported as
   `unverified_models` in the success envelope rather than silently trusted.

5. **Clone or check out each node pack** at its pinned commit. A pack is considered changed by
   comparing git HEAD before and after — a same-commit checkout is correctly a no-op. Packs are placed
   *before* dependency resolution, because the unified resolve reads their on-disk requirements.

6. **Resolve and install dependencies once**, for all packs together, under host-pin constraints that
   prevent anything moving `torch`, `torchvision`, `torchaudio`, or `numpy`. `uv` resolves;
   `python -m pip` installs. A tripwire re-checks the pins afterward with full local versions. Failure
   → `VENV_INCOHERENT`. See
   [ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md).

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
`VENV_INCOHERENT`, `EXTERNAL_COMFY_NEEDS_PACKS`, `COMFY_START_FAILED`, `NODE_CLASS_MISSING`,
`PREFLIGHT_MISSING_INPUTS`, `RUN_ERROR`. A failure on the Go side of the call (rather than in the
runner) defers with a free-form `run-graph failed: …` reason and a lowercase `err_class`, not a typed
code — the `GPU_BUSY` and `TIMEOUT` tokens in the MCP tool description are advisory text, not codes
this path emits.

### Known defects

> **`require is not defined` in the model leg — open, verified 2026-07-18.**
>
> `render/manifest-satisfy.mjs` calls `require()` in three places inside an ESM module (in
> `writeSentinel` and `download`), where `require` does not exist. Two distinct failures result:
>
> 1. **A model present on disk with a declared `sha256` but no `.sha-ok` sentinel** — for example
>    placed by hand, or left by an interrupted earlier run — does not match the skip condition, so it
>    falls through to the download branch. The fetch succeeds, then `require` throws, and it is caught
>    and reported as `MODEL_DOWNLOAD_FAILED` with detail `require is not defined`.
> 2. **A fully successful fresh download** then calls `writeSentinel`, which is *outside* the
>    try/catch. That throw escapes the satisfier entirely and exits the process, producing an untyped
>    failure rather than a typed Defer.
>
> Setting `sha256: null` avoids both, because the skip condition then tests only file existence —
> which is why manifests with nulled hashes pass end to end. That is a workaround, not a fix, and it
> gives up hash verification.
>
> Note this is **not** a verification bug: there is no code path that hashes an already-present file.
> The gate is sentinel-based. Adding present-file verification would be a feature, not a repair.
>
> The three `require()` calls are the defect. Related gap: `defaultSatisfyDeps` — the entire
> production glue block — is not imported by any test, which is why the suite is green while this
> path is broken.

## External dependencies

A local ComfyUI installation with `uv` in its virtual environment, git for pack checkout, and network
access for pack clones and model downloads.

## Invariants and assumptions

1. Provisioning is additive and never moves the protected host packages.
2. Packs are placed before dependency resolution.
3. The node-class gate runs after ComfyUI is up, not before.
4. Failures are typed Defers, not crashes — with the escaping-throw case above as a known violation.
5. The harness never adds models of its own; manifests are the caller's responsibility — which is
   also the boundary for
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md).

## Security and privacy notes

This is a **trusted-caller interface**: it executes caller-supplied graphs and installs
caller-specified code from caller-specified repositories at caller-specified commits. Pinning to exact
commits is what makes it auditable; it is not a sandbox. Do not expose it to an untrusted caller.

## Observability and debugging

The defer `code` identifies the stage; `ref` identifies the offending pack, model, or node class.

> **Diagnosability gap:** host-pin drift and an ordinary dependency conflict both surface as
> `VENV_INCOHERENT` with detail `conflicting installed dependencies`. The actual drift diagnostic —
> expected versus observed pins — is written only to stderr. Read stderr before assuming a plain
> conflict.

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
