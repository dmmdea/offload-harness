### Added - offload_run_template: list, inspect and run the ComfyUI workflow templates a node carries, behind a licence gate, with trust on first use for model files

- **What it does.** One MCP tool, `offload_run_template`, with an `action`: `list` (filterable, paged, sorted, the same bytes for the
  same tree), `get` (one template's manifest and licence class), `readiness` (each model present or missing, directory-aware; each
  input file; node packs; ComfyUI's version against the template's minimum; the licence verdict) and `run`. `run` asks
  `render/run-template.mjs resolve` for the gate, and only for a template it passes sends a `run_graph` task through the same call
  `offload_run_graph` makes (route `local`: the same lease, ComfyUI lifecycle, satisfier and envelope), with the template **name**, a
  runtime manifest and optional `overrides` and `seed`. The runner (`comfy-run-graph.mjs --template NAME`) converts the template with the
  converter once ComfyUI is up, with that ComfyUI's own `/object_info` and version, and before preflight; a refusal is the typed defer
  `TEMPLATE_CONVERT_REFUSED`, and a model file the converted graph loads that the manifest does not name is `TEMPLATE_MODEL_UNLISTED`. An
  override sets values and never retargets a file or names an embedding: one that targets a model or input loader node, an input
  that holds a model file name, a value that is a model file name, or a string value with `embedding:` in it is refused as `override_is_file`,
  because the licence gate, the FLUX bar and the trust-on-first-use check read only the models the template names (one limit is stated with the embeddings, below: text that string nodes assemble while the graph runs is not seen). The manifest generator is `render/template-manifest.mjs` (templates to manifest entries: models as
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
  manifest carries no completeness mark and the Go side has no backstop for one; the refusal stops a short list from the case above, and the
  runner's comparison (next bullet) stops it whatever its cause (`hostneed/manifest.go` says so).
- **Model files only the converter reads.** The manifest is the catalog's static reading of a template and the graph is the converter's, two
  readers of one file, and the licence gate, the FLUX bar, trust on first use and the host-RAM figure read only the manifest. Three ways they
  differed: an active subgraph instance's own `widgets_values` (the converter routes them into an inner loader, through `proxyWidgets` or as
  the literal a subgraph input takes), a loader whose `widgets_values` is an object keyed by input name (the converter reads both forms, the
  list was read only in its list form), and a loader that stores fewer values than it has widgets and takes the listing's default. A template
  with the first or the second resolved to `run` with the licence class of its other files (or none), and loaded the file the list lacked.
  Two changes close them. The catalog now reads both forms of `widgets_values` and the values of an active instance, so such a file reaches
  the licence class, the FLUX bar and readiness before a run (an unmapped one refuses the template as `license_unknown`; a value of a muted
  or bypassed instance is not read, and one an annotation covers is not listed twice). And the runner compares the converted graph with the
  manifest before preflight (`unlistedModels`, `template-run.mjs`): a model file the graph loads that the manifest does not name defers
  `TEMPLATE_MODEL_UNLISTED` (the first such file is the `ref`) and nothing is posted. The comparison reads the graph that will run, so it
  covers the third way, which the catalog cannot see, and any other. A file matches an entry as the catalog matches an annotation to a widget
  (by the name under the class directory or by its base name, ignoring case and slash direction, `modelCovered`); a graph run is not compared.
  The proxied value is listed with no class, and its precedence over the inner loader's own annotation is not modelled: when both name
  a file, both are required, which can ask for one file more than the graph loads and never one fewer. Measured on the installed package
  (`json 0.1.96`, 578 templates, read only): the catalog lists the same 897 model entries, with the same licence class, FLUX flag and verdict
  per template, before and after this change.
- **Embeddings named by the prompt text.** ComfyUI's CLIP tokenizer loads `models/embeddings/<name>`, then `<name>.safetensors`, `.pt` and
  `.bin`, from a word of the prompt that starts with `embedding:` (`comfy/sd1_clip.py`, `load_embed`, read in ComfyUI 0.38.0). No loader node
  and no file extension is involved, so a text override such as `embedding:evil_ti, a cat`, `embedding:evil_ti.pt, a cat` or
  `(embedding:evil_ti:1.2) cat` was accepted (only a value ending in `.safetensors` was refused), the runner's comparison saw no file, and the
  licence gate and trust on first use never read it, which contradicted "an override never retargets a file". One reading of the reference
  (`embeddingRefs` and `embeddingCovered` in `templates-catalog.mjs`) serves the catalog and the runner's comparison; the override check keeps
  its own, wider matcher (`mentionsEmbedding`), so that what it lets through is never a text the reader finds a file in.
  - Where ComfyUI looks. Not in the directories of the class one after the other: `expand_directory_list` puts every embeddings root and every
    subfolder below it (links followed) into a Python `set`, and `load_embed` opens the file of the first directory of that set that holds the
    name, so the directory that answers is chosen by hash order and differs from one process to the next (a verbatim copy of the loop, over
    hash seeds 0 to 11, opened `e.pt` of the root 5 times, `sd15/e.safetensors` 2 times and `sdxl/e.safetensors` 5 times). Two things follow. A
    file that sits only in a subfolder (`sd15/foo.safetensors`, a common layout) is the file `embedding:foo` loads. And a name that more than one
    directory holds (the root and a subfolder, two subfolders, two registered roots) is not one file: the trust record, the hash and the licence
    approval would attach to a file ComfyUI may not open.
  - Override: any string value that contains `embedding:`, in any letter case, is refused as `override_is_file`. ComfyUI matches a lower-case
    word that starts with the identifier; matching it anywhere and in any case is the fail-closed side.
  - Runner: a string input of the converted graph that names an embedding the way ComfyUI reads it is read as text and no longer as a file
    name. Each name it asks for is reported as `models/embeddings/<name>` (the first is the `ref` of `TEMPLATE_MODEL_UNLISTED`) unless the
    manifest names, under `models/embeddings/`, a file for it: the name as written or with `.safetensors`, `.pt` or `.bin` appended, at the top
    of the directory or in any subfolder (whole path segments, never a base name that only ends alike; case and slash direction ignored), and a
    name that is empty, absolute or has a `..` segment is never covered. A manifest entry under another class does not cover it.
  - Catalog: a prompt widget of an active node (or an active subgraph instance's own values; not a note, not a muted or bypassed node) that
    names an embedding is a requirement under the `embeddings` class with no source, unless an annotation of that class covers it by the
    name or with one of the three extensions, in the root or a subfolder; the annotation then records the name it covers (`embedding_refs`). A
    template whose own prompt names an unannotated embedding is therefore `license_unknown` and refused; an annotated one is read by the
    licence gate like any weights file. Text that only looks like a reference to the override's wider matcher (`Embedding: a primer`,
    `wordembedding:vec`) names nothing here.
  - Readiness resolves a name the way ComfyUI does, directory by directory: the candidates are the name as written and then with the three
    extensions, in the roots and every subfolder. A file the class does not list answers the name as written too (no extension, or a whole
    name that is the reference), because `load_embed` asks `isfile` for the name first with no extension filter; the inventory records those
    files (`unlisted_files`) for this class. One directory that holds a file is `present` and the runtime manifest names that file
    (`models/embeddings/<sub>/<name>.<ext>` for one in a subfolder, which the host-RAM figure can size and the satisfier and the trust check
    can find); within one directory the order is as written, `.safetensors`, `.pt`, `.bin`. More than one directory is the new status
    `ambiguous`, listing the files, and the run is refused as `models_missing` with the entry named, so the manifest never names a file ComfyUI
    might not open. A directory is one entry of ComfyUI's set however it is reached, and the inventory tells a link from a copy: it records
    where each embeddings file really is (`reals`), and hits that open the same real file are one hit. A root that is a link to another
    registered root (a model drive reached through the default directory), a link inside a root, a root registered inside another and one
    file linked into two directories are each one directory; two copies stay two files and stay `ambiguous`. An annotated file that is not
    the one ComfyUI opens for the name a prompt gives (a `.safetensors` beside an annotated `.pt`) is the new status `shadowed`. The lookup
    follows the volume, as `isfile` does: on a case-sensitive volume a file in another case is `case_mismatch`, and under a root whose volume
    folds case (NTFS by default; the scan asks each root through the io, one of its entries spelled in the other case, and records the roots
    that do as `case_insensitive_dirs`) the comparison is in lower case, so a case variant is a hit, reported by its spelling on disk, and
    two case variants in two directories are `ambiguous`; a node with none is `models_missing`, listed by the name the text gives.
  - A loader widget's embeddings file is not a prompt name. An annotated entry of the embeddings class that records no prompt name is the
    file a `ConditioningLoader` widget carries (three templates of the installed package, the Marigold depth, albedo and surface-normal
    ones). That node lists the directories by relative path (a subfolder file is `sd15/x`) and opens its value with the exact path under the
    first root, so it is met like a file of any other class: a subfolder-only file is `in_subfolder`, and a root file with a copy in a
    subfolder is `present`. The prompt lookup applies to an unannotated requirement and to an annotation that records the prompt names it
    covers (`embedding_refs`); `readinessFor` forwards `annotated` so the run gate makes the same split.
  - Other names ComfyUI may try (`embedding_probes`). Two readings of a prompt open files the requirement does not name. `_try_get_embedding`
    asks `load_embed` for the name as written first (`embedding:foo, a cat` asks for `foo,`) and strips the commas only when nothing
    answers; and the LLM-based text encoders (Z-Image, Flux.2, ERNIE and others) hand the same directory on with `disable_weights`, so the
    text is not cut at parentheses and no group weight is read (`embedding:foo(bar)` names `foo(bar)`, `a cat ( embedding:1.2 )` names
    `1.2`). A template does not say which encoder runs, and the two readings differ in both directions, so the CLIP reading stays the
    requirement. `embeddingProbes` returns the other names; they are not requirements (listing them would refuse every prompt with a comma
    after the name), the catalog row carries them as `embedding_probes` only when there are some, and readiness adds an entry (`shadowed`,
    `probe: true`, the file in `found`) after the requirements when a file of the node answers one. The template is then not ready and the
    run gate refuses it as `models_missing`, naming the probe name, because ComfyUI may open a file that no manifest names.
  - Grammar: the reader is a port of the tokenizer path of `comfy/sd1_clip.py` (`token_weights`, `tokenize_with_weights`,
    `_try_get_embedding`), compared with the extracted functions of that file on 150,000 generated texts with no difference. The text is cut
    at parentheses and a closed group loses a numeric tail, so `(embedding:name:1.2)` is `name` and a bare `embedding:name:1.2` is the name
    `name:1.2`, which ComfyUI fails to find; the identifier is lower case and starts the text or follows whitespace or a parenthesis, so
    `Embedding:`, `wordembedding:` and `a,embedding:x` are not references, while `an embedding: soft light` is (ComfyUI looks for a file named
    `soft`); whitespace after the identifier is skipped; the name ends at whitespace, `(`, `<` or `[` (an unescaped `)` with no group to
    close stays in it); commas come off both ends; `\(` and `\)` are characters of the name. Deviations from ComfyUI, each named: the name
    returned is the one after the comma retry and the reading is the CLIP tokenizer's, the one embeddings are made for, and the names the
    other tries (the name as written, the reading without weights) are probes, not requirements (above); `embedding:` with no readable
    name, and a text nested more than 500 groups deep (ComfyUI reads about 900), are the empty name, which every caller refuses; the
    inventory skips `.git` directories (ComfyUI's walk enters them); the volume's case rule is probed once per root and applied to every
    subfolder of it, so a single directory marked case-sensitive inside a case-folding volume, or a link into another volume, is judged by its
    root's answer.
  - Limit: the override check and the runner's comparison both read the string values of the graph, so an embedding assembled while the graph
    runs by string nodes (a concatenation of two overrides, for example) is neither refused nor compared with the manifest, and the licence
    gate, the hash and the trust record never see the file it loads. "An override never retargets a file or names an embedding" is true of the
    values an override sets, not of text built from them.
  - Measured read only on the installed package (`json 0.1.96`, 578 templates): the catalog is byte-identical before and after this round
    (897 model entries, 3 of them annotations of the embeddings class), and so are the per-template verdicts and the readiness counts on a
    snapshot of the measuring machine (649 requirements missing, 214 present). The only `embedding:` text in the package is in the markdown notes
    of four `video_minimax_h3_*` templates, which are not read (they are refused as `license_conditional`). That snapshot holds no embedding
    file, so the resolver was measured on fixture trees and on random trees on a real file system (see the tests below).
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
- **Model-file tests.** Written red first: the runner test (a converted graph that loads a file the manifest does not name was posted, not
  deferred) and the three `unlistedModels` tests of `render/comfy-run-graph.test.mjs` and `render/template-run.test.mjs`; then two tests of
  `render/templates-catalog.test.mjs` and two of `render/template-manifest.test.mjs` (the list lacked the instance's file and the object-form
  file, and both templates were `run`). The guards that passed before the change pin what must not move: a graph run is not compared, a file
  is matched by path, base name and case, a muted or bypassed instance is not read, and on the two hand-written templates the converter's graph
  loads only files the catalog names.
- **Embedding tests.** Written red first: 18 tests failed on the code before the change (the three override shapes and the letter-case one,
  six of `unlistedModels`, `embeddingRefs`, the catalog requirement, annotation coverage and the subgraph instance, three readiness ones,
  and the verdict of a template with a baked-in embedding). The guards that already passed pin what must not move: a prompt that only
  talks about embeddings goes through, a muted node and a note add nothing, and an annotated embedding runs. The reading and the
  resolution those tests first pinned were corrected afterwards; their tests and mutation checks are in the next bullet.
- **Embedding lookup and grammar tests.** Written red first, one per behaviour: 13 tests of `render/templates-catalog.test.mjs` (seven of the
  grammar, two of the catalog, three of readiness and one of `embeddingCovered`), 4 of `render/template-manifest.test.mjs` (an embedding held
  only in a subfolder, an annotated one in a subfolder, a name held in the root and in a subfolder, an annotated file another file shadows), 2 of
  `render/template-run.test.mjs`, and the root prose test `run_template_docs_test.go` (the docs say how ComfyUI finds an embedding and name the
  string-node limit; it refuses the first wording of the lookup, a walk through the class directories in order that matched a whole path only). The tests that pinned the
  old premise were replaced: "the first one in ComfyUI's order decides" (two roots) became the two-root ambiguous case, and "a nested listed name
  is not matched by its base name alone" became its opposite. Guards written with them pass on the new code and bite under mutation: the
  override check is at least as wide as the reader (a corpus), Python's whitespace set and float() rules, a text of 150,000 groups, a text
  nested 600 deep, a model file name that holds an identifier-like word, and a file under another class. Two comparisons were run in a scratch
  directory and are not committed: the reader against the extracted functions of `comfy/sd1_clip.py` on 150,000 generated texts (27,440 with a
  reference), no difference (it found one: float() does not strip U+001C, which `str.split()` does); and the resolver against a verbatim copy of
  `load_embed` over every order the directory set can take, on 600 random trees on a real file system (6,000 name checks: 1,457 present, 565
  ambiguous, 3,978 missing), no difference. Thirty mutations of the new code, one per scratch copy of `render/`, each fail at least one test: the
  suffix match of the resolver and of the coverage test, merging the subfolders of a root into one directory, merging the roots (one entry
  per file), the last candidate winning, only the first root of a file, case-exact coverage, the directory-escape guard, an annotation not
  verified against its names, not recorded, not passed to readiness, no case diagnosis, no wrong-class diagnosis, the ambiguous listing
  emptied, no explanation of an unclear embedding in the refusal, a case-insensitive identifier (twice), a bare weight stripped, no cut at
  parentheses, no skip of whitespace, any group tail as a weight, float() without `inf`, `nan` or digit underscores or with the wrong
  whitespace, JavaScript's whitespace, escapes not honoured, no depth bound, a spread of a group's pieces, and a gate on the wide matcher in
  the catalog and in the runner.
- **Embedding resolver round (six findings of a second review).** Each fix has a commit of failing tests before it. Loader widget
  (`render/templates-catalog.test.mjs` 1, `render/template-manifest.test.mjs` 2 failed): the exact-path rule, a root file with a subfolder
  copy, and the run gate; the one earlier assertion that pinned the prompt lookup for an annotation without prompt names was changed to the
  loader rule. Physical directory (4 failed, one on a real junction in a scratch tree): a root that links to another root, a link inside a
  root, a root registered inside another, and two copies that must stay ambiguous. Unlisted files (4 and 1 failed): an extension-less file
  answering the name first, the same stem in another directory, an annotation it shadows, and the inventory record. Case folding (4 and 1
  failed, with a probe checked against what the volume does on the real file system): a case variant alone, two variants in two directories,
  and the run gate. Name as written (5 failed) and the reading without weights (3 failed): the probe list, the catalog row, readiness and
  the run gate. Nineteen mutations of the new code, each applied to the real file, run, and restored byte-identical, each fail at least one
  test: dispatch on the class name alone, `annotated` not forwarded by `readinessFor`, the hit key back to root and subfolder, no collapse by
  real file, real paths not recorded, unlisted files not recorded or not candidates, the case probe always false, the lookup ignoring the
  folding roots, the folding roots not recorded, the name as written dropped, probe findings not added, not passed by `readinessFor`, not
  explained in the refusal, not recorded on the row, not passed by the readiness table, the reading without weights dropped, read
  without its escapes, and the empty name probed. The reader's expected probe names were compared with the extracted functions of `comfy/sd1_clip.py` on a dozen
  texts in both modes. Measured read only on the installed package (578 templates): the catalog rows are byte-identical before and after,
  and the three Marigold templates' conditioning file reads `present`, `in_subfolder` and `present` for a root file, a subfolder file and
  both, as before the previous round.
- **Manifest-side normalisation tests.** `unlistedModels` lower-cases the manifest path, turns its backslashes into slashes and drops the
  `models/<class>/` prefix, and every test manifest was lower case with forward slashes, so no test could fail on that step. Both fixtures
  (`LISTED_MODELS` in `template-run.test.mjs`, `LISTED` in `comfy-run-graph.test.mjs`) now hold a mixed-case entry
  (`Qwen-Image-Lightning-8steps-V1.0.safetensors`) and a backslash entry under a mixed-case folder, with a graph that names each exactly,
  as four separate tests. Production was already right, so they pass; each was shown to bite by mutating the step in a scratch copy of
  `render/`: without the lower-casing 5 fail, without the slash normalisation 2 fail (both backslash ones), and no pre-existing test fails
  under either.
- **Not verified here.** The package the counts below were measured on, re-scanned for unexpanded subgraph instances (only the newer copy was).
  The runner's comparison against the converter's output for the real templates under a live ComfyUI: it is tested on injected graphs, on the
  two hand-written templates with their saved listing, and by unit, and the real package was only read by the catalog (a template the
  converter loads a file for that the catalog did not name would show as a `TEMPLATE_MODEL_UNLISTED` defer on a live run).
  An embedding loaded by a live ComfyUI: the directory set and the tokenizer path are read from ComfyUI's source (0.38.0) and compared with
  its extracted functions in Python, not run in a live ComfyUI, and no template of the installed package names one in an active prompt, so the
  catalog path has no real template to be measured on. The resolver on the real model tree of a node: the measuring machine's snapshot holds
  no embedding file. The junction layout of a model drive was reproduced with a real junction in a scratch tree, not on a node. The case
  probe was checked on the file system of the measuring machine only (it folds case); a case-sensitive volume is covered by an io that
  does not fold, not by a second machine. The probe names (the name as written, the reading without weights) were not run through a live
  encoder: ComfyUI's extracted functions give the names, and which text encoder a template uses is not asked.
  One template running end to end under a GPU lease, the acceptance measurement of this tool, queued after this
  change. The production wiring of the conversion against a live ComfyUI (the `/object_info` and `/system_stats` reads in
  `comfy-run-graph.mjs`; the flow and the converter are tested with injected values, and the converter on saved listings). The time of a
  first sighting hash on a 20 GB file. The Go door's production path on a box's real tree (tested on a fixture tree and a canned script). The template run's host-RAM
  declaration against a real template under a lease on a node (tested on stat tables and sparse files through the real admission).
- **Not built.** Fleet placement of a template; model downloads (a missing model is a refusal that lists it); an acknowledgement path for a
  conditional or non-commercial template; staging a template's sample input files; custom node packs; a CLI verb and a fleet task for the tool.
