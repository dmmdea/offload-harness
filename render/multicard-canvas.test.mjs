// node --test render/multicard-canvas.test.mjs
// The canvas cap of the card-only image recipe (register PF-3). The qwen-image/gguf-q5_1 row (the 2512 Q5_1 GGUF across two cards) ran out of memory on its first
// live run, at 928x1664 (1.54 MP). No canvas of this row has been measured, so the cap is CHOSEN, not measured: it is the largest canvas the evidence shows a same-size
// Q5_1 GGUF ran on two cards (the 2511 edit at 1.47 MP), and it sits below the canvas that failed. A call above it is refused by name, before anything starts where the
// runner can say so, instead of being held on two cards until the sampler runs out of memory. Pure: no card table, no ComfyUI.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as R from "./multicard-recipes.mjs";
import * as W from "./wan-multicard.mjs";
import * as I from "./multicard-image.mjs";
import { buildRenderGraph, UsageError } from "./comfy-render.mjs";
import { scratchDir } from "./test-scratch.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));

const IMAGE_ROW = "qwen-image/gguf-q5_1";
const OOM = { width: 928, height: 1664 }; // the canvas of the live two-card run that ran out of memory
const row = (id = IMAGE_ROW) => R.RECIPES.find((r) => r.id === id);

// ---- the table ----------------------------------------------------------------------------------------------------------------

test("the image row carries a pixel cap below the canvas that ran out of memory, and its evidence says the cap was chosen, not measured", () => {
  const r = row();
  assert.ok(r.maxPixels > 0, "the row has a cap");
  assert.ok(r.maxPixels < OOM.width * OOM.height, "and the canvas that failed is above it");
  assert.ok(r.maxPixels >= 1.47e6, "it is not below the 1.47 MP edit canvas a same-size Q5_1 GGUF is recorded to have run on two cards");
  assert.match(r.evidence, /chosen, not measured/);
  assert.equal(r.basis, "predicted", "a cap does not promote the row");
});

test("the edit rows keep their megapixel cap and the image row has no megapixel one (the two caps mean different things)", () => {
  assert.equal(row().maxMegapixels, undefined);
  for (const id of ["qwen-image-edit-2511/fp8mixed", "qwen-image-edit-2511/gguf-q5_1"]) {
    assert.equal(row(id).maxMegapixels, 2.0, id);
    assert.equal(row(id).maxPixels, undefined, id);
  }
});

// ---- canvasRefusal ----------------------------------------------------------------------------------------------------------------

test("canvasRefusal: the canvas that ran out of memory is refused, in words that name the row, both sizes, the cap, that it is chosen, and the flags to change", () => {
  const why = R.canvasRefusal(row(), OOM.width, OOM.height);
  assert.match(why, /qwen-image\/gguf-q5_1/);
  assert.match(why, /928x1664/);
  assert.match(why, /1\.54 MP/);
  assert.match(why, /at most 1\.47 MP/);
  assert.match(why, /chosen, not measured/);
  assert.match(why, /--width.*--height/);
  assert.match(why, /single placement/);
  assert.doesNotMatch(why, /[A-Za-z]:[\/]|GPU-[0-9a-f]{8}/, "no path or card id");
});

test("canvasRefusal: a canvas at or under the cap passes, one pixel row over it is refused, and the family default (1328x1328) is above it", () => {
  const cap = row().maxPixels;
  const w = 1024, h = Math.floor(cap / w);
  assert.equal(R.canvasRefusal(row(), w, h), "", "at the cap (rounded down to a whole row)");
  assert.equal(R.canvasRefusal(row(), 1024, 1024), "");
  assert.equal(R.canvasRefusal(row(), 512, 512), "");
  assert.notEqual(R.canvasRefusal(row(), w, h + 1 + Math.ceil((cap - w * h) / w)), "", "over the cap");
  assert.notEqual(R.canvasRefusal(row(), 1328, 1328), "", "the qwen-image default canvas is above the cap");
});

test("canvasRefusal: the cap itself is taken and one pixel over it is not (a row with a round cap, so the boundary is exact)", () => {
  const round = { ...row(), maxPixels: 1000000 };
  assert.equal(R.canvasRefusal(round, 1000, 1000), "");
  assert.notEqual(R.canvasRefusal(round, 1000, 1001), "");
  assert.notEqual(R.canvasRefusal(round, 1000001, 1), "");
  assert.match(R.canvasRefusal(round, 1000, 1001), /1.00 MP.*pixels/s, "a row with no maxPixelsNote still says what it can");
});

test("canvasRefusal: a row with no pixel cap, or a size that is not a pair of positive numbers, says nothing", () => {
  assert.equal(R.canvasRefusal(row("qwen-image-edit-2511/fp8mixed"), 6000, 4000), "", "the edit rows have their own megapixel cap, enforced by the edit runner");
  for (const [w, h] of [[undefined, 1664], [928, undefined], [NaN, 1664], [928, 0], [-928, 1664], [-2000, -2000], ["928", "1664x"]]) {
    assert.equal(R.canvasRefusal(row(), w, h), "", `${w} x ${h}`);
  }
});

// ---- the graph entry point of the image runner ----------------------------------------------------------------------------------

const POS = ["out.png", "a clay character", "7"];
const GGUF = ["--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf"];
const flagsOf = (argv) => { const f = {}; for (let i = 0; i < argv.length; i += 2) f[argv[i].slice(2)] = argv[i + 1]; return f; };
const build = (argv, { pos = POS, ...extra } = {}) => buildRenderGraph({ pos, flags: flagsOf(argv), env: {}, random: () => 0.5, ...extra });
const MC = { multicard: { residentGb: 3 } };
const refused = (e) => e instanceof UsageError && /qwen-image\/gguf-q5_1.*928x1664.*at most 1\.47 MP/s.test(e.message);

test("comfy-render: --placement multicard at the canvas that ran out of memory is refused by name, named by flags or by position", () => {
  assert.throws(() => build([...GGUF, "--placement", "multicard", "--width", "928", "--height", "1664"], MC), refused);
  assert.throws(() => build([...GGUF, "--placement", "multicard"], { ...MC, pos: [...POS, "928", "1664"] }), refused);
});

test("comfy-render: --placement multicard with no size named is refused too, because the family default (1328x1328) is above the cap, and says which flags to pass", () => {
  assert.throws(() => build([...GGUF, "--placement", "multicard"], MC), (e) => e instanceof UsageError && /1328x1328.*at most 1\.47 MP.*--width.*--height/s.test(e.message));
});

test("comfy-render: --placement multicard at a canvas under the cap builds the multicard graph", () => {
  const g = build([...GGUF, "--placement", "multicard", "--width", "1024", "--height", "1024"], MC).graph;
  assert.equal(g["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
});

test("comfy-render: the single placement is not held to the cap (this is the card-only recipe's limit, not the model's)", () => {
  const g = build([...GGUF, "--width", "928", "--height", "1664"]).graph;
  assert.equal(g["1"].class_type, "UnetLoaderGGUF");
});

// ---- the planner: refused before the card table is read ------------------------------------------------------------------------------

const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const CARDS = W.parseCardTable([`0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`, `1, ${U1}, Test GPU A, 16311, 16050, Disabled, No`].join("\n"));
const LEASE = { GPU_LEASE_DEVICES: `${U0},${U1}`.toLowerCase() };
const never = () => { throw new Error("the card table was read"); };
const plan = (over = {}) => I.planImageMulticard({
  route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c",
  cards: () => CARDS, listFn: () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF"], ...over,
});

test("planImageMulticard: a canvas above the cap is refused before the card table is read, a canvas under it and none at all are planned", () => {
  assert.throws(() => plan({ canvas: OOM, cards: never }), /qwen-image\/gguf-q5_1.*928x1664.*at most 1\.47 MP/s);
  assert.equal(plan({ canvas: { width: 1024, height: 1024 } }).recipe.id, IMAGE_ROW);
  assert.equal(plan().recipe.id, IMAGE_ROW, "a call that names no canvas is judged where the size is known (the graph builder)");
  assert.equal(plan({ canvas: {} }).recipe.id, IMAGE_ROW);
});

// ---- the runner, as a process: a single call is refused before the lease's cards are read, a batch is judged job by job ---------------------------------

function scrubbedEnv(comfyDir) {
  const env = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (/^(GPU_LEASE_|COMFY|LLAMA_SWAP|SDCPP|OFFLOAD_|CUDA_)/i.test(k) || /^path$/i.test(k)) continue;
    env[k] = v;
  }
  env.PATH = dirname(process.execPath);
  env.LLAMA_SWAP_API = "http://127.0.0.1:9";
  env.COMFY_DIR = comfyDir;
  env.COMFY_WAIT_SEC = "5";
  return env;
}
const run = (script, args, env) => new Promise((resolve, reject) => {
  const c = spawn(process.execPath, [join(HERE, script), ...args], { env, stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
  let out = "", err = "";
  c.stdout.on("data", (d) => { out += d; });
  c.stderr.on("data", (d) => { err += d; });
  c.on("error", reject);
  c.on("close", (code) => resolve({ code, out, err }));
});

test("comfy-generate: a single multicard call that names the canvas that ran out of memory is refused with exit 2 before any card is read; one that names a small canvas gets as far as the cards", async (t) => {
  const dir = scratchDir(t, "mc-canvas-");
  const env = scrubbedEnv(dir);
  const flags = ["--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf", "--placement", "multicard", "--no-lock"];
  const big = await run("comfy-generate.mjs", [join(dir, "o.png"), "a bottle", ...flags, "--width", "928", "--height", "1664"], env);
  assert.equal(big.code, 2, big.err);
  assert.match(big.err, /PLACEMENT REFUSED:.*928x1664.*at most 1.47 MP/s);
  assert.doesNotMatch(big.err, /needs the two cards/, "the canvas is judged before the cards");
  const small = await run("comfy-generate.mjs", [join(dir, "o.png"), "a bottle", ...flags, "--width", "1024", "--height", "1024"], env);
  assert.equal(small.code, 2, small.err);
  assert.match(small.err, /PLACEMENT REFUSED:.*needs the two cards it runs on/s, "the small canvas passes the cap and meets the next refusal (this process names no lease)");
});

test("comfy-generate: a --batch is not refused for a shared canvas (a job names its own size; the render of each job judges it)", async (t) => {
  const dir = scratchDir(t, "mc-canvas-");
  const env = scrubbedEnv(dir);
  const jobs = join(dir, "jobs.jsonl");
  writeFileSync(jobs, JSON.stringify({ prompt: "p", out: join(dir, "a.png"), width: 1024, height: 1024 }) + "\n");
  const b = await run("comfy-generate.mjs", ["--batch", jobs, "--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf", "--placement", "multicard", "--no-lock", "--width", "928", "--height", "1664"], env);
  assert.equal(b.code, 2, b.err);
  assert.doesNotMatch(b.err, /cannot take a/);
  assert.match(b.err, /needs the two cards it runs on/);
});
