// node --test render/multicard-builders.test.mjs
// The card-only placement in the graph builders of the image and edit routes (wf-qwen-image-21, wf-qwen-image, wf-qwen-image-edit) and in
// the two runners' graph entry points (comfy-render.mjs buildRenderGraph, comfy-edit.mjs planEdit). Pure: no card table, no ComfyUI.
// The single placement (the default) is pinned to graphs built before placements existed by image-single-golden.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildQwenImage21, buildQwenImage21Edit } from "./wf-qwen-image-21.mjs";
import { buildQwenImage, QWEN_IMAGE_PRESETS } from "./wf-qwen-image.mjs";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS } from "./wf-qwen-image-edit.mjs";
import { buildRenderGraph, UsageError } from "./comfy-render.mjs";
import { planEdit, UsageError as EditUsageError } from "./comfy-edit.mjs";
import { allocationString, multicardLoaders, RECIPES } from "./multicard-recipes.mjs";

const INT8 = "qwen_image_2.1_int8_convrot.safetensors";
const BF16 = "qwen_image_2.1_bf16.safetensors";
const QI21 = { prompt: "a glass bottle", unet: BF16, clip: "qwen3vl_8b_bf16.safetensors", vae: "qwen_image_2.1_vae_bf16.safetensors", seed: 42 };
const classes = (g) => Object.values(g).map((n) => n.class_type);
const rest = (g) => Object.fromEntries(Object.entries(g).filter(([id]) => !["1", "2", "3"].includes(id)));
// the 2.1 multicard graph also routes the conditioning through node 14 (OffloadFreeAfterEncode), which frees the encoder before the sampler; this drops it and points
// its readers back at node 4, so what is left compares with the single graph link for link
const rest21 = (g) => {
  const back = JSON.parse(JSON.stringify(rest(g)), (k, v) => (Array.isArray(v) && v.length === 2 && v[0] === "14" ? ["4", v[1]] : v));
  delete back["14"];
  return back;
};
const RELEASE = { class_type: "OffloadFreeAfterEncode", inputs: { positive: ["4", 0], negative: ["4", 1], clip: ["2", 0] } };

// ---- qwen-image-2.1 text-to-image -------------------------------------------------------------------------------

test("2.1 int8_convrot multicard builds the row's graph: the DiT whole on cuda:0, the encoder split by the harness's loader, node 14 frees it, and a resident share is refused", () => {
  const g = buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard" });
  assert.deepEqual([g["1"].class_type, g["2"].class_type, g["3"].class_type], ["UNETLoaderMultiGPU", "OffloadCLIPLoaderDisTorch2", "VAELoaderMultiGPU"]);
  assert.equal(g["1"].inputs.device, "cuda:0");
  assert.equal(g["2"].inputs.expert_mode_allocations, "cuda:1,5gb;cuda:0,*");
  assert.deepEqual(g["14"], RELEASE);
  assert.throws(() => buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard", residentGb: 3 }), /whole DiT on the compute card/);
  // the single graph of the same file is untouched
  assert.equal(buildQwenImage21({ ...QI21, unet: INT8 })["1"].class_type, "UNETLoader");
});

// The bf16 row's loaders on a compute-first launch (the 2.1 model's fused kernels run on cuda:0 only, so cuda:0 is the compute card): the DiT through DisTorch2 with
// its resident share on cuda:0, the text encoder through the harness's own loader (built off the cards, then split over both: under --gpu-only the stock DisTorch2
// loader would build the whole 16.33 GiB encoder on one card first), computing on cuda:0 with 8 GiB parked on cuda:1, the VAE on cuda:1 (the storage card).
test("2.1 bf16 multicard builds the row's graph: the three loaders change and node 14 frees the encoder, and the text encoder is the harness's own loader", () => {
  const single = buildQwenImage21({ ...QI21 });
  const g = buildQwenImage21({ ...QI21, placement: "multicard" });
  assert.deepEqual(rest21(g), rest(single), "every node but the three loaders and the release is the single graph's, link for link");
  assert.deepEqual(g["14"], RELEASE, "the encoder is freed between the encode and the sampler");
  assert.deepEqual([g["1"].class_type, g["2"].class_type, g["3"].class_type], ["UNETLoaderDisTorch2MultiGPU", "OffloadCLIPLoaderDisTorch2", "VAELoaderMultiGPU"]);
  assert.equal(g["1"].inputs.compute_device, "cuda:0");
  assert.equal(g["1"].inputs.expert_mode_allocations, "cuda:0,8gb;cuda:1,*");
  assert.equal(g["2"].inputs.expert_mode_allocations, "cuda:1,8gb;cuda:0,*", "8 GiB of the encoder is parked on the storage card");
  assert.equal(g["2"].inputs.device, "cuda:0", "the encoder computes on the compute card, where its kernels run");
  assert.equal(g["3"].inputs.device, "cuda:1");
  assert.equal(g["2"].inputs.type, "qwen_image");
  assert.equal(buildQwenImage21({ ...QI21, placement: "multicard", residentGb: 6 })["1"].inputs.expert_mode_allocations, "cuda:0,6gb;cuda:1,*", "the override moves the DiT's share, not the encoder's");
  assert.equal(buildQwenImage21({ ...QI21, placement: "multicard", residentGb: 6 })["2"].inputs.expert_mode_allocations, "cuda:1,8gb;cuda:0,*");
  // the single graph of the same file is untouched: the stock loaders, and no MultiGPU class anywhere in it
  assert.deepEqual([single["1"].class_type, single["2"].class_type, single["3"].class_type], ["UNETLoader", "CLIPLoader", "VAELoader"]);
  assert.ok(!classes(single).some((c) => /MultiGPU|Offload/.test(c)), "no MultiGPU class in the single graph");
});

// The graph shape both 2.1 rows are pinned by: only nodes 1-3 change, link for link; the bf16 DiT through DisTorch2 and the int8 DiT a plain pin, both on cuda:0 (compute-first);
// eject_models false; node 14 frees the encoder before the sampler.
test("2.1 multicard, both rows: only the three loaders change, and they are the row's own", () => {
  const LOADERS = {
    bf16: ["UNETLoaderDisTorch2MultiGPU", "OffloadCLIPLoaderDisTorch2", "VAELoaderMultiGPU"],
    int8_convrot: ["UNETLoaderMultiGPU", "OffloadCLIPLoaderDisTorch2", "VAELoaderMultiGPU"],
  };
  const FILES = { bf16: BF16, int8_convrot: INT8 };
  const rows = RECIPES.filter((r) => r.route === "image" && r.family === "qwen-image-2.1");
  assert.ok(rows.length >= 2 && rows.every((r) => !r.blocked), "both 2.1 image rows are launchable, none of them held back");
  for (const r of rows) {
    const unet = FILES[r.quant];
    assert.ok(unet && LOADERS[r.quant], `${r.id}: name its DiT file and its three loader classes here`);
    const single = buildQwenImage21({ ...QI21, unet });
    const g = buildQwenImage21({ ...QI21, unet, placement: "multicard" });
    assert.deepEqual(rest21(g), rest(single), `${r.id}: every node but the three loaders and the release is the single graph's, link for link`);
    assert.deepEqual(g["14"], RELEASE, r.id);
    assert.deepEqual([g["1"].class_type, g["2"].class_type, g["3"].class_type], LOADERS[r.quant], r.id);
    assert.deepEqual({ 1: g["1"], 2: g["2"], 3: g["3"] }, multicardLoaders(r, { unet, clip: QI21.clip, vae: QI21.vae }), `${r.id}: the loaders are the recipe's own`);
    assert.ok(!classes(g).some((c) => ["UNETLoader", "CLIPLoader", "VAELoader"].includes(c)), `${r.id}: no stock loader is left beside the MultiGPU ones`);
    assert.equal(g["2"].inputs.device, "cuda:0", r.id);
    assert.equal(g["3"].inputs.device, "cuda:1", r.id);
  }
  // the bf16 DiT: through DisTorch2, compute on cuda:0 (compute-first), the recipe's resident share in the allocation, models kept
  const bf16 = buildQwenImage21({ ...QI21, placement: "multicard" });
  assert.equal(bf16["1"].inputs.compute_device, "cuda:0");
  assert.equal(bf16["1"].inputs.expert_mode_allocations, "cuda:0,8gb;cuda:1,*");
  assert.equal(bf16["1"].inputs.eject_models, false);
  assert.equal(buildQwenImage21({ ...QI21, placement: "multicard", residentGb: 6 })["1"].inputs.expert_mode_allocations, "cuda:0,6gb;cuda:1,*");
  // the int8 DiT is a plain pin on the compute card, cuda:0, and has no share to set
  assert.equal(buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard" })["1"].inputs.device, "cuda:0");
  assert.throws(() => buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard", residentGb: 3 }), /whole DiT on the compute card/);
});

test("2.1: with no placement, or single, the graph is the one built before placements existed", () => {
  const base = buildQwenImage21({ ...QI21 });
  assert.deepEqual(buildQwenImage21({ ...QI21, placement: "single" }), base);
  assert.deepEqual(buildQwenImage21({ ...QI21, placement: "single", residentGb: 9 }), base, "a resident share means nothing to the single placement");
  assert.equal(base["1"].class_type, "UNETLoader");
});

test("2.1 multicard refuses what has no recipe, before it builds a graph", () => {
  assert.throws(() => buildQwenImage21({ ...QI21, placement: "multicard", clip: "qwen3vl_8b_int8.safetensors" }), /int8.*30 dB/s);
  assert.throws(() => buildQwenImage21({ ...QI21, placement: "multicard", unet: "qwen_image_2.1_fp8.safetensors" }), /no measured card-only recipe for qwen-image-2\.1/);
  assert.throws(() => buildQwenImage21({ ...QI21, placement: "bogus" }), /placement must be single\|multicard/);
});

test("the 2.1 edit graph under multicard changes the three loaders and routes the conditioning through node 14; the cache device defaults to gpu and never pins host RAM", () => {
  const e = { images: ["a.png"], prompt: "p", unet: BF16, clip: QI21.clip, vae: QI21.vae, seed: 1 };
  const g = buildQwenImage21Edit({ ...e, placement: "multicard" });
  const single = buildQwenImage21Edit({ ...e, cacheDevice: "gpu" });
  const back = JSON.parse(JSON.stringify(rest(g)), (k, v) => (Array.isArray(v) && v.length === 2 && v[0] === "14" ? ["5", v[1]] : v));
  delete back["14"];
  assert.deepEqual(back, rest(single), "every node but the three loaders and the release is the single graph's, link for link");
  assert.deepEqual(g["14"], { class_type: "OffloadFreeAfterEncode", inputs: { positive: ["5", 0], negative: ["5", 1], clip: ["2", 0] } });
  assert.equal(g["4"].inputs.device, "gpu");
  assert.throws(() => buildQwenImage21Edit({ ...e, placement: "multicard", cacheDevice: "cpu" }), /host RAM/);
});

// ---- qwen-image 2512 --------------------------------------------------------------------------------------------

const Q2512 = { prompt: "a matte bottle", unet: "qwen-image-2512-Q5_1.gguf", ...QWEN_IMAGE_PRESETS.full, seed: 7 };

test("2512 GGUF multicard: the GGUF DisTorch2 loader on cuda:1, the fp8 encoder and the VAE whole on cuda:0, the rest unchanged", () => {
  const single = buildQwenImage({ ...Q2512 });
  const g = buildQwenImage({ ...Q2512, placement: "multicard" });
  assert.deepEqual(rest(g), rest(single));
  assert.deepEqual(g["1"], {
    class_type: "UnetLoaderGGUFDisTorch2MultiGPU",
    inputs: { unet_name: Q2512.unet, compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,10gb;cuda:0,*", eject_models: false },
  });
  assert.deepEqual(g["2"], { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: "qwen_2.5_vl_7b_fp8_scaled.safetensors", type: "qwen_image", device: "cuda:0" } });
  assert.deepEqual(g["3"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: "qwen_image_vae.safetensors", device: "cuda:0" } });
});

test("2512 multicard with the Lightning LoRA: the same LoRA node on the multicard DiT", () => {
  const p = QWEN_IMAGE_PRESETS.lightning4;
  const g = buildQwenImage({ ...Q2512, ...p, placement: "multicard" });
  assert.deepEqual(g["4"], { class_type: "LoraLoaderModelOnly", inputs: { model: ["1", 0], lora_name: p.lora, strength_model: 1.0 } });
});

test("2512 multicard: bf16 safetensors has no recipe (it fits no card), an int8 encoder is never used, a K-quant GGUF has no recipe", () => {
  assert.throws(() => buildQwenImage({ ...Q2512, unet: "qwen_image_2512_bf16.safetensors", placement: "multicard" }), /bf16 fits no card.*Q5_1 GGUF/s);
  assert.throws(() => buildQwenImage({ ...Q2512, clip: "qwen_2.5_vl_7b_int8.safetensors", placement: "multicard" }), /int8.*30 dB/s);
  assert.throws(() => buildQwenImage({ ...Q2512, unet: "qwen-image-2512-Q4_K_M.gguf", placement: "multicard" }), /no measured card-only recipe/);
  assert.throws(() => buildQwenImage({ ...Q2512, placement: "nope" }), /placement must be single\|multicard/);
});

test("2512: single and no placement are the same graph", () => {
  const base = buildQwenImage({ ...Q2512 });
  assert.deepEqual(buildQwenImage({ ...Q2512, placement: "single" }), base);
  assert.deepEqual(buildQwenImage({ ...Q2512, placement: "single", residentGb: 5 }), base);
});

// ---- Qwen-Image-Edit 2511 ---------------------------------------------------------------------------------------

const E2511 = { image: "in.png", prompt: "make the light orange", ...QWEN_EDIT_PRESETS.lightning8, seed: 7 };
const FP8 = "qwen_image_edit_2511_fp8mixed.safetensors";
const EGGUF = "qwen-image-edit-2511-Q5_1.gguf";

test("2511 fp8mixed multicard: the measured split, with the bypass LoRA loader (the merging one ran out of memory)", () => {
  const single = buildQwenImageEdit({ ...E2511, unet: FP8 });
  const g = buildQwenImageEdit({ ...E2511, unet: FP8, placement: "multicard" });
  assert.deepEqual(g["1"], {
    class_type: "UNETLoaderDisTorch2MultiGPU",
    inputs: { unet_name: FP8, weight_dtype: "default", compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,9gb;cuda:0,*", eject_models: false },
  });
  assert.deepEqual(g["2"], { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: "qwen_2.5_vl_7b_fp8_scaled.safetensors", type: "qwen_image", device: "cuda:0" } });
  assert.deepEqual(g["3"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: "qwen_image_vae.safetensors", device: "cuda:0" } });
  assert.deepEqual(g["6"], { class_type: "LoraLoaderBypassModelOnly", inputs: { model: ["1", 0], lora_name: QWEN_EDIT_PRESETS.lightning8.lora, strength_model: 1.0 } });
  // every other node is the single graph's, except the LoRA node's class
  const strip = (x) => Object.fromEntries(Object.entries(x).filter(([id]) => !["1", "2", "3", "6"].includes(id)));
  assert.deepEqual(strip(g), strip(single));
  assert.equal(single["6"].class_type, "LoraLoaderModelOnly", "the single graph keeps the merging loader");
});

test("2511 GGUF multicard: the GGUF DisTorch2 loader and the ordinary LoRA loader (the GGUF patches are lazy)", () => {
  const g = buildQwenImageEdit({ ...E2511, unet: EGGUF, placement: "multicard" });
  assert.equal(g["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
  assert.equal(g["1"].inputs.expert_mode_allocations, "cuda:1,3gb;cuda:0,*");
  assert.equal(g["6"].class_type, "LoraLoaderModelOnly");
  assert.equal(buildQwenImageEdit({ ...E2511, unet: EGGUF, placement: "multicard", residentGb: 0 })["1"].inputs.expert_mode_allocations, "cuda:1,0gb;cuda:0,*", "0 is the shape the benchmark ran");
});

test("2511 multicard without a LoRA has no LoRA node, bypass or otherwise", () => {
  const g = buildQwenImageEdit({ ...E2511, ...QWEN_EDIT_PRESETS.full, unet: FP8, placement: "multicard" });
  assert.ok(!classes(g).some((c) => /Lora/.test(c)));
  assert.deepEqual(g["7"].inputs.model, ["1", 0]);
});

test("2511 multicard refuses what has no recipe; single and no placement are the graph that was built before", () => {
  assert.throws(() => buildQwenImageEdit({ ...E2511, unet: "qwen-image-edit-2511-Q4_K_M.gguf", placement: "multicard" }), /no measured card-only recipe for qwen-image-edit-2511/);
  assert.throws(() => buildQwenImageEdit({ ...E2511, unet: FP8, clip: "qwen_2.5_vl_7b_int8.safetensors", placement: "multicard" }), /int8.*30 dB/s);
  assert.throws(() => buildQwenImageEdit({ ...E2511, unet: FP8, placement: "x" }), /placement must be single\|multicard/);
  const base = buildQwenImageEdit({ ...E2511, unet: FP8 });
  assert.deepEqual(buildQwenImageEdit({ ...E2511, unet: FP8, placement: "single", residentGb: 4 }), base);
});

// ---- the runners' entry points ----------------------------------------------------------------------------------

const POS = ["out.png", "a clay character", "7"];
const flagsOf = (argv) => { const f = {}; for (let i = 0; i < argv.length; i += 2) f[argv[i].slice(2)] = argv[i + 1]; return f; };
const build = (argv, extra = {}) => buildRenderGraph({ pos: POS, flags: flagsOf(argv), env: {}, random: () => 0.5, ...extra });
const QI21_ARGV = ["--family", "qwen-image-2.1", "--ckpt", BF16, "--clip", QI21.clip, "--vae", QI21.vae];

test("comfy-render: --placement multicard builds the multicard graph of the family, handed the plan the runner made", () => {
  // a canvas under the row's pixel cap: the family default (1328x1328) is above it and is refused (render/multicard-canvas.test.mjs)
  const g2 = build(["--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf", "--placement", "multicard", "--width", "1024", "--height", "1024"], { multicard: { residentGb: 3 } }).graph;
  assert.equal(g2["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
  assert.equal(g2["1"].inputs.expert_mode_allocations, allocationString(3));
  // the 2.1 bf16 binding builds at the family's default 2048x2048 canvas, which is exactly the row's cap; its int8_convrot sibling builds too, with no resident share (a plain pin)
  const g21 = build([...QI21_ARGV, "--placement", "multicard"], { multicard: { residentGb: 8 } }).graph;
  assert.equal(g21["2"].class_type, "OffloadCLIPLoaderDisTorch2");
  assert.equal(g21["1"].inputs.expert_mode_allocations, allocationString(8, "cuda:0", "cuda:1"));
  const gi = build(["--family", "qwen-image-2.1", "--ckpt", INT8, "--clip", QI21.clip, "--vae", QI21.vae, "--placement", "multicard"], { multicard: { residentGb: null } }).graph;
  assert.deepEqual([gi["1"].class_type, gi["2"].class_type, gi["14"].class_type], ["UNETLoaderMultiGPU", "OffloadCLIPLoaderDisTorch2", "OffloadFreeAfterEncode"]);
});

test("comfy-render: a --placement multicard that was not planned is refused, never built as the single placement", () => {
  assert.throws(() => build([...QI21_ARGV, "--placement", "multicard"]), (e) => e instanceof UsageError && /--placement multicard.*plan/.test(e.message));
});

test("comfy-render: --placement single, and none, are the single graph; an unknown placement is refused", () => {
  const base = build([...QI21_ARGV]);
  assert.deepEqual(build([...QI21_ARGV, "--placement", "single"]), base);
  assert.throws(() => build([...QI21_ARGV, "--placement", "both"]), (e) => e instanceof UsageError && /--placement must be single\|multicard/.test(e.message));
});

test("comfy-render: the pool flags stay refused on 2.1 (a different placement) on the single placement; the next test has them beside --placement multicard", () => {
  for (const k of ["--pool-vvram", "--pool-compute", "--pool-donor"]) {
    assert.throws(() => build([...QI21_ARGV, k, "12"]), (e) => e instanceof UsageError && /no pooled loader wired/.test(e.message), k);
  }
});

// With --placement multicard the recipe is looked up BEFORE the 2.1 branch's pool-flag refusal (comfy-render.mjs buildRenderGraph), so a 2.1 DiT whose row is held back is
// refused by the hold and the pool refusal is not reached. A launchable row gets as far as the pool refusal, which must still refuse: the pool flags are a different
// placement. Each 2.1 row is judged in the state the table has it, so setting a `blocked` on one flips that row's expectation to the hold instead of leaving the
// combination unpinned; both rows are launchable today.
test("comfy-render: pool flags with --placement multicard on a 2.1 row are refused by the hold if the row is held back, and as a pool-wiring error when it is launchable", () => {
  const FILES = { bf16: BF16, int8_convrot: INT8 };
  const rows = RECIPES.filter((r) => r.route === "image" && r.family === "qwen-image-2.1");
  assert.ok(rows.length >= 2, "the 2.1 image rows are in the table");
  for (const r of rows) {
    assert.ok(FILES[r.quant], `${r.id}: name its DiT file here`);
    for (const k of ["--pool-vvram", "--pool-compute", "--pool-donor"]) {
      const argv = ["--family", "qwen-image-2.1", "--ckpt", FILES[r.quant], "--clip", QI21.clip, "--vae", QI21.vae, k, "12", "--placement", "multicard"];
      const want = r.blocked ? /held back/ : /no pooled loader wired/;
      assert.throws(() => build(argv, { multicard: { residentGb: 8 } }), (e) => e instanceof UsageError && want.test(e.message), `${r.id} ${k} with multicard`);
    }
  }
});

test("comfy-render: --placement multicard on a family with no recipe, or on a --graph file, is refused by name", () => {
  assert.throws(() => build(["--family", "hidream-o1", "--ckpt", "o1.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /no measured card-only recipe for hidream-o1/.test(e.message));
  assert.throws(() => build(["--family", "qwen-image", "--ckpt", "qwen_image_2512_bf16.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /bf16 fits no card/.test(e.message));
  assert.throws(() => build(["--ckpt", "x.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /no measured card-only recipe for sdxl/.test(e.message));
  assert.throws(() => build(["--graph", "wf.json", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /--graph/.test(e.message));
});

// The rows' compute peaks assume the activations of a canvas of at most `maxMegapixels` (2.0 MP). The runner honours --megapixels, COMFY_EDIT_MEGAPIXELS and
// gen_edit_megapixels verbatim (resolveEditMegapixels), so a pinned canvas above the cap is a canvas no peak was measured or scaled for: on a 16 GB card it is the
// out-of-memory with both cards held. A canvas the runner derives from the source is held to the cap by resolveEditMegapixels itself.
test("comfy-edit: a canvas the call pins above the recipe's cap is refused before ComfyUI starts, whichever way it is pinned; at the cap, and derived from the source, it runs", () => {
  const plan = (flags, extra = {}) => planEdit({ pos: ["o.png", "in.png", "make it orange"], flags, env: {}, measure: () => ({ width: 928, height: 1664 }), exists: () => true, random: () => 0.5, ...extra });
  const mc = { multicard: { residentGb: 9 } };
  const refused = (e) => e instanceof EditUsageError && /--megapixels 4.*qwen-image-edit-2511\/fp8mixed.*at most 2\.0/s.test(e.message) && /nothing was started/.test(e.message);
  assert.throws(() => plan({ unet: FP8, placement: "multicard", megapixels: "4" }, mc), refused);
  assert.throws(() => plan({ unet: FP8, placement: "multicard" }, { ...mc, env: { COMFY_EDIT_MEGAPIXELS: "4" } }), (e) => e instanceof EditUsageError && /COMFY_EDIT_MEGAPIXELS 4.*at most 2\.0/s.test(e.message));
  assert.throws(() => plan({ unet: EGGUF, placement: "multicard", megapixels: "2.5" }, { multicard: { residentGb: 3 } }), (e) => e instanceof EditUsageError && /qwen-image-edit-2511\/gguf-q5_1.*at most 2\.0/s.test(e.message));
  // the cap itself, a smaller pin and a source-derived canvas all run
  assert.match(plan({ unet: FP8, placement: "multicard", megapixels: "2" }, mc).describe, /mp 2/);
  assert.match(plan({ unet: FP8, placement: "multicard", megapixels: "0.9" }, mc).describe, /mp 0\.9/);
  const huge = plan({ unet: FP8, placement: "multicard" }, { ...mc, measure: () => ({ width: 6000, height: 4000 }) });
  assert.match(huge.describe, /mp 2\b/, "a 24 MP source is held to the 2.0 MP cap by the resolver");
  // the single placement is not this recipe's business: a 4 MP pin is today's behaviour, byte for byte
  assert.match(plan({ unet: FP8, megapixels: "4" }).describe, /mp 4\b/);
  assert.match(plan({ unet: FP8, placement: "single", megapixels: "4" }).describe, /mp 4\b/);
});

test("comfy-edit: --placement multicard plans the 2511 recipe from the --unet the binding names", () => {
  const plan = (flags, extra = {}) => planEdit({ pos: ["o.png", "in.png", "make it orange"], flags, env: {}, measure: () => ({ width: 928, height: 1664 }), exists: () => true, random: () => 0.5, ...extra });
  const fp8 = plan({ unet: FP8, placement: "multicard" }, { multicard: { residentGb: 9 } });
  const g = fp8.build(["staged.png"]);
  assert.equal(g["1"].class_type, "UNETLoaderDisTorch2MultiGPU");
  assert.equal(g["6"].class_type, "LoraLoaderBypassModelOnly");
  assert.match(fp8.describe, /multicard/);
  const gg = plan({ unet: EGGUF, placement: "multicard" }, { multicard: { residentGb: 3 } }).build(["staged.png"]);
  assert.equal(gg["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
  // not planned: refused
  assert.throws(() => plan({ unet: FP8, placement: "multicard" }), (e) => e instanceof EditUsageError && /--placement multicard.*plan/.test(e.message));
  // no recipe: refused by name, as are an unmatched 2.1 file and a K-quant
  assert.throws(() => plan({ unet: "qwen-image-edit-2511-Q4_K_M.gguf", placement: "multicard" }, { multicard: { residentGb: 3 } }), (e) => e instanceof EditUsageError && /no measured card-only recipe/.test(e.message));
  assert.throws(() => plan({ unet: "qwen_image_2.1_fp8.safetensors", family: "qwen-image-2.1", clip: QI21.clip, vae: QI21.vae, placement: "multicard" }, { multicard: { residentGb: 3 } }), (e) => e instanceof EditUsageError && /no measured card-only recipe for qwen-image-2\.1 edit/.test(e.message));
  // single placement is the graph that was built before
  assert.deepEqual(plan({ unet: FP8, placement: "single" }).build(["staged.png"]), plan({ unet: FP8 }).build(["staged.png"]));
  assert.throws(() => plan({ unet: FP8, placement: "both" }), (e) => e instanceof EditUsageError && /--placement must be single\|multicard/.test(e.message));
});
