# Run-graph manifest satisfaction

## Purpose

How `offload_run_graph` takes a caller-supplied ComfyUI graph plus a Node Manifest, makes the
environment real (node packs from allowlisted repositories, model files present), runs the graph, and returns
node-addressed outputs — or a typed Defer explaining precisely what it could not satisfy.

This is the generic primitive that lets a workflow repository own graph authoring while the harness
owns execution.

## Trigger

`offload_run_graph` (MCP) or `run-graph` (CLI), with `graph_path` or `graph_json`, `manifest_path` or
`manifest_json`, an `out_dir`, and an optional VRAM reservation. `offload_run_template` (MCP, action `run`) enters the same flow
with a template **name** in place of the graph (see [Template runs](#template-runs)).

## Participants

The Go entry (`internal/rungraph`), the Node runner (`render/comfy-run-graph.mjs`), the satisfier
(`render/manifest-satisfy.mjs`), the manifest parser (`render/manifest.mjs`), preflight
(`render/preflight-graph-file.mjs`), the GPU lock, and a local ComfyUI installation with its virtual
environment.

## Step-by-step flow

1. **Parse the manifest.** Normalized to `schema_version`, `workflow`, `comfyui_min_version`,
   `node_packs[{name, repo}]`, and `models[{path, source_url, sha256}]`. `repo`
   is required per pack; `name` is derived from the repository when absent. A `commit` on a pack is accepted and not read. Only `path` is required
   per model — `sha256` defaults to null and is an optional assertion: it is never what a file is checked against (step 4), and it is not part of the cache key
   (`manifestHash` keys a model on its `path` and `source_url`).

1a. **Refuse any pack that is not on the allowlist.** A pack makes the node clone its repository and install its requirements, which is
   the caller's code running on the node, so a pack is taken only when its `repo` is a row of
   `render/node-packs.json` (exact string, and the row's directory name or the one the repository gives
   it). A fleet node refuses an unlisted pack at the door (`400` naming the pack, before a job or a temp file exists:
   `mediacap.CheckManifestNodePacks`), and the satisfier refuses again before the first tooling probe, model download or git
   command with the typed defer `NODE_PACK_NOT_ALLOWED` (`ref` = the pack). The table holds the packs the shipped graph builders
   need; a workflow that needs another adds a reviewed row to the file and to its Go copy.

2. **Check tooling.** The satisfier's hard requirement is `uv` alongside the ComfyUI virtual
   environment's Python. Missing → `SATISFIER_UNAVAILABLE`. Skipped entirely for models-only or empty
   manifests.

3. **Check the ComfyUI version** against `comfyui_min_version` → `COMFY_VERSION_BELOW_MIN`.

3a. **Refuse any model that is not under `models/` from a listed host.** A `models[]` entry is downloaded into the ComfyUI
   tree before ComfyUI starts, so its `path` must be `models/<class>/<file>` (backslashes read as separators; no empty, `.` or
   `..` segment, nothing beginning with a space or ending with a space or a dot, no control character or `:*?"<>|`) and its
   `source_url`, when it has one, must be `https://<host>/...` on a plain DNS name that is, or is a subdomain of, a
   `model_hosts` entry of `render/node-packs.json`. A fleet node refuses an entry that is not (`400` naming it, before a job
   or a temp file exists: `mediacap.CheckManifestModels`), and the satisfier refuses again, before it looks for, hashes or
   fetches any file, with the typed defer `MODEL_NOT_ALLOWED` (`ref` = the path). Every entry is read before the first is
   provisioned. `render/testdata/manifest-model-cases.json` is the one table both checks are tested against.

4. **Satisfy models.** Presence is resolved across ComfyUI's **full model search path** — the
   canonical `<comfy_dir>/<model.path>` first, then every directory registered for that model class
   in `<comfy_dir>/extra_model_paths.yaml`. This matters on machines that keep the model tree off the
   OS drive (e.g. a `V:` Optane tree): checking `comfy_dir` alone made such a model read as *missing*
   and re-download tens of GB per run. The YAML is parsed by a minimal, dependency-free reader that
   understands `base_path` plus `category: dir` entries and block scalars (`unet: |` listing several
   dirs); it is fail-safe — anything unparseable yields no extra roots, i.e. the old comfyDir-only
   behavior.

   **The publisher's digest.** A model with a `source_url` is checked against what its publisher says the file is, read at run time
   (`publisherDigest` in `render/manifest-satisfy.mjs`, [ADR 0099](../architecture/decisions/0099-frontier-updates-floors-never-pins.md)
   section 3): for a Hugging Face `resolve` URL the `X-Linked-ETag` (the LFS sha256) and `X-Linked-Size` of a HEAD that is not followed,
   else the tree API's `lfs.oid` and size; for a GitHub release asset the release API's `digest` and size; any other host publishes
   nothing the node reads. A Hugging Face file that is not in LFS has a size but no sha256. The manifest's own `sha256`, when given, is an
   assertion: if it differs from a published digest the entry defers `MODEL_SHA_MISMATCH` naming both, and the publisher's digest always
   counts; when the publisher publishes none, the assertion is checked as given and the file is still listed in `unverified_models`.

   **A file that is already there.** Its `<file>.sha-ok` record, JSON `{oid, size, mtime}`, is fresh when the file's size and modification time
   are the recorded ones and the oid is the publisher's current one: the file is kept without being read. Otherwise its size is compared with the
   publisher's (a different size is a truncated or other file and is replaced without hashing), then it is hashed **once**; the published
   build is *adopted* by writing the record beside the file that was found, anything else is replaced by a fresh download (so a newer
   published build replaces an older one), and with no `source_url` it defers `MODEL_SHA_MISMATCH`, naming the real problem rather than the
   misleading "missing on disk". When the publisher cannot be reached a file whose record still fits its size and time is kept and the result
   carries a warning saying so; a file with no such record is kept and listed in `unverified_models`, never deleted. A record in the older format (a
   bare hash) reads as none, so the file is hashed once more.

   **A download** goes to `<file>.part`, is checked for size and sha256 against the publisher, and only then renamed onto the destination, so a
   partial file never sits there and an interrupted download is not trusted by the next run. A mismatch deletes the `.part` and is retried once;
   a second mismatch defers `MODEL_SHA_MISMATCH` naming the digest wanted and the one got. A missing model whose publisher could not be asked
   for a digest (and that the manifest asserts none for) is not downloaded: it defers `MODEL_DOWNLOAD_FAILED`, which is retryable. No
   `source_url` and not on disk → `MODEL_DOWNLOAD_FAILED`. Models whose digest did not come from the publisher are reported as
   `unverified_models` in the success envelope rather than silently trusted.

5. **Clone each node pack the node does not hold yet** at the head of its repository's default branch. A pack already on
   disk is used as it is: a render request never moves an installed pack, so a caller holding only the render bearer
   cannot make a node fetch and run whatever an upstream pushed a minute ago. Moving a pack to a newer commit is the
   operator's update (`git -C custom_nodes/<name> pull --ff-only`, or the fleet's update tooling). The commit a pack is at
   is recorded beside it in `custom_nodes/<name>.installed.json` (schema 2: `{schema, repo, commit, installed_at, checked_at,
   previous, rejected}`; `repo` is the origin the checkout reports, `previous` the commit it held before, `rejected` the
   commits an install of it failed on; rewritten only when the commit or repository changes). The dependency marker and the
   run-graph cache are keyed on the commits, so a pack the operator moved, replaced or deleted is provisioned again on the next
   request that names it. A fresh clone is a change; an installed pack is not. If a later step of the same satisfy fails
   (resolve, `pip check`, the host-constraint tripwire), the directory this satisfy cloned is removed again and its commit is
   appended to `rejected`; a later request that finds the repository's newest commit in `rejected` defers
   `NODE_PACK_REJECTED` (ref: the pack) instead of cloning and failing again, and a newer commit is tried afresh. A pack that
   was already on disk is never removed. Packs are placed *before* dependency
   resolution, because the unified resolve reads their on-disk requirements.

6. **Resolve and install dependencies once**, for all packs together, under host constraints (read from the installed
   venv on each run and discarded after, never stored) that
   prevent anything moving `torch`, `torchvision`, `torchaudio`, or `numpy`. The resolve runs on
   every satisfy EXCEPT the proven re-run: it is skipped only when no checkout moved AND a
   **persisted marker** proves this exact set of pack commits was already resolved+checked successfully.
   `uv` resolves; `python -m pip` installs. The marker (`<venv>/.offload-deps-satisfied` — it
   attests venv state, so it lives inside the venv and dies with it; holding the pack-set
   key `name@commit|…`, built from the commits the checkout stage reports), written ONLY after a fully successful resolve+check — "git didn't move this
   run" alone proves nothing: a prior run that checked packs out and failed before installing must
   still resolve. A tripwire re-checks the held host versions afterward with full local versions; the cheap
   `pip check` coherence gate always runs. Failure → `VENV_INCOHERENT`, with pip's own lines in the detail. A failure whose
   lines were all there before this satisfy (its baseline is read before the checkout) does not defer: the venv is no
   worse than it was, and a conflict between two packs' own requirements can never be resolved here (a Windows node,
   2026-10-29: albumentations 2.0.8 requires exactly albucore 0.0.24, albumentationsx needs a newer one). The run goes on and
   the result carries `warnings` naming the lines. See
   [ADR 0007](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md).

   **Spawn failures are classified apart** (live report 2026-07-20: a transient `spawn UNKNOWN`
   after a long batch was misreported as venv incoherence): if a satisfier subprocess (git, uv,
   pip) fails to *start*, it is retried once after 500 ms, then defers **`SATISFIER_SPAWN_FAILED`**
   — never `VENV_INCOHERENT`, because "the check could not run" says nothing about the venv. One
   deliberate, narrow fail-open: when the pack set is unchanged AND the satisfied-marker proves it
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

   A required input is satisfied by its own key **or**, for a dynamic autogrow group
   (`COMFY_AUTOGROW_V3`), by at least `min` dotted child keys. The frontend serialises such
   groups only as children — `ComfyMathExpression`'s `values` arrives as `values.a`, `values.b`
   and never as `values` — so an exact-key check would defer graphs ComfyUI accepts. This
   matters for graphs exported from official ComfyUI workflow templates, which use these
   nodes freely.

9. **Execute, poll, and fetch outputs**, then tear down through the same GPU lifecycle.

## Template runs

`offload_run_template` runs a ComfyUI workflow template through this same flow: the door resolves the template (the gate and the
runtime manifest, [the tool's section](../systems/mcp-server.md#workflow-templates-offload_run_template)) and sends a `run_graph`
task with the template name, the manifest and optional overrides and seed, on route `local`. Two steps differ, and everything else
in the nine steps above is unchanged:

- **Trust on first use, between steps 4 and 5, on every run.** A manifest model that declares no hash and asks for `trust: "tofu"`
  is checked by `verifyTofuModels`: the first run hashes it and records the sha256, size and modification time in `<file>.tofu`
  beside the file; a later run with the same size and time trusts the record, a changed file is hashed again, and a hash that is not
  the recorded one defers `MODEL_SHA_MISMATCH`. It runs when the environment marker says the manifest was already satisfied, too,
  because the marker says the environment was provisioned, not that a file is still the one that was trusted. A record that cannot be
  read or written defers `MODEL_TOFU_RECORD`. A graph run's manifest asks for none of this and is checked as before.
- **Conversion, between steps 7 and 8.** A template is a UI-format workflow, and its API graph needs the running ComfyUI's
  `/object_info` and version, which exist only once ComfyUI is up. The runner converts it there
  ([`template-run.mjs`](../../render/template-run.mjs), [the converter](../systems/media-generation.md#template-to-api-converter-rendertemplate-to-graphmjs)),
  applies the overrides, and hands the converted graph to the preflight of step 8 and to the POST of step 9. A refusal (an unknown
  node class, a ComfyUI older than the template, an override that names a node or input the graph lacks, or one that would retarget a
  model or input file or name an embedding, which the gate never looked at) defers
  `TEMPLATE_CONVERT_REFUSED`, its `ref` the converter's own code, and the ComfyUI that was started is torn down as for any defer.
  The converted graph is then compared with the manifest: a model file it loads that the manifest does not name (the catalog and the converter
  are two readers of the template, and the licence gate, the FLUX bar and trust on first use read only the manifest) defers
  `TEMPLATE_MODEL_UNLISTED`, its `ref` the first such file, before preflight and before anything is posted. An embedding the prompt text
  names (`embedding:<name>`, read as ComfyUI's tokenizer reads it) is such a file, as `models/embeddings/<name>`, and a file for it at the top
  of that directory or in any subfolder lists it. The gate that wrote the manifest accepted the name only when exactly one case-exact file
  answers it (`embedding_unverified` otherwise): prompt embeddings fail closed even though ComfyUI warns and skips a missing one, because a
  missing or different embedding silently changes the output of a vetted template. Limit: this comparison and the override check both read the string values of the graph, so
  an embedding assembled while the graph runs by string nodes (a concatenation of two overrides, for example) is seen by neither.

The runtime manifest names each model by the path it was found at and has no `source_url`: step 4 therefore never downloads for a
template run, and a model that went missing defers `MODEL_DOWNLOAD_FAILED` ("missing on disk and no source_url"). Step 4 looks for
a `diffusion_models` file under `models/unet` and a `text_encoders` file under `models/clip` too, as ComfyUI does.

## Data and state changes

Node packs cloned into ComfyUI's custom-nodes directory at the head of their default branch, each with a
`<name>.installed.json` record of the commit it got; Python packages installed
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
`PREFLIGHT_MISSING_INPUTS`, `RUN_ERROR`, and, on a template run only, `TEMPLATE_CONVERT_REFUSED`, `TEMPLATE_MODEL_UNLISTED` and `MODEL_TOFU_RECORD`. A failure on the Go side of the call (rather than in the
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
   download branch. Presence now spans the full search path, and a present file whose manifest entry declares a sha is
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
   The run-graph cache (`.offload-owned.json`) holds the manifest hash and the sorted `name@commit` key of the packs on disk
   when it was written; it skips this satisfy only when both still match. The hash covers each model's `path` and `source_url` and no
   caller hash, so a run whose manifest is unchanged does not read the publisher again until the cache is invalidated.
3. The node-class gate runs after ComfyUI is up, not before.
4. Failures are typed Defers, not crashes.
   Model presence is resolved across ComfyUI's full search path, so the harness and ComfyUI agree on
   whether a model exists; a file the graph can load is never re-downloaded.
5. The harness never adds models of its own; manifests are the caller's responsibility — which is
   also the boundary for
   [ADR 0011](../architecture/decisions/0011-flux-family-license-prohibition.md).

## Security and privacy notes

This is a **trusted-caller interface**: it executes caller-supplied graphs and installs
code from repositories the node allowlists (`render/node-packs.json`, no commit is read from the caller). The allowlist and
the recorded commit are what make it auditable; it is not a sandbox. Do not expose it to an untrusted caller.

## Observability and debugging

The defer `code` identifies the stage; `ref` identifies the offending pack, model, or node class.

> **Diagnosability (fixed in stages):** since v0.22.5 the defer detail names the actual problem —
> host-constraint drift reports *which* held package moved (expected vs observed), a genuine conflict
> reports pip's own message. Since v0.22.13 a subprocess that failed to *spawn* is a separate code
> entirely (`SATISFIER_SPAWN_FAILED`) rather than masquerading as incoherence.

A caller-supplied `out_dir` **is** created for you — `resolveOutDir`
([`internal/pipeline/pipeline.go`](../../internal/pipeline/pipeline.go)) runs `MkdirAll` on the
caller's path, not only on the defaulted media dir used when `out_dir` is empty; the standalone
`.mjs` runner also `mkdirSync`s the directory before writing each fetched output. A path the process
cannot create is not an opaque `RUN_ERROR`: it defers early with `cannot create out_dir: …`.

## Testing notes

`render/manifest-satisfy.test.mjs` covers the models leg, defer codes, command builders, argument
injection rejection, and the host-constraint helpers. `render/comfy-run-graph.test.mjs` covers the
defer envelope and preflight paths. Go-side: `internal/rungraph/`, `internal/pipeline/pipeline_rungraph_test.go`.
Run the Node suites with `node --test render/*.test.mjs` from the repo root.

## Source map

- [`render/manifest-satisfy.mjs`](../../render/manifest-satisfy.mjs) — satisfier, host constraints, tripwire
- [`render/manifest.mjs`](../../render/manifest.mjs) — schema normalization and hashing
- [`render/comfy-run-graph.mjs`](../../render/comfy-run-graph.mjs) — orchestration and envelope
- [`render/template-run.mjs`](../../render/template-run.mjs) — a template by name, its conversion and overrides (template runs)
- [`render/template-manifest.mjs`](../../render/template-manifest.mjs) — the gate and the runtime manifest of a template run
- [`render/preflight-graph-file.mjs`](../../render/preflight-graph-file.mjs) — node-class gate
- [`internal/rungraph/`](../../internal/rungraph/) — Go entry and result types

## Related docs

- [../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md](../architecture/decisions/0007-host-torch-pinned-additive-provisioning.md)
- [zero-warm-generation.md](zero-warm-generation.md)
- [../systems/media-generation.md](../systems/media-generation.md)
