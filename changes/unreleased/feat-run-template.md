### Added - offload_run_template: list, inspect and run the ComfyUI workflow templates a node carries, behind a licence gate, with trust on first use for model files

- **What it does.** One MCP tool, `offload_run_template`, with an `action`: `list` (filterable, paged, sorted, the same bytes for the
  same tree), `get` (one template's manifest and licence class), `readiness` (each model present or missing, directory-aware; each
  input file; node packs; ComfyUI's version against the template's minimum; the licence verdict) and `run`. `run` asks
  `render/run-template.mjs resolve` for the gate, and only for a template it passes sends a `run_graph` task through the same call
  `offload_run_graph` makes (route `local`: the same lease, ComfyUI lifecycle, satisfier and envelope), with the template **name**, a
  runtime manifest and optional `overrides` and `seed`. The runner (`comfy-run-graph.mjs --template NAME`) converts the template with the
  converter once ComfyUI is up, with that ComfyUI's own `/object_info` and version, and before preflight; a refusal is the typed defer
  `TEMPLATE_CONVERT_REFUSED`. An override sets values and never retargets a file: one that targets a model or input loader node, an input
  that holds a model file name, or a value that is a model file name is refused as `override_is_file`, because the licence gate, the FLUX bar
  and the trust-on-first-use check read only the models the template names. The manifest generator is `render/template-manifest.mjs` (templates to manifest entries: models as
  `models/<class>/<name>` with the source and the hash a template declares, node packs, licence class, API or not, input files read or not).
- **The gate.** This release runs a template only if it is not a paid-API one, not FLUX-family, needs no custom node pack, is readable and
  has a permissive licence class (or no weights). A paid-API template is a Defer (`deferred:true`, class `api_paid`, never cloud). Every
  other case is refused with a typed class: `flux_barred`, `custom_nodes`, `license_unknown` (a repository the map does not list, a weights
  file with no Hugging Face source, or a map entry that was inferred from a sibling and not read: unknown is not permissive),
  `license_conditional`, `license_non_commercial`, `template_unreadable`, `subgraph_unexpanded`, `bad_model_path`, and what the node lacks with the exact list:
  `models_missing`, `inputs_missing`, `comfyui_too_old`. Nothing is leased, started or sent for a refused template. A run never downloads a
  model: the runtime manifest names each model by the path it was found at and carries no source.
- **Where it runs.** On this machine's lane only. The fleet door takes an API-format graph and a template is converted on the node that
  runs it, so a call that names a route beyond local, remotes or a node is refused as `fleet_not_offered`. A template that reads input files
  (an active `LoadImage`, `LoadImageMask`, `LoadImageOutput`, `LoadVideo` or `LoadAudio`) is a local-only ticket for its own reason, and the
  answer says so (`local_only`, `fleet_note`): a node can match an input file only by name.
- **Host RAM.** A template run carries `template` and `manifest_path` and no graph file, so the run-graph lane read nothing and declared
  the media class default: the largest family the box binds (33.1 GiB on a box that binds a Wan 2.2 pair) for a template whose files
  are 23.1 GiB, held by the host-RAM brake though the box could run it, and on a box that binds no ComfyUI render family, 0 (source
  `none`), so a big template got no host-RAM check at all. The lane now declares the model files the runtime manifest names
  (`models/<class>/<name>`, sized under the ComfyUI model roots; `hostneed.ForManifestModels` and `ForManifestFile`), by the rule
  a graph's loader files get: the sum, each file once, in full when together they do not fit the card (less the call's
  `reserve_vram`), 0 when they fit, source `graph loaders`. Any doubt keeps the class default exactly as before: no manifest, one that
  cannot be read, one that names nothing, an entry that is not `models/<class>/<name>`, or one file the box cannot find or size (no
  partial sum). The lane probe reads the same figure through the same function, so it agrees with the admission. A name is looked up
  under every class directory, as a graph's is, so a name held under two classes is sized as the larger. An estimate from the files'
  sizes, not a measurement.
- **Subgraph depth.** The catalog expands eight levels of subgraph instances and the converter up to sixty-four, and a template's models, licence
  class, FLUX bar and host-RAM figure are read from the catalog's walk. A template with one loader within eight levels and another at a deeper
  level got a manifest naming only the shallow file while the converted graph loaded both (reproduced at nine nested instances), and the host-RAM
  figure summed the short list. A template with an instance the walk left unexpanded (nested past eight levels, or a subgraph that contains
  itself) is now refused as `subgraph_unexpanded`, ahead of the checks that read the graph, in `list`, `get`, `readiness` and `run`. The two depth
  limits are kept apart on purpose: the converter bounds a branching nest by counting nodes, and the catalog walk has no such count, so its depth is
  what keeps it small; a template nested deeper than eight is refused rather than run, though the converter could convert it. No template of a
  602-template copy of a newer package (`json 0.1.108`, 203 with subgraphs) has an unexpanded instance; that is evidence for the counts below, not a
  re-scan of the package they were measured on. The runtime
  manifest carries no completeness mark and the Go side has no backstop for one: the refusal comes before any manifest is written, so the one
  producer never writes a short list (`hostneed/manifest.go` says so).
- **Why a tool and not a `template=` argument on `offload_run_graph`.** Measured at 0.186.0: an average tool costs 1,019 tokens in the full
  profile and 672 compact, so one more tool is about +3.2% of the 31-tool full list. The registered design accepts that cost, and the
  `template=` argument was only its fallback for a cost that proved unacceptable. The new tool's own entry is 2,833 bytes of `tools/list` in
  the full profile and 2,671 compact, against means of 4,102 and 2,631 over the 32 tools of the default box (bytes, not tokens). It has a
  reviewed compact keep-list entry (three sentences: what a run is allowed for and refused with, that it never downloads a model, where it runs).
- **Trust on first use for model files.** Most template annotations carry no hash (10 of 1,135 do). The first run that meets a model with no
  declared hash records its sha256, size and modification time in `<file>.tofu` beside it; a later run with the same size and time trusts the
  record without hashing, a changed file is hashed again, and a hash that is not the recorded one defers `MODEL_SHA_MISMATCH`, naming both. The
  check (`verifyTofuModels`) runs on every run, a cached one included, before ComfyUI starts. It is a different record from the `.sha-ok`
  sentinel, which means "matched a pinned hash". A record that cannot be read or written defers `MODEL_TOFU_RECORD`. The first sighting of a
  large file pays the hash once (minutes for 20 GB); bytes replaced under an unchanged size and time are not seen.
- **Also changed.** The manifest parser carries an optional `trust` per model. The satisfier now looks for a `diffusion_models` file under
  `models/unet` and a `text_encoders` file under `models/clip` too, as ComfyUI does (a run-graph manifest that names the modern class was
  reported missing for a file in the legacy directory). The catalog row names the input files its active loaders read (`input_files`).
  ADR 0058 is Accepted, dated 2026-10-14, with the third licence state `conditional`; its index row says so.
- **Measured catalog at this head** (installed package `comfyui-workflow-templates 0.11.70`, `json 0.1.96`; the research's 246 was a heuristic):
  578 templates in the index; 337 are paid-API (all three API signals agree); 80 are runnable by this release (5 with no weights; 75 with 247
  model entries, 237 of them trusted on first use and 10 with a declared hash; 55 of the 80 read input files); the others are refused as
  25 `flux_barred`, 63 `license_conditional`, 10 `license_non_commercial` and 63 `license_unknown` by first class (39, 64, 30 and 71 templates carry
  those reasons, counted over every kind, paid-API included; among the 236 local templates that need weights the catalog's tally is 75
  permissive, 63 conditional, 29 non-commercial and 69 unknown). `custom_nodes` is 0 for this package. 85 of 135 repositories of the local templates that need weights are not in the licence
  map (83 of 133 in the research). On one node (<node-a>, ComfyUI 0.38.0), 4 of the 80 were ready to run that day (73 lacked a model file, 52
  lacked an input file they read).
- **Tests.** Written red first: the red commit failed 39 + 12 + 16 tests of the three new Node files, 11 of the satisfier's and 6 of the
  run-graph flow's, 5 of `internal/rungraph`, 2 of the pipeline, 19 of the MCP door (a stub answered a bare deferral) and the ADR test.
  `node --test render/template-manifest.test.mjs render/run-template.test.mjs render/template-run.test.mjs` (a paid-API template is a Defer with
  a typed class; an unlisted licence is refused; a template that reads input files is local-only; `list`, `get` and `readiness` are
  deterministic on a fixture catalog), the satisfier tests (a hash mismatch is refused; the check runs on a cached run; a real-file round trip),
  `internal/mcpserver/runtemplate_test.go` and `toolsprofile_runtemplate_test.go` (the compact keep-list test covers the tool), and a Go test
  that runs the real script on a tree built from the catalog fixtures. Docs: `docs/systems/mcp-server.md` ("Workflow templates"),
  `docs/flows/run-graph-manifest-satisfaction.md` ("Template runs"), `docs/systems/media-generation.md`, `render/README.md`.
- **Subgraph depth tests.** Written red first: 4 of 5 new tests of `render/template-manifest.test.mjs` failed (a template nested nine deep, and one whose
  subgraph contains itself, were run instead of refused; resolve and list agreed); the fifth, a template nested exactly eight deep, passed before the
  change and pins the boundary.
- **Not verified here.** The package the counts below were measured on, re-scanned for unexpanded subgraph instances (only the newer copy was).
  One template running end to end under a GPU lease, the acceptance measurement of this tool, queued after this
  change. The production wiring of the conversion against a live ComfyUI (the `/object_info` and `/system_stats` reads in
  `comfy-run-graph.mjs`; the flow and the converter are tested with injected values, and the converter on saved listings). The time of a
  first sighting hash on a 20 GB file. The Go door's production path on a box's real tree (tested on a fixture tree and a canned script). The template run's host-RAM
  declaration against a real template under a lease on a node (tested on stat tables and sparse files through the real admission).
- **Not built.** Fleet placement of a template; model downloads (a missing model is a refusal that lists it); an acknowledgement path for a
  conditional or non-commercial template; staging a template's sample input files; custom node packs; a CLI verb and a fleet task for the tool.
