// render/template-to-graph.mjs - the template-to-API converter (stub: the tests are written first and are red against it).
export const REFUSAL = Object.freeze({
  BAD_TEMPLATE: "bad_template",
  BAD_OBJECT_INFO: "bad_object_info",
  BAD_VERSION: "bad_version",
  COMFY_VERSION_UNKNOWN: "comfy_version_unknown",
  COMFY_TOO_OLD: "comfyui_too_old",
  UNKNOWN_NODE_CLASS: "unknown_node_class",
  GROUP_NODE_UNSUPPORTED: "group_node_unsupported",
  SUBGRAPH_DEFINITION_MISSING: "subgraph_definition_missing",
  SUBGRAPH_CYCLE: "subgraph_cycle",
  SUBGRAPH_WIDGET_MISMATCH: "subgraph_widget_mismatch",
  DANGLING_LINK: "dangling_link",
  LINK_MISMATCH: "link_mismatch",
  LINK_CYCLE: "link_cycle",
  WIDGET_MISMATCH: "widget_mismatch",
  UNSUPPORTED_INPUT_TYPE: "unsupported_input_type",
  PRIMITIVE_WITHOUT_VALUE: "primitive_without_value",
  BAD_SEED: "bad_seed",
});

export function parseVersion() { return null; }
export function compareVersions() { throw new RangeError("not implemented"); }
export function compareApiGraphs() { return ["not implemented"]; }
export function convertTemplate() {
  return { ok: false, refusal: { code: "not_implemented", message: "not implemented", details: {} } };
}
