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
