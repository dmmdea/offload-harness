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

import { allOutputsByNode } from "./comfy-output.mjs";

test("allOutputsByNode maps every save node to its files with kind", () => {
  const outputs = {
    "9":  { images: [{ filename: "a.png", subfolder: "", type: "output" }] },
    "12": { images: [{ filename: "mask.png", subfolder: "sub", type: "output" }] },
    "20": { gifs:   [{ filename: "v.mp4", subfolder: "", type: "output" }] },
  };
  const got = allOutputsByNode(outputs);
  assert.deepEqual(got["9"],  [{ filename: "a.png", subfolder: "", type: "output", kind: "image" }]);
  assert.deepEqual(got["12"], [{ filename: "mask.png", subfolder: "sub", type: "output", kind: "image" }]);
  assert.deepEqual(got["20"], [{ filename: "v.mp4", subfolder: "", type: "output", kind: "gif" }]);
});

test("allOutputsByNode returns {} for empty/nullish", () => {
  assert.deepEqual(allOutputsByNode(null), {});
  assert.deepEqual(allOutputsByNode({ "1": {} }), {});
});
