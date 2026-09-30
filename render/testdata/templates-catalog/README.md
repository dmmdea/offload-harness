# templates-catalog fixtures

Inputs for `render/templates-catalog.test.mjs`. Nothing here is executed and nothing here is a
ComfyUI install.

## Real, trimmed (upstream MIT)

`site-packages/comfyui_workflow_templates_json/templates/` holds eight workflow templates and an
`index.json` that lists them. Seven are excerpts of the `comfyui-workflow-templates-json` 0.1.94
wheel; `templates_purz_pixel_sort_image` comes from the upstream repository at 0.1.97 (it is the
smallest template that declares `requiresCustomNodes`, a field the 0.1.94 wheel has none of). The
directory is an excerpt, not a snapshot of any version: it does not stand in for a real package's
counts.

They are copied from `github.com/Comfy-Org/workflow_templates`, MIT, Copyright (c) 2023-present
Comfy Org. The licence text is in `LICENSE-comfy-workflow-templates.txt` and the repository's
`NOTICE` names it.

Trim rules (applied by a one-off script, so a fixture stays a real graph in every field the catalog
reads):

- Kept: node `id`, `type`, `mode`; `properties.cnr_id`, `properties.models`,
  `properties.proxyWidgets`; `widgets_values` for loaders, `PrimitiveNode`, notes and any node whose
  values name a model file; `definitions.subgraphs[]` with `id`, `name` and the same node fields.
- Dropped: `links`, `groups`, `config`, `extra`, node geometry and slot arrays, every other widget
  value, and in `index.json` the thumbnail, usage, date, logo and tutorial fields.

`site-packages/*.dist-info/METADATA` are six-line stand-ins for the real files: only the
directory name and the `Version:` line are read.

## Synthetic (hand-written, not upstream)

- `synthetic/templates/` is a second mini package: `synthetic_walk_matrix.json` (nested subgraphs,
  bypassed and muted nodes at both levels, a bypassed instance, a dangling instance, an
  unreferenced definition), `synthetic_unannotated.json` (loader widgets with no model
  annotations), `synthetic_custom_nodes.json` (a pack node, one active and one bypassed), and two
  index entries that isolate one API signal each (`synthetic_open_source_false`: only the index says it
  is paid; `api_synthetic_prefix_only`: only the name prefix does). `synthetic_missing_file` is listed
  in the index and has no file on purpose.
- `comfy_api_nodes/` is a stand-in for ComfyUI's API node modules: only `node_id=` declarations, plus
  a Pydantic-style field that the scan must not read. The scan's other two spellings (`NODE_ID = "X"`
  and `_cloud_schema("X", ...)`) are tested from inline text.
- `license-map.fixture.json` is a nine-entry map in the shape of `render/templates-license-map.json`:
  `example-org/...` entries for the synthetic cases and a few real repo ids the real templates
  download from. Its entries are not evidence about any repo.

Fixture repository ids are either real ones that appear in the real templates above or
`example-org/...`. Paths in the tests use `D:/x/...` or `/srv/x/...` forms.
