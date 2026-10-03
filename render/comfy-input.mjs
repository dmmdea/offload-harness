// comfy-input.mjs — staging a source file into ComfyUI's input directory under a name no other
// runner process can also pick. Every ComfyUI instance shares ONE input directory, and a runner
// deletes what it staged once its render ends. With one instance per card several runner
// processes stage at once, so a name built from the clock and the file's basename alone could
// collide: one process would overwrite another's input, and its cleanup would then unlink the
// file from under the other. The name carries the clock, the process id, a random suffix and a
// per-process sequence number, so it is unique across processes (and across a reused pid) and
// inside one process even when two inputs share a basename (an image and its mask).
// Dependency-free; the clock, pid, randomness and copy are injectable for tests.
import { copyFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { basename, join } from "node:path";
import { COMFY_DIR } from "./comfy-lifecycle.mjs";

let counter = 0;
const randomHex = () => randomBytes(3).toString("hex");

/** stagedInputName: `<prefix>_<ms>-<pid>-<random>_<n>_<basename>`; the file's own name stays last. */
export function stagedInputName(prefix, srcPath, { now = Date.now, pid = process.pid, rand = randomHex, n } = {}) {
  const seq = n === undefined ? counter++ : n;
  return `${prefix}_${now()}-${pid}-${rand()}_${seq}_${basename(srcPath)}`;
}

/** stageInput copies srcPath into <inputDir> (ComfyUI's input directory) and returns the name LoadImage/LoadVideo reads. */
export function stageInput(prefix, srcPath, { inputDir = join(COMFY_DIR, "input"), copy = copyFileSync, ...nameOpts } = {}) {
  const name = stagedInputName(prefix, srcPath, nameOpts);
  copy(srcPath, join(inputDir, name));
  return name;
}
