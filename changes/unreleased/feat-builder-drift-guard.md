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
  and the widget. Template side, when `COMFY_TEMPLATES_DIR` or `COMFYUI_DIR` (or `--templates-dir`/`--comfy-dir`
  on the CLI) points at the package or at a candidate extract: the same widgets are read from the template JSON,
  and a change fails naming the builder, the template and the package version the builder was last verified
  against. A wiring item keeps the `_distilled` Wan Animate 2 template's start and end percent on separate
  subgraph inputs, the shape of the PR #1308 defect.
- **Coverage.** A new `wf-*.mjs` file must be guarded or listed in `NOT_TEMPLATE_DERIVED` with a reason, or a test
  fails. Not guarded, with reasons in the spec: `wf-hunyuan15-i2v` (its recipe is not the one template of the
  package that shares its family), `wf-sdxl-inpaint`, `wf-upscale`.
- **Where the value is read.** The sampler's own serialized widget is stale once its input is linked, so a locator
  points at the primitive behind the template's switch, or at the widget the subgraph instance promotes.
- **First run on package 0.1.96.** Eleven builders, eighteen views; every guarded widget equals its template
  except eight entries, recorded and left for a person to judge. Unreviewed: the `wf-h3-av` turbo path (template 6
  steps, builder 8), the `wf-qwen-image-edit` full preset (template CFG 4, builder 3.0) and the
  `wf-qwen-image-21` edit encoder resolution (template 0, builder 1024). Deliberate and stated in the builder
  headers: the `wf-qwen-image-21` step count and the `wf-wan-animate2` pose-cache device and dtype. No builder
  recipe was changed.
- **Tests.** `render/builder-drift.test.mjs`: the committed manifest against the builders; a synthetic package
  written from the specs (no upstream data) in which every widget of every builder is moved once on each side and
  must turn the check red naming the builder; the PR #1308 wiring shape; a missing node, type or widget; a package
  version that moved with every widget unchanged (a note); the CLI. Red then green was shown with a planted builder
  widget and with planted template widgets in a scratch copy of the package. `node --test "render/**/*.test.mjs"`.
- **Docs.** `docs/systems/media-generation.md` (Builder drift guard) and `render/README.md`.
