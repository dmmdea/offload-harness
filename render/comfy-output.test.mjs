import { test } from "node:test";
import assert from "node:assert";
import { firstOutputFile } from "./comfy-output.mjs";

test("reads VHS_VideoCombine gifs, native videos, and images", () => {
  // VHS_VideoCombine writes under .gifs even for mp4.
  assert.deepEqual(
    firstOutputFile({ "9": { gifs: [{ filename: "x.mp4", subfolder: "", type: "output" }] } }),
    { filename: "x.mp4", subfolder: "", type: "output" }
  );
  // native SaveVideo -> .videos
  assert.equal(firstOutputFile({ "9": { videos: [{ filename: "y.webm" }] } }).filename, "y.webm");
  // SaveAudio (ACE-Step music / TTS) -> .audio
  assert.equal(firstOutputFile({ "9": { audio: [{ filename: "m.flac" }] } }).filename, "m.flac");
  // image fallback (so the same helper serves comfy-render too)
  assert.equal(firstOutputFile({ "9": { images: [{ filename: "z.png" }] } }).filename, "z.png");
  // nothing yet
  assert.equal(firstOutputFile({ "9": {} }), null);
  assert.equal(firstOutputFile({}), null);
});
