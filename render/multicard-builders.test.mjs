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
import { allocationString } from "./multicard-recipes.mjs";

const INT8 = "qwen_image_2.1_int8_convrot.safetensors";
const BF16 = "qwen_image_2.1_bf16.safetensors";
const QI21 = { prompt: "a glass bottle", unet: BF16, clip: "qwen3vl_8b_bf16.safetensors", vae: "qwen_image_2.1_vae_bf16.safetensors", seed: 42 };
const classes = (g) => Object.values(g).map((n) => n.class_type);
const rest = (g) => Object.fromEntries(Object.entries(g).filter(([id]) => !["1", "2", "3"].includes(id)));

// ---- qwen-image-2.1 text-to-image -------------------------------------------------------------------------------

test("2.1 int8_convrot multicard: only the three loaders change; every other node is the single graph's, link for link", () => {
  const single = buildQwenImage21({ ...QI21, unet: INT8 });
  const g = buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard" });
  assert.deepEqual(rest(g), rest(single));
  assert.deepEqual(g["1"], { class_type: "UNETLoaderMultiGPU", inputs: { unet_name: INT8, weight_dtype: "default", device: "cuda:1" } });
  assert.equal(g["2"].class_type, "CLIPLoaderDisTorch2MultiGPU");
  assert.equal(g["2"].inputs.device, "cuda:0");
  assert.deepEqual(g["3"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: QI21.vae, device: "cuda:0" } });
  assert.ok(!classes(g).includes("UNETLoader") && !classes(g).includes("CLIPLoader") && !classes(g).includes("VAELoader"), "no stock loader is left beside the MultiGPU ones");
});

test("2.1 bf16 multicard: the DiT through DisTorch2, compute on cuda:1, the resident share in the allocation", () => {
  const g = buildQwenImage21({ ...QI21, placement: "multicard" });
  assert.equal(g["1"].class_type, "UNETLoaderDisTorch2MultiGPU");
  assert.equal(g["1"].inputs.compute_device, "cuda:1");
  assert.equal(g["1"].inputs.expert_mode_allocations, allocationString(8));
  assert.equal(g["1"].inputs.eject_models, false);
  assert.equal(buildQwenImage21({ ...QI21, placement: "multicard", residentGb: 6 })["1"].inputs.expert_mode_allocations, "cuda:1,6gb;cuda:0,*");
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
  assert.throws(() => buildQwenImage21({ ...QI21, unet: INT8, placement: "multicard", residentGb: 3 }), /whole DiT on the compute card/);
});

test("the 2.1 edit graph has no card-only placement: it takes no placement argument and builds as it did", () => {
  const e = { images: ["a.png"], prompt: "p", unet: BF16, clip: QI21.clip, vae: QI21.vae, seed: 1 };
  assert.deepEqual(buildQwenImage21Edit({ ...e, placement: "multicard" }), buildQwenImage21Edit(e), "an unknown argument is not read");
});

// ---- qwen-image 2512 --------------------------------------------------------------------------------------------

const Q2512 = { prompt: "a matte bottle", unet: "qwen-image-2512-Q5_1.gguf", ...QWEN_IMAGE_PRESETS.full, seed: 7 };

test("2512 GGUF multicard: the GGUF DisTorch2 loader on cuda:1, the fp8 encoder and the VAE whole on cuda:0, the rest unchanged", () => {
  const single = buildQwenImage({ ...Q2512 });
  const g = buildQwenImage({ ...Q2512, placement: "multicard" });
  assert.deepEqual(rest(g), rest(single));
  assert.deepEqual(g["1"], {
    class_type: "UnetLoaderGGUFDisTorch2MultiGPU",
    inputs: { unet_name: Q2512.unet, compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,3gb;cuda:0,*", eject_models: false },
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
  const { graph } = build([...QI21_ARGV, "--placement", "multicard"], { multicard: { residentGb: 8 } });
  assert.equal(graph["1"].class_type, "UNETLoaderDisTorch2MultiGPU");
  assert.equal(graph["1"].inputs.expert_mode_allocations, allocationString(8));
  const g2 = build(["--family", "qwen-image", "--ckpt", "qwen-image-2512-Q5_1.gguf", "--placement", "multicard"], { multicard: { residentGb: 3 } }).graph;
  assert.equal(g2["1"].class_type, "UnetLoaderGGUFDisTorch2MultiGPU");
});

test("comfy-render: a --placement multicard that was not planned is refused, never built as the single placement", () => {
  assert.throws(() => build([...QI21_ARGV, "--placement", "multicard"]), (e) => e instanceof UsageError && /--placement multicard.*plan/.test(e.message));
});

test("comfy-render: --placement single, and none, are the single graph; an unknown placement is refused", () => {
  const base = build([...QI21_ARGV]);
  assert.deepEqual(build([...QI21_ARGV, "--placement", "single"]), base);
  assert.throws(() => build([...QI21_ARGV, "--placement", "both"]), (e) => e instanceof UsageError && /--placement must be single\|multicard/.test(e.message));
});

test("comfy-render: the pool flags stay refused on 2.1 (a different placement), with or without --placement", () => {
  for (const k of ["--pool-vvram", "--pool-compute", "--pool-donor"]) {
    assert.throws(() => build([...QI21_ARGV, k, "12"]), (e) => e instanceof UsageError && /no pooled loader wired/.test(e.message), k);
    assert.throws(() => build([...QI21_ARGV, k, "12", "--placement", "multicard"], { multicard: { residentGb: 8 } }), (e) => e instanceof UsageError && /no pooled loader wired/.test(e.message), k + " with multicard");
  }
});

test("comfy-render: --placement multicard on a family with no recipe, or on a --graph file, is refused by name", () => {
  assert.throws(() => build(["--family", "krea2", "--ckpt", "k.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /no measured card-only recipe for krea2.*--pool-vvram/s.test(e.message));
  assert.throws(() => build(["--family", "qwen-image", "--ckpt", "qwen_image_2512_bf16.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /bf16 fits no card/.test(e.message));
  assert.throws(() => build(["--ckpt", "x.safetensors", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /no measured card-only recipe for sdxl/.test(e.message));
  assert.throws(() => build(["--graph", "wf.json", "--placement", "multicard"], { multicard: { residentGb: 3 } }), (e) => e instanceof UsageError && /--graph/.test(e.message));
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
  // no recipe: refused by name, as are the 2.1 edit family and a K-quant
  assert.throws(() => plan({ unet: "qwen-image-edit-2511-Q4_K_M.gguf", placement: "multicard" }, { multicard: { residentGb: 3 } }), (e) => e instanceof EditUsageError && /no measured card-only recipe/.test(e.message));
  assert.throws(() => plan({ unet: BF16, family: "qwen-image-2.1", clip: QI21.clip, vae: QI21.vae, placement: "multicard" }, { multicard: { residentGb: 3 } }), (e) => e instanceof EditUsageError && /no measured card-only recipe for qwen-image-2\.1 edit/.test(e.message));
  // single placement is the graph that was built before
  assert.deepEqual(plan({ unet: FP8, placement: "single" }).build(["staged.png"]), plan({ unet: FP8 }).build(["staged.png"]));
  assert.throws(() => plan({ unet: FP8, placement: "both" }), (e) => e instanceof EditUsageError && /--placement must be single\|multicard/.test(e.message));
});
