// node --test render/multicard-recipes.test.mjs
// The card-only recipes of the image and edit routes (render/multicard-recipes.json, read by render/multicard-recipes.mjs): which
// (route, family, quant) has one, which loader sits on which card, and what is refused by name. Pure: no card table, no ComfyUI.
// internal/hostneed keeps a copy of the figures that decide whether a call declares host RAM, and a Go test pins that copy to this file.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  RECIPES, MULTICARD_STORAGE, MULTICARD_COMPUTE, allocationString, findRecipe, multicardLoaders, recipePeaks, recipeResident,
} from "./multicard-recipes.mjs";
import { NODE_PACKS } from "./comfy-nodes.mjs";

const QI21 = { route: "image", family: "qwen-image-2.1", clip: "qwen3vl_8b_bf16.safetensors" };
const find = (o) => findRecipe(o);
const row = (id) => RECIPES.find((r) => r.id === id);
const refusal = (o) => { try { findRecipe(o); } catch (e) { return e.message; } assert.fail("expected a refusal for " + JSON.stringify(o)); };

test("the devices: the storage card is cuda:0 and the compute card cuda:1, the same two the Wan placement names", () => {
  assert.equal(MULTICARD_STORAGE, "cuda:0");
  assert.equal(MULTICARD_COMPUTE, "cuda:1");
});

test("the table holds the six recipes, each with its evidence and an honest basis; the 2512 Q5_1 and krea2 bf16 image rows and the 2511 fp8mixed edit row were measured card-only", () => {
  assert.deepEqual(RECIPES.map((r) => r.id).sort(), [
    "krea2/bf16", "qwen-image-2.1/bf16", "qwen-image-2.1/int8_convrot", "qwen-image-edit-2511/fp8mixed", "qwen-image-edit-2511/gguf-q5_1", "qwen-image/gguf-q5_1",
  ]);
  for (const r of RECIPES) {
    assert.ok(["measured", "predicted"].includes(r.basis), r.id);
    assert.ok(typeof r.evidence === "string" && r.evidence.length > 40, `${r.id}: the evidence is said`);
    assert.ok(r.peakGiB.compute > 0 && r.peakGiB.storage > 0 && r.activationsGiB > 0, r.id);
    assert.ok(["image", "edit"].includes(r.route), r.id);
  }
  assert.deepEqual(RECIPES.filter((r) => r.basis === "measured").map((r) => r.id), ["qwen-image/gguf-q5_1", "krea2/bf16", "qwen-image-edit-2511/fp8mixed"]);
  // a row that may not launch says why, in a sentence; the qwen-image-2.1 int8_convrot row is held back, so the launchable image rows are the 2512 Q5_1 GGUF, krea2 bf16 and the 2.1 bf16
  assert.deepEqual(RECIPES.filter((r) => "blocked" in r).map((r) => r.id).sort(), ["qwen-image-2.1/int8_convrot"]);
  assert.deepEqual(RECIPES.filter((r) => r.route === "image" && !("blocked" in r)).map((r) => r.id).sort(), ["krea2/bf16", "qwen-image-2.1/bf16", "qwen-image/gguf-q5_1"]);
  for (const r of RECIPES.filter((x) => "blocked" in x)) assert.ok(typeof r.blocked === "string" && r.blocked.length > 80, `${r.id}: the reason is said`);
  assert.ok(Object.isFrozen(RECIPES) && RECIPES.every((r) => Object.isFrozen(r)), "a recipe is not edited in place");
});

test("findRecipe: the DiT's file picks the row", () => {
  // the 2.1 files pick their rows too: the bf16 one is returned, the int8_convrot one is held back, so its pick shows in the refusal
  assert.equal(find({ ...QI21, unet: "qwen_image_2.1_bf16.safetensors" }).id, "qwen-image-2.1/bf16");
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_int8_convrot.safetensors" }), /the qwen-image-2\.1\/int8_convrot recipe is held back/);
  assert.equal(find({ route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf" }).id, "qwen-image/gguf-q5_1");
  // krea2's text encoder loads with its own CLIP type; a graph built with qwen_image would read the Qwen3-VL encoder as the wrong model
  const k = find({ route: "image", family: "krea2", unet: "krea2_turbo_bf16.safetensors" });
  assert.equal(k.id, "krea2/bf16");
  assert.equal(multicardLoaders(k, { unet: "krea2_turbo_bf16.safetensors", clip: k.defaultClip, vae: "qwen_image_vae.safetensors" })["2"].inputs.type, "krea2");
  assert.equal(find({ route: "edit", family: "qwen-image-edit-2511", unet: "qwen_image_edit_2511_fp8mixed.safetensors" }).id, "qwen-image-edit-2511/fp8mixed");
  assert.equal(find({ route: "edit", family: "qwen-image-edit-2511", unet: "qwen-image-edit-2511-Q5_1.gguf" }).id, "qwen-image-edit-2511/gguf-q5_1");
  // the builders' own default text encoder stands in when the call names none
  assert.equal(find({ route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: undefined }).id, "qwen-image/gguf-q5_1");
});

// The int8_convrot DiT is a comfy_kitchen int8 model. ComfyUI-MultiGPU's int8 dispatch path hits an illegal memory access when the compute device is not
// cuda:0 (upstream issue 220, unfixed, and the reason the pooled LTX-2.5 seat keeps compute cuda:0). This row puts that DiT on cuda:1, and the benchmark arms
// it cites ran the int8 DiT on cuda:0 with the text encoder on cuda:1: the placement itself never ran. So the runner refuses it by name, before a lease is spent.
test("the int8_convrot recipe is held back: its DiT on cuda:1 was never run, and the int8 path is known to fault on a non-default device", () => {
  const msg = refusal({ ...QI21, unet: "qwen_image_2.1_int8_convrot.safetensors" });
  assert.match(msg, /qwen-image-2\.1\/int8_convrot.*held back/s);
  assert.match(msg, /cuda:1.*illegal memory access/s);
  assert.match(msg, /cuda:0.*text encoder on cuda:1/s, "the arms the row cites ran the other way round");
  assert.match(msg, /blocked/, "and says where to lift it");
  assert.match(refusal({ ...QI21, unet: "qwen-image-2.1-UC-int8_convrot.safetensors" }), /held back/, "the live fast family's file name matches the row");
  // the data of the held-back row is kept (the proof arm builds its graph from it), and the loaders it names are unchanged
  assert.equal(row("qwen-image-2.1/int8_convrot").unet.loader, "plain");
  // the bf16 DiT of the same family is not held back, and its encoder is no longer a reason to hold the int8 row back either
  assert.equal(find({ ...QI21, unet: "qwen_image_2.1_bf16.safetensors" }).id, "qwen-image-2.1/bf16");
  assert.doesNotMatch(msg, /cannot be built on the storage card/, "the harness's own loader builds the bf16 encoder; the int8 DiT on cuda:1 is the only reason left");
  // the order of refusals is unchanged: an int8 text encoder is refused as one first
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_int8_convrot.safetensors", clip: "qwen3vl_8b_int8_convrot.safetensors" }), /int8.*30 dB/s);
});

// The bf16 row was predicted from a benchmark whose text encoder lived in host RAM, which the card-only placement forbids. Run card-only with the stock DisTorch2 loader, under
// --gpu-only ComfyUI built the whole 16.33 GiB encoder on the 15.9 GiB storage card before the allocation string that splits it was attached: out of memory 1.4 s in.
// The row now names the harness's own loader (distorch2-host), which builds the encoder off the cards and places it block by block.
test("the bf16 recipe is launchable through the harness's own text-encoder loader, which builds the 16.33 GiB encoder off the cards", () => {
  for (const unet of ["qwen_image_2.1_bf16.safetensors", "qwen_image_2.1_uc_bf16.safetensors"]) {
    assert.equal(find({ ...QI21, unet }).id, "qwen-image-2.1/bf16", unet);
  }
  const r = row("qwen-image-2.1/bf16");
  assert.ok(!("blocked" in r));
  assert.equal(r.unet.loader, "distorch2");
  assert.deepEqual(r.clip, { loader: "distorch2-host", residentGb: 8 });
  assert.equal(r.basis, "predicted");
  assert.deepEqual(r.peakGiB, { compute: 11.0, storage: 10.1 });
  assert.deepEqual(recipePeaks(r, null), { compute: 11.0, storage: 10.1 });
  assert.equal(recipePeaks(r, 4).storage, 14.1, "a smaller resident share moves the DiT's bytes onto the storage card, one for one");
  assert.equal(r.maxPixels, 2048 * 2048);
  assert.match(r.maxPixelsNote, /chosen, not measured/);
  assert.match(r.evidence, /16\.33 GiB \(the 13\.3 GiB once written here was the DiT file/, "the evidence line's wrong figure is corrected, not left standing");
  // an int8 text encoder is still refused as one first, and a file no row matches is still no recipe
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_int8.safetensors" }), /int8.*30 dB/s);
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_fp8.safetensors" }), /no measured card-only recipe for qwen-image-2\.1/);
});

test("a (family, quant) with no recipe is refused by name, never guessed", () => {
  assert.match(refusal({ route: "image", family: "qwen-image", unet: "qwen_image_2512_bf16.safetensors" }), /no measured card-only recipe for qwen-image .*qwen_image_2512_bf16\.safetensors.*bf16 fits no card.*Q5_1 GGUF/s);
  assert.match(refusal({ route: "image", family: "krea2", unet: "krea2_fp8.safetensors" }), /no measured card-only recipe for krea2.*bf16/s);
  assert.match(refusal({ route: "image", family: "hidream-o1", unet: "o1.safetensors" }), /no measured card-only recipe for hidream-o1/);
  assert.match(refusal({ route: "image", family: "sdxl", unet: "x.safetensors" }), /no measured card-only recipe for sdxl/);
  assert.match(refusal({ route: "image", family: "", unet: "x.safetensors" }), /no measured card-only recipe for sdxl/, "no family is the generic SDXL graph");
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_bf16.gguf" }), /no measured card-only recipe for qwen-image-2\.1/);
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_fp8.safetensors" }), /no measured card-only recipe for qwen-image-2\.1/);
  assert.match(refusal({ route: "image", family: "qwen-image", unet: "qwen-image-2512-Q4_K_M.gguf" }), /no measured card-only recipe for qwen-image/);
  assert.match(refusal({ route: "edit", family: "qwen-image-edit-2511", unet: "qwen-image-edit-2511-Q4_K_M.gguf" }), /no measured card-only recipe for qwen-image-edit-2511/);
  assert.match(refusal({ route: "edit", family: "qwen-image-2.1", unet: "qwen_image_2.1_bf16.safetensors" }), /no measured card-only recipe for qwen-image-2\.1 edit/);
  assert.match(refusal({ route: "image", family: "qwen-image-2.1" }), /needs the DiT file/, "no DiT named: nothing to match a recipe against");
});

// The sentence a family with no row at all gets (comfy-generate and comfy-edit hand planImageMulticard whatever --family says, with no closed-set gate in front of
// it) is read from the table, so it cannot name a recipe the table holds back or leave out one it launches: it listed qwen-image-2.1 as having recipes after both of
// that family's rows were held back.
test("the refusal of a family with no row names the route's launchable rows and its held-back ones, read from the table", () => {
  for (const route of ["image", "edit"]) {
    const rows = RECIPES.filter((r) => r.route === route);
    const msg = refusal({ route, family: "z-image-turbo", unet: "z.safetensors" });
    assert.match(msg, /no measured card-only recipe for z-image-turbo( edit)? \(z\.safetensors\)/, route);
    const [, offered = "", held = ""] = /recipes for (.*?) only(.*)$/s.exec(msg) || [];
    assert.ok(offered, `${route}: the sentence says which rows have a recipe: ${msg}`);
    for (const r of rows.filter((x) => !x.blocked)) assert.ok(offered.includes(r.id), `${route}: ${r.id} is launchable and is listed: ${msg}`);
    for (const r of rows.filter((x) => x.blocked)) {
      assert.ok(!offered.includes(r.id), `${route}: ${r.id} is held back and is not offered: ${msg}`);
      assert.ok(held.includes(r.id), `${route}: ${r.id} is named as held back: ${msg}`);
    }
    if (rows.some((x) => x.blocked)) assert.match(held, /held back/, route);
    else assert.doesNotMatch(msg, /held back/, route + ": a route with no held-back row says nothing of a hold");
  }
  // the 2.1 bf16 row launches and is listed; the int8_convrot row is held back and is named as such
  assert.match(refusal({ route: "image", family: "flux", unet: "flux.safetensors" }), /recipes for [^;]*qwen-image-2\.1\/bf16[^;]* only; qwen-image-2\.1\/int8_convrot is held back/);
  assert.match(refusal({ route: "image", family: "flux", unet: "flux.safetensors" }), /qwen-image\/gguf-q5_1/);
});

// The qwen-image-2.1 hint is written for the image route (its two rows). The edit route has no row for the family, and the 2.1 edit graph takes no placement at all,
// so the image rows would not give it a recipe: the edit refusal says that, and names nothing as held back.
test("the edit route's refusal of qwen-image-2.1 says the 2.1 edit graph has no card-only placement, not that image rows are held back", () => {
  const msg = refusal({ route: "edit", family: "qwen-image-2.1", unet: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_bf16.safetensors" });
  assert.match(msg, /no measured card-only recipe for qwen-image-2\.1 edit/);
  assert.match(msg, /2\.1 edit graph has no card-only placement/);
  assert.doesNotMatch(msg, /held back|int8_convrot|\bbf16 rows\b/, "the image rows' holds are not the edit route's business");
  assert.match(msg, /qwen-image-edit-2511\/fp8mixed/, "it still says which edit rows do have a recipe");
  // the image route keeps the hint that stops an unmatched 2.1 DiT file reading as "bind one of the rows listed before it"
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_fp8.safetensors" }), /the recipes of this family are int8_convrot, bf16; the card-only recipe is the bf16 DiT with the bf16 Qwen3-VL-8B text encoder.*int8_convrot row is held back/s);
});

test("an int8 text encoder is never placed: it failed the 30 dB gate (PSNR 28.65), whatever the recipe", () => {
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_int8.safetensors" }), /int8.*30 dB/s);
  assert.match(refusal({ ...QI21, unet: "qwen_image_2.1_int8_convrot.safetensors", clip: "qwen3vl_8b_int8_convrot.safetensors" }), /int8.*30 dB/s);
  assert.match(refusal({ route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: "qwen_2.5_vl_7b_int8.safetensors" }), /int8.*30 dB/s);
  // the int8_convrot DiT is a different thing from an int8 text encoder, and is a recipe of its own
  assert.equal(row("qwen-image-2.1/int8_convrot").quant, "int8_convrot");
});

test("only the text encoder the recipe was built for is accepted", () => {
  assert.match(refusal({ route: "edit", family: "qwen-image-edit-2511", unet: "qwen_image_edit_2511_fp8mixed.safetensors", clip: "qwen_2.5_vl_7b_bf16.safetensors" }), /text encoder .*qwen_2\.5_vl_7b_bf16.*fp8/s);
  // a held-back row is reported before its text encoder is compared: the hold is the answer, whichever encoder the call names
  assert.match(refusal({ ...QI21, clip: "qwen3vl_8b_fp8.safetensors", unet: "qwen_image_2.1_int8_convrot.safetensors" }), /qwen-image-2\.1\/int8_convrot.*held back/s);
  assert.match(refusal({ ...QI21, clip: "qwen3vl_8b_fp8.safetensors", unet: "qwen_image_2.1_bf16.safetensors" }), /text encoder .*qwen3vl_8b_fp8.*bf16/s);
  assert.match(refusal({ route: "image", family: "qwen-image", unet: "qwen-image-2512-Q5_1.gguf", clip: "qwen_2.5_vl_7b_bf16.safetensors" }), /text encoder .*fp8/s);
});

test("allocationString: the unit letter DisTorch2 requires, the remainder on the storage card", () => {
  assert.equal(allocationString(3), "cuda:1,3gb;cuda:0,*");
  assert.equal(allocationString(0), "cuda:1,0gb;cuda:0,*");
  assert.equal(allocationString(2.345), "cuda:1,2.35gb;cuda:0,*");
  for (const bad of [-1, NaN, Infinity, "3", undefined]) assert.throws(() => allocationString(bad), /residentGb/, String(bad));
});

test("recipeResident: the recipe's default, an override only where the DiT is split, and never negative", () => {
  const split = find({ route: "edit", family: "qwen-image-edit-2511", unet: "qwen_image_edit_2511_fp8mixed.safetensors" });
  const int8 = row("qwen-image-2.1/int8_convrot");
  assert.equal(recipeResident(split), split.unet.residentGb);
  assert.equal(recipeResident(split, 6), 6);
  assert.equal(recipeResident(split, 0), 0);
  assert.equal(recipeResident(int8), null, "a plain pin has no resident share");
  assert.throws(() => recipeResident(int8, 3), /whole DiT on the compute card/);
  assert.throws(() => recipeResident(split, -1), /resident/);
});

// ---- the loader nodes, exactly -----------------------------------------------------------------------------

test("2.1 int8_convrot: the DiT whole on the compute card, the bf16 text encoder split over both cards by the harness's loader, the VAE on the storage card", () => {
  const r = row("qwen-image-2.1/int8_convrot");
  const l = multicardLoaders(r, { unet: "qwen_image_2.1_int8_convrot.safetensors", clip: "qwen3vl_8b_bf16.safetensors", vae: "qwen_image_2.1_vae_bf16.safetensors" });
  assert.deepEqual(l["1"], { class_type: "UNETLoaderMultiGPU", inputs: { unet_name: "qwen_image_2.1_int8_convrot.safetensors", weight_dtype: "default", device: "cuda:1" } });
  assert.deepEqual(l["2"], {
    class_type: "OffloadCLIPLoaderDisTorch2",
    inputs: { clip_name: "qwen3vl_8b_bf16.safetensors", type: "qwen_image", device: "cuda:0", expert_mode_allocations: allocationString(r.clip.residentGb), eject_models: false },
  });
  assert.deepEqual(l["3"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: "qwen_image_2.1_vae_bf16.safetensors", device: "cuda:0" } });
});

test("2.1 bf16: the DiT through DisTorch2 with its resident share on the compute card, the encoder through the harness's loader with 8 GiB on it", () => {
  const r = row("qwen-image-2.1/bf16");
  const l = multicardLoaders(r, { unet: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_bf16.safetensors", vae: "v.safetensors" });
  assert.deepEqual(l["1"], {
    class_type: "UNETLoaderDisTorch2MultiGPU",
    inputs: { unet_name: "qwen_image_2.1_bf16.safetensors", weight_dtype: "default", compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: allocationString(r.unet.residentGb), eject_models: false },
  });
  assert.deepEqual(l["2"], {
    class_type: "OffloadCLIPLoaderDisTorch2",
    inputs: { clip_name: "qwen3vl_8b_bf16.safetensors", type: "qwen_image", device: "cuda:0", expert_mode_allocations: "cuda:1,8gb;cuda:0,*", eject_models: false },
  });
  const o = multicardLoaders(r, { unet: "qwen_image_2.1_bf16.safetensors", clip: "qwen3vl_8b_bf16.safetensors", vae: "v.safetensors", residentGb: 6 });
  assert.equal(o["1"].inputs.expert_mode_allocations, "cuda:1,6gb;cuda:0,*");
});

test("2512 and 2511 GGUF: the GGUF DisTorch2 loader, compute on cuda:1; the fp8 text encoder and the VAE pinned to cuda:0 and never to cuda:1", () => {
  for (const [route, family, unet] of [["image", "qwen-image", "qwen-image-2512-Q5_1.gguf"], ["edit", "qwen-image-edit-2511", "qwen-image-edit-2511-Q5_1.gguf"]]) {
    const r = find({ route, family, unet });
    const l = multicardLoaders(r, { unet, clip: "qwen_2.5_vl_7b_fp8_scaled.safetensors", vae: "qwen_image_vae.safetensors" });
    assert.deepEqual(l["1"], {
      class_type: "UnetLoaderGGUFDisTorch2MultiGPU",
      inputs: { unet_name: unet, compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: allocationString(r.unet.residentGb), eject_models: false },
    }, family);
    assert.deepEqual(l["2"], { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: "qwen_2.5_vl_7b_fp8_scaled.safetensors", type: "qwen_image", device: "cuda:0" } }, family);
    assert.deepEqual(l["3"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: "qwen_image_vae.safetensors", device: "cuda:0" } }, family);
  }
});

test("2511 fp8mixed: the measured split, compute on cuda:1 with 9 GiB resident", () => {
  const unet = "qwen_image_edit_2511_fp8mixed.safetensors";
  const r = find({ route: "edit", family: "qwen-image-edit-2511", unet });
  assert.equal(r.unet.residentGb, 9);
  assert.equal(r.loraLoader, "LoraLoaderBypassModelOnly", "the merging loader ran out of memory (RESULTS 4.2); the bypass one is what was measured");
  const l = multicardLoaders(r, { unet, clip: "qwen_2.5_vl_7b_fp8_scaled.safetensors", vae: "qwen_image_vae.safetensors" });
  assert.deepEqual(l["1"], {
    class_type: "UNETLoaderDisTorch2MultiGPU",
    inputs: { unet_name: unet, weight_dtype: "default", compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,9gb;cuda:0,*", eject_models: false },
  });
});

test("no recipe puts a text encoder that is fp8 on cuda:1: it killed the process in two measured runs", () => {
  for (const r of RECIPES) {
    const unet = r.unetMatch.includes("int8") ? "qwen_image_2.1_int8_convrot.safetensors" : r.unetMatch.includes("bf16") ? "qwen_image_2.1_bf16.safetensors" : r.unetMatch.includes("fp8mixed") ? "qwen_image_edit_2511_fp8mixed.safetensors" : "m-Q5_1.gguf";
    const clip = r.defaultClip || "qwen3vl_8b_bf16.safetensors";
    const node = multicardLoaders(r, { unet, clip, vae: "v.safetensors" })["2"];
    const devices = [node.inputs.device, ...String(node.inputs.expert_mode_allocations || "").split(";").map((s) => s.split(",")[0]).filter(Boolean)];
    assert.equal(node.inputs.device, "cuda:0", `${r.id}: the text encoder's own device is the storage card`);
    if (/fp8/i.test(clip)) {
      assert.deepEqual(node.class_type, "CLIPLoaderMultiGPU", `${r.id}: an fp8 text encoder is pinned whole to cuda:0`);
      assert.ok(!devices.includes("cuda:1"), `${r.id}: ${devices}`);
    }
  }
});

test("every non-core class a recipe's loaders name has a MISSING_NODE hint naming its pack", () => {
  const classes = new Set();
  for (const r of RECIPES) {
    const unet = r.unetMatch.includes("int8") ? "u-int8.safetensors" : r.unetMatch.includes("bf16") ? "u-bf16.safetensors" : r.unetMatch.includes("fp8mixed") ? "u-fp8mixed.safetensors" : "u-Q5_1.gguf";
    for (const n of Object.values(multicardLoaders(r, { unet, clip: r.defaultClip || "qwen3vl_8b_bf16.safetensors", vae: "v.safetensors" }))) classes.add(n.class_type);
    if (r.loraLoader) classes.add(r.loraLoader);
  }
  for (const cls of classes) {
    if (["LoraLoaderModelOnly", "LoraLoaderBypassModelOnly"].includes(cls)) continue; // core ComfyUI
    assert.ok(NODE_PACKS[cls], `${cls} has no pack hint`);
  }
  assert.ok(classes.has("OffloadCLIPLoaderDisTorch2") && classes.has("UNETLoaderMultiGPU"));
});

// A row that claims a canvas cap (`maxMegapixels`) has its activations scaled to it: the benchmark measured the 2511 edit at 1.4727 MP, and the activations
// grow with the canvas (2.6 GiB there, 3.5 at 2.0 MP, which is how the fp8mixed row was made). The GGUF edit row said 2.0 MP and kept its 1.47 MP figure.
test("a row with a canvas cap holds the activations of that cap, and its compute peak holds the resident share beside them", () => {
  const MEASURED_AT = 1.4727; // MP of the benchmark's edit canvas (section 4.6)
  const MEASURED_ACTIVATIONS = { "qwen-image-edit-2511/fp8mixed": 2.6, "qwen-image-edit-2511/gguf-q5_1": 1.9 }; // GiB on the compute card at that canvas
  const capped = RECIPES.filter((r) => r.maxMegapixels > 0);
  assert.deepEqual(capped.map((r) => r.id).sort(), Object.keys(MEASURED_ACTIVATIONS).sort(), "every capped row has its measured activations here");
  for (const r of capped) {
    const atCap = (MEASURED_ACTIVATIONS[r.id] * r.maxMegapixels) / MEASURED_AT;
    assert.ok(r.activationsGiB >= atCap - 0.06, `${r.id}: ${r.activationsGiB} GiB of activations is below the ${atCap.toFixed(2)} GiB the ${r.maxMegapixels} MP cap needs`);
    assert.ok(r.peakGiB.compute + 1e-9 >= (r.unet.residentGb || 0) + r.activationsGiB, `${r.id}: the compute peak ${r.peakGiB.compute} is less than the resident share ${r.unet.residentGb} plus ${r.activationsGiB} GiB of activations`);
  }
});

test("the recipe file names no machine, user or path", () => {
  const text = JSON.stringify(RECIPES);
  assert.ok(!/[A-Za-z]:[\\/]|\/home\/|\/Users\/|\.local|GPU-[0-9a-f]{8}/i.test(text));
});
