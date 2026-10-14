### Added - builder drift guard: the template widgets each ComfyUI workflow builder copied are hashed and checked on both sides

- **What it does.** Eleven `render/wf-*.mjs` builders were copied from templates of the
  `comfyui-workflow-templates-json` package, and a template widget that changes under a builder that copied it
  (upstream workflow_templates PR #1308: a pose-end percent wired to the pose-start input) went unseen. New
  `render/builder-drift.mjs` (library and CLI: `check`, `explain`, `record`), `render/builder-drift-specs.mjs` (which
  widget of which template each builder copied, and where it sits in the builder's graph) and the committed
  `render/testdata/builder-drift.json`: per builder and view, the template name, the package versions, a sha256 of
  the recipe widgets as the template carries them and as the builder's output graph carries them, and one digest
  pair per widget with a status (`same`, `deliberate`, `unreviewed`, `template-only`). Names and hashes only; no
  template JSON, text or widget value is stored.
- **Two checks.** Builder side, offline, always in `node --test`: one test per builder (`builder drift: wf-krea2`)
  rebuilds the graph with fixed arguments and compares each widget, and a moved widget fails naming the builder
  and the widget. Template side, when a templates directory is named (`--templates-dir`, `--comfy-dir`,
  `COMFY_TEMPLATES_DIR` or `COMFYUI_DIR`, resolved in that order by the CLI and the tests alike): the same widgets
  are read from the template JSON, and a change fails naming the builder, the template and the package version the
  builder was last verified against; with every widget unchanged the recorded `templateSha256` must equal the
  recomputed hash, so a hand-edited manifest fails. A wiring item keeps the `_distilled` Wan Animate 2 template's
  start and end percent on separate subgraph inputs, the shape of the PR #1308 defect.
- **Fails closed.** A flag that takes a value and gets none (`--templates-dir "$UNSET"`, `--comfy-dir`, `--manifest`)
  exits 2, as does a flag the CLI does not know; neither can skip the template side. `check --require-templates`
  makes a missing template side an error (exit 2) for upgrade runbooks. `check --strict` also fails (exit 1) on
  every `unreviewed` manifest entry. A template or manifest file with a byte-order mark reads; a file that is not
  JSON, a `null` file or a template with no `nodes` array fails with a message that names the file and, on the
  template side, the builder. A wiring check tells two sources apart by node id and output slot, not by node type
  (two separate nodes of one type are two sources; the message still names the type).
- **Coverage.** A new `wf-*.mjs` file must be guarded or listed in `NOT_TEMPLATE_DERIVED` with a reason, or a test
  fails. Not guarded, with reasons in the spec: `wf-hunyuan15-i2v` (its recipe is not the one template of the
  package that shares its family), `wf-sdxl-inpaint`, `wf-upscale`.
- **Where the value is read.** A widget whose input is linked keeps a stale serialized value; the value that runs
  comes through the link. A locator that points at a linked widget therefore throws, naming the builder and the
  key, and the spec points at the source instead: the widget the root subgraph instance promotes, the primitive
  that feeds it (read on the branch being checked), or, for the legacy form in which the instance lists the widget
  in `proxyWidgets`, carries no value at that position of the list and has nothing wired to its input for the
  widget, the interior widget once those facts are checked. A pass-through math
  node (the LTX frame rate) is pinned as a template-only item.
- **First run on package 0.1.96.** Eleven builders, eighteen views; every guarded widget equals its template
  except six entries, recorded and left for a person to judge. Unreviewed: the `wf-qwen-image-edit` full preset
  (template CFG 4, builder 3.0, probably a real builder defect, recorded as an open item) and the `wf-qwen-image-21` edit encoder resolution (template 0, builder 1024).
  Deliberate and stated in the builder headers: the `wf-qwen-image-21` step count (both views) and the
  `wf-wan-animate2` pose-cache device and dtype. The `wf-h3-av` turbo path equals its template (the instance
  promotes 8 steps, the builder runs 8). No builder recipe was changed.
- **Tests.** `render/builder-drift.test.mjs`: the committed manifest against the builders; a synthetic package
  written from the specs (no upstream data) in which every widget of every builder is moved once on each side and
  must turn the check red naming the builder; the PR #1308 wiring shape; a missing node, type or widget; a package
  version that moved with every widget unchanged (a note); a link planted on the widget every locator names; an
  input entry with `link: null` (three serialized shapes) on the node of every locator, which must not read as a
  link; the legacy proxied form, its promoted value looked up at the widget's position in `proxyWidgets` and a link
  into the instance's input refused; a hand-edited `templateSha256`; the directory order; the coverage and manifest-shape
  branches, each checked by removing the branch in a scratch copy. The review round added tests for node identity in
  the wiring check, flags without a value, `--require-templates`, `--strict`, byte-order marks and `null` files, the
  builder graph's class check, the subgraph-name filter of the node lookup, and a literal list for the template-only
  widget locators; each new test was shown red against its mutant (key by node type, key by node id only, empty value
  accepted, missing value read as true, unknown flag accepted, each flag ignored, strict failing `deliberate` too,
  byte-order mark not stripped, nodes array or manifest shape unchecked, class check removed, name filter removed,
  template-only locator renamed) and the file restored byte-clean. Red then green was shown with planted effective
  values (root-instance steps, LoRA strength and frame rate) in a scratch copy of the package: the guard fails
  naming `wf-h3-av` and `wf-ltx25-i2v`, and a guard that read the stale widgets stays green on the same copy.
  `node --test "render/**/*.test.mjs"`.
- **Known limits, stated in the docs.** Node mode is never read (a bypassed or muted node still reads as live); wiring
  is mostly unguarded (one wiring item, the `wf-wan22-i2v` pairs share locators; a `feeds` locator would cover it);
  `W` and `I` ignore `proxyWidgets`; the two `unreviewed` entries are green in a plain `check`.
- **Docs.** `docs/systems/media-generation.md` (Builder drift guard) and `render/README.md`.
