# template-goldens fixtures

Inputs for `render/template-to-graph.test.mjs`. Nothing here is executed and nothing here is a ComfyUI install.

Every file in this folder is **hand-written and synthetic**. None is an export from the ComfyUI frontend, and none is a copy of
an upstream template:

- `object-info.synthetic.json` is a small `/object_info` listing in the shape a running ComfyUI serves (input spec tuples,
  `input_order`, `output`, `output_node`, `hidden`, the `COMFY_DYNAMICCOMBO_V3`, `COMFY_AUTOGROW_V3` and
  `COMFY_MATCHTYPE_V3` types, the `control_after_generate`, `image_upload`, `audio_upload`, `widgetType`, `forceInput` and
  `socketless` options). It carries 28 invented-or-minimal node classes, enough for every behaviour the tests pin. It is not
  evidence about any real node's schema.
- `basic-t2i.workflow.json` / `basic-t2i.api.json` and `subgraph-i2v.workflow.json` / `subgraph-i2v.api.json` are UI-format
  graphs and the API-format graphs a person worked out from the two formats' public descriptions, by hand, node by node. The
  `.api.json` files were not produced by the converter. The subgraph fixture reproduces the shapes a real exported template
  has: expanded ids `<instance>:<inner>`, subgraph links as objects, input node id `-10` and output node id `-20`, an
  instance with fewer `inputs` than its definition, instance `widgets_values` aligned with the definition's widget inputs, an
  instance link that wins over its widget value.
- `index.synthetic.json` is a three-entry stand-in for the templates package's `index.json`, the file that carries
  `minComfyUIVersion` (a template file itself does not).

What this folder cannot show: that the converter agrees with the frontend's own export on real templates. That comparison
needs real templates, a real `/object_info` and a frontend-exported API graph for each, and it is run privately (see
`docs/systems/media-generation.md`, "Template-to-API converter"); the test reads such pairs from `TEMPLATE_GOLDEN_DIR` when
the variable is set and skips otherwise.
