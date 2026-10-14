### Added - template-to-API converter: a ComfyUI UI-format workflow in, the API-format prompt graph out

- **What it does.** `render/template-to-graph.mjs` converts the UI-format workflow a template ships (`nodes[]`, `links[]`,
  `definitions.subgraphs`, `widgets_values`) into the `{"<id>": {"class_type", "inputs"}}` graph that `POST /prompt` and the
  render builders take. It drops muted nodes, passes bypassed nodes through by type (the i-th output of a type takes the i-th connected input of that type, and unequal counts are refused as `ambiguous_bypass`), expands subgraphs (ids
  `<instance>:<inner>`) with their input and output rewiring, resolves `Reroute` and `PrimitiveNode`, skips `Note` and
  `MarkdownNote`, and maps `widgets_values` to named inputs in the `/object_info` listing's input order, counting the hidden
  `control_after_generate` slot, the upload slots and the dotted names of dynamic combos and autogrow groups. `--seed` pins the
  seed inputs and warns about a seed-named input it left alone (`seed_not_pinned`). A template whose `minComfyUIVersion` exceeds the ComfyUI version passed in is refused with a typed reason (the
  version is passed in, never assumed, a known minimum with no version given is a refusal too, and a version given with no minimum to compare it with is a `version_check_skipped` warning); an unknown node class, a
  widget layout that does not add up and an input type with no rule are typed refusals, never a guess. A JSON CLI prints the
  graph or the refusal (`--template`, `--object-info`, `--comfy-version`, `--seed`, `--index`). It is a pure function of the
  template, a listing the caller supplies and options: no ComfyUI, no GPU, no network, no file written, and
  `render/comfy-lifecycle.mjs` is untouched. Wires are followed in a loop with one visited set, so a chain of 100,000 `Reroute`
  nodes resolves without a stack overflow; expansion is bounded (more than 100,000 nodes and subgraph instances, or subgraphs
  nested more than 64 levels, is a typed `graph_too_large` refusal), and a node id two scopes both give or a subgraph definition id
  that appears twice is a `bad_template`, never whichever came last. Nothing calls it yet. Clean room: the GPL-3.0 frontend and ComfyUI's server code
  were not read as source.
- **Why.** `run-graph` and `POST /prompt` take API-format graphs only and the templates catalog could list and classify
  templates but not run one. This is the harness-side converter the catalog's docs said was missing, without a
  third-party custom node.
- **Tests.** `node --test render/template-to-graph.test.mjs` (86 tests, plus two that read private data and skip
  unless `TEMPLATE_GOLDEN_DIR` or `TEMPLATE_SURVEY_DIR` is set): one per behaviour, on hand-built UI-format graphs, a synthetic
  listing and two hand-worked goldens; a mutated widget order turns a golden red, the version refusal and the unknown-class
  refusal are pinned. Written red first (the stub commit fails 51 of 55). A run of 24 one-line mutations of the converter,
  each applied to a copy and proved applied, turned the suite red every time; a review then found twelve that survived, the fixes
  for its findings were written red first (8 of the new tests failed before them), and a later run of 34 one-line mutations, those
  twelve among them, turned the suite red every time too. A release review then found the subgraph output-slot match
  unpinned (dropping it left every test green and put 37 wrong wires into 9 converted templates of the survey below); a synthetic
  test with three outputs of three types read in crossed order now pins it, the graph check compares each wire's output type with
  the type the target input declares, and tests pin cycle detection through bypassed nodes, a bypassed input with no `link` key,
  100,000-hop chains, an expansion bomb, the nesting bound and both id collisions; each of those tests was shown red against a
  one-line mutant of the converter.
- **Checked against real data, privately.** On two saved workflows with an API-format file beside each, the converted graph
  matched value for value (50 of 50 and 20 of 20 nodes, one a subgraph template with dynamic combos). Over the 593 files of
  `comfyui-workflow-templates-json` 0.1.96 with an `/object_info` listing saved on 2026-08-14 (older than the package), 405
  convert and every converted graph is a sound graph (each wire reaches a node in the graph and an output slot its class
  has, and carries a type the target input accepts); the other 188 are typed refusals (unknown classes newer than the listing, schema skew, custom frontend widgets, files
  that are not workflows, two bypassed nodes whose outputs cannot be paired with their inputs). A fuzz of 29,928 malformed-field
  variants of the convertible ones ends in a graph or a typed refusal every time, never an `internal_error` (921 on the first version).
- **Not done.** The acceptance comparison, at least 10 non-API templates node by node against exports of the frontend, is a
  follow-up (the two private pairs are API-format files whose origin is unverified, and every node in both is mode 0, so no
  comparison covers a bypassed or a muted node): it needs a running ComfyUI for a current `/object_info` and a scripted frontend export, which is work under a
  GPU lease, and it was not run. Docs: `docs/systems/media-generation.md` ("Template-to-API converter"), `render/README.md`.
