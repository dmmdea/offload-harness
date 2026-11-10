// noise-seed.mjs — the sampler seed of a run that regenerates a SOURCE image (the edit and inpaint routes).
//
// ComfyUI draws a KSampler's starting noise from torch's CPU generator seeded with the KSampler seed, in the latent's own
// shape. A source rendered at seed S on one canvas and then edited at seed S on the same canvas therefore starts from the
// exact noise tensor that produced the source, and Qwen-Image-Edit-2511 burns that edit: posterized colour and clipped
// channels. Measured on a two-A2 node on 2026-11-08, with source, prompt, weights, LoRA and cards unchanged:
//   - seed 7, the seed the source was rendered with: 21.5 % of the pixels had a channel at 0 and 14.0 % one at 255;
//   - seeds 8 and 1234: 1.1 % and 0.3 %, clean edits.
// A caller who keeps one seed for a whole pipeline (render, then edit) does exactly that. So does a chain of edits at one
// seed, because each edit's output is the next edit's source.
//
// sourceNoiseSeed mixes the caller's seed with the bytes of every source image. The same seed and the same files always
// give the same render, and the noise that made a source is not drawn again: a chained edit's source is a new file, so its
// noise is new too.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

/**
 * sourceNoiseSeed(seed, sources) is the KSampler seed for a run that regenerates `sources` (paths, target first) at the
 * caller's `seed`. The result has 52 bits: an exact JSON number, inside ComfyUI's seed range (0 .. 2^64-1). `read` is
 * injectable for tests; it must return the file's bytes.
 */
export function sourceNoiseSeed(seed, sources, { read = readFileSync } = {}) {
  const h = createHash("sha256").update(`noise-seed\0${seed}`);
  for (const p of sources) h.update("\0").update(read(p));
  return Number.parseInt(h.digest("hex").slice(0, 13), 16);
}
