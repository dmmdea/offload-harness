// comfy-output.mjs — find the produced file descriptor in a ComfyUI /history outputs
// object. VHS_VideoCombine puts the mp4 under node.gifs (a long-standing quirk — the
// key is "gifs" regardless of container); native SaveVideo/SaveWEBM use node.videos;
// SaveAudio (ACE-Step music / TTS) uses node.audio; image nodes use node.images. The
// descriptor shape {filename,subfolder,type} is identical for all, so /view fetches any
// of them the same way. Returns {filename, subfolder, type} or null.
export function firstOutputFile(outputs) {
  for (const node of Object.values(outputs || {})) {
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
