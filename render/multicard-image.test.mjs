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
import { RECIPES } from "./multicard-recipes.mjs";

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
const INT8 = "qwen_image_2.1_int8_convrot.safetensors";
// qwen-image-2.1 has two rows: the bf16 one launches, the int8_convrot one is held back (render/multicard-recipes.json "blocked") and the planner refuses it by name.
const HELD21 = { family: "qwen-image-2.1", unet: BF16, clip: "qwen3vl_8b_bf16.safetensors" };
// The launchable image row, and the default of plan(): the 2512 Q5_1 GGUF (10 GiB of the DiT resident on the compute card, measured peaks 11.9 / 8.3 GiB, 1.6 GiB of activations).
const GGUF = { family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined };
// The measured edit row: the 2511 fp8mixed DiT through DisTorch2 (9 GiB resident), the other launchable row that splits its DiT.
const FP8 = { route: "edit", family: "qwen-image-edit-2511", unet: "qwen_image_edit_2511_fp8mixed.safetensors", clip: undefined };
const noPacks = () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF"];
const INSTALLED = () => ["ComfyUI-MultiGPU", "ComfyUI-GGUF", "ComfyUI-VideoHelperSuite", "ComfyUI-Manager"];
const plan = (over = {}) => I.planImageMulticard({
  route: "image", ...GGUF, flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c", cards: () => CARDS,
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
  assert.equal(I.planImageMulticard({ route: "image", ...GGUF, flags: {}, env: {}, comfyDir: "/c", cards: never }), null);
  assert.equal(I.planImageMulticard({ route: "image", ...GGUF, flags: { placement: "single" }, env: { ...LEASE }, comfyDir: "/c", cards: never }), null);
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
  assert.equal(p.recipe.id, "qwen-image/gguf-q5_1");
  assert.equal(p.residentGb, 10);
  assert.equal(p.env.COMFY_INSTANCE, p.launch.key);
  assert.equal(p.env.COMFY_CARD_UUID, "");
  assert.match(p.launch.key, /^mc-/);
});

// The 2.1 bf16 recipe launches: its 16.33 GiB text encoder is built off the cards by the harness's own loader, so the pair of 16 GB cards holds it (a live run with the
// stock DisTorch2 loader built the whole encoder on the storage card and ran out of memory there).
test("the 2.1 bf16 binding plans on two 16 GB cards under the lease, with 8 GiB of the DiT resident on the compute card", () => {
  for (const unet of [BF16, "qwen_image_2.1_uc_bf16.safetensors"]) {
    const p = I.planImageMulticard({ route: "image", ...HELD21, unet, flags: { placement: "multicard" }, env: { ...LEASE }, comfyDir: "/c", listFn: INSTALLED, cards: () => CARDS });
    assert.equal(p.recipe.id, "qwen-image-2.1/bf16", unet);
    assert.equal(p.residentGb, 8);
  }
  const six = I.planImageMulticard({ route: "image", ...HELD21, flags: { placement: "multicard", "resident-gb": "6" }, env: { ...LEASE }, comfyDir: "/c", listFn: INSTALLED, cards: () => CARDS });
  assert.equal(six.residentGb, 6);
});

test("say names the recipe, the two cards and their roles, once", () => {
  const lines = [];
  plan({ say: (l) => lines.push(l) });
  assert.equal(lines.length, 1);
  assert.match(lines[0], /^placement: multicard on the GPU lease: storage cuda:0 = idx 0 .*; compute cuda:1 = idx 2 .*; recipe qwen-image\/gguf-q5_1, 10 GiB of the DiT resident on the compute card; basis measured/);
});

test("--resident-gb reaches the plan; a plain-pin recipe refuses it; one the compute card cannot hold beside the activations is refused with the numbers", () => {
  assert.equal(plan({ flags: { placement: "multicard", "resident-gb": "7.5" } }).residentGb, 7.5);
  // the one plain-pin recipe (int8_convrot) is held back, so the planner refuses it by name whatever else the call says, before any card table is read
  assert.throws(() => plan({ ...HELD21, unet: INT8, flags: { placement: "multicard", "resident-gb": "3" }, cards: never }), /int8_convrot.*held back/s);
  assert.throws(() => plan({ ...HELD21, unet: INT8, cards: never }), /int8_convrot.*held back/s);
  // the compute card (15.9 GiB) less the recipe's 1.6 GiB of activations and the 0.5 GiB context holds at most 13.8 GiB of the DiT
  assert.throws(() => plan({ flags: { placement: "multicard", "resident-gb": "14" } }), /--resident-gb 14.*compute card \(idx 2, 15\.9 GiB\).*1\.6 GiB for the sampler's activations.*at most 13\.8 GiB/s);
});

test("a recipe whose per-card peak does not fit the leased cards (after --reserve-vram) is refused with the card and the figure", () => {
  // 12 GiB cards: the recipe's 10 GiB share leaves the compute card too little beside the 1.6 GiB of activations and the 0.5 GiB context
  const small = W.parseCardTable([`0, ${U0}, Small A, 12288, 12000, Disabled, No`, `2, ${U2}, Small B, 12288, 12000, Disabled, No`].join("\n"));
  assert.throws(() => plan({ cards: () => small }), /compute card \(idx 2, 12\.0 GiB\).*qwen-image\/gguf-q5_1.*at most 9\.9 GiB/s);
  // 13 GiB cards hold the 11.9 GiB compute peak under the default 1.0 GiB reserve, and not under a 1.5 GiB one
  const tight = W.parseCardTable([`0, ${U0}, Tight A, 13312, 13000, Disabled, No`, `2, ${U2}, Tight B, 13312, 13000, Disabled, No`].join("\n"));
  assert.ok(plan({ cards: () => tight }), "11.9 GiB fits 12.0 usable");
  assert.throws(() => plan({ cards: () => tight, flags: { placement: "multicard", "reserve-vram": "1.5" } }), /needs the compute card.*to hold 11\.9 GiB.*13\.0 GiB \(11\.5 GiB usable/s);
});

test("the compute card's peak moves with the resident share, and --reserve-vram counts against both cards", () => {
  // 2512 Q5_1 GGUF: 10 GiB resident is 11.9 GiB on the compute card. 13.2 GiB passes the activations check (at most 13.8) but is 15.1 GiB against 14.9 usable.
  assert.ok(plan({ flags: { placement: "multicard", "resident-gb": "12" } }), "13.9 GiB fits 14.9 GiB usable");
  assert.throws(() => plan({ flags: { placement: "multicard", "resident-gb": "13.2" } }), /needs the compute card.*to hold 15\.1 GiB.*14\.9 GiB usable after --reserve-vram 1/s);
  assert.ok(plan({ flags: { placement: "multicard", "resident-gb": "13.2", "reserve-vram": "0.1" } }), "the same share fits when the call asks for less reserve");
});

// The DiT's total is the invariant: --resident-gb N keeps N GiB of it on the compute card and the allocation string sends the remainder to the STORAGE card, so a
// share below the recipe's puts the same bytes there. The storage card's room is the thinner one (14.9 usable on a 16 GB card); a split that does not fit it is
// refused before anything starts, not run to an out-of-memory error with both cards held.
test("a resident share below the recipe's moves the same bytes onto the storage card, and a split that card cannot hold is refused", () => {
  const share = (gb, extra = {}) => ({ ...FP8, flags: { placement: "multicard", "resident-gb": String(gb), ...extra } });
  // fp8mixed: 19.1 GiB DiT, 9 GiB resident, 11.0 GiB on the storage card at the row's share. A share of 5 is 15.0 GiB there against 14.9 usable.
  assert.equal(plan(share(8)).residentGb, 8, "8 GiB: 12.0 GiB on the storage card fits");
  assert.equal(plan(share(6)).residentGb, 6, "6 GiB: 14.0 GiB on the storage card fits");
  assert.throws(() => plan(share(5)), /needs the storage card \(idx 0.*to hold 15\.0 GiB.*14\.9 GiB usable after --reserve-vram 1/s);
  assert.throws(() => plan(share(0)), /needs the storage card.*to hold 20\.0 GiB/s, "the whole DiT on the storage card is the benchmark's out-of-memory at load");
  // a larger share does not lower the storage figure: no run measured it, so the row's own 11.0 stands (and the compute card refuses it first at 14.9)
  assert.equal(plan(share(10)).residentGb, 10);
  // --reserve-vram counts against the storage card too
  assert.ok(plan(share(5, { "reserve-vram": "0.5" })), "15.0 GiB fits 15.4 usable");
  assert.throws(() => plan(share(7, { "reserve-vram": "3.5" })), /needs the storage card.*to hold 13\.0 GiB.*12\.4 GiB usable/s, "13.0 GiB on the storage card against 15.9 - 3.5");
  // the 2512 Q5_1 GGUF: 8.3 GiB on the storage card at the row's 10 GiB share, so a share of 0 counts all 10 GiB more there (18.3 GiB)
  assert.throws(() => plan({ flags: { placement: "multicard", "resident-gb": "0" } }), /needs the storage card.*to hold 18\.3 GiB/s);
  assert.throws(() => plan({ flags: { placement: "multicard", "resident-gb": "3" } }), /needs the storage card.*to hold 15\.3 GiB/s, "the 3 GiB share that ran out of memory on the storage card at 928x1664");
  assert.equal(plan({ flags: { placement: "multicard", "resident-gb": "4" } }).residentGb, 4, "14.3 GiB on the storage card fits 14.9 usable");
});

test("refused before any card table is read: no recipe, a --graph file, and no card named at all", () => {
  assert.throws(() => plan({ cards: never, family: "hidream-o1", unet: "o1.safetensors" }), /no measured card-only recipe for hidream-o1/);
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
  assert.equal(plan({ flags: { placement: "multicard", cards: "2,0" }, env: {} }).pair.compute.index, 2);
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
  assert.equal(plan({ flags: { placement: "multicard", "seed=7": "--negative" } }).recipe.id, "qwen-image/gguf-q5_1");
});

test("the launch loads the packs the recipe needs: MultiGPU always, ComfyUI-GGUF for a GGUF recipe, each by name, refused when absent", () => {
  const names = (list) => I.imagePackDirs("/c", { packs: ["multigpu"] }, () => list);
  assert.deepEqual(names(["ComfyUI-MultiGPU", "x"]), ["ComfyUI-MultiGPU"]);
  assert.deepEqual(I.imagePackDirs("/c", { packs: ["multigpu", "gguf"] }, () => ["ComfyUI-GGUF", "ComfyUI-MultiGPU"]), ["ComfyUI-MultiGPU", "ComfyUI-GGUF"]);
  assert.throws(() => names(["ComfyUI-GGUF"]), /no comfyui-multigpu pack/);
  assert.throws(() => I.imagePackDirs("/c", { packs: ["multigpu", "gguf"] }, () => ["ComfyUI-MultiGPU"]), /no comfyui-gguf pack/);
  const p = plan();
  assert.ok(p.launch.flags.includes("ComfyUI-GGUF") && p.launch.flags.includes("ComfyUI-MultiGPU") && p.launch.flags.includes("offload_multicard"));
  assert.ok(!p.launch.flags.includes("ComfyUI-VideoHelperSuite"), "an image recipe has no use for the video pack");
  const q = plan(FP8);
  assert.ok(!q.launch.flags.includes("ComfyUI-GGUF"), "a safetensors recipe does not load the GGUF pack");
  assert.throws(() => plan({ family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined, listFn: () => ["ComfyUI-MultiGPU"] }), /no comfyui-gguf pack/);
});

test("lifecycleOptions: a plan hands withGpuSlot its launch and environment; no plan hands it nothing", () => {
  assert.deepEqual(I.lifecycleOptions(null), {});
  const p = plan();
  assert.deepEqual(I.lifecycleOptions(p), { launch: p.launch, instanceEnv: p.env });
});

// What a ComfyUI answers for GET /object_info/<class> when the class is installed: { <class>: { input: { required, optional, hidden } } }. `declare` builds that
// from the inputs a recipe's loader node sets, so the fake server knows exactly the inputs the runner is about to send; `without` names one it does not know.
const objectInfoFor = (plan, { pack = true, absent = [], without = {}, bare = [] } = {}) => {
  const nodes = I.recipeLoaderNodes(plan);
  const byClass = {};
  for (const n of Object.values(nodes)) byClass[n.class_type] = n.inputs;
  return async (url) => {
    const cls = decodeURIComponent(String(url).split("/object_info/")[1] || "");
    if (!/object_info/.test(String(url))) return { ok: true, json: async () => ({ devices: GOOD_DEVICES }) };
    if (cls === "OffloadFreeForDecode") return { ok: true, json: async () => (pack ? { OffloadFreeForDecode: { input: { required: {} } } } : {}) };
    if (absent.includes(cls) || !byClass[cls]) return { ok: true, json: async () => ({}) };
    if (bare.includes(cls)) return { ok: true, json: async () => ({ [cls]: {} }) };
    const known = Object.keys(byClass[cls]).filter((k) => !(without[cls] || []).includes(k));
    return { ok: true, json: async () => ({ [cls]: { input: { required: Object.fromEntries(known.slice(0, 2).map((k) => [k, ["STRING"]])), optional: Object.fromEntries(known.slice(2).map((k) => [k, ["STRING"]])) } } }) };
  };
};
const GOOD_DEVICES = [{ type: "cuda", vram_total: 16311 * 1024 * 1024 }, { type: "cuda", vram_total: 16311 * 1024 * 1024 }];

test("verifyImagePlacement stops the run, before anything is submitted, when ComfyUI's cards are not the pinned pair or the harness's pack did not load", async () => {
  const p = plan();
  const stats = (devices) => async () => ({ ok: true, json: async () => ({ devices }) });
  await I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p) });
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: stats([GOOD_DEVICES[0]]) }), /IMAGE-PLACEMENT-MISMATCH.*1 CUDA device.*Nothing was submitted/s);
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: async () => { throw new Error("down"); } }), /IMAGE-PLACEMENT-MISMATCH.*no device list/s);
  await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p, { pack: false }) }), /IMAGE-PLACEMENT-PACK.*offload_multicard.*Nothing was submitted/s);
});

// A plan for the 2.1 bf16 row: the DiT through DisTorch2 and the text encoder through the harness's own loader, so the check of their classes and inputs is covered
// with the row's data, beside the other launchable rows' own.
const heldPlan = () => {
  const r = RECIPES.find((x) => x.id === "qwen-image-2.1/bf16");
  return { ...plan(), recipe: r, residentGb: r.unet.residentGb };
};

test("verifyImagePlacement asks for the recipe's own loader classes too: a class the pack does not register is MISSING_NODE with the pack hint, and nothing is submitted", async () => {
  const p = heldPlan();
  for (const [cls, pack] of [["OffloadCLIPLoaderDisTorch2", /offload_multicard/], ["UNETLoaderDisTorch2MultiGPU", /ComfyUI-MultiGPU/], ["VAELoaderMultiGPU", /ComfyUI-MultiGPU/]]) {
    await assert.rejects(I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p, { absent: [cls] }) }), (e) => e.message.startsWith("MISSING_NODE: ") && e.message.includes(cls) && pack.test(e.message) && /Nothing was submitted/.test(e.message), cls);
  }
  // a GGUF recipe also needs ComfyUI-GGUF to have registered its DisTorch2 loader
  const g = plan();
  await I.verifyImagePlacement("http://x", g, { fetchImpl: objectInfoFor(g) });
  await assert.rejects(I.verifyImagePlacement("http://x", g, { fetchImpl: objectInfoFor(g, { absent: ["UnetLoaderGGUFDisTorch2MultiGPU"] }) }), /MISSING_NODE: .*UnetLoaderGGUFDisTorch2MultiGPU.*ComfyUI-GGUF/s);
});

test("verifyImagePlacement refuses a loader whose pack no longer declares an input the recipe sets: ComfyUI would drop it and the split would silently not happen", async () => {
  const e = plan(FP8); // the measured edit row: its DiT goes through DisTorch2
  await assert.rejects(
    I.verifyImagePlacement("http://x", e, { fetchImpl: objectInfoFor(e, { without: { UNETLoaderDisTorch2MultiGPU: ["expert_mode_allocations"] } }) }),
    /^Error: IMAGE-PLACEMENT-NODE-INPUT: .*UNETLoaderDisTorch2MultiGPU.*expert_mode_allocations.*qwen-image-edit-2511\/fp8mixed.*silently.*Nothing was submitted/s,
  );
  const p = heldPlan(); // the 2.1 bf16 row: the DiT through DisTorch2, the text encoder through the harness's own loader
  await assert.rejects(
    I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p, { without: { UNETLoaderDisTorch2MultiGPU: ["expert_mode_allocations"] } }) }),
    /^Error: IMAGE-PLACEMENT-NODE-INPUT: .*UNETLoaderDisTorch2MultiGPU.*expert_mode_allocations.*qwen-image-2\.1\/bf16.*silently.*Nothing was submitted/s,
  );
  await assert.rejects(
    I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p, { without: { OffloadCLIPLoaderDisTorch2: ["expert_mode_allocations", "eject_models"] } }) }),
    /OffloadCLIPLoaderDisTorch2.*expert_mode_allocations, eject_models/s,
  );
  // an answer that carries no input section says nothing about inputs, so it is not a refusal
  await I.verifyImagePlacement("http://x", e, { fetchImpl: objectInfoFor(e, { bare: ["UNETLoaderDisTorch2MultiGPU"] }) });
  await I.verifyImagePlacement("http://x", p, { fetchImpl: objectInfoFor(p, { bare: ["UNETLoaderDisTorch2MultiGPU"] }) });
});

test("verifyImagePlacement cannot tell whether a recipe class is there when /object_info does not answer: that is a refusal, not a pass", async () => {
  const p = plan();
  const inner = objectInfoFor(p);
  await assert.rejects(
    I.verifyImagePlacement("http://x", p, { fetchImpl: async (url) => (/object_info\/VAELoaderMultiGPU/.test(String(url)) ? { ok: false, status: 500, json: async () => ({}) } : inner(url)) }),
    /IMAGE-PLACEMENT-PACK: cannot tell.*VAELoaderMultiGPU.*HTTP 500.*Nothing was submitted/s,
  );
});

test("recipeLoaderNodes: the three loader nodes the recipe puts in a graph, from the plan's own share", () => {
  const nodes = I.recipeLoaderNodes(plan({ flags: { placement: "multicard", "resident-gb": "7.5" } }));
  assert.deepEqual(Object.keys(nodes), ["1", "2", "3"]);
  assert.equal(nodes["1"].inputs.expert_mode_allocations, "cuda:1,7.5gb;cuda:0,*");
  assert.equal(I.recipeLoaderNodes(plan({ family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined }))["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
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
  const c = spawn(process.execPath, [join(HERE, script), ...args], { env, stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
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
  const k = await run("comfy-generate.mjs", [join(dir, "o.png"), "p", "--family", "hidream-o1", "--ckpt", "o1.safetensors", "--placement", "multicard"], env);
  assert.equal(k.code, 2, k.err);
  assert.match(k.err, /no measured card-only recipe for hidream-o1/);
  const jobs = join(dir, "jobs.jsonl");
  writeFileSync(jobs, JSON.stringify({ prompt: "p", out: join(dir, "a.png") }) + "\n");
  const b = await run("comfy-generate.mjs", ["--batch", jobs, "--family", "qwen-image", "--ckpt", "qwen_image_2512_bf16.safetensors", "--placement", "multicard"], env);
  assert.equal(b.code, 2, b.err);
  assert.match(b.err, /bf16 fits no card/);
});

test("the wiring: comfy-generate, comfy-render and comfy-edit hand the plan to withGpuSlot and to the function that renders (the order inside it is multicard-image-run.test.mjs's)", () => {
  const code = (f) => readFileSync(join(HERE, f), "utf8").split(/\r?\n/).filter((l) => !/^\s*\/\//.test(l)).join("\n");
  const gen = code("comfy-generate.mjs"), rnd = code("comfy-render.mjs"), edt = code("comfy-edit.mjs");
  assert.equal((gen.match(/\.\.\.lifecycleOptions\(placement\)/g) || []).length, 2, "the single render and the batch both start the pair's instance");
  assert.match(gen, /comfyApi\(flags\.api, placement \? placement\.env : process\.env\)/);
  assert.match(rnd, /\.\.\.lifecycleOptions\(placement\)/);
  assert.match(edt, /\.\.\.lifecycleOptions\(placement\)/);
  // both ways into comfy-render's generate() (the --no-lifecycle child and the standalone slot) hand it the plan; comfy-edit's render step gets it too
  assert.equal((rnd.match(/generate\(out, API, built, flags, placement\)/g) || []).length, 2);
  assert.match(edt, /renderEdit\(\{ out, API, plan, placement, inputDir \}\)/);
});

// ---- the compute card ranked by capability (0.185.0): the Wan planner's role choice, on the image and edit placements ---------------------------------
// Here U0 (idx 0) is the LOWER uuid, so the fast card is idx 0 and the slow one idx 2: the size rule (larger computes) and the uuid rule (higher computes)
// both say idx 2, and only the board-power rule says idx 0.
const RANKED = [
  { ...CARDS[0], name: "Test GPU Fast", totalMiB: 16303, powerLimitW: 300 },
  { ...CARDS[2], name: "Test GPU Slow", totalMiB: 16311, powerLimitW: 180 },
];

test("the image placement ranks the compute card by board power, and falls back to the old rule when the power is not reported", () => {
  const p = plan({ cards: () => RANKED });
  assert.equal(p.pair.compute.index, 0);
  assert.equal(p.pair.storage.index, 2);
  assert.equal(p.pair.why, "the faster card: 300 W board power against 180 W");
  assert.equal(p.launch.key, "mc-cccc3333-aaaa1111", "the instance key follows the roles: storage first");
  const unread = plan({ cards: () => RANKED.map((c) => ({ ...c, powerLimitW: 0 })) });
  assert.equal(unread.pair.compute.index, 2, "the larger card computes, as before");
  assert.match(unread.pair.why, /^the larger card \(16311 MiB against 16303 MiB\) \(board power not reported for idx 2, idx 0\)$/);
});

test("WAN_COMPUTE_UUID names the compute card of the image placement too, refuses a token that names no card, and ignores one outside the pair", () => {
  // equal sizes and no power: the higher uuid (idx 2) computes unless the card is named
  assert.equal(plan().pair.compute.index, 2);
  const named = plan({ env: { ...LEASE, WAN_COMPUTE_UUID: "0" } });
  assert.equal(named.pair.compute.index, 0);
  assert.equal(named.pair.storage.index, 2);
  assert.equal(named.pair.why, "named by WAN_COMPUTE_UUID");
  assert.equal(plan({ env: { ...LEASE, WAN_COMPUTE_UUID: "aaaa1111" } }).pair.compute.index, 0, "a prefix");
  // it beats the board power
  assert.equal(plan({ cards: () => RANKED, env: { ...LEASE, WAN_COMPUTE_UUID: "2" } }).pair.compute.index, 2);
  assert.equal(plan({ cards: () => RANKED }).pair.compute.index, 0, "(the control: unnamed, the fast card computes)");
  // names no card of the table: refused in the image runners' words, with the two cards
  assert.throws(() => plan({ env: { ...LEASE, WAN_COMPUTE_UUID: "deadbeef" } }), /^Error: IMAGE-PLACEMENT: WAN_COMPUTE_UUID names "deadbeef", which is no card in the driver's table.*\(idx 2, idx 0\)/);
  // a real card that is not in the pair: ignored, with a note in the plan
  const outside = plan({ env: { ...LEASE, WAN_COMPUTE_UUID: "1" } });
  assert.equal(outside.pair.compute.index, 2);
  assert.equal(outside.pair.note, 'WAN_COMPUTE_UUID names "1", which does not name exactly one of the two cards (idx 2, idx 0); ranked instead');
  assert.equal(plan().pair.note, "", "(the control: nothing ignored, no note)");
});

test("the override is judged like any other choice: the recipe's resident and peak checks run on the card it names, not on the larger one", () => {
  // 24 GiB beside 16 GiB: unnamed, the larger card (idx 0) computes. The GGUF recipe holds 11.9 GiB on the compute card at its 10 GiB share (+1 GiB per GiB of share)
  // and 8.3 GiB on the storage card (which a larger share does not lower), against the 1 GiB --reserve-vram.
  const unequal = W.parseCardTable([`0, ${U0}, Test GPU Big, 24576, 24000, Disabled, No`, `2, ${U2}, Test GPU Small, 16384, 16000, Disabled, No`].join("\n"));
  const naming = { ...LEASE, WAN_COMPUTE_UUID: "2" };
  const share = (gb) => ({ cards: () => unequal, flags: { placement: "multicard", "resident-gb": String(gb) } });
  // the controls: the same two shares, unnamed, fit the 24 GiB card that computes
  for (const gb of [14, 13.5]) assert.equal(plan(share(gb)).pair.compute.index, 0, `${gb} GiB resident, unnamed`);
  // 14 GiB resident is over what the 16 GiB card holds beside the recipe's 1.6 GiB of activations and the 0.5 GiB context (13.9): the resident check refuses it ON IDX 2
  assert.throws(() => plan({ ...share(14), env: naming }), /--resident-gb 14 leaves the compute card \(idx 2, 16\.0 GiB\).*1\.6 GiB for the sampler's activations.*at most 13\.9 GiB/s);
  // 13.5 GiB passes that check but is a 15.4 GiB peak on the compute card against 15.0 usable: the peak check refuses it ON IDX 2
  assert.throws(() => plan({ ...share(13.5), env: naming }), /needs the compute card \(idx 2, Test GPU Small\) to hold 15\.4 GiB, and it has 16\.0 GiB \(15\.0 GiB usable after --reserve-vram 1\)/);
  // the positive control: a share the named card holds is planned, and the named card computes
  const held = plan({ ...share(12), env: naming });
  assert.deepEqual([held.pair.storage.index, held.pair.compute.index], [0, 2]);
  assert.equal(held.pair.why, "named by WAN_COMPUTE_UUID");
});

test("the image placement's one line ends with the rule that chose the compute card, and the note of an ignored override", () => {
  const lines = [];
  plan({ cards: () => RANKED, say: (l) => lines.push(l) });
  assert.equal(lines.length, 1);
  assert.match(lines[0], /^placement: multicard on the GPU lease: storage cuda:0 = idx 2 .*; compute cuda:1 = idx 0 .*; recipe qwen-image\/gguf-q5_1, 10 GiB of the DiT resident on the compute card; basis measured; compute card chosen: the faster card: 300 W board power against 180 W$/);
  lines.length = 0;
  plan({ cards: () => [...RANKED, CARDS[1]], env: { ...LEASE, WAN_COMPUTE_UUID: "1" }, say: (l) => lines.push(l) });
  assert.match(lines[0], /compute card chosen: the faster card: 300 W board power against 180 W \(WAN_COMPUTE_UUID names "1", which does not name exactly one of the two cards \(idx 2, idx 0\); ranked instead\)$/);
});

test("planImageMulticard reads the card table through queryCardsRanked unless it is handed one", () => {
  const asked = [];
  const smi = (cmd, args, opts) => {
    asked.push({ fields: args[0], opts });
    return /power/.test(args[0])
      ? { status: 0, stdout: `${U0}, 300.00\n${U2}, 180.00` }
      : { status: 0, stdout: `0, ${U0}, Test GPU Fast, 16303, 16000, Disabled, No\n2, ${U2}, Test GPU Slow, 16311, 16050, Disabled, No` };
  };
  const p = plan({ cards: undefined, spawnSync: smi });
  assert.equal(p.pair.compute.index, 0);
  assert.equal(asked.length, 2);
  assert.equal(asked[1].fields, "--query-gpu=uuid,power.default_limit");
  assert.ok(asked.every((a) => a.opts.windowsHide === true));
});
