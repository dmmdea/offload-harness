// node --test render/multicard-image.test.mjs
// The card-only placement of the image and edit routes (--placement multicard on comfy-generate.mjs / comfy-render.mjs / comfy-edit.mjs): which two
// cards, which recipe, what is refused before a lease is spent, and how the runners wire the plan into the lifecycle. Pure: the card table, the pack
// scan, the lease and the HTTP probes are injected; the only real I/O is scratch folders (scratchDir) under the OS temp dir, gone when their test ends,
// and the child processes of the refusal tests, which are refused before anything starts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as W from "./wan-multicard.mjs";
import * as I from "./multicard-image.mjs";
import { SHARED_BINDING_FLAGS, jobArgs } from "./batch-jobs.mjs";
import { scratchDir } from "./test-scratch.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
// Synthetic ids: a repeated-nibble head is a placeholder to the leak gate, and none of these names a real card.
const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000";
const CSV = [
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n");
const CARDS = W.parseCardTable(CSV);
const LEASE = { GPU_LEASE_DEVICES: "gpu-cccc3333-dddd-eeee-ffff-000000000000,gpu-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" };
const BF16 = "qwen_image_2.1_bf16.safetensors";
const QI21 = { family: "qwen-image-2.1", unet: BF16, clip: "qwen3vl_8b_bf16.safetensors" };
const noPacks = () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF"];
const INSTALLED = () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF", "ComfyUI-VideoHelperSuite", "ComfyUI-Manager"];
const plan = (over = {}) => I.planImageMulticard({
  route: "image", ...QI21, flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c", cards: () => CARDS,
  listFn: INSTALLED, ...over,
});
const never = () => { throw new Error("the card table was read"); };

test("the flags are the Wan ones generalised: --placement, --resident-gb, --cards", () => {
  assert.deepEqual([...I.IMAGE_FLAG_NAMES], ["placement", "resident-gb", "cards"]);
  for (const k of ["placement", "resident-gb", "cards"]) assert.ok(SHARED_BINDING_FLAGS.includes(k), `${k} is a shared binding flag of the batch (not per-job)`);
  const args = jobArgs({ out: "o.png", prompt: "p" }, { placement: "multicard", "resident-gb": "6", cards: "a,b" });
  for (const [k, v] of [["--placement", "multicard"], ["--resident-gb", "6"], ["--cards", "a,b"]]) assert.equal(args[args.indexOf(k) + 1], v, k);
  assert.ok(!jobArgs({ out: "o.png", prompt: "p", placement: "multicard" }, {}).includes("--placement"), "a job may not set the binding");
});

test("single is the default: no card table is read, nothing is launched", () => {
  assert.equal(I.planImageMulticard({ route: "image", ...QI21, flags: {}, env: {}, comfyDir: "/c", cards: never }), null);
  assert.equal(I.planImageMulticard({ route: "image", ...QI21, flags: { placement: "single" }, env: { ...LEASE }, comfyDir: "/c", cards: never }), null);
  assert.equal(I.imagePlacementMode({}), "single");
  assert.equal(I.imagePlacementMode({ placement: "multicard" }), "multicard");
  assert.throws(() => I.imagePlacementMode({ placement: "both" }), /--placement must be single\|multicard, got "both"/);
  assert.throws(() => I.imagePlacementMode({ placement: undefined }), /--placement must be/);
});

test("--resident-gb: absent is the recipe's, 0 to 64 accepted, anything else refused", () => {
  assert.equal(I.imageResidentGb({}), undefined);
  assert.equal(I.imageResidentGb({ "resident-gb": "6.5" }), 6.5);
  assert.equal(I.imageResidentGb({ "resident-gb": "0" }), 0);
  for (const bad of ["-1", "65", "abc", "", undefined]) assert.throws(() => I.imageResidentGb({ "resident-gb": bad }), /--resident-gb must be a number of GiB from 0 to 64/, String(bad));
});

test("a plan: the lease's two non-display cards, the same pair, key and launch the Wan names plan for the same lease", () => {
  const p = plan();
  const w = W.planMulticard({
    flags: { "wan-placement": "multicard", "high-unet": "h.safetensors", "low-unet": "l.safetensors" }, env: { ...LEASE }, comfyDir: "/c", isWanGraph: true, cards: () => CARDS,
    launchFn: (pr) => W.multicardLaunch(pr, { comfyDir: "/c", packDirsFn: noPacks }),
  });
  const p2 = plan({ launchFn: (pr) => W.multicardLaunch(pr, { comfyDir: "/c", packDirsFn: noPacks }) });
  assert.deepEqual(p2.launch.flags, w.launch.flags, "with the same packs the argv is the same");
  assert.equal(plan().launch.key, w.launch.key);
  assert.equal(p.pair.storage.uuid, w.pair.storage.uuid);
  assert.equal(p.pair.compute.uuid, w.pair.compute.uuid);
  assert.equal(p.launch.key, w.launch.key);
  assert.equal(p.launch.cudaVisibleDevices, w.launch.cudaVisibleDevices);
  assert.deepEqual(p.launch.env, w.launch.env);
  assert.equal(p.recipe.id, "qwen-image-2.1/bf16");
  assert.equal(p.residentGb, 8);
  assert.equal(p.env.COMFY_INSTANCE, p.launch.key);
  assert.equal(p.env.COMFY_CARD_UUID, "");
  assert.match(p.launch.key, /^mc-/);
});

test("say names the recipe, the two cards and their roles, once", () => {
  const lines = [];
  plan({ say: (l) => lines.push(l) });
  assert.equal(lines.length, 1);
  assert.match(lines[0], /^placement: multicard on the GPU lease: storage cuda:0 = idx 2 .*; compute cuda:1 = idx 0 .*; recipe qwen-image-2\.1\/bf16, 8 GiB of the DiT resident on the compute card; basis predicted/);
});

test("--resident-gb reaches the plan; a plain-pin recipe refuses it; one the compute card cannot hold beside the activations is refused with the numbers", () => {
  assert.equal(plan({ flags: { placement: "multicard", "resident-gb": "6" } }).residentGb, 6);
  assert.throws(() => plan({ unet: "qwen_image_2.1_int8_convrot.safetensors", flags: { placement: "multicard", "resident-gb": "3" } }), /whole DiT on the compute card/);
  assert.equal(plan({ unet: "qwen_image_2.1_int8_convrot.safetensors" }).residentGb, null);
  assert.throws(() => plan({ flags: { placement: "multicard", "resident-gb": "14" } }), /--resident-gb 14.*compute card \(idx 0, 15\.9 GiB\).*2\.5 GiB for the sampler's activations.*at most 12\.9 GiB/s);
});

test("a recipe whose per-card peak does not fit the leased cards (after --reserve-vram) is refused with the card and the figure", () => {
  const small = W.parseCardTable([`0, ${U0}, Small A, 12288, 12000, Disabled, No`, `2, ${U2}, Small B, 12288, 12000, Disabled, No`].join("\n"));
  assert.throws(() => plan({ cards: () => small }), /qwen-image-2\.1\/bf16.*storage card.*14\.2 GiB.*12\.0 GiB.*usable/s);
  // a call that asks for less reserve fits where the default one does not
  const tight = W.parseCardTable([`0, ${U0}, Tight A, 15360, 15000, Disabled, No`, `2, ${U2}, Tight B, 15360, 15000, Disabled, No`].join("\n"));
  assert.throws(() => plan({ cards: () => tight }), /usable/);
  assert.ok(plan({ cards: () => tight, flags: { placement: "multicard", "reserve-vram": "0.2" } }), "a 0.2 GiB reserve leaves 14.8 GiB");
});

test("refused before any card table is read: no recipe, a --graph file, and no card named at all", () => {
  assert.throws(() => plan({ cards: never, family: "krea2", unet: "k.safetensors" }), /no measured card-only recipe for krea2.*--pool-vvram/s);
  assert.throws(() => plan({ cards: never, unet: "qwen_image_2512_bf16.safetensors", family: "qwen-image" }), /bf16 fits no card/);
  assert.throws(() => plan({ cards: never, clip: "qwen3vl_8b_int8.safetensors" }), /int8.*30 dB/s);
  assert.throws(() => plan({ cards: never, isGraph: true }), /posts a --graph file/);
  assert.throws(() => plan({ cards: never, env: {} }), /needs the two cards it runs on.*gpu reserve --devices.*--cards <card>,<card>/s);
  assert.throws(() => plan({ cards: never, family: "hidream-o1" }), /no measured card-only recipe for hidream-o1/);
});

test("refused with the reason: one card, three cards, the display card, a display state the driver did not report", () => {
  const name = (...us) => ({ flags: { placement: "multicard", cards: us.join(",") }, env: {} });
  assert.throws(() => plan(name(U0)), /exactly two cards, and --cards names 1/);
  assert.throws(() => plan(name(U0, U1, U2)), /exactly two cards, and --cards names 3/);
  assert.throws(() => plan(name(U0, U1)), /idx 1 .*drives a display/);
  const odd = W.parseCardTable([`0, ${U0}, A, 16311, 16000, Disabled, No`, `2, ${U2}, B, 16311, 16000, [N/A], [N/A]`].join("\n"));
  assert.throws(() => plan({ ...name(U0, U2), cards: () => odd }), /cannot tell whether idx 2 .*drives a display/);
  assert.throws(() => plan({ flags: { placement: "multicard", cards: "9,0" }, env: {} }), /"9", which is no card in the driver's table/);
});

test("--cards names the pair only when no lease does, and can only confirm a lease's cards", () => {
  assert.equal(plan({ flags: { placement: "multicard", cards: "2,0" }, env: {} }).pair.compute.index, 0);
  assert.equal(plan({ flags: { placement: "multicard", cards: `${U2},${U0}` }, env: { ...LEASE } }).launch.key.startsWith("mc-"), true);
  const one = { GPU_LEASE_DEVICES: "gpu-cccc3333-dddd-eeee-ffff-000000000000" };
  assert.throws(() => plan({ flags: { placement: "multicard", cards: "2,0" }, env: { ...one } }), /--cards names idx 0, which the GPU lease does not hold.*can only confirm/s);
  assert.throws(() => plan({ flags: { placement: "multicard", cards: "" }, env: {} }), /--cards needs <card>,<card>/);
});

test("an '=' spelling of the placement's own flags is refused: the runners' parsers do not split it, so the call would run single, undeclared", () => {
  assert.throws(() => plan({ flags: { "placement=multicard": "--seed" } }), /--placement=multicard.*spelled with '='.*write --placement multicard/s);
  assert.throws(() => plan({ flags: { "resident-gb=6": "x" } }), /spelled with '='/);
  assert.throws(() => plan({ flags: { "cards=0,2": "x" } }), /spelled with '='/);
  assert.throws(() => plan({ flags: { "seed=7": "--placement", "x": 1 } }), /took --placement as its value/);
  // another flag spelled with '=' that swallowed something else is none of this placement's business
  assert.equal(plan({ flags: { placement: "multicard", "seed=7": "--negative" } }).recipe.id, "qwen-image-2.1/bf16");
});

test("the launch loads the packs the recipe needs: MultiGPU always, ComfyUI-GGUF for a GGUF recipe, each by name, refused when absent", () => {
  const names = (list) => I.imagePackDirs("/c", { packs: ["multigpu"] }, () => list);
  assert.deepEqual(names(["ComfyUI-MultiGPU", "x"]), ["ComfyUI-MultiGPU"]);
  assert.deepEqual(I.imagePackDirs("/c", { packs: ["multigpu", "gguf"] }, () => ["ComfyUI-GGUF", "ComfyUI-MultiGPU"]), ["ComfyUI-MultiGPU", "ComfyUI-GGUF"]);
  assert.throws(() => names(["ComfyUI-GGUF"]), /no comfyui-multigpu pack/);
  assert.throws(() => I.imagePackDirs("/c", { packs: ["multigpu", "gguf"] }, () => ["ComfyUI-MultiGPU"]), /no comfyui-gguf pack/);
  const p = plan({ family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined });
  assert.ok(p.launch.flags.includes("ComfyUI-GGUF") && p.launch.flags.includes("ComfyUI-MultiGPU") && p.launch.flags.includes("offload_multicard"));
  assert.ok(!p.launch.flags.includes("ComfyUI-VideoHelperSuite"), "an image recipe has no use for the video pack");
  const q = plan();
  assert.ok(!q.launch.flags.includes("ComfyUI-GGUF"), "a safetensors recipe does not load the GGUF pack");
  assert.throws(() => plan({ family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined, listFn: () => ["ComfyUI-MultiGPU"] }), /no comfyui-gguf pack/);
});

test("lifecycleOptions: a plan hands withGpuSlot its launch and environment; no plan hands it nothing", () => {
  assert.deepEqual(I.lifecycleOptions(null), {});
  const p = plan();
  assert.deepEqual(I.lifecycleOptions(p), { launch: p.launch, instanceEnv: p.env });
});

test("verifyImagePlacement stops the run, before anything is submitted, when ComfyUI's cards are not the pinned pair or the harness's pack did not load", async () => {
  const p = plan();
  const stats = (devices) => async () => ({ ok: true, json: async () => ({ devices }) });
  const good = [{ type: "cuda", vram_total: 16311 * 1024 * 1024 }, { type: "cuda", vram_total: 16311 * 1024 * 1024 }];
  const object = (have) => async (url) => (/object_info/.test(String(url)) ? { ok: true, json: async () => (have ? { OffloadFreeForDecode: {} } : {}) } : stats(good)());
  await I.verifyImagePlacement("http://x", p, { fetchImpl: object(true) });
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: stats([good[0]]) }), /IMAGE-PLACEMENT-MISMATCH.*1 CUDA device.*Nothing was submitted/s);
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: async () => { throw new Error("down"); } }), /IMAGE-PLACEMENT-MISMATCH.*no device list/s);
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: object(false) }), /IMAGE-PLACEMENT-PACK.*offload_multicard.*Nothing was submitted/s);
});

test("portNote says which port a pair instance falls back to, in the image runners' words", () => {
  assert.match(W.portNote({}, {}, "http://127.0.0.1:8189", "placement"), /^placement: no --api or COMFY_API was given/);
  assert.equal(W.portNote({ api: "http://x" }, {}, "http://y", "placement"), "");
  assert.match(W.portNote({}, {}, "http://127.0.0.1:8189"), /^wan-placement: no --api/, "the Wan wording is the default");
});

// ---- the runners, as processes: refused before anything starts -------------------------------------------------------

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
  const c = spawn(process.execPath, [join(HERE, script), ...args], { env, stdio: ["ignore", "pipe", "pipe"] });
  let out = "", err = "";
  c.stdout.on("data", (d) => { out += d; });
  c.stderr.on("data", (d) => { err += d; });
  c.on("error", reject);
  c.on("close", (code) => resolve({ code, out, err }));
});

test("comfy-generate, comfy-render and comfy-edit refuse a multicard call that names no card, with exit 2 and before anything starts", async (t) => {
  const dir = scratchDir(t, "mc-img-");
  const env = scrubbedEnv(dir);
  const flags = ["--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf", "--placement", "multicard", "--no-lock"];
  const gen = await run("comfy-generate.mjs", [join(dir, "o.png"), "a bottle", ...flags], env);
  assert.equal(gen.code, 2, gen.err);
  assert.match(gen.err, /PLACEMENT REFUSED:.*needs the two cards it runs on/s);
  const ren = await run("comfy-render.mjs", [join(dir, "o.png"), "a bottle", ...flags], env);
  assert.equal(ren.code, 2, ren.err);
  assert.match(ren.err, /needs the two cards it runs on/);
  writeFileSync(join(dir, "in.png"), "x");
  const edit = await run("comfy-edit.mjs", [join(dir, "o.png"), join(dir, "in.png"), "make it orange", "--unet", "qwen_image_edit_2511_fp8mixed.safetensors", "--placement", "multicard"], env);
  assert.equal(edit.code, 2, edit.err);
  assert.match(edit.err, /needs the two cards it runs on/);
});

test("comfy-generate refuses a family with no recipe by name, and a batch is refused the same way", async (t) => {
  const dir = scratchDir(t, "mc-img-");
  const env = scrubbedEnv(dir);
  const k = await run("comfy-generate.mjs", [join(dir, "o.png"), "p", "--family", "krea2", "--ckpt", "k.safetensors", "--placement", "multicard"], env);
  assert.equal(k.code, 2, k.err);
  assert.match(k.err, /no measured card-only recipe for krea2/);
  const jobs = join(dir, "jobs.jsonl");
  writeFileSync(jobs, JSON.stringify({ prompt: "p", out: join(dir, "a.png") }) + "\n");
  const b = await run("comfy-generate.mjs", ["--batch", jobs, "--family", "qwen-image", "--ckpt", "qwen_image_2512_bf16.safetensors", "--placement", "multicard"], env);
  assert.equal(b.code, 2, b.err);
  assert.match(b.err, /bf16 fits no card/);
});

test("the wiring: comfy-generate and comfy-edit hand the plan to withGpuSlot, and every runner verifies the pair before it submits", () => {
  const gen = readFileSync(join(HERE, "comfy-generate.mjs"), "utf8");
  const rnd = readFileSync(join(HERE, "comfy-render.mjs"), "utf8");
  const edt = readFileSync(join(HERE, "comfy-edit.mjs"), "utf8");
  assert.equal((gen.match(/\.\.\.lifecycleOptions\(placement\)/g) || []).length, 2, "the single render and the batch both start the pair's instance");
  assert.match(gen, /comfyApi\(flags\.api, placement \? placement\.env : process\.env\)/);
  assert.match(rnd, /\.\.\.lifecycleOptions\(placement\)/);
  assert.equal((rnd.match(/verifyImagePlacement\(API, placement\)/g) || []).length, 1, "one place verifies, and both the standalone path and the --no-lifecycle child go through it");
  assert.match(edt, /\.\.\.lifecycleOptions\(placement\)/);
  assert.match(edt, /verifyImagePlacement\(/);
  // the verification comes before the first thing that is posted
  assert.ok(rnd.indexOf("verifyImagePlacement(API, placement)") < rnd.indexOf("submitGraph("), "comfy-render verifies before submitting");
  assert.ok(edt.indexOf("verifyImagePlacement(API, placement)") < edt.indexOf("submitGraph("), "comfy-edit verifies before submitting");
});
