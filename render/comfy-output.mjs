// comfy-output.mjs — find the produced file descriptor in a ComfyUI /history outputs
// object. VHS_VideoCombine puts the mp4 under node.gifs (a long-standing quirk — the
// key is "gifs" regardless of container); native SaveVideo/SaveWEBM use node.videos;
// SaveAudio (ACE-Step music / TTS) uses node.audio; image nodes use node.images. The
// descriptor shape {filename,subfolder,type} is identical for all, so /view fetches any
// of them the same way. Returns {filename, subfolder, type} or null.
//
// `graph` (optional, second arg) is the API-format node map the caller submitted — the
// same object it built with a wf-*.mjs builder (or loaded via --graph) and already holds
// at the call site. When given, any node whose graph class_type starts with "Load" is
// skipped even if its /history outputs entry carries a file. Measured 2026-09-23: the
// native `LoadVideo` node echoes a UI preview of its OWN input into its outputs entry —
// same shape as a real Save* result — and outputs is keyed by node id, which
// Object.values()/Object.entries() enumerate in ASCENDING NUMERIC order regardless of
// insertion order. wf-wan-animate2.mjs's `LoadVideo` node "240" therefore sorted before
// its own `SaveVideo` node "246", and firstOutputFile silently returned the driving
// video unmodified — full render time elapsed, exit 0, "WROTE <out>" printed. A loader
// never legitimately produces the result, whatever kind of file it happens to echo, so
// excluding every Load* class is the general fix, not a WAN-Animate-2 special case.
// Callers that hold a graph should always pass it; without one the scan falls back to
// the pre-fix behavior (needed only for ad-hoc/legacy callers with no graph in scope).
export function firstOutputFile(outputs, graph) {
  for (const [nodeId, node] of Object.entries(outputs || {})) {
    if (graph && /^Load/.test(graph[nodeId]?.class_type || "")) continue;
    const f = node?.gifs?.[0] || node?.videos?.[0] || node?.audio?.[0] || node?.images?.[0];
    if (f) return { filename: f.filename, subfolder: f.subfolder || "", type: f.type || "output" };
  }
  return null;
}

const KINDS = [["images", "image"], ["gifs", "gif"], ["videos", "video"], ["audio", "audio"]];

// allOutputsByNode: every output file ComfyUI recorded, keyed by the node id that
// produced it, so a generic caller can address each SaveImage/SaveMask/etc. by the
// node id it put in its own graph — without the harness interpreting graph semantics.
export function allOutputsByNode(outputs) {
  const out = {};
  for (const [nodeId, node] of Object.entries(outputs || {})) {
    const files = [];
    for (const [key, kind] of KINDS) {
      for (const f of node?.[key] || []) {
        files.push({ filename: f.filename, subfolder: f.subfolder || "", type: f.type || "output", kind });
      }
    }
    if (files.length) out[nodeId] = files;
  }
  return out;
}
