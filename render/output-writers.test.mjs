// node --test render/output-writers.test.mjs
//
// Every direct file write in render/ is accounted for. The 2026-10-09 incident was ONE idiom,
// `writeFileSync(out, buf)`, repeated in every runner (it opens and so truncates the target before it
// writes, so a full disk left 21 zero-byte pngs that read as finished). A runner added tomorrow copies
// whichever sibling is nearest, so this lists, per module, the write primitives that may stay and why
// each is not the delivery of a final output. A call that is not listed fails here: if it delivers an
// output, use writeFileAtomic / copyAtomic / commitPartial from atomic-out.mjs; if it does not, list it
// with the reason. Comments are not code and are not counted. Test fixtures (igpu-stub.mjs,
// igpu-testkit.mjs) never ship behaviour and are skipped.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PRIMITIVES = ["writeFileSync", "appendFileSync", "copyFileSync", "cpSync", "createWriteStream", "renameSync", "writeSync"];
const FIXTURES = new Set(["igpu-stub.mjs", "igpu-testkit.mjs"]);

// module -> [calls that may stay, why none of them delivers a final output]
const ALLOWED = {
  "audiocpp-generate.mjs": [{ copyFileSync: 1 }, "stages the engine's wav into the runner's own partial file; deliverFile renames it onto out only after the QA gate"],
  "comfy-generate.mjs": [{ writeFileSync: 1, appendFileSync: 1 }, "the --results JSONL: created empty, then one appended row per job (the append is the contract; callers read it while it grows)"],
  "comfy-inpaint.mjs": [{ writeFileSync: 1, appendFileSync: 2 }, "the --results JSONL, as in comfy-generate.mjs"],
  "comfy-lifecycle.mjs": [{ writeFileSync: 1, createWriteStream: 1, renameSync: 2 }, "ComfyUI's console log, its rotation and its archive"],
  "comfy-ownership.mjs": [{ writeFileSync: 3, renameSync: 1 }, "launch-owner marker files (the one rename is already a temp+rename)"],
  "compose-hyperframes.mjs": [{ writeFileSync: 4, cpSync: 2, renameSync: 1 }, "work-dir files (the template project, the batch rows) and the result JSON's own temp+rename; the delivery move is moveInto -> copyAtomic"],
  "gpu-lock.mjs": [{ writeFileSync: 1 }, "the lease's unload marker"],
  "igpu-engine.mjs": [{ writeFileSync: 1, renameSync: 1 }, "the owner file; deliverFile, the staged-partial rename the other lanes share"],
  "manifest-satisfy.mjs": [{ writeFileSync: 3, createWriteStream: 1 }, "node-pack and model installs (sha-gated bookkeeping and downloads), not a render output"],
  "templates-catalog.mjs": [{ writeFileSync: 1 }, "the dev tool that writes the template catalog"],
};

const stripComments = (src) => src
  .replace(/\/\*[\s\S]*?\*\//g, "")
  .replace(/(^|[\s;{}(])\/\/.*$/gm, "$1");

function writesByModule() {
  const found = {};
  for (const name of readdirSync(HERE).sort()) {
    if (!name.endsWith(".mjs") || name.endsWith(".test.mjs") || FIXTURES.has(name)) continue;
    const src = stripComments(readFileSync(join(HERE, name), "utf8"));
    const counts = {};
    for (const p of PRIMITIVES) {
      const n = (src.match(new RegExp(String.raw`\b${p}\s*\(`, "g")) || []).length;
      if (n) counts[p] = n;
    }
    if (Object.keys(counts).length) found[name] = counts;
  }
  return found;
}

test("every direct file write in render/ is a listed non-output write: a final output goes through atomic-out.mjs", () => {
  const found = writesByModule();
  const expected = Object.fromEntries(Object.entries(ALLOWED).map(([name, [counts]]) => [name, counts]));
  const drift = [];
  for (const name of new Set([...Object.keys(found), ...Object.keys(expected)])) {
    const a = JSON.stringify(found[name] || {}); const e = JSON.stringify(expected[name] || {});
    if (a !== e) drift.push(`${name}: found ${a}, listed ${e}`);
  }
  assert.deepEqual(drift, [], "a direct write that is not listed.\n" +
    "If it delivers a final output (a render, a clip, a result file a caller reads), use atomic-out.mjs: " +
    "a write straight onto the target truncates it first, so a full disk leaves a 0-byte file that looks finished.\n" +
    "If it is not an output, list it in ALLOWED with the reason.\n");
});

test("the writers that deliver outputs import atomic-out.mjs", () => {
  for (const name of [
    "comfy-render.mjs", "comfy-edit.mjs", "comfy-inpaint.mjs", "comfy-animate.mjs", "comfy-music.mjs",
    "comfy-upscale.mjs", "comfy-video.mjs", "comfy-run-graph.mjs", "sdcpp-generate.mjs", "captions-groups.mjs",
    "compose-hyperframes.mjs",
  ]) {
    assert.match(readFileSync(join(HERE, name), "utf8"), /from "\.\/atomic-out\.mjs"/, `${name} delivers an output and must use atomic-out.mjs`);
  }
});
