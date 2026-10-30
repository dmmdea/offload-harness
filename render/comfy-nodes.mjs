// comfy-nodes.mjs — node-class preflight for the render runners: ask the running ComfyUI
// whether it can build every node class a graph names BEFORE the graph is POSTed.
//
// Why (<node-e> parity audit, 2026-09-23): the Wan 2.2 graph ends in VHS_VideoCombine, a
// custom-node class. On a box without ComfyUI-VideoHelperSuite the POST came back 400
// `missing_node_type`, the runner died on it through process.exit, and Windows libuv
// aborted the exit (0xc0000409) — so the caller's defer read as a crash, not as "install
// this pack". The preflight turns that into one line naming the class and the pack, and
// nothing is submitted.
//
// It is a preflight, not a gate: when /object_info cannot be read (older server, transport
// error) it steps aside and the submission reports whatever the server reports. A class
// counts as missing only on a positive answer — /object_info/<class> returns 200 with an
// object that does not carry that class.
//
// Dependency-free, Node 18+.

// NODE_PACKS names the custom-node pack behind each non-core class the shipped builders
// emit (render/wf-*.mjs). Every other class they use is core ComfyUI. The Go side keeps
// the same table for `doctor` (internal/mediacap/routeneeds.go); a class missing here is
// still reported, just without the pack hint.
// ComfyUI-MultiGPU archives upstream 2026-09-30 (issue #223, no successor endorsed).
// The mirror's default branch carries one local Windows P2P/cudart
// fix already carried by the fleet, on top of upstream's last code commit; mirrored privately at dmmdea/ComfyUI-MultiGPU-mirror
// (a clone of this repository has no access to it: the public upstream is the fallback, both are rows of node-packs.json). See
// docs/systems/media-generation.md "Archival".
const MULTIGPU_ARCHIVAL_NOTE =
  " — ARCHIVED upstream 2026-09-30 (no successor, still public and cloneable); the maintainer's mirror https://github.com/dmmdea/ComfyUI-MultiGPU-mirror is PRIVATE (it carries one local Windows P2P/cudart fix): install from it only with access to it, otherwise from the public upstream https://github.com/pollockjj/ComfyUI-MultiGPU";

export const NODE_PACKS = Object.freeze({
  VHS_VideoCombine: "ComfyUI-VideoHelperSuite (https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite)",
  UNETLoaderDisTorch2MultiGPU: `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)${MULTIGPU_ARCHIVAL_NOTE}`,
  UnetLoaderGGUFDisTorch2MultiGPU:
    `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU) together with ComfyUI-GGUF (https://github.com/city96/ComfyUI-GGUF)${MULTIGPU_ARCHIVAL_NOTE}`,
  UnetLoaderGGUF: "ComfyUI-GGUF (https://github.com/city96/ComfyUI-GGUF)",
  // Device-pinned CLIP/VAE loaders (2026-09-24, the pooled display-card-CLIP
  // fix — see wf-krea2.mjs/wf-ltx25-i2v.mjs): same pack, same pin, as the
  // DiT's own DisTorch2 loader above.
  CLIPLoaderMultiGPU: `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)${MULTIGPU_ARCHIVAL_NOTE}`,
  VAELoaderMultiGPU: `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)${MULTIGPU_ARCHIVAL_NOTE}`,
  // The card-only image/edit recipes (render/multicard-recipes.mjs): the plain DiT pin and the DisTorch2 text-encoder loader, both
  // registered by the same pack (its NODE_CLASS_MAPPINGS names them).
  UNETLoaderMultiGPU: `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)${MULTIGPU_ARCHIVAL_NOTE}`,
  CLIPLoaderDisTorch2MultiGPU: `ComfyUI-MultiGPU (https://github.com/pollockjj/ComfyUI-MultiGPU)${MULTIGPU_ARCHIVAL_NOTE}`,
  // The harness's own pack (render/comfyui-ext), which --wan-placement multicard adds through --extra-model-paths-config and
  // never installs into custom_nodes. Missing means the pack failed to import: the reason is in ComfyUI's console log.
  OffloadFreeForDecode: "offload_multicard, the harness's own pack in render/comfyui-ext (loaded by --wan-placement multicard; when it is missing it failed to import, see ComfyUI's console log)",
  // the 2.1 bf16 recipe's text-encoder loader (a 16.33 GiB encoder built off the cards): same harness pack, which also needs ComfyUI-MultiGPU
  OffloadCLIPLoaderDisTorch2: "offload_multicard, the harness's own pack in render/comfyui-ext (it wraps ComfyUI-MultiGPU's CLIPLoaderDisTorch2MultiGPU; when it is missing it failed to import, see ComfyUI's console log)",
  // the LTX-2.5 card-only graph's release of the text encoder between the prompt and the first sampler: same harness pack
  OffloadFreeAfterEncode: "offload_multicard, the harness's own pack in render/comfyui-ext (loaded by --placement multicard on the image and edit routes and by --wan-placement multicard --model ltx25; when it is missing it failed to import, see ComfyUI's console log)",
});

// The classes of the packs this harness ships itself (render/comfyui-ext): never installed into a ComfyUI's custom_nodes.
const HARNESS_OWN_CLASSES = new Set(["OffloadFreeForDecode", "OffloadCLIPLoaderDisTorch2", "OffloadFreeAfterEncode"]);

// graphClasses: the distinct class_type values of an API-format graph, in first-seen order.
export function graphClasses(graph) {
  const seen = new Set();
  for (const node of Object.values(graph || {})) {
    if (node && typeof node.class_type === "string" && node.class_type) seen.add(node.class_type);
  }
  return [...seen];
}

// fetchNodeInfo asks /object_info/<class> for every class named. Returns { checked: true, info: { <class>: <the server's entry for it, or undefined when it
// has none> } }, or { checked: false, reason } when the server could not answer the question (the caller then decides what an unanswered question means).
export async function fetchNodeInfo(api, classes, { fetchImpl = fetch, timeoutMs = 10_000 } = {}) {
  const info = {};
  for (const cls of classes) {
    let r;
    try {
      r = await fetchImpl(`${api}/object_info/${encodeURIComponent(cls)}`, { signal: AbortSignal.timeout(timeoutMs) });
    } catch (e) {
      return { checked: false, reason: `GET /object_info/${cls} failed: ${e?.message || e}` };
    }
    if (!r || !r.ok) {
      return { checked: false, reason: `GET /object_info/${cls} answered HTTP ${r?.status}` };
    }
    let body;
    try { body = await r.json(); } catch {
      return { checked: false, reason: `GET /object_info/${cls} returned no JSON` };
    }
    info[cls] = body && typeof body === "object" && Object.prototype.hasOwnProperty.call(body, cls) ? body[cls] : undefined;
  }
  return { checked: true, info };
}

// missingNodeClasses asks /object_info/<class> for every class the graph names.
// Returns { checked: true, missing: [...] }, or { checked: false, reason } when the server
// could not answer the question (the caller then submits and lets the server speak).
export async function missingNodeClasses(api, graph, { fetchImpl = fetch, timeoutMs = 10_000 } = {}) {
  const classes = graphClasses(graph);
  const res = await fetchNodeInfo(api, classes, { fetchImpl, timeoutMs });
  if (!res.checked) return { checked: false, missing: [], reason: res.reason };
  return { checked: true, missing: classes.filter((cls) => res.info[cls] === undefined) };
}

// missingNodeMessage is the one line a defer carries: greppable code, every missing class
// with its pack, where to install it, and that nothing was submitted.
export function missingNodeMessage(api, missing, comfyDir = "") {
  const parts = missing.map((cls) => (NODE_PACKS[cls] ? `${cls} (custom node pack ${NODE_PACKS[cls]})` : cls));
  const where = comfyDir ? `${comfyDir.replace(/[\\/]+$/, "")}/custom_nodes` : "ComfyUI's custom_nodes";
  // The harness's own pack is never installed into custom_nodes (--wan-placement multicard loads it through --extra-model-paths-config), so for it the
  // sentence is not "install it": it is that it failed to import.
  const installable = missing.some((cls) => !HARNESS_OWN_CLASSES.has(cls));
  const tail = installable
    ? ` — install the pack into ${where} and restart ComfyUI (\`local-offload doctor\` lists every route's node classes).`
    : " — the harness's own pack is never installed into custom_nodes (--wan-placement multicard loads it through --extra-model-paths-config), so it failed to import: ComfyUI's console log says why.";
  return `MISSING_NODE: ComfyUI at ${api} has no node class ${parts.join(", ")}${tail} Nothing was submitted.`;
}

// assertNodeClasses throws MISSING_NODE when the server positively lacks a class, and logs
// (never throws) when the preflight could not run.
export async function assertNodeClasses(api, graph, { fetchImpl = fetch, comfyDir = "", log = (m) => console.error(m) } = {}) {
  const res = await missingNodeClasses(api, graph, { fetchImpl });
  if (!res.checked) {
    log(`comfy-nodes: node-class preflight skipped (${res.reason}); the submission reports any missing class`);
    return;
  }
  if (res.missing.length) throw new Error(missingNodeMessage(api, res.missing, comfyDir));
}
