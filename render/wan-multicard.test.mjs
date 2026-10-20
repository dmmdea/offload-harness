// node --test render/wan-multicard.test.mjs
// The card-only placement for Wan 2.2 (comfy-video.mjs --wan-placement multicard). Pure: the card table, the pack scan, the spawn,
// the HTTP probes and the pid checks are injected; the only real I/O is scratch folders (scratchDir) under the OS temp dir, gone when their test ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync, writeFileSync, appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import {
  buildWan22I2V, WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE,
} from "./wf-wan22-i2v.mjs";
import * as M from "./wan-multicard.mjs";
import * as L from "./comfy-lifecycle.mjs";
import * as O from "./comfy-ownership.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { scratchDir } from "./test-scratch.mjs";
import { buildLtx25I2V } from "./wf-ltx25-i2v.mjs";
import { parseArgs, buildGraphFromArgs, buildGraphForRun, verifyMulticardDevices, verifyMulticardPack } from "./comfy-video.mjs";

// Synthetic ids: a repeated-nibble head is a placeholder to the leak gate, and none of these names a real card.
const U0 = "GPU-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-aaaa1111-dddd-eeee-ffff-000000000000";
const CSV = [
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n");
const CARDS = M.parseCardTable(CSV);
const FP8 = { highUnet: "high_fp8.safetensors", lowUnet: "low_fp8.safetensors" };
const ARGS = { imagePath: "s.png", prompt: "p", seed: 7, width: 720, height: 1280, length: 81, ...FP8 };

const lines = () => { const got = []; return { got, warn: (l) => got.push(l) }; };
const pair = () => M.resolveWanCards({ tokens: [U2, U0], source: "the GPU lease", cards: CARDS });
const EXPERTS = { "high-unet": "high_fp8.safetensors", "low-unet": "low_fp8.safetensors" };
const LEASE = { GPU_LEASE_DEVICES: "gpu-aaaa1111-dddd-eeee-ffff-000000000000,gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee" };
// planMulticard on the synthetic table, under a lease that holds cards 2 and 0 unless a test says otherwise
const planWith = (flags = {}, over = {}) => M.planMulticard({
  flags: { "wan-placement": "multicard", ...EXPERTS, ...flags }, env: { ...LEASE }, comfyDir: "/c", isWanGraph: true, cards: () => CARDS,
  launchFn: (p) => M.multicardLaunch(p, { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] }), ...over,
});

// ---- the builder ------------------------------------------------------------------------------------------

test("single is the default placement and builds exactly the graph that was built before placements existed", () => {
  assert.deepEqual(WAN_PLACEMENTS, ["single", "multicard"]);
  assert.equal(WAN_PLACEMENT_DEFAULT, "single");
  for (const extra of [{}, { decode: "plain" }, { decode: "tiled" }, { loader: "gguf-distorch" }, { fast: true }, { upscaleModel: "u.pth", upscaleWidth: 1920, upscaleHeight: 1080 }]) {
    const base = buildWan22I2V({ ...ARGS, ...extra });
    assert.deepEqual(buildWan22I2V({ ...ARGS, ...extra, placement: "single" }), base, JSON.stringify(extra));
    assert.deepEqual(buildWan22I2V({ ...ARGS, ...extra, placement: "single", residentGb: 9 }), base, "residentGb means nothing to the single placement");
    assert.ok(!Object.values(base).some((n) => n.class_type === "OffloadFreeForDecode"), "no multicard node in a single-placement graph");
  }
});

test("multicard: text encoder and VAE on cuda:0, both experts through DisTorch2 with compute on cuda:1 and the measured allocation", () => {
  const g = buildWan22I2V({ ...ARGS, placement: "multicard" });
  assert.deepEqual(g["2"], { class_type: "VAELoaderMultiGPU", inputs: { vae_name: "wan_2.1_vae.safetensors", device: WAN_STORAGE_DEVICE } });
  assert.deepEqual(g["3"], { class_type: "CLIPLoaderMultiGPU", inputs: { clip_name: "umt5_xxl_fp8_e4m3fn_scaled.safetensors", type: "wan", device: WAN_STORAGE_DEVICE } });
  assert.equal(WAN_STORAGE_DEVICE, "cuda:0");
  assert.equal(WAN_COMPUTE_DEVICE, "cuda:1");
  for (const [id, unet] of [["7", "high_fp8.safetensors"], ["9", "low_fp8.safetensors"]]) {
    assert.deepEqual(g[id], {
      class_type: "UNETLoaderDisTorch2MultiGPU",
      inputs: { unet_name: unet, weight_dtype: "default", compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,3gb;cuda:0,*", eject_models: false },
    }, `expert ${id}`);
  }
  // the rest of the recipe is the native one, untouched: samplers, split, seeds, cfg
  const single = buildWan22I2V({ ...ARGS });
  for (const id of ["4", "5", "6", "8", "10", "11", "12", "14"]) assert.deepEqual(g[id], single[id], "node " + id);
});

test("multicard: the resident share is the allocation string's, with the unit letter DisTorch2 requires", () => {
  assert.equal(WAN_RESIDENT_GB_DEFAULT, 3);
  const alloc = (residentGb) => buildWan22I2V({ ...ARGS, placement: "multicard", residentGb })["7"].inputs.expert_mode_allocations;
  assert.equal(alloc(3), "cuda:1,3gb;cuda:0,*");
  assert.equal(alloc(3.5), "cuda:1,3.5gb;cuda:0,*");
  assert.equal(alloc(0), "cuda:1,0gb;cuda:0,*");
  assert.equal(alloc(2.345), "cuda:1,2.35gb;cuda:0,*", "rounded to two decimals");
  assert.throws(() => alloc(-1), /residentGb/);
  assert.throws(() => alloc(NaN), /residentGb/);
});

test("multicard + plain decode: a node frees the cards between the last sampler and the VAEDecode, and saves the latent", () => {
  const g = buildWan22I2V({ ...ARGS, placement: "multicard", decode: "plain" });
  assert.deepEqual(g["21"], { class_type: "OffloadFreeForDecode", inputs: { samples: ["12", 0], vae: ["2", 0], save_prefix: "latents/render_wan" } });
  assert.deepEqual(g["13"], { class_type: "VAEDecode", inputs: { samples: ["21", 0], vae: ["2", 0] } }, "the decode reads the freed latent");
  assert.deepEqual(g["14"].inputs.images, ["13", 0]);
  // an auto decode that reads a 16 GiB card is the plain decode, so it frees too
  const auto = buildWan22I2V({ ...ARGS, placement: "multicard", decode: "auto", vramTotalBytes: 16 * 1024 ** 3 });
  assert.equal(auto["13"].class_type, "VAEDecode");
  assert.equal(auto["21"].class_type, "OffloadFreeForDecode");
  // the upscale chain still reads the decode's id
  const up = buildWan22I2V({ ...ARGS, placement: "multicard", decode: "plain", upscaleModel: "u.pth", upscaleWidth: 1920, upscaleHeight: 1080 });
  assert.deepEqual(up["18"].inputs.image, ["13", 0]);
  assert.equal(up["21"].inputs.samples[0], "12");
});

test("multicard + tiled decode (and an auto that reads a small card): today's tiled node, no free node", () => {
  for (const extra of [{}, { decode: "tiled" }, { decode: "auto", vramTotalBytes: 8 * 1024 ** 3 }, { decode: "auto" }]) {
    const g = buildWan22I2V({ ...ARGS, placement: "multicard", ...extra });
    assert.equal(g["13"].class_type, "VAEDecodeTiled", JSON.stringify(extra));
    assert.deepEqual(g["13"].inputs, { samples: ["12", 0], vae: ["2", 0], tile_size: 256, overlap: 64, temporal_size: 32, temporal_overlap: 8 }, "the opt-in tiled decode is unchanged");
    assert.ok(!g["21"], "nothing frees the cards: the tiled decode fits beside the expert (+2.3 GiB measured)");
  }
});

test("every non-core class the multicard graph names has a MISSING_NODE hint, the harness's own node included", async () => {
  const { graphClasses, NODE_PACKS } = await import("./comfy-nodes.mjs");
  const core = new Set(["LoadImage", "CLIPTextEncode", "WanImageToVideo", "ModelSamplingSD3", "KSamplerAdvanced", "VAEDecode", "VAEDecodeTiled", "ImageScale", "ImageUpscaleWithModel", "UpscaleModelLoader"]);
  for (const decode of ["plain", "tiled"]) {
    const g = buildWan22I2V({ ...ARGS, placement: "multicard", decode, upscaleModel: "u.pth", upscaleWidth: 1920, upscaleHeight: 1080 });
    for (const c of graphClasses(g)) if (!core.has(c)) assert.ok(NODE_PACKS[c], `${c} has no pack hint`);
  }
});

test("multicard refuses what was not measured, and a bad placement, with the reason", () => {
  assert.throws(() => buildWan22I2V({ ...ARGS, placement: "everywhere" }), /placement must be single\|multicard/);
  assert.throws(() => buildWan22I2V({ ...ARGS, placement: "multicard", fast: true }), /native recipe only/);
  // a Q8_0 GGUF expert is built: the GGUF flavour of the DisTorch2 loader, the same compute card and allocation, no weight_dtype
  const q8 = buildWan22I2V({ ...ARGS, highUnet: "Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf", lowUnet: "Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf", placement: "multicard", residentGb: 2 });
  for (const [id, unet] of [["7", "Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf"], ["9", "Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf"]]) {
    assert.deepEqual(q8[id], { class_type: "UnetLoaderGGUFDisTorch2MultiGPU", inputs: { unet_name: unet, compute_device: "cuda:1", virtual_vram_gb: 0, donor_device: "cpu", expert_mode_allocations: "cuda:1,2gb;cuda:0,*", eject_models: false } });
  }
  // the single placement still takes both
  assert.doesNotThrow(() => buildWan22I2V({ ...ARGS, fast: true }));
});

test("multicard ignores --wan-loader with one line when it would have chosen another loader, and says nothing otherwise", () => {
  const native = lines();
  const g = buildWan22I2V({ ...ARGS, placement: "multicard", loader: "native", warn: native.warn });
  assert.equal(g["7"].class_type, "UNETLoaderDisTorch2MultiGPU", "the placement IS the loader");
  assert.equal(native.got.length, 1);
  assert.match(native.got[0], /multicard.*DisTorch2.*native/);
  for (const loader of ["auto", "gguf-distorch"]) {
    const quiet = lines();
    buildWan22I2V({ ...ARGS, placement: "multicard", loader, warn: quiet.warn });
    assert.deepEqual(quiet.got, [], loader);
  }
});

// ---- the flags --------------------------------------------------------------------------------------------

test("--wan-placement: absent is single, a known value passes, anything else is refused (never guessed)", () => {
  assert.equal(M.wanPlacementMode({}), "single");
  assert.equal(M.wanPlacementMode({ "wan-placement": "single" }), "single");
  assert.equal(M.wanPlacementMode({ "wan-placement": "multicard" }), "multicard");
  assert.throws(() => M.wanPlacementMode({ "wan-placement": "multi-card" }), /single\|multicard/);
  assert.throws(() => M.wanPlacementMode({ "wan-placement": undefined }), /single\|multicard/, "a dangling flag");
});

test("--wan-resident-gb: default 3, 0 to 64 accepted, anything else refused", () => {
  assert.equal(M.wanResidentGb({}), 3);
  assert.equal(M.wanResidentGb({ "wan-resident-gb": "4.5" }), 4.5);
  assert.equal(M.wanResidentGb({ "wan-resident-gb": "0" }), 0);
  for (const bad of ["-1", "65", "abc", "", undefined, "NaN"]) assert.throws(() => M.wanResidentGb({ "wan-resident-gb": bad }), /--wan-resident-gb/, String(bad));
});

// ---- the card table and the choice of cards ---------------------------------------------------------------

test("parseCardTable reads the driver's csv: a display is display_active Enabled OR display_attached Yes; an unknown is not a yes", () => {
  assert.equal(CARDS.length, 3);
  assert.deepEqual(CARDS.map((c) => [c.index, c.uuid, c.totalMiB, c.freeMiB, c.display]), [
    [0, U0, 16311, 16000, false], [1, U1, 16303, 13800, true], [2, U2, 16311, 16050, false],
  ]);
  const lit = M.parseCardTable(`0, ${U0}, X, 100, 50, Enabled, No`);
  assert.equal(lit[0].display, true, "active alone");
  const unknown = M.parseCardTable(`0, ${U0}, X, 100, 50, [N/A], [Not Supported]\n1, ${U1}, X, 100, 50`);
  assert.deepEqual(unknown.map((c) => c.display), [false, false]);
  assert.deepEqual(unknown.map((c) => c.displayUnknown), [true, true], "and said: a selection must not treat unknown as screenless");
  assert.deepEqual(CARDS.map((c) => c.displayUnknown), [false, false, false]);
  assert.deepEqual(M.parseCardTable(`0, ${U0}, X, [N/A], 50, Disabled, No`), [], "a row without a size is no card");
  assert.deepEqual(M.parseCardTable("garbage\n\nindex, uuid, name"), [], "headers and noise are skipped");
});

test("queryCards asks for display_attached and falls back to display_active alone on a driver that refuses it", () => {
  const asked = [];
  const spawn = (cmd, args) => {
    asked.push(args[0]);
    return /display_attached/.test(args[0]) ? { status: 2, stdout: "" } : { status: 0, stdout: `0, ${U0}, X, 100, 50, Enabled` };
  };
  const cards = M.queryCards(spawn);
  assert.equal(asked.length, 3, "the full query, once more (a transient failure is not a driver that lacks the column), then the fallback");
  assert.equal(cards[0].display, true);
  assert.throws(() => M.queryCards(() => ({ status: 1, stdout: "" })), /nvidia-smi gave no card table/);
  assert.throws(() => M.queryCards(() => ({ status: 0, stdout: "no cards here" })), /listed no card/);
});

test("wanCardTokens: --wan-cards, then the lease, then a UUID CUDA_VISIBLE_DEVICES; nothing says nothing", () => {
  const none = () => [];
  assert.deepEqual(M.wanCardTokens({ flags: { "wan-cards": "2, 0" }, env: { GPU_LEASE_DEVICES: "gpu-x" }, leaseDevicesFn: () => ["gpu-x"] }), { tokens: ["2", "0"], source: "--wan-cards" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: {}, leaseDevicesFn: () => ["gpu-cccc3333", "gpu-aaaa1111"] }), { tokens: ["gpu-cccc3333", "gpu-aaaa1111"], source: "the GPU lease" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: { CUDA_VISIBLE_DEVICES: `${U2},${U0}` }, leaseDevicesFn: none }), { tokens: [U2, U0], source: "CUDA_VISIBLE_DEVICES" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: { CUDA_VISIBLE_DEVICES: "0,2" }, leaseDevicesFn: none }), { tokens: [], source: "" }, "bare indices are no evidence: their order is not the driver's");
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: {}, leaseDevicesFn: none }), { tokens: [], source: "" });
});

test("resolveWanCards: indices, UUIDs, UUID prefixes and lease ids all name the card; of two equal cards the one with the higher UUID computes", () => {
  const ids = (tokens) => { const r = M.resolveWanCards({ tokens, source: "t", cards: CARDS }); return [r.storage.index, r.compute.index]; };
  assert.ok(U2.toLowerCase() < U0.toLowerCase(), "the fixture: idx 2's id sorts before idx 0's, as on the machine the benchmark ran on");
  assert.deepEqual(ids(["0", "2"]), [2, 0], "equal cards take their roles from their UUIDs, not from the order they were named in");
  assert.deepEqual(ids(["2", "0"]), [2, 0]);
  assert.deepEqual(ids([U2, U0]), [2, 0]);
  assert.deepEqual(ids(["gpu-aaaa1111-dddd-eeee-ffff-000000000000", "gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee"]), [2, 0], "the lease's own spelling");
  assert.deepEqual(ids(["aaaa1111", "GPU-CCCC3333"]), [2, 0], "a prefix, either case");
  assert.deepEqual(ids(["0", U0, "2"]), [2, 0], "a card named twice counts once");
});

// F06 of the review of 2026-10-11. The lease sorts its ids (internal/gpulease NormalizeDevices), so a pair taken from the lease always arrives lower id first
// and the benchmark's film run had the lower id on cuda:0 (storage). The same pair named with --wan-cards 0,2 arrived in the order the caller typed it and
// got the opposite roles, a split nobody measured. Equal cards now take their roles from their ids, whoever names them and in whatever order.
test("two cards of one size take their roles from their UUIDs: the lower id stores (cuda:0) and the higher computes (cuda:1), however they are named", () => {
  const roles = (tokens, source = "--wan-cards") => { const r = M.resolveWanCards({ tokens, source, cards: CARDS }); return [r.storage.index, r.compute.index]; };
  for (const tokens of [["0", "2"], ["2", "0"], [U0, U2], [U2, U0], ["0", U2.toLowerCase()], ["gpu-aaaa1111", "gpu-cccc3333"], ["gpu-cccc3333", "gpu-aaaa1111"]]) {
    assert.deepEqual(roles(tokens), [2, 0], tokens.join());
  }
  // the same pair through the whole plan: --wan-cards in either order launches ComfyUI with the same pin, storage first
  const viaCards = (cards) => planWith({ "wan-cards": cards }, { env: {} }).launch.cudaVisibleDevices;
  assert.equal(viaCards("0,2"), `${U2},${U0}`);
  assert.equal(viaCards("2,0"), `${U2},${U0}`);
  assert.equal(planWith().launch.cudaVisibleDevices, `${U2},${U0}`, "and it is the pin the lease's own order gives");
});

test("resolveWanCards: the larger card computes whatever the order", () => {
  const big = [{ index: 0, uuid: U0, name: "S", totalMiB: 16000, freeMiB: 1, display: false }, { index: 2, uuid: U2, name: "B", totalMiB: 24000, freeMiB: 1, display: false }];
  for (const tokens of [["0", "2"], ["2", "0"]]) {
    const r = M.resolveWanCards({ tokens, source: "t", cards: big });
    assert.equal(r.compute.index, 2, tokens.join());
    assert.equal(r.storage.index, 0);
  }
});

test("resolveWanCards refuses: no cards, one, three, an unknown token, and the display card", () => {
  const go = (tokens) => () => M.resolveWanCards({ tokens, source: "the GPU lease", cards: CARDS });
  assert.throws(go([]), /needs the two cards it runs on.*gpu reserve --devices.*--wan-cards/);
  assert.throws(go(["0"]), /exactly two cards, and the GPU lease names 1/);
  assert.throws(go(["0", "1", "2"]), /exactly two cards, and the GPU lease names 3/);
  assert.throws(go(["0", "7"]), /"7".*no card in the driver's table/);
  assert.throws(go(["0", "deadbeef"]), /"deadbeef"/);
  assert.throws(go(["0", "1"]), /idx 1 \(Test GPU B\) drives a display.*never uses the display card/);
  assert.throws(go([U1, U2]), /drives a display/);
  assert.throws(go(["0", "aaaa"]), /no card in the driver's table/, "a prefix under 8 characters names nothing");
});

// ---- the launch -------------------------------------------------------------------------------------------

test("packDirs finds the packs by name whatever the case, and refuses a missing one", () => {
  const listing = (names) => () => names;
  assert.deepEqual(M.packDirs("/c", listing(["x", "ComfyUI-MultiGPU", "comfyui-videohelpersuite"])), ["ComfyUI-MultiGPU", "comfyui-videohelpersuite"]);
  assert.throws(() => M.packDirs("/c", listing(["ComfyUI-VideoHelperSuite"])), /no comfyui-multigpu pack.*DisTorch2/);
  assert.throws(() => M.packDirs("/c", listing(["ComfyUI-MultiGPU"])), /no comfyui-videohelpersuite pack.*mp4/);
  assert.throws(() => M.packDirs("/nowhere/at/all"), /no comfyui-multigpu pack/, "an unreadable custom_nodes is no pack");
});

test("LTX-2.5 under multicard: the launch lists the compute card first and the DiT computes on cuda:0 (an int8 DiT faults elsewhere, ComfyUI-MultiGPU #220)", () => {
  const opts = { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU"] };
  const launch = M.multicardLaunch(pair(), { ...opts, order: M.CARD_ORDER_COMPUTE_FIRST });
  assert.equal(launch.cudaVisibleDevices, `${U0},${U2}`, "compute first: it becomes cuda:0");
  assert.deepEqual(launch.cards, [U0, U2]);
  assert.equal(launch.key, "mcc-cccc3333-aaaa1111");
  assert.notEqual(launch.key, M.multicardLaunch(pair(), opts).key, "a kept storage-first ComfyUI is never taken for this one");
  const g = buildLtx25I2V({ imagePath: "s.png", prompt: "p", multicard: { residentGb: 9 } });
  assert.equal(g["5"].inputs.compute_device, "cuda:0");
  assert.equal(g["5"].inputs.expert_mode_allocations, "cuda:0,9gb;cuda:1,*");
});

test("multicardLaunch: the two uuids storage first, the placement's flags, the reader switched on, an instance of its own", () => {
  const launch = M.multicardLaunch(pair(), { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] });
  assert.equal(launch.cudaVisibleDevices, `${U2},${U0}`, "storage first: it becomes cuda:0");
  assert.deepEqual(launch.cards, [U2, U0]);
  assert.equal(launch.key, "mc-aaaa1111-cccc3333");
  assert.match(launch.key, /^[A-Za-z0-9_-]{1,48}$/, "a valid instance key");
  assert.deepEqual(launch.flags, ["--gpu-only", "--disable-pinned-memory", "--extra-model-paths-config", M.EXT_YAML,
    "--disable-all-custom-nodes", "--whitelist-custom-nodes", "ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite", "offload_multicard"]);
  assert.deepEqual(launch.env, { CUDA_DEVICE_ORDER: "PCI_BUS_ID", OFFLOAD_RO_LOAD: "1" });
  assert.throws(() => M.multicardLaunch(pair(), { comfyDir: "" }), /COMFY_DIR is not set/);
  assert.throws(() => M.multicardLaunch(pair(), { comfyDir: "/c", yaml: "/no/such/file.yaml", packDirsFn: () => [] }), /extension file is missing/);
});

test("the files the placement ships exist where the launch points: a yaml that adds the node folder, and the folder", () => {
  assert.ok(existsSync(M.EXT_YAML), M.EXT_YAML);
  assert.ok(existsSync(join(M.EXT_DIR, "nodes", M.EXT_NODE_PACK, "__init__.py")), "the pack ComfyUI is told to load");
});

test("multicardEnv drops the pins and the dynamic-VRAM setting that contradict two cards under --gpu-only, and names the instance", () => {
  const launch = M.multicardLaunch(pair(), { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] });
  const filmEnv = { COMFY_DIR: "/c", COMFY_CUDA_DEVICE: "2", COMFY_DYNAMIC_VRAM: "on", COMFY_CARD_UUID: U0, COMFY_API: "http://127.0.0.1:8189", COMFY_EXTRA_ARGS: "", CUDA_VISIBLE_DEVICES: `${U0},${U2}`, KEEP: "me" };
  const env = M.multicardEnv(filmEnv, launch);
  assert.equal(env.COMFY_INSTANCE, launch.key);
  assert.equal(env.COMFY_CARD_UUID, "");
  assert.equal(env.COMFY_CUDA_DEVICE, "");
  assert.equal(env.COMFY_DYNAMIC_VRAM, "");
  assert.ok(!("CUDA_VISIBLE_DEVICES" in env), "ensureComfy sets it from the launch");
  assert.equal(env.COMFY_API, "http://127.0.0.1:8189", "the caller's port is kept");
  assert.equal(env.KEEP, "me");
  assert.equal(filmEnv.COMFY_DYNAMIC_VRAM, "on", "the caller's own environment is not mutated");
  // and the profile ensureComfy reads from it asks for none of those
  assert.deepEqual(L.resolveLaunchProfile(env), { cudaDevice: "", dynamicVram: "" });
  assert.equal(L.resolveInstance({ env }).key, launch.key);
});

test("checkComfyDevices compares ComfyUI's own device list with the pinned pair: count, then size in order", () => {
  const dev = (mib, type = "cuda") => ({ type, vram_total: mib * 1024 ** 2, name: "cuda:0 Test GPU A : cudaMallocAsync" });
  assert.deepEqual(M.checkComfyDevices([dev(16311), dev(16311)], pair()), []);
  assert.match(M.checkComfyDevices([dev(16311)], pair())[0], /sees 1 CUDA device.*not the 2/);
  assert.match(M.checkComfyDevices([dev(16311), dev(16311), dev(16311)], pair())[0], /sees 3/);
  assert.match(M.checkComfyDevices([dev(1, "cpu"), dev(16311), dev(16311)], pair()).join(), /^$|^(?!.*cpu)/, "a cpu entry is not a card");
  assert.match(M.checkComfyDevices([dev(8000), dev(16311)], pair())[0], /cuda:0 \(storage\) should be idx 2 \(15\.9 GiB\) but ComfyUI reports 7\.8 GiB/);
  assert.equal(M.checkComfyDevices(undefined, pair()).length, 1);
  const big = M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards: [{ ...CARDS[0], totalMiB: 16000 }, { ...CARDS[2], totalMiB: 24000 }] });
  assert.deepEqual(M.checkComfyDevices([dev(16000), dev(24000)], big), [], "storage (smaller) first, compute second");
  assert.equal(M.checkComfyDevices([dev(24000), dev(16000)], big).length, 2, "the order is checked, not only the pair");
});

// F15. All three cards of the machine are 15.9 GiB, and the display card (a different model) is within the 5 percent the size check allows, so a pin that
// landed on it passed the size check. ComfyUI names each device as "cuda:N <torch device name> : <allocator>"; the nvidia-smi name must be inside it.
test("checkComfyDevices also compares the device's name with the card's, so another model of the same size is caught", () => {
  const cards = [{ ...CARDS[0], name: "Card Y", totalMiB: 16311 }, { ...CARDS[2], name: "Card X", totalMiB: 16311 }];
  const named = M.resolveWanCards({ tokens: [U2, U0], source: "t", cards });
  const dev = (name, mib = 16311) => ({ type: "cuda", vram_total: mib * 1024 ** 2, name });
  assert.deepEqual(M.checkComfyDevices([dev("cuda:0 Card X : cudaMallocAsync"), dev("cuda:1 Card Y : cudaMallocAsync")], named), []);
  assert.deepEqual(M.checkComfyDevices([dev("cuda:0 CARD  x : native"), dev("cuda:1 card y : native")], named), [], "case and spacing are not a difference");
  // the display card (another model, 16303 MiB, inside the size tolerance) where the compute card should be
  const wrong = M.checkComfyDevices([dev("cuda:0 Card X : cudaMallocAsync"), dev("cuda:1 Card Z : cudaMallocAsync", 16303)], named);
  assert.equal(wrong.length, 1);
  assert.match(wrong[0], /cuda:1 \(compute\) should be idx 0 \(Card Y\) but ComfyUI reports cuda:1 Card Z : cudaMallocAsync/);
  // swapped, the two names say so even though the sizes agree
  assert.equal(M.checkComfyDevices([dev("cuda:0 Card Y : cudaMallocAsync"), dev("cuda:1 Card X : cudaMallocAsync")], named).length, 2);
  // a device that reports no name, or a card the driver gave none, is not a mismatch (the size check still stands)
  assert.deepEqual(M.checkComfyDevices([dev(""), dev("")], named), []);
  const unnamed = M.resolveWanCards({ tokens: [U2, U0], source: "t", cards: cards.map((c) => ({ ...c, name: "" })) });
  assert.deepEqual(M.checkComfyDevices([dev("cuda:0 Card Q"), dev("cuda:1 Card R")], unnamed), []);
});

// F16. ComfyUI's main.py starts dynamic VRAM when --enable-dynamic-vram is given, whatever else the argv says (cli_args.enables_dynamic_vram returns True
// before it looks at --gpu-only). COMFY_EXTRA_ARGS lands after the placement's flags, so an extra arg could switch streaming back on under a lease that
// declared no host RAM, and the reuse check only looked for --gpu-only.
test("a multicard launch drops --enable-dynamic-vram from the operator's extra args, said, and keeps the rest", () => {
  const launch = fakeLaunch();
  const instance = L.resolveInstance({ api: "http://127.0.0.1:18852", env: { COMFY_INSTANCE: launch.key } });
  const warned = [];
  const flags = L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1 --enable-dynamic-vram --y", launch, profile: { cudaDevice: "", dynamicVram: "", multicard: { cards: launch.cards } }, instance, comfyDir: "/c", warn: (l) => warned.push(l) });
  assert.ok(!flags.includes("--enable-dynamic-vram"));
  assert.ok(flags.includes("--gpu-only"));
  assert.deepEqual(flags.slice(-3), ["--x", "1", "--y"]);
  assert.equal(warned.filter((l) => /--enable-dynamic-vram.*dropped/.test(l)).length, 1, warned.join("\n"));
  const none = [];
  L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1", launch, profile: { cudaDevice: "", dynamicVram: "", multicard: { cards: launch.cards } }, instance, comfyDir: "/c", warn: (l) => none.push(l) });
  assert.deepEqual(none, [], "nothing to say when the extra args are clean");
  // outside the placement the operator's flag is theirs
  assert.ok(L.launchFlags({ reserveVram: "1.0", extraArgs: "--enable-dynamic-vram" }).includes("--enable-dynamic-vram"));
});

test("a running ComfyUI is not the multicard placement's when its argv turns dynamic VRAM on", () => {
  const mc = { cudaDevice: "", dynamicVram: "", multicard: { cards: ["a", "b"] } };
  assert.equal(L.profileMismatch(["main.py", "--gpu-only"], mc), "");
  assert.match(L.profileMismatch(["main.py", "--gpu-only", "--enable-dynamic-vram"], mc), /dynamic VRAM on.*multicard placement needs off/);
  assert.match(L.profileMismatch(["main.py", "--enable-dynamic-vram", "--gpu-only"], mc), /dynamic VRAM on/, "the order of the flags does not matter");
});

test("planMulticard: null for single without reading any card; a plan for multicard; refusals before anything starts", () => {
  const boom = () => { throw new Error("must not be read"); };
  assert.equal(M.planMulticard({ flags: {}, comfyDir: "/c", isWanGraph: true, cards: boom }), null);
  assert.equal(M.planMulticard({ flags: { "wan-placement": "single" }, comfyDir: "/c", isWanGraph: false, cards: boom }), null, "single on any graph is today's path");
  const said = [];
  const plan = M.planMulticard({
    flags: { "wan-placement": "multicard", "wan-resident-gb": "4", ...EXPERTS }, env: { ...LEASE, COMFY_DYNAMIC_VRAM: "on" },
    comfyDir: "/c", isWanGraph: true, cards: () => CARDS, launchFn: (p) => M.multicardLaunch(p, { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] }), say: (l) => said.push(l),
  });
  assert.equal(plan.residentGb, 4);
  assert.equal(plan.pair.storage.index, 2);
  assert.equal(plan.pair.compute.index, 0);
  assert.equal(plan.env.COMFY_DYNAMIC_VRAM, "");
  assert.equal(said.length, 1);
  assert.match(said[0], /^wan-placement: multicard on the GPU lease: storage cuda:0 = idx 2 .*; compute cuda:1 = idx 0 .*; 4 GiB of each expert resident/);
  const refuse = (over, re) => assert.throws(() => M.planMulticard({ flags: { "wan-placement": "multicard", ...EXPERTS }, env: {}, comfyDir: "/c", isWanGraph: true, cards: () => CARDS, ...over }), re);
  refuse({ isWanGraph: false, flags: { "wan-placement": "multicard", model: "ltx25" } }, /places the Wan 2\.2 graph.*--model ltx25/);
  refuse({ isWanGraph: false, flags: { "wan-placement": "multicard", graph: "g.json" } }, /--graph file/);
  refuse({}, /needs the two cards it runs on/);
  refuse({ env: { GPU_LEASE_DEVICES: `${U0},${U1}` } }, /drives a display/);
  refuse({ env: LEASE, flags: { "wan-placement": "multicard", "wan-resident-gb": "x", ...EXPERTS } }, /--wan-resident-gb/);
});

test("decodeFellBack finds ComfyUI's out-of-memory retry in a console stretch, and only that", () => {
  assert.equal(M.decodeFellBack("Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n"), true);
  assert.equal(M.decodeFellBack("Prompt executed in 5000 seconds\n"), false);
  assert.equal(M.decodeFellBack(""), false);
  assert.equal(M.decodeFellBack(undefined), false);
});

// ---- the ComfyUI lifecycle --------------------------------------------------------------------------------

const BASE = ["--disable-smart-memory", "--cache-none", "--reserve-vram", "1.0"];
const fakeLaunch = () => M.multicardLaunch(pair(), { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] });

test("launchFlags without a launch is the argv it always was, and null means without", () => {
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1" }), [...BASE, "--x", "1"]);
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1", launch: null }), [...BASE, "--x", "1"]);
  assert.deepEqual(L.launchFlags({ reserveVram: "1.0", extraArgs: "--x 1", launch: {} }), [...BASE, "--x", "1"], "a launch with no flags adds none");
});

test("launchFlags with a multicard launch: the base flags, the placement's, the instance's own, the operator's extras last", () => {
  const launch = fakeLaunch();
  const instance = L.resolveInstance({ api: "http://127.0.0.1:18852", env: { COMFY_INSTANCE: launch.key } });
  const warned = [];
  const flags = L.launchFlags({ reserveVram: "1.0", extraArgs: "--cuda-device 1 --x 1", launch, profile: { cudaDevice: "", dynamicVram: "", multicard: { cards: launch.cards } }, instance, comfyDir: "/c", warn: (l) => warned.push(l) });
  assert.deepEqual(flags, [...BASE, ...launch.flags, "--port", "18852", "--output-directory", join("/c", "instances", launch.key, "output"), "--temp-directory", join("/c", "instances", launch.key), "--database-url", "sqlite:///" + join("/c", "instances", launch.key, "comfyui.db"), "--x", "1"]);
  assert.ok(!flags.includes("--cuda-device"), "a --cuda-device would make main.py rewrite the uuid pin");
  assert.equal(warned.length, 1);
  assert.match(warned[0], /--cuda-device\/--default-device.*dropped/);
  assert.equal(flags.filter((f) => f === "--disable-pinned-memory").length, 1);
});

function lifecycleHarness(t, { up = async () => false } = {}) {
  const dir = scratchDir(t, "wan-mc-");
  const launch = fakeLaunch();
  const env = M.multicardEnv({ COMFY_CUDA_DEVICE: "2", COMFY_DYNAMIC_VRAM: "on", COMFY_CARD_UUID: U0, GPU_LEASE_EPOCH: "9" }, launch);
  const spawns = []; const logs = []; let started = false;
  return {
    dir, launch, env, spawns, logs,
    opts: {
      api: "http://127.0.0.1:18852", comfyDir: dir, py: "py", pollMs: 1, env, launch, log: (l) => logs.push(l),
      envFor: () => { throw new Error("a multicard launch sets its own child env"); },
      comfyUp: async (a) => (started ? true : up(a)), portFree: async () => true, mkdirs: () => {},
      killPid: () => { throw new Error("never kill here"); },
      spawn: (py, args, o) => { started = true; spawns.push({ args, env: o.env }); return { pid: 7001, kill() {} }; },
    },
  };
}

test("ensureComfy with a multicard launch: the placement's argv, the two uuids storage first in the child env, the reader on, a marker with the pair", async (t) => {
  const h = lifecycleHarness(t);
  const child = await L.ensureComfy(h.opts);
  assert.equal(child.pid, 7001);
  const { args, env } = h.spawns[0];
  assert.deepEqual(args, ["main.py", ...BASE, ...h.launch.flags, "--port", "18852", "--output-directory", join(h.dir, "instances", h.launch.key, "output"), "--temp-directory", join(h.dir, "instances", h.launch.key), "--database-url", "sqlite:///" + join(h.dir, "instances", h.launch.key, "comfyui.db")]);
  assert.equal(env.CUDA_VISIBLE_DEVICES, `${U2},${U0}`);
  assert.equal(env.CUDA_DEVICE_ORDER, "PCI_BUS_ID");
  assert.equal(env.OFFLOAD_RO_LOAD, "1");
  assert.equal(env.COMFY_CARD_UUID, "", "the caller's single-card pin was dropped before the launch");
  assert.equal(args.filter((a) => a === "--disable-pinned-memory").length, 1, "once, not once more by the multi-GPU rule");
  assert.ok(!h.logs.some((l) => /not bound to a card/.test(l)), "the pair IS the pin: no unpinned-instance warning");
  const rec = O.readLaunchOwner(h.dir, h.launch.key);
  assert.deepEqual(rec.profile.multicard, { cards: [U2, U0] });
  assert.equal(rec.leaseEpoch, 9);
  assert.deepEqual(rec.args, args, "the fingerprint is the exact argv");
});

test("ensureComfy without a launch leaves the child env to envFor, exactly as before", async (t) => {
  const h = lifecycleHarness(t);
  const sentinel = { SENTINEL: "1" };
  let calls = 0;
  const opts = { ...h.opts, launch: null, env: { GPU_LEASE_EPOCH: "9" }, api: undefined, envFor: () => { calls++; return sentinel; } };
  await L.ensureComfy(opts);
  assert.equal(calls, 1);
  assert.equal(h.spawns[0].env, sentinel);
  assert.ok(!h.spawns[0].args.includes("--gpu-only"));
});

const runningArgv = (h) => ["main.py", ...BASE, ...h.launch.flags, "--port", "18852", "--output-directory", join(h.dir, "instances", h.launch.key, "output"), "--temp-directory", join(h.dir, "instances", h.launch.key)];
const profileOf = (h) => ({ cudaDevice: "", dynamicVram: "", multicard: { cards: h.launch.cards } });
const verdict = (h, marker, argv) => L.reuseVerdict({
  api: "http://127.0.0.1:18852", comfyDir: h.dir, profile: profileOf(h), key: h.launch.key, port: 18852,
  systemArgv: async () => argv, readLaunch: () => marker, alive: () => true,
});

test("reuseVerdict: a running ComfyUI is reused only when the launch marker proves it is ours, on this pair, in this order", async (t) => {
  const h = lifecycleHarness(t);
  const argv = runningArgv(h);
  const marker = (cards, extra = {}) => ({ pid: 5, args: argv, key: h.launch.key, port: 18852, profile: { multicard: { cards } }, ...extra });
  assert.deepEqual(await verdict(h, marker(h.launch.cards), argv), { reuse: true });
  // the pair swapped: cuda:0 would be the other card
  const swapped = await verdict(h, marker([U0, U2]), argv);
  assert.equal(swapped.restart, true, "ours, orphaned, wrong pair: replaced");
  assert.match(swapped.reason, /launched on .* needs .*storage card first/);
  // one the harness did not start (no marker)
  const foreign = await verdict(h, null, argv);
  assert.equal(foreign.reuse, false);
  assert.match(foreign.reason, /no launch marker of this harness proves it was started on/);
  // without --gpu-only it is not this placement whatever the marker says
  const noGpuOnly = await verdict(h, marker(h.launch.cards), argv.filter((a) => a !== "--gpu-only"));
  assert.equal(noGpuOnly.reuse, false);
  assert.match(noGpuOnly.reason, /without --gpu-only/);
  // still in use by another live process of ours
  const noGpu = argv.filter((a) => a !== "--gpu-only");
  const busy = await verdict(h, marker(h.launch.cards, { ownerPid: process.pid + 1, args: noGpu }), noGpu);
  assert.equal(busy.reuse, false);
  assert.match(busy.reason, /still in use by process/);
});

test("withGpuSlot hands ensureComfy the launch and its environment only when it has one", async () => {
  const launch = fakeLaunch();
  const env = M.multicardEnv({}, launch);
  const seen = [];
  const slot = (extra) => withGpuSlot({
    noLock: true, comfyManaged: true, api: "http://127.0.0.1:18852", instanceEnv: env, freeLlamaSwap: async () => {}, freeComfy: async () => true,
    ensureComfy: async (o) => { seen.push(o); return null; }, ...extra,
  }, async () => "ran");
  assert.equal(await slot({ launch }), "ran");
  assert.equal(seen[0].launch, launch);
  assert.equal(seen[0].env, env, "the environment the instance was resolved from");
  assert.equal(seen[0].api, "http://127.0.0.1:18852");
  assert.equal(await slot({}), "ran");
  assert.ok(!("launch" in seen[1]) && !("env" in seen[1]), "without a launch ensureComfy gets the options it always got");
});

// ---- the runner -------------------------------------------------------------------------------------------

test("buildGraphFromArgs builds the multicard graph only when handed a plan, and the single graph otherwise", () => {
  const flags = { "high-unet": "high_fp8.safetensors", "low-unet": "low_fp8.safetensors", "wan-decode": "plain", width: "720", height: "1280", frames: "81", seed: "7" };
  const stage = () => "s.png";
  const single = buildGraphFromArgs(["o.mp4", "still.png", "p"], flags, { stage }).graph;
  assert.equal(single["7"].class_type, "UNETLoader", "native loader for a safetensors expert, as today");
  assert.ok(!single["21"]);
  const multi = buildGraphFromArgs(["o.mp4", "still.png", "p"], flags, { stage, multicard: { residentGb: 4 } }).graph;
  assert.equal(multi["7"].class_type, "UNETLoaderDisTorch2MultiGPU");
  assert.equal(multi["7"].inputs.expert_mode_allocations, "cuda:1,4gb;cuda:0,*");
  assert.equal(multi["21"].class_type, "OffloadFreeForDecode");
  assert.equal(multi["11"].inputs.steps, 20);
  assert.equal(multi["11"].inputs.cfg, 3.5);
  assert.equal(multi["6"].inputs.width, 720);
  assert.equal(multi["6"].inputs.length, 81);
});

test("verifyMulticardDevices stops the run, before anything is submitted, when ComfyUI's cards are not the pinned pair", async () => {
  const dev = (mib) => ({ type: "cuda", vram_total: mib * 1024 ** 2, name: "cuda:0 Test GPU A : cudaMallocAsync" });
  const reply = (devices) => async () => ({ ok: true, json: async () => ({ devices }) });
  const plan = { pair: pair() };
  await verifyMulticardDevices("http://x", plan, { fetchImpl: reply([dev(16311), dev(16311)]) });
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: reply([dev(16311)]) }), /WAN-PLACEMENT-MISMATCH: ComfyUI sees 1 CUDA device.*Nothing was submitted/);
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: async () => ({ ok: false, status: 500 }) }), /WAN-PLACEMENT-MISMATCH: GET \/system_stats gave no device list \(HTTP 500\)/);
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: async () => { throw new Error("refused"); } }), /gave no device list \(refused\)/);
});

// ---- the review round (2026-10-11): what a wrong spelling, a wrong pair or a missing pack must not do silently -----------

test("an '=' spelling of the placement's own flags is refused: the runner's parser does not split it, so it would run single, undeclared", () => {
  const boom = () => { throw new Error("must not be read"); };
  const flags = (k) => ({ [k]: "--wan-decode" });
  assert.throws(() => M.planMulticard({ flags: flags("wan-placement=multicard"), comfyDir: "/c", isWanGraph: true, cards: boom }), /--wan-placement=multicard is spelled with '='.*write --wan-placement multicard/);
  assert.throws(() => M.planMulticard({ flags: flags("wan-resident-gb=4"), comfyDir: "/c", isWanGraph: true, cards: boom }), /--wan-resident-gb=4 is spelled with '='.*write --wan-resident-gb 4/);
  assert.throws(() => M.planMulticard({ flags: flags("wan-cards=0,2"), comfyDir: "/c", isWanGraph: true, cards: boom }), /--wan-cards=0,2 is spelled with '='.*write --wan-cards 0,2/);
  // a flag spelled with '=' takes the NEXT argument as its value: when that is one of ours, ours is lost and the call would run single
  for (const own of ["wan-placement", "wan-resident-gb", "wan-cards"]) {
    assert.throws(() => M.planMulticard({ flags: { "seed=7": `--${own}` }, comfyDir: "/c", isWanGraph: true, cards: boom }), new RegExp(`--seed=7 took --${own} as its value.*write each flag as --name value`), own);
  }
  assert.equal(M.planMulticard({ flags: { "seed=7": "x" }, comfyDir: "/c", isWanGraph: true, cards: boom }), null, "swallowing something that is not ours is the single path's old behaviour");
  // no other '=' key is this placement's business: the single path is what it was, flag by flag
  assert.equal(M.planMulticard({ flags: { "high-unet=x.safetensors": "y" }, comfyDir: "/c", isWanGraph: true, cards: boom }), null);
  assert.equal(M.planMulticard({ flags: { "wan-decode=plain": "x" }, comfyDir: "/c", isWanGraph: true, cards: boom }), null);
  // (internal/hostneed splits '=' and pins `--wan-placement=multicard` as the multicard call it sizes; that is why this side refuses the spelling
  // instead of running it as single: the lease and the run can never disagree about a call that starts.)
});

test("verifyMulticardPack stops the run when the harness's node pack did not load, under any decode, and when it cannot tell", async () => {
  const plan = { launch: { key: "mc-aaaa1111-cccc3333" } };
  const reply = (body) => async () => ({ ok: true, status: 200, json: async () => body });
  await verifyMulticardPack("http://x", plan, { fetchImpl: reply({ OffloadFreeForDecode: { input: {} } }) });
  await assert.rejects(verifyMulticardPack("http://x", plan, { fetchImpl: reply({}) }),
    /WAN-PLACEMENT-PACK: ComfyUI at http:\/\/x has no OffloadFreeForDecode.*failed to import.*offload-comfyui-mc-aaaa1111-cccc3333\.log.*Nothing was submitted/s);
  await assert.rejects(verifyMulticardPack("http://x", plan, { fetchImpl: async () => ({ ok: false, status: 500 }) }), /WAN-PLACEMENT-PACK: .*cannot tell.*HTTP 500.*Nothing was submitted/s);
  await assert.rejects(verifyMulticardPack("http://x", plan, { fetchImpl: async () => { throw new Error("refused"); } }), /cannot tell.*refused/s);
});

// (the order of the device check, the pack check and the graph build in generate() is pinned by behaviour in wan-multicard-run.test.mjs)

test("the missing-node message for the harness's own pack says it failed to import, not 'install it into custom_nodes'", async () => {
  const { missingNodeMessage, NODE_PACKS } = await import("./comfy-nodes.mjs");
  const own = missingNodeMessage("http://x", ["OffloadFreeForDecode"], "/c");
  assert.match(own, /^MISSING_NODE: ComfyUI at http:\/\/x has no node class OffloadFreeForDecode/);
  assert.match(own, /never installed into custom_nodes.*failed to import.*Nothing was submitted\.$/s);
  assert.doesNotMatch(own, /install the pack into/);
  // a third-party class keeps today's sentence byte for byte
  assert.equal(missingNodeMessage("http://x", ["UNETLoaderDisTorch2MultiGPU"], "/c"),
    `MISSING_NODE: ComfyUI at http://x has no node class UNETLoaderDisTorch2MultiGPU (custom node pack ${NODE_PACKS.UNETLoaderDisTorch2MultiGPU}) — install the pack into /c/custom_nodes and restart ComfyUI (\`local-offload doctor\` lists every route's node classes). Nothing was submitted.`);
  // mixed: the pack sentence still applies to the class that has one
  const mixed = missingNodeMessage("http://x", ["OffloadFreeForDecode", "VAELoaderMultiGPU"], "/c");
  assert.match(mixed, /install the pack into \/c\/custom_nodes/);
});

test("under multicard a decode that is the tiled one is said to seam, once, and plain says nothing", async () => {
  const flags = { ...EXPERTS, width: "720", height: "1280", frames: "81", seed: "7" };
  const run = async (extra, withPlan = true) => {
    const said = [];
    await buildGraphForRun(["o.mp4", "s.png", "p"], { ...flags, ...extra }, "http://x", { stage: () => "s.png", log: (m) => said.push(m), ...(withPlan ? { multicard: { residentGb: 3 } } : {}) });
    return said.filter((m) => /WAN-PLACEMENT-NOTE/.test(m));
  };
  const tiled = await run({});
  assert.equal(tiled.length, 1);
  assert.match(tiled[0], /tiled decode.*can seam where its tiles meet, in time at frames 24, 48 and 72.*--wan-decode plain/s, "the film's 21 latent frames meet three times");
  const short = await run({ frames: "29" });
  assert.equal(short.length, 1);
  assert.doesNotMatch(short[0], /in time at frame/, "a clip of 8 latent frames decodes in one temporal tile");
  assert.equal((await run({ "wan-decode": "tiled" })).length, 1);
  assert.deepEqual(await run({ "wan-decode": "plain" }), []);
  assert.deepEqual(await run({}, false), [], "the single placement's tiled default is what it always was, and says nothing new");
});

// ComfyUI's VAEDecodeTiled (nodes.py) reads temporal_size and temporal_overlap in frames and divides both by the Wan VAE's temporal compression 4 (the
// overlap never under 1 nor over half the tile); tiled_scale_multidim (comfy/utils.py) starts a tile every (tile - overlap) latent frames while the
// start is under (latent frames - overlap), and writes it at pixel frame 4 x start (sd.py: the Wan VAE's upscale_index_formula is 4). With the
// builder's 32 / 8 that is tiles of 8 latent frames that overlap by 2: the film's 81 frames are 21 latent frames, starts 0, 6, 12, 18.
test("tiledSeamFrames reproduces the temporal tiling of VAEDecodeTiled: where one tile meets the next", () => {
  assert.deepEqual(M.tiledSeamFrames(81), [24, 48, 72], "the film");
  assert.deepEqual(M.tiledSeamFrames(49), [24], "13 latent frames: starts 0 and 6");
  assert.deepEqual(M.tiledSeamFrames(33), [24], "9 latent frames are one over a tile");
  assert.deepEqual(M.tiledSeamFrames(29), [], "8 latent frames fit one tile: no temporal seam");
  assert.deepEqual(M.tiledSeamFrames(65), [24, 48]);
  assert.deepEqual(M.tiledSeamFrames(121), [24, 48, 72, 96]);
  assert.deepEqual(M.tiledSeamFrames(81, 64, 8), [56], "the node's own default of 64 / 8: tiles of 16, overlap 2, stride 14");
  assert.deepEqual(M.tiledSeamFrames(81, 8, 8), Array.from({ length: 19 }, (_, i) => 4 * (i + 1)), "a temporal size under twice the overlap halves the overlap: tile 2, overlap 1");
  assert.deepEqual(M.tiledSeamFrames(81, 28, 16), [20, 40, 60], "a tile under twice the overlap halves the overlap first: 16 becomes 8, so tile 7 and overlap 2 (not 3)");
  assert.deepEqual(M.tiledSeamFrames(81, 32, 3), [28, 56], "an overlap under 4 frames is still one latent frame: tile 8, overlap 1");
  assert.equal(M.describeFrames([24, 48, 72]), "frames 24, 48 and 72");
  assert.equal(M.describeFrames([24, 48]), "frames 24 and 48");
  assert.equal(M.describeFrames([24]), "frame 24");
  assert.equal(M.describeFrames([]), "");
});

test("planMulticard refuses --fast and an unnamed expert before anything starts, and takes a GGUF expert with the GGUF pack", (t) => {
  assert.doesNotThrow(() => planWith());
  assert.throws(() => planWith({ fast: true }), /native recipe only.*drop --fast/);
  const nodes = scratchDir(t, "wan-mc-");
  for (const d of ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite", "ComfyUI-GGUF"]) mkdirSync(join(nodes, "custom_nodes", d), { recursive: true });
  const gguf = planWith({ "high-unet": "Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf", "low-unet": "Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf" }, {
    comfyDir: nodes, launchFn: (p, o) => M.multicardLaunch(p, o),
  });
  assert.equal(gguf.launch.gguf, true);
  assert.ok(gguf.launch.flags.includes("ComfyUI-GGUF"), "the GGUF pack is whitelisted");
  assert.equal(planWith().launch.gguf, undefined, "an fp8 launch is the old shape");
  for (const flag of ["high-unet", "low-unet"]) {
    const flags = { "wan-placement": "multicard", ...EXPERTS };
    delete flags[flag];
    assert.throws(() => planWith({}, { flags }), new RegExp(`needs --${flag}.*fp8 safetensors file or a Q8_0 GGUF file.*default experts are Q4_K GGUF`));
    assert.throws(() => planWith({ [flag]: "" }), new RegExp(`needs --${flag}`), "an empty name is no name");
  }
});

test("--wan-cards can confirm the lease's cards or name them when no lease does, and never replaces a lease", () => {
  const held = ["gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee", "gpu-aaaa1111-dddd-eeee-ffff-000000000000"];
  assert.doesNotThrow(() => planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => held }), "the same two cards: confirmed");
  assert.doesNotThrow(() => planWith({ "wan-cards": "2,0" }, { leaseDevicesFn: () => held }), "in the other order");
  assert.throws(() => planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => ["gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee", "gpu-bbbb2222-cccc-dddd-eeee-ffffffffffff"] }),
    /--wan-cards names idx 2, which the GPU lease does not hold.*the lease is the authority/);
  assert.throws(() => planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => ["gpu-bbbb2222", "gpu-dddd4444"] }), /does not hold/);
  // no lease: --wan-cards is the caller's word, but the roles of two equal cards come from their ids, not from the order it was typed in
  const p = planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => [], env: {} });
  assert.deepEqual([p.pair.storage.index, p.pair.compute.index], [2, 0]);
  // a dangling or empty flag names no card and is not skipped for the lease
  for (const bad of [undefined, "", " , "]) assert.throws(() => M.wanCardTokens({ flags: { "wan-cards": bad }, env: {}, leaseDevicesFn: () => held }), /--wan-cards needs/, JSON.stringify(bad));
});

test("a token that names no card says how to name one", () => {
  assert.throws(() => M.resolveWanCards({ tokens: ["0", "7"], source: "--wan-cards", cards: CARDS }), /"7".*no card in the driver's table.*name the card by its nvidia-smi index or its UUID/);
});

test("a card whose display state the driver did not report is refused, never taken for a screenless one", () => {
  const cards = M.parseCardTable([`0, ${U0}, A, 16311, 16000, Disabled, No`, `2, ${U2}, A, 16311, 16000, [N/A], [Not Supported]`].join("\n"));
  assert.throws(() => M.resolveWanCards({ tokens: ["0", "2"], source: "the GPU lease", cards }), /cannot tell whether idx 2 \(A\) drives a display.*display_active \[N\/A\], display_attached \[Not Supported\]/);
  // the six-column fallback (a driver that lacks display_attached) leaves it unknown too: a monitor on a sleeping card says Disabled
  const six = M.parseCardTable(`0, ${U0}, A, 16311, 16000, Disabled\n2, ${U2}, A, 16311, 16000, Disabled`);
  assert.throws(() => M.resolveWanCards({ tokens: ["0", "2"], source: "the GPU lease", cards: six }), /cannot tell whether idx 0/);
  // a known card beside an unknown one that is not chosen is fine
  assert.doesNotThrow(() => M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards: [...CARDS, ...M.parseCardTable(`3, ${U1}, A, 100, 50, [N/A], [N/A]`)] }));
});

test("a row without a numeric size is no card, so a size check can never pass on NaN", () => {
  assert.deepEqual(M.parseCardTable(`0, ${U0}, A, [N/A], 50, Disabled, No\n2, ${U2}, A, 16311, [N/A], Disabled, No`).map((c) => c.index), [2], "a size that is not a number drops the card");
  assert.equal(M.parseCardTable(`2, ${U2}, A, 16311, [N/A], Disabled, No`)[0].freeMiB, 0, "a free column that is not a number is 0");
});

test("--wan-resident-gb above what the compute card can hold beside the sampler's activations at THIS geometry is refused with the numbers", () => {
  // The sampler's activations are the benchmark's reserved figures (RESULTS 3.5) as an upper envelope below the film's size, and the film-size
  // run's 12.2 GiB (internal/hostneed wanActivationsGiB: the compute card's measured 15.18 GiB peak less its 3 GiB resident share) from there up.
  // The compute card keeps WAN_COMPUTE_CONTEXT_GIB (0.5) for the CUDA context beyond that figure. --reserve-vram is a model-load margin, not an
  // activation ceiling: it is the storage card's margin (the storage test below), as in the lease sizing.
  // The default geometry (832x480x49: 20,280 tokens, 6.3 GiB): 15.9 - 6.3 - 0.5 = 9.1 GiB at most
  assert.equal(planWith({ "wan-resident-gb": "9" }).residentGb, 9);
  assert.throws(() => planWith({ "wan-resident-gb": "10" }), /--wan-resident-gb 10 .*compute card \(idx 0, 15\.9 GiB\).*832x480x49.*at most 9\.1/);
  assert.throws(() => planWith({ "wan-resident-gb": "40" }), /--wan-resident-gb 40/);
  // the film's geometry (720x1280x81: 75,600 tokens, 12.2 GiB beyond the resident share: the film-size run's compute-card peak, 15.18 GiB, less its
  // 3 GiB resident): the measured 3 passes, and so does 3.2, the planner's ceiling; above it the compute card has no room left for its peak, 90 s before the hand-over to the second expert
  const film = { width: "720", height: "1280", frames: "81" };
  assert.equal(planWith({ ...film, "wan-resident-gb": "3" }).residentGb, 3, "the measured recipe is never refused by its own planner");
  assert.equal(planWith({ ...film, "wan-resident-gb": "3.2" }).residentGb, 3.2, "15.9 - 12.2 - 0.5 = 3.2 GiB at most");
  assert.throws(() => planWith({ ...film, "wan-resident-gb": "3.3" }), /--wan-resident-gb 3\.3 .*720x1280x81 \(75600 tokens\).*at most 3\.2/);
  assert.throws(() => planWith({ ...film, "wan-resident-gb": "4" }), /--wan-resident-gb 4 .*at most 3\.2/);
  assert.throws(() => planWith({ ...film, "wan-resident-gb": "8" }), /--wan-resident-gb 8 /);
  // a bigger clip than the film's is held at the film's figure (the planner has no measurement above it, and says so: geometryNote)
  assert.equal(planWith({ width: "1280", height: "1280", frames: "121", "wan-resident-gb": "3.2" }).residentGb, 3.2);
  assert.throws(() => planWith({ width: "1280", height: "1280", frames: "121", "wan-resident-gb": "3.3" }), /--wan-resident-gb 3\.3 /);
  // --reserve-vram does not take the compute card's room (its check is the storage card's). The film-size run itself ran at the runner's default reserve of 1.0;
  // that a larger one leaves the compute card's peak where it was is assumed, not measured.
  assert.equal(planWith({ ...film, "reserve-vram": "2", "wan-resident-gb": "3" }).residentGb, 3);
  // a small clip needs little: the estimate has a 2 GiB floor (480x832x17: 7,800 tokens, 2.4 GiB)
  assert.equal(planWith({ width: "480", height: "832", frames: "17", "wan-resident-gb": "12" }).residentGb, 12, "15.9 - 2.4 - 0.5 = 13.0 GiB at most");
  assert.throws(() => planWith({ width: "480", height: "832", frames: "17", "wan-resident-gb": "14" }), /--wan-resident-gb 14 .*at most 13\.0/);
});

test("WAN_FILM_TOKENS is the measured film clip's geometry (720x1280x81), the size above which the planner has no measurement", () => {
  assert.equal(M.WAN_FILM_TOKENS, 75600);
  assert.equal(M.wanGeometry({ width: "720", height: "1280", frames: "81" }).tokens, M.WAN_FILM_TOKENS);
  assert.equal(M.wanGeometry({ width: "1280", height: "720", frames: "81" }).tokens, M.WAN_FILM_TOKENS, "the same tokens in the other orientation");
});

test("a clip bigger than the film's is admitted at the film's figure, and the planner says it has no measurement above the film's size", () => {
  // wanActivationsGib holds the figure at the film-size run's 12.2 GiB above 75,600 tokens. The sampler's activations grow with the tokens, so
  // the resident-share check there is only as good as that figure: the planner does not refuse what it cannot size, it says so before the check,
  // with the number a proportional extrapolation gives, and leaves the share (and the wait) to the caller.
  const said = [];
  const say = (l) => said.push(l);
  const notes = () => said.filter((l) => /^WAN-GEOMETRY-NOTE: /.test(l));
  for (const flags of [{}, { width: "720", height: "1280", frames: "81" }, { width: "1280", height: "720", frames: "81" }, { width: "480", height: "832", frames: "17" }]) {
    planWith({ ...flags, "wan-resident-gb": "3" }, { say });
    assert.deepEqual(notes(), [], JSON.stringify(flags) + " is at or under the film's size: nothing to say");
  }
  const big = { width: "1280", height: "1280", frames: "121", "wan-resident-gb": "3" };
  planWith(big, { say });
  assert.equal(notes().length, 1, said.join("\n"));
  assert.match(notes()[0], /1280x1280x121 \(198400 tokens\) is bigger than the measured film clip \(720x1280x81, 75600 tokens\)/);
  assert.match(notes()[0], /holds the sampler's activations at the film's 12\.2 GiB/);
  assert.match(notes()[0], /in proportion to its tokens .*about 32\.0 GiB/);
  assert.match(notes()[0], /--wan-resident-gb/);
  assert.match(notes()[0], /--wait-sec/);
  // one latent frame more than the film (85 frames: 79,200 tokens) is already past what was measured
  said.length = 0;
  planWith({ width: "720", height: "1280", frames: "85", "wan-resident-gb": "3" }, { say });
  assert.match(notes()[0] || "", /720x1280x85 \(79200 tokens\)/);
  // a refusal still carries the note ahead of it: the reader sees why the figure is soft before the numbers it produced
  said.length = 0;
  assert.throws(() => planWith({ ...big, "wan-resident-gb": "40" }, { say }), /--wan-resident-gb 40 /);
  assert.equal(notes().length, 1, "the note comes before the refusal");
});

test("wanActivationsGib is never under the benchmark's reserved allocator peaks (RESULTS 3.5) and is capped at the film's figure", () => {
  // compute-card allocator peak, reserved GiB, by tokens: the async allocator, the native one (+0.4) and sage attention (+0.9) at 20,280
  for (const [tokens, reserved] of [[7800, 2.19], [14040, 3.66], [20280, 5.31], [20280, 5.67], [20280, 6.16], [32400, 5.50]]) {
    assert.ok(M.wanActivationsGib(tokens) >= reserved, `${tokens} tokens: ${M.wanActivationsGib(tokens)} GiB against a measured ${reserved}`);
  }
  assert.equal(M.wanActivationsGib(75600), M.WAN_FILM_ACTIVATIONS_GIB, "the film's size is the cap");
  assert.equal(M.wanActivationsGib(200000), M.WAN_FILM_ACTIVATIONS_GIB, "and the planner holds it there: nothing above the film's size was measured");
  assert.equal(M.wanActivationsGib(100), 2, "a floor under a tiny clip");
  assert.equal(M.WAN_FILM_ACTIVATIONS_GIB, 12.2, "the film-size run's compute-card peak (15.18 GiB) less its 3 GiB resident share, the figure internal/hostneed declares against");
  assert.equal(M.WAN_COMPUTE_CONTEXT_GIB, 0.5, "the CUDA context kept beyond it: 15.9 - 12.2 - 0.5 = 3.2 GiB, the film-size ceiling");
});

test("the storage card is checked too: the rest of the expert, the VAE and the context must fit it", () => {
  // The expert is 13.4 GiB (internal/hostneed's documented size of a Wan 2.2 fp8 expert); the storage card holds expert - share + 0.8.
  const cardsOf = (a, b) => M.parseCardTable([
    `0, ${U0}, Test GPU A, ${a * 1024}, 16000, Disabled, No`,
    `2, ${U2}, Test GPU A, ${b * 1024}, 16050, Disabled, No`,
  ].join("\n"));
  const planOn = (a, b, flags = {}) => planWith(flags, { cards: () => cardsOf(a, b) });
  // 24 GiB computing, 8 GiB storing: 13.4 - 3 + 0.8 = 11.2 GiB of donor blocks does not fit 8 - 1.0
  assert.throws(() => planOn(24, 8), /storage card \(idx 2, 8\.0 GiB\).*11\.2 GiB.*7\.0 GiB usable/);
  assert.throws(() => planOn(8, 24), /storage card \(idx 0, 8\.0 GiB\)/, "whichever of the pair is the small one");
  // a bigger share on the compute card is the way out, and the message says how much would do
  assert.throws(() => planOn(24, 8), /--wan-resident-gb of at least 7\.2/);
  assert.equal(planOn(24, 8, { "wan-resident-gb": "8" }).residentGb, 8, "13.4 - 8 + 0.8 = 6.2 of 7.0 usable, and 8 + 6.3 fits the 24 GiB card");
  // the measured pair: equal cards, the default share and none at all fit (13.4 + 0.8 = 14.2 of 14.9 usable)
  assert.equal(planOn(16, 16).residentGb, 3);
  assert.equal(planOn(16, 16, { "wan-resident-gb": "0" }).residentGb, 0);
  assert.throws(() => planOn(16, 12, { "wan-resident-gb": "0" }), /storage card/);
  // --reserve-vram is the storage card's margin: 5 GiB of it leaves 11.0 of 16, the donor blocks and the VAE need 11.2
  assert.throws(() => planOn(16, 16, { "reserve-vram": "5" }), /storage card \(idx 2, 16\.0 GiB\).*11\.2 GiB.*11\.0 GiB usable.*at least 3\.2/);
  assert.equal(M.WAN_EXPERT_GIB, 13.4);
  assert.equal(M.WAN_STORAGE_OVERHEAD_GIB, 0.8);
});

test("a UUID prefix that names two cards is ambiguous and refused, never resolved to the first", () => {
  const A = "GPU-dddd4444-aaaa-bbbb-cccc-111111111111";
  const B = "GPU-dddd4444-aaaa-bbbb-cccc-222222222222";
  const cards = M.parseCardTable([`0, ${A}, Test GPU A, 16311, 16000, Disabled, No`, `2, ${B}, Test GPU A, 16311, 16050, Disabled, No`].join("\n"));
  assert.throws(() => M.resolveWanCards({ tokens: ["dddd4444", "2"], source: "--wan-cards", cards }), /"dddd4444" matches 2 cards \(idx 0, idx 2\).*longer prefix/);
  assert.deepEqual(M.resolveWanCards({ tokens: ["dddd4444-aaaa-bbbb-cccc-1", "2"], source: "--wan-cards", cards }).storage.index, 0, "a long enough prefix names one card");
});

test("portNote names the port a multicard instance falls back to when no --api or COMFY_API gives it one", () => {
  assert.match(M.portNote({}, {}, "http://127.0.0.1:8189"), /no --api or COMFY_API.*127\.0\.0\.1:8189.*--api http:\/\/127\.0\.0\.1:<port>/);
  assert.equal(M.portNote({ api: "http://127.0.0.1:18852" }, {}, "http://127.0.0.1:18852"), "");
  assert.equal(M.portNote({}, { COMFY_API: "http://127.0.0.1:18852" }, "http://127.0.0.1:18852"), "");
});

test("packDirs keeps the reason a custom_nodes listing failed", () => {
  assert.throws(() => M.packDirs("/c", () => { throw new Error("EACCES: nope"); }), /no comfyui-multigpu pack.*EACCES: nope/s);
});

// ---- the fix round (2026-10-11): what the review of the merged branch found --------------------------------------------

// The '=' spelling, through the runner's own parser: internal/hostneed splits `--name=value` (strings.Cut), this runner's parseArgs does not, so
// a flag spelled with '=' swallows the NEXT token whole, whatever that token is. The lease reads the call as multicard and declares no host RAM;
// the runner must never read it as anything else than a refusal.
test("a flag spelled with '=' that swallowed one of the placement's own flags, in ANY spelling of it, is refused (the parser's own output)", () => {
  const boom = () => { throw new Error("must not be read"); };
  const plan = (argv) => { const { flags } = parseArgs(argv); return M.planMulticard({ flags, comfyDir: "/c", isWanGraph: true, cards: boom }); };
  const lost = [
    ["--seed=7", "--wan-placement=multicard"],
    ["--seed=7", "--wan-resident-gb=4"],
    ["--seed=7", "--wan-cards=0,2"],
    ["--steps", "--wan-placement=multicard", "--wan-decode", "plain"],
    ["--seed=7", "--wan-placement", "multicard"],
  ];
  for (const argv of lost) {
    assert.throws(() => plan(["o.mp4", "s.png", "p", ...argv]), /took --wan-.* as its value/, argv.join(" "));
  }
  // flags that merely START like ours are not ours: nothing is lost, the single path runs untouched
  assert.equal(plan(["o.mp4", "s.png", "p", "--seed=7", "--wan-decode=plain"]), null);
  assert.equal(plan(["o.mp4", "s.png", "p", "--negative", "--wan-placements are words"]), null, "a prompt that is not one of our flags passes");
  assert.equal(plan(["o.mp4", "s.png", "p", "--seed", "7"]), null);
});

// ---- the console watch: an out-of-memory wedge ComfyUI does not report (the review of 2026-10-11) ---------------------------
// The benchmark's results note: "After an OOM inside DisTorch2 distribution ComfyUI can wedge with no error in history". The log of that
// run (the full-size probe) shows how: the node's error handler logs "Got an OOM, unloading all loaded models" and the unload itself runs
// out of memory, which kills the prompt worker thread. The lines below are that log's, with the ANSI colours ComfyUI prints.

const ESC = String.fromCharCode(27);
const WEDGE_LOG = [
  `  0%|          | 0/1 [00:00<?, ?it/s]${ESC}[1m${ESC}[33m[WARNING]${ESC}[0m FP8 _scaled_mm failed: Allocation on device 0 would exceed allowed memory. (out of memory)`,
  "Currently allocated     : 14.44 GiB",
  `${ESC}[1m${ESC}[31m[ERROR]${ESC}[0m !!! Exception during processing !!! Allocation on device 0 would exceed allowed memory. (out of memory)`,
  `${ESC}[1m${ESC}[31m[ERROR]${ESC}[0m Got an OOM, unloading all loaded models.`,
  "Exception in thread Thread-5 (prompt_worker):",
  "Traceback (most recent call last):",
  "torch.OutOfMemoryError: Allocation on device 0 would exceed allowed memory. (out of memory)",
].join("\n");

test("oomWedgeLine: the line that says the prompt worker is gone, found through the colour codes, and only that", () => {
  assert.equal(M.oomWedgeLine(WEDGE_LOG), "[ERROR] Got an OOM, unloading all loaded models.");
  assert.equal(M.oomWedgeLine("Exception in thread Thread-5 (prompt_worker):\nTraceback"), "Exception in thread Thread-5 (prompt_worker):");
  // recoverable or already reported: ComfyUI falls back, or puts the error in /history where the poll reads it
  assert.equal(M.oomWedgeLine(WEDGE_LOG.split("\n").slice(0, 3).join("\n")), "", "the _scaled_mm warning and the node error alone are not the wedge");
  assert.equal(M.oomWedgeLine("Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n"), "");
  assert.equal(M.oomWedgeLine("Exception in thread Thread-9 (some_other_worker):"), "");
  assert.equal(M.oomWedgeLine(""), "");
  assert.equal(M.oomWedgeLine(undefined), "");
});

test("comfyLogCursor reads only what the console gains after it was made, whatever the file held before", (t) => {
  const dir = scratchDir(t, "wan-mc-");
  const key = "mc-cccc3333-aaaa1111";
  const path = L.comfyLogPath(dir, key);
  writeFileSync(path, "an earlier run: Got an OOM, unloading all loaded models.\n");
  const cur = L.comfyLogCursor(dir, key);
  assert.deepEqual(cur.read(), { text: "", state: "ok" }, "the earlier run's lines are not this run's");
  appendFileSync(path, "step 1\n");
  assert.deepEqual(cur.read(), { text: "step 1\n", state: "ok" });
  assert.deepEqual(cur.read(), { text: "", state: "ok" }, "nothing new, nothing returned");
  // a line cut by the end of a read is finished by the next one
  appendFileSync(path, "Got an OOM, unl");
  const a = cur.read();
  appendFileSync(path, "oading all loaded models.\n");
  const b = cur.read();
  assert.equal(M.oomWedgeLine(a.text), "");
  assert.equal(M.oomWedgeLine(b.text), "Got an OOM, unloading all loaded models.");
  // a log that was replaced by a shorter one is read from its start
  writeFileSync(path, "fresh\n");
  assert.deepEqual(cur.read(), { text: "fresh\n", state: "ok" });
  // the capture's own cap marker means everything after it is lost to a reader
  appendFileSync(path, "\n... [offload-comfyui.log capped at 5MB for this run] ...\n");
  assert.equal(cur.read().state, "capped");
});

test("comfyLogCursor: a console file that does not exist is said, and one that appears later is read from its start", (t) => {
  const dir = scratchDir(t, "wan-mc-");
  const cur = L.comfyLogCursor(dir, "mc-cccc3333-aaaa1111");
  assert.deepEqual(cur.read(), { text: "", state: "absent" });
  writeFileSync(L.comfyLogPath(dir, "mc-cccc3333-aaaa1111"), "boot\n");
  assert.deepEqual(cur.read(), { text: "boot\n", state: "ok" });
});

test("createOomWatch remembers ComfyUI's plain-decode retry across reads, and says whether the console could be trusted to tell", () => {
  const verdict = (reads) => {
    const queue = [...reads];
    const watch = M.createOomWatch({ cursor: { path: "/c/x.log", read: () => queue.shift() || { text: "", state: "ok" } }, say: () => {} });
    for (let i = 0; i < reads.length; i++) watch();
    return watch.verdict;
  };
  const RETRY = "Warning: Ran out of memory when regular VAE decoding, retrying with tiled VAE decoding.\n";
  assert.equal(verdict([{ text: "a\n", state: "ok" }, { text: "b\n", state: "ok" }]), "clean");
  assert.equal(verdict([{ text: RETRY, state: "ok" }, { text: "later\n", state: "ok" }]), "fallback", "a retry seen on an earlier read is not forgotten");
  assert.equal(verdict([{ text: "a\n", state: "ok" }, { text: "", state: "absent" }]), "blind", "a console that is gone at the end cannot say");
  assert.equal(verdict([{ text: "x", state: "capped" }, { text: "", state: "ok" }]), "blind", "a capture that reached its cap is blind for the rest of the run, whatever it reads later");
  assert.equal(verdict([{ text: RETRY, state: "ok" }, { text: "", state: "absent" }]), "fallback", "seen is seen");
  assert.equal(verdict([]), "clean", "a watch never asked has seen nothing wrong");
});

test("createOomWatch: silent while the console is quiet, one message naming the line and the log when the worker is gone, a loud word when it cannot see", () => {
  const reads = [];
  const said = [];
  const cursor = { path: "/c/offload-comfyui-mc-x.log", read: () => reads.shift() };
  const watch = M.createOomWatch({ cursor, say: (m) => said.push(m) });
  reads.push({ text: "", state: "ok" }, { text: "step 2\n", state: "ok" });
  assert.equal(watch(), null);
  assert.equal(watch(), null);
  reads.push({ text: WEDGE_LOG, state: "ok" });
  const msg = watch();
  assert.match(msg, /^WAN-OOM: ComfyUI ran out of memory and its prompt worker did not survive it/);
  assert.match(msg, /\[ERROR\] Got an OOM, unloading all loaded models\./);
  assert.match(msg, /offload-comfyui-mc-x\.log/);
  assert.match(msg, /ComfyUI must be restarted/);
  assert.deepEqual(said, []);
  // blind: said once each, never a verdict
  const blind = [];
  const w2 = M.createOomWatch({ cursor: { path: "/c/x.log", read: () => blind.shift() }, say: (m) => said.push(m) });
  blind.push({ text: "", state: "absent" }, { text: "", state: "absent" }, { text: "", state: "capped" }, { text: "", state: "capped" });
  for (let i = 0; i < 4; i++) assert.equal(w2(), null);
  assert.equal(said.filter((m) => /^WAN-OOM-WATCH: .*no console file/.test(m)).length, 1);
  assert.equal(said.filter((m) => /^WAN-OOM-WATCH: .*capped/.test(m)).length, 1);
});

// ---- the compute card ranked by capability (0.185.0) ---------------------------------------------------------------------------------
// The size rule picked the compute card by exact bytes, so a 16303 MiB card lost the role to a 16311 MiB card half its speed. A board's design power
// (nvidia-smi power.default_limit, read by a second query) now ranks two cards of one size (within 1 percent), from a ratio of 1.25.
// THE FIXTURES put the fast card on the LOWER uuid (U2 sorts before U0) and make it the SMALLER card by 8 MiB (16303 against 16311). Every rule that is
// not the board-power rule then disagrees with it: the larger card computes -> idx 0, the higher uuid computes -> idx 0, an unreported power -> idx 0.
// Only the power rule picks idx 2, so a mutation of any one rule turns an answer.
const rankCards = ({ slowW, fastW, slowMiB = 16311, fastMiB = 16303 } = {}) => [
  { index: 0, uuid: U0, name: "Test GPU Slow", totalMiB: slowMiB, freeMiB: 16000, display: false, displayUnknown: false, displayActive: "Disabled", displayAttached: "No", powerLimitW: slowW },
  { index: 2, uuid: U2, name: "Test GPU Fast", totalMiB: fastMiB, freeMiB: 16050, display: false, displayUnknown: false, displayActive: "Disabled", displayAttached: "No", powerLimitW: fastW },
];
// [storage index, compute index]
const rolesOf = (cards, tokens = ["0", "2"], extra = {}) => { const r = M.resolveWanCards({ tokens, source: "t", cards, ...extra }); return [r.storage.index, r.compute.index]; };
const rankedWhy = (cards) => M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards }).why;

test("two cards within 1 percent of one size: the board with the higher design power computes, whatever the token order", () => {
  const cards = rankCards({ slowW: 180, fastW: 300 });
  for (const tokens of [["0", "2"], ["2", "0"], [U0, U2], [U2, U0]]) assert.deepEqual(rolesOf(cards, tokens), [0, 2], tokens.join());
  assert.equal(rankedWhy(cards), "the faster card: 300 W board power against 180 W");
  // the mirror: the fast card is idx 0 and carries the higher uuid, and the slow one is the larger: the size rule would have put the slow card on compute
  const mirror = [{ ...cards[1], index: 0, uuid: U0 }, { ...cards[0], index: 2, uuid: U2 }];
  assert.deepEqual(rolesOf(mirror), [2, 0]);
  assert.equal(rankedWhy(mirror), "the faster card: 300 W board power against 180 W");
});

test("the equal-size band is 1 percent of the larger card, and outside it the larger card computes whatever the board power", () => {
  assert.equal(M.VRAM_EQUAL_FRACTION, 0.01);
  // 16311 - 16148 = 163 MiB, inside 1 percent of 16311 (163.11): one size, so the faster board computes
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300, fastMiB: 16148 })), [0, 2]);
  // 164 MiB under: more than 1 percent, so the sizes are different and the larger card computes
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300, fastMiB: 16147 })), [2, 0]);
  assert.match(rankedWhy(rankCards({ slowW: 180, fastW: 300, fastMiB: 16147 })), /^the larger card \(16311 MiB against 16147 MiB\)$/);
  // far apart: a small fast card does not take the role from a big slow one
  assert.deepEqual(rolesOf(rankCards({ slowW: 100, fastW: 300, slowMiB: 24576, fastMiB: 16384 })), [2, 0]);
});

test("a board power that is not reported on either card ranks nothing: today's rule applies, and the reason says so", () => {
  for (const [slowW, fastW] of [[undefined, undefined], [0, 300], [undefined, 300], [NaN, 300], [180, 0], [300, undefined]]) {
    const cards = rankCards({ slowW, fastW });
    const r = M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards });
    // the larger card (16311 MiB, idx 0) computes: exactly what the planner did before the board power was read
    assert.deepEqual([r.storage.index, r.compute.index], [2, 0], `${slowW} / ${fastW}`);
    assert.match(r.why, /^the larger card \(16311 MiB against 16303 MiB\) \(board power not reported for idx \d/, `${slowW} / ${fastW}`);
  }
  // the positive control: the same two cards with both powers read are ranked
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300 })), [0, 2]);
  // two cards of exactly one size and no power: the higher uuid computes, as it always did
  const same = rankCards({ slowW: undefined, fastW: undefined, fastMiB: 16311 });
  assert.deepEqual(rolesOf(same), [2, 0]);
  assert.match(rankedWhy(same), /^two cards of one size: the higher UUID computes \(board power not reported for idx 0, idx 2\)$/);
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300, fastMiB: 16311 })), [0, 2], "and the same size with both powers read is ranked");
  // the remark belongs to a pair within the 1 percent size band (the docs and the CHANGELOG say so): outside it the power was never going to rank the pair
  assert.equal(rankedWhy(rankCards({ slowW: undefined, fastW: undefined, fastMiB: 16147 })), "the larger card (16311 MiB against 16147 MiB)");
});

test("board power ranks from 1.25 times the other card, inclusive, and not under it", () => {
  assert.equal(M.POWER_RANK_RATIO, 1.25);
  const at = (slowW, fastW) => rolesOf(rankCards({ slowW, fastW }));
  assert.deepEqual(at(100, 124), [2, 0], "1.24 times: a factory overclock of one chip, not another class of card: the old rule");
  assert.match(rankedWhy(rankCards({ slowW: 100, fastW: 124 })), /^the larger card \(16311 MiB against 16303 MiB\) \(board power 100 W and 124 W differ by less than 1\.25 times\)$/);
  assert.deepEqual(at(100, 125), [0, 2], "exactly 1.25 times ranks");
  assert.deepEqual(at(100, 300), [0, 2]);
});

test("WAN_COMPUTE_UUID names the compute card of the pair: an index, a UUID or a prefix of 8+ characters, beating the size and the speed", () => {
  // idx 0 and idx 2 are the same size here, so without a word idx 0 (the higher uuid) computes
  for (const token of ["2", U2, U2.toLowerCase(), "aaaa1111", "GPU-AAAA1111", "gpu-aaaa1111", " 2 "]) {
    const r = M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards: CARDS, compute: token });
    assert.deepEqual([r.storage.index, r.compute.index], [0, 2], JSON.stringify(token));
    assert.equal(r.why, "named by WAN_COMPUTE_UUID");
    assert.equal(r.note, "");
  }
  for (const token of ["", "   ", undefined]) assert.deepEqual(rolesOf(CARDS.filter((c) => c.index !== 1), ["0", "2"], { compute: token }), [2, 0], "no word: the old roles");
  assert.deepEqual(rolesOf(CARDS, ["0", "2"], { compute: "0" }), [2, 0], "and naming the card that would compute anyway changes nothing");
  // it beats the board power as well: the slow card is named, so it computes
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300 }), ["0", "2"], { compute: "0" }), [2, 0]);
  assert.deepEqual(rolesOf(rankCards({ slowW: 180, fastW: 300 }), ["0", "2"]), [0, 2], "(the control: unnamed, the fast card computes)");
  // through the plan, with the environment variable: idx 2 computes and the instance key follows the roles
  const plan = planWith({}, { env: { ...LEASE, WAN_COMPUTE_UUID: "2" } });
  assert.equal(plan.pair.compute.index, 2);
  assert.equal(plan.pair.storage.index, 0);
  assert.equal(plan.launch.cudaVisibleDevices, `${U0},${U2}`);
  assert.equal(plan.launch.key, "mc-cccc3333-aaaa1111");
  assert.equal(planWith().pair.compute.index, 0, "the control: the lease alone leaves the roles as they were");
  assert.equal(M.computeOverride({ WAN_COMPUTE_UUID: "  2 " }), "2");
  assert.equal(M.computeOverride({}), "");
  assert.equal(M.computeOverride({ WAN_COMPUTE_UUID: "   " }), "");
});

test("the override is judged like any other choice: a named card that cannot hold the share is refused with the numbers", () => {
  const cardsOf = (a, b) => M.parseCardTable([`0, ${U0}, Test GPU A, ${a * 1024}, 16000, Disabled, No`, `2, ${U2}, Test GPU A, ${b * 1024}, 16050, Disabled, No`].join("\n"));
  const film = { width: "720", height: "1280", frames: "81", "wan-resident-gb": "4" };
  const table = cardsOf(24, 12);
  assert.equal(planWith(film, { cards: () => table }).pair.compute.index, 0, "the 24 GiB card computes, and 4 GiB resident fits it");
  // 12 GiB - 12.2 GiB of film-size activations - 0.5 GiB of context: nothing fits there
  assert.throws(() => planWith(film, { cards: () => table, env: { ...LEASE, WAN_COMPUTE_UUID: "2" } }), /--wan-resident-gb 4 .*compute card \(idx 2, 12\.0 GiB\).*720x1280x81.*at most -0\.7/);
  // The 16 GiB card of a 24 + 16 GiB pair is the measured compute card, so naming it is NOT refused at the default share: 16.0 - 12.2 - 0.5 = 3.3 GiB holds the share of 3
  // (the lease sizing judges the larger card, so nothing refuses either). It is refused only for a share over what it holds.
  const mixed = cardsOf(24, 16);
  const filmDefault = { width: "720", height: "1280", frames: "81" };
  const namesSmaller = { ...LEASE, WAN_COMPUTE_UUID: "2" };
  const admitted = planWith(filmDefault, { cards: () => mixed, env: namesSmaller });
  assert.deepEqual([admitted.pair.storage.index, admitted.pair.compute.index, admitted.residentGb], [0, 2, 3], "24 + 16 naming the 16 GiB card, default share: admitted");
  assert.throws(() => planWith({ ...filmDefault, "wan-resident-gb": "3.4" }, { cards: () => mixed, env: namesSmaller }), /--wan-resident-gb 3\.4 .*compute card \(idx 2, 16\.0 GiB\).*720x1280x81.*at most 3\.3/);
  assert.equal(planWith({ ...filmDefault, "wan-resident-gb": "3.4" }, { cards: () => mixed }).pair.compute.index, 0, "(the control: unnamed, the 24 GiB card computes and holds 3.4)");
});

test("an override that does not name exactly one of the two cards is ignored with a note, and one that names no card at all is refused", () => {
  // idx 3 begins like idx 2: a prefix names both on the table, though only one of them is in the pair
  const twin = M.parseCardTable(`3, GPU-aaaa1111-9999-9999-9999-999999999999, Test GPU A, 16311, 16000, Disabled, No`);
  const table = [...CARDS, ...twin];
  const go = (cards, token) => { const r = M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards, compute: token }); return { roles: [r.storage.index, r.compute.index], why: r.why, note: r.note }; };
  const ignored = (token) => `WAN_COMPUTE_UUID names ${JSON.stringify(token)}, which does not name exactly one of the two cards (idx 0, idx 2); ranked instead`;
  // a real card that is not in the pair
  const outside = go(CARDS, "1");
  assert.deepEqual(outside.roles, [2, 0]);
  assert.equal(outside.note, ignored("1"));
  assert.match(outside.why, /^two cards of one size: the higher UUID computes/);
  // a prefix that begins two cards of the table: ambiguous, though only one of them is in the pair (the table-wide rule of the other tokens)
  const ambiguous = go(table, "aaaa1111");
  assert.deepEqual(ambiguous.roles, [2, 0]);
  assert.equal(ambiguous.note, ignored("aaaa1111"));
  assert.deepEqual(go(table, "aaaa1111-d").roles, [0, 2], "the control: a longer prefix names one card, and it computes");
  // a token that names both cards of the pair is ambiguous too
  const both = M.parseCardTable([`0, GPU-dddd4444-aaaa-bbbb-cccc-111111111111, Test GPU A, 16311, 16000, Disabled, No`, `2, GPU-dddd4444-aaaa-bbbb-cccc-222222222222, Test GPU A, 16311, 16050, Disabled, No`].join("\n"));
  assert.equal(go(both, "dddd4444").note, ignored("dddd4444"));
  assert.deepEqual(go(both, "dddd4444-aaaa-bbbb-cccc-1").roles, [2, 0], "(the control: a longer prefix names idx 0, which then computes, against the higher uuid's default)");
  assert.deepEqual(go(both, "dddd4444-aaaa-bbbb-cccc-2").roles, [0, 2]);
  // the ranking still decides when the override is ignored, and says so
  const ranked = go([...rankCards({ slowW: 180, fastW: 300 }), CARDS[1]], "1");
  assert.deepEqual(ranked.roles, [0, 2]);
  assert.equal(ranked.why, "the faster card: 300 W board power against 180 W");
  assert.equal(ranked.note, ignored("1"));
  // names no card on the table (a typo, a stale uuid): refused, naming the token and the two cards, and the ranking is not run
  for (const token of ["7", "deadbeef", "GPU-", "aaaa", "GPU-ffffffff", "GPU-aaaa"]) {
    assert.throws(() => go(CARDS, token), new RegExp(`^Error: WAN-PLACEMENT: WAN_COMPUTE_UUID names ${JSON.stringify(token)}, which is no card in the driver's table.*\\(idx 0, idx 2\\)`), token);
  }
  assert.deepEqual(go(CARDS, "2").roles, [0, 2], "(the control: a card that exists is taken)");
  // an empty or blank token is absent: no note, no refusal
  for (const token of ["", "  "]) assert.deepEqual(go(CARDS, token), { roles: [2, 0], why: "two cards of one size: the higher UUID computes (board power not reported for idx 0, idx 2)", note: "" });
});

test("a refused override that is digits alone says it was read as an index, and how a UUID prefix made of digits is spelled", () => {
  // about 2 percent of 8-hex prefixes are all digits: GPU-11112222-... is begun by "11112222", which the token rule reads as an index (nvidia-smi's index is a small number)
  const digits = M.parseCardTable([`0, GPU-11112222-bbbb-cccc-dddd-eeeeeeeeeeee, Test GPU A, 16311, 16000, Disabled, No`, `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`].join("\n"));
  const go = (token) => M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards: digits, compute: token });
  const refusal = (token) => { try { go(token); } catch (e) { return e.message; } return ""; };
  assert.equal(go("GPU-11112222").compute.index, 0, "the control: the same prefix with its GPU- spelling names the card");
  const digitsOnly = refusal("11112222");
  assert.match(digitsOnly, /^WAN-PLACEMENT: WAN_COMPUTE_UUID names "11112222", which is no card in the driver's table/);
  assert.match(digitsOnly, /a token of digits alone is read as an nvidia-smi index; a UUID prefix made of digits needs the "GPU-" spelling \(GPU-11112222\)/);
  // a token with a letter in it is no index, so the clause would mislead there: the control
  const letters = refusal("deadbeef");
  assert.match(letters, /^WAN-PLACEMENT: WAN_COMPUTE_UUID names "deadbeef", which is no card/);
  assert.doesNotMatch(letters, /digits alone/);
  // and every refusal says how the 8 characters are counted: after the optional GPU- (the rule the docs state)
  for (const msg of [digitsOnly, letters, refusal("GPU-aaaa")]) assert.match(msg, /a UUID prefix of 8 or more characters after the optional "GPU-", or unset it/);
});

test("the override cannot add a card: --wan-cards still has to be confirmed by the lease, whatever WAN_COMPUTE_UUID names", () => {
  const held = ["gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee", "gpu-bbbb2222-cccc-dddd-eeee-ffffffffffff"];
  assert.throws(() => planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => held, env: { WAN_COMPUTE_UUID: "1" } }), /--wan-cards names idx 2, which the GPU lease does not hold/);
  assert.throws(() => planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => held, env: { WAN_COMPUTE_UUID: "2" } }), /does not hold/);
  // the control: a lease that holds the pair confirms it, and the override then names the compute card
  const both = ["gpu-cccc3333-bbbb-cccc-dddd-eeeeeeeeeeee", "gpu-aaaa1111-dddd-eeee-ffff-000000000000"];
  assert.equal(planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => both, env: { WAN_COMPUTE_UUID: "2" } }).pair.compute.index, 2);
  assert.equal(planWith({ "wan-cards": "0,2" }, { leaseDevicesFn: () => both, env: {} }).pair.compute.index, 0);
});

test("the plan says which rule chose the compute card at the end of the one line it says, with the note of an ignored override", () => {
  const said = [];
  const say = (l) => said.push(l);
  const table = rankCards({ slowW: 180, fastW: 300 });
  planWith({}, { cards: () => table, say });
  assert.equal(said.length, 1);
  assert.match(said[0], /^wan-placement: multicard on the GPU lease: storage cuda:0 = idx 0 .*; compute cuda:1 = idx 2 .*; 3 GiB of each expert resident on the compute card; compute card chosen: the faster card: 300 W board power against 180 W$/);
  said.length = 0;
  planWith({}, { cards: () => table, say, env: { ...LEASE, WAN_COMPUTE_UUID: "0" } });
  assert.equal(said.length, 1);
  assert.match(said[0], /; compute cuda:1 = idx 0 .*; compute card chosen: named by WAN_COMPUTE_UUID$/);
  said.length = 0;
  planWith({}, { cards: () => [...table, CARDS[1]], say, env: { ...LEASE, WAN_COMPUTE_UUID: "1" } });
  assert.equal(said.length, 1);
  assert.match(said[0], /; compute card chosen: the faster card: 300 W board power against 180 W \(WAN_COMPUTE_UUID names "1", which does not name exactly one of the two cards \(idx 2, idx 0\); ranked instead\)$/);
});

// ---- the card table with the board power: queryCardsRanked --------------------------------------------------------------------------
const DISPLAY_CSV = [`0, ${U0}, Test GPU Slow, 16311, 16000, Disabled, No`, `2, ${U2}, Test GPU Fast, 16303, 16050, Disabled, No`].join("\n");
// a fake nvidia-smi: the display ladder answers `display`, the power query answers `power` after `powerFails` failures
const smiFake = ({ display = DISPLAY_CSV, power = "", powerFails = 0, displayFails = 0 } = {}) => {
  const calls = [];
  let pf = powerFails, df = displayFails;
  const spawn = (cmd, args, opts) => {
    calls.push({ cmd, fields: args[0], format: args[1], opts });
    if (/power/.test(args[0])) { if (pf > 0) { pf--; return { status: 2, stdout: "" }; } return { status: 0, stdout: power }; }
    if (df > 0) { df--; return { status: 2, stdout: "" }; }
    return { status: 0, stdout: display };
  };
  spawn.calls = calls;
  return spawn;
};

test("queryCardsRanked asks the display ladder first and unchanged, then the board power in a second hidden query, and joins it by UUID", () => {
  const smi = smiFake({ power: `${U0}, 180.00\n${U2.toUpperCase()}, 300.00` });
  const cards = M.queryCardsRanked(smi);
  assert.equal(smi.calls.length, 2, "the display table once, the power once");
  assert.match(smi.calls[0].fields, /^--query-gpu=index,uuid,name,memory\.total,memory\.free,display_active,display_attached$/);
  assert.equal(smi.calls[1].fields, "--query-gpu=uuid,power.default_limit");
  for (const c of smi.calls) {
    assert.equal(c.cmd, "nvidia-smi");
    assert.equal(c.format, "--format=csv,noheader,nounits");
    assert.equal(c.opts.windowsHide, true, `${c.fields}: no console window on Windows`);
  }
  assert.deepEqual(cards.map((c) => [c.index, c.powerLimitW]), [[0, 180], [2, 300]], "joined by the lower-cased UUID, whatever case the driver spells it in");
  assert.deepEqual(cards.map((c) => c.displayUnknown), [false, false], "the display table is what queryCards reads");
  // the whole way through the roles
  assert.deepEqual(rolesOf(cards), [0, 2]);
});

test("queryCardsRanked retries a failed power query once, and a power that cannot be read leaves every card at 0 and refuses nothing", () => {
  const power = `${U0}, 180.00\n${U2}, 300.00`;
  const once = smiFake({ power, powerFails: 1 });
  assert.deepEqual(M.queryCardsRanked(once).map((c) => c.powerLimitW), [180, 300], "a transient failure is retried: the roles do not flip on one bad read");
  assert.equal(once.calls.length, 3);
  assert.ok(once.calls.every((c) => c.opts.windowsHide === true), "the retry is hidden too");
  const never = smiFake({ power, powerFails: 9 });
  const cards = M.queryCardsRanked(never);
  assert.equal(never.calls.length, 3, "the table once, the power twice, then it gives up");
  assert.deepEqual(cards.map((c) => c.powerLimitW), [0, 0]);
  assert.deepEqual(cards.map((c) => c.displayUnknown), [false, false], "and the display detection is not touched by it");
  assert.deepEqual(rolesOf(cards), [2, 0], "unranked: the larger card computes, as before the power was read");
  const throws = (cmd, args, opts) => { if (/power/.test(args[0])) throw new Error("spawn failed"); return { status: 0, stdout: DISPLAY_CSV }; };
  assert.deepEqual(M.queryCardsRanked(throws).map((c) => c.powerLimitW), [0, 0], "a throwing spawn is a failed read too");
});

test("queryCardsRanked reads only a power that is a positive number, and ignores a row that is no card of the table", () => {
  const readOf = (cell) => M.queryCardsRanked(smiFake({ power: `${U0}, ${cell}\n${U2}, 250.50\nGPU-ffff0000-0000-0000-0000-000000000000, 400.00` })).map((c) => c.powerLimitW);
  assert.deepEqual(readOf("180.00"), [180, 250.5]);
  for (const cell of ["[N/A]", "[Not Supported]", "", "0.00", "-5", "abc", "Infinity"]) assert.deepEqual(readOf(cell), [0, 250.5], JSON.stringify(cell));
  const short = M.queryCardsRanked(smiFake({ power: `${U0}\n${U2}, 250.50\nnoise` }));
  assert.deepEqual(short.map((c) => c.powerLimitW), [0, 250.5], "a row without its column is no reading");
});

test("queryCardsRanked leaves the display ladder as queryCards has it: the full query twice, then without display_attached, then the power", () => {
  const asked = [];
  const smi = (cmd, args, opts) => {
    asked.push({ fields: args[0], opts });
    if (/power/.test(args[0])) return { status: 0, stdout: `${U0}, 180.00` };
    return /display_attached/.test(args[0]) ? { status: 2, stdout: "" } : { status: 0, stdout: `0, ${U0}, X, 100, 50, Enabled` };
  };
  const cards = M.queryCardsRanked(smi);
  assert.equal(asked.length, 4);
  assert.deepEqual(asked.map((a) => /power/.test(a.fields) ? "power" : /display_attached/.test(a.fields) ? "full" : "six"), ["full", "full", "six", "power"]);
  assert.ok(asked.every((a) => a.opts.windowsHide === true));
  assert.equal(cards[0].display, true);
  assert.equal(cards[0].displayUnknown, true, "the six-column fallback still leaves the display state unknown");
  assert.equal(cards[0].powerLimitW, 180);
  // no card table at all: the same refusals as queryCards, and no power is asked for
  const none = smiFake({ displayFails: 9 });
  assert.throws(() => M.queryCardsRanked(none), /nvidia-smi gave no card table/);
  assert.ok(none.calls.every((c) => !/power/.test(c.fields)));
  assert.throws(() => M.queryCardsRanked(() => ({ status: 0, stdout: "no cards here" })), /listed no card/);
});

test("planMulticard reads the card table through queryCardsRanked unless it is handed one: a faster board computes with no injected table", () => {
  const smi = smiFake({ power: `${U0}, 180.00\n${U2}, 300.00` });
  const plan = planWith({}, { cards: undefined, spawnSync: smi });
  assert.equal(plan.pair.compute.index, 2);
  assert.equal(plan.pair.why, "the faster card: 300 W board power against 180 W");
  assert.deepEqual(smi.calls.map((c) => /power/.test(c.fields) ? "power" : "table"), ["table", "power"]);
});

// The mixed pair (two models of one size class) is checked role by role, so a pin that landed the cards the wrong way round is caught. The checker half of
// this test holds before the ranking existed (the pair is built by hand); the last assertion is the roles the planner now gives such a pair.
test("a mixed pair is verified role by role: the compute card's own model name is checked on cuda:1", () => {
  const cards = rankCards({ slowW: 180, fastW: 300 });
  const mixed = { storage: cards[0], compute: cards[1] };
  const dev = (name, mib) => ({ type: "cuda", vram_total: mib * 1024 ** 2, name });
  assert.deepEqual(M.checkComfyDevices([dev("cuda:0 Test GPU Slow : cudaMallocAsync", 16311), dev("cuda:1 Test GPU Fast : cudaMallocAsync", 16303)], mixed), []);
  const swapped = M.checkComfyDevices([dev("cuda:0 Test GPU Fast : cudaMallocAsync", 16303), dev("cuda:1 Test GPU Slow : cudaMallocAsync", 16311)], mixed);
  assert.equal(swapped.length, 2);
  assert.match(swapped.join("\n"), /cuda:1 \(compute\) should be idx 2 \(Test GPU Fast\)/);
  assert.match(swapped.join("\n"), /cuda:0 \(storage\) should be idx 0 \(Test GPU Slow\)/);
  const r = M.resolveWanCards({ tokens: ["0", "2"], source: "t", cards });
  assert.deepEqual([r.storage.index, r.compute.index], [mixed.storage.index, mixed.compute.index], "the planner's roles for this pair are the ones the checker verifies");
});

// ---- sage attention (0.185.0) ---------------------------------------------------------------------------------------------------------
// The film session of 2026-10-12 ran the film clip with --use-sage-attention in COMFY_EXTRA_ARGS: the compute card read 15,482 MiB in use at resident 0 (15.119 GiB)
// and 15,216 MiB at resident 2 on 720x1280x65 (14.859 GiB, 12.859 beyond the share), 3 GiB more than the default attention's 12.2. The planner knew neither, so a
// sage call with the default share of 3 (3 + 12.2 + 0.5 = 15.7 of 15.93) was admitted and ran out of memory. Every env below is an EXPLICIT object, so no test
// reads the host's COMFY_EXTRA_ARGS (a film host exports it).
const SAGE_ENV = { ...LEASE, COMFY_EXTRA_ARGS: "--use-sage-attention" };
const FILM = { width: "720", height: "1280", frames: "81" };
const CLIP65 = { width: "720", height: "1280", frames: "65" };
// ONE table, read by this file and by internal/hostneed's TestTheSageTokenRuleMatchesTheRunner (render/testdata/sage-token-table.json): a row changed there turns
// an answer in both languages, and a row added is checked by both.
const SAGE_TOKENS = JSON.parse(readFileSync(new URL("./testdata/sage-token-table.json", import.meta.url), "utf8")).rows.map((r) => [r.extra, r.sage]);

test("sage attention in COMFY_EXTRA_ARGS: the film clip keeps at most 0.2 GiB of an expert resident, so the default share of 3 is refused and 0 is admitted", () => {
  const sage = (flags) => planWith({ ...FILM, ...flags }, { env: SAGE_ENV });
  assert.throws(() => sage({ "wan-resident-gb": "3" }), /--wan-resident-gb 3 .*720x1280x81 \(75600 tokens\).*sage.*at most 0\.2/);
  assert.throws(() => sage({}), /--wan-resident-gb 3 .*sage.*at most 0\.2/, "the default share is the measured 3, and it is refused under sage");
  assert.equal(sage({ "wan-resident-gb": "0" }).residentGb, 0);
  assert.equal(sage({ "wan-resident-gb": "0.2" }).residentGb, 0.2);
  assert.throws(() => sage({ "wan-resident-gb": "0.3" }), /--wan-resident-gb 0\.3 .*at most 0\.2/);
  // the control: the same calls without the flag are the default sizing, where 3 and 3.2 are admitted
  assert.equal(planWith({ ...FILM, "wan-resident-gb": "3" }).residentGb, 3);
  assert.equal(planWith({ ...FILM, "wan-resident-gb": "3.2" }).residentGb, 3.2);
  // the refusal names the mode and the way out, and drops the default-mode tail (the measured recipe that keeps 3 is the pytorch one, the very share refused here)
  const msg = (() => { try { sage({ "wan-resident-gb": "3" }); } catch (e) { return e.message; } return ""; })();
  assert.match(msg, /^WAN-PLACEMENT: /);
  assert.match(msg, /--use-sage-attention in COMFY_EXTRA_ARGS/);
  assert.match(msg, /--wan-resident-gb 0 is the measured setting at film size/);
  assert.doesNotMatch(msg, /measured film-size recipe keeps/);
});

test("sage attention: the measured 65-frame clip admits a 2 GiB share and refuses 3", () => {
  const sage = (gb, clip = CLIP65) => planWith({ ...clip, "wan-resident-gb": String(gb) }, { env: SAGE_ENV });
  assert.equal(M.wanGeometry(CLIP65).tokens, 61200);
  assert.equal(sage(2).residentGb, 2, "the share the film session measured at this clip: a flat 15.2 GiB below the film size would refuse it");
  assert.throws(() => sage(3), /--wan-resident-gb 3 .*720x1280x65 \(61200 tokens\).*sage.*at most 2\.5/);
  // the control: the default sizing admits 3 at the same clip (15.93 - 12.2 - 0.5 = 3.2)
  assert.equal(planWith({ ...CLIP65, "wan-resident-gb": "3" }).residentGb, 3);
  // Where the default share of 3 stops being admitted is geometry, not a round token count: 720x1280x61 (57,600 tokens) is past the point where sage leaves the default curve
  // (about 56,800) and still admits 3 (12.325 GiB of activations, at most 3.1), while 720x1280x65 (61,200 tokens, above) refuses it. The share of 3 is first refused at about 58,250 tokens.
  const CLIP61 = { width: "720", height: "1280", frames: "61" };
  assert.equal(M.wanGeometry(CLIP61).tokens, 57600);
  assert.ok(M.wanActivationsGib(57600, "sage") > M.wanActivationsGib(57600), "sage has left the default curve by 57,600 tokens");
  assert.equal(sage(3, CLIP61).residentGb, 3, "720x1280x61 under sage still admits the default share");
  assert.throws(() => sage(3.2, CLIP61), /--wan-resident-gb 3\.2 .*720x1280x61 \(57600 tokens\).*sage.*at most 3\.1/);
});

test("wanActivationsGib(tokens, 'sage') meets the film session's inequalities, and departs from the default only above the default's cap", () => {
  const sage = (t) => M.wanActivationsGib(t, "sage");
  const dflt = (t) => M.wanActivationsGib(t);
  assert.equal(M.WAN_FILM_ACTIVATIONS_SAGE_GIB, 15.2);
  // the two measured points: 12.859 GiB beyond the share at 61,200 tokens, 15.119 GiB at the film's 75,600, each rounded up
  assert.ok(sage(61200) >= 12.86 && sage(61200) <= 13.4, `61200 tokens: ${sage(61200)}`);
  assert.equal(sage(75600), M.WAN_FILM_ACTIVATIONS_SAGE_GIB);
  assert.ok(sage(75600) > 12.4 && sage(75600) >= 15.12 && sage(75600) <= 15.4, `75600 tokens: ${sage(75600)}`);
  assert.equal(sage(200000), sage(75600), "held at the film's figure above the film's size: nothing was measured there");
  // between the points it rises with the tokens: a flat 15.2 below the film size would refuse the measured 65-frame clip
  assert.ok(sage(68400) > sage(61200) && sage(68400) < sage(75600), `68400 tokens: ${sage(68400)}`);
  // below the default's cap sage is the default curve: the benchmark's 6.16 GiB reserved at 20,280 tokens with sage is inside the default's 6.29
  assert.equal(sage(20280), dflt(20280));
  assert.ok(sage(20280) >= 6.16);
  assert.equal(sage(7800), dflt(7800));
  let previous = 0;
  for (let t = 0; t <= 140000; t += 400) {
    assert.ok(sage(t) >= dflt(t), `${t} tokens: sage ${sage(t)} under the default ${dflt(t)}`);
    assert.ok(sage(t) >= previous, `${t} tokens: ${sage(t)} fell from ${previous}`);
    if (t <= 56000) assert.equal(sage(t), dflt(t), `${t} tokens: the default's cap (12.2 GiB, from 39,400 tokens) is above the sage line until about 57,000`);
    previous = sage(t);
  }
  assert.ok(sage(57600) > dflt(57600), "and from about 57,000 tokens it departs");
  // the default call is what it was, and any mode but "sage" is the default
  assert.equal(M.wanActivationsGib(75600), M.WAN_FILM_ACTIVATIONS_GIB);
  for (const mode of ["default", "flash", undefined]) assert.equal(M.wanActivationsGib(75600, mode), M.WAN_FILM_ACTIVATIONS_GIB, String(mode));
});

test("wanAttentionMode reads COMFY_EXTRA_ARGS token by token, and isSageAttentionToken is the one predicate", () => {
  assert.ok(SAGE_TOKENS.length >= 16 && SAGE_TOKENS.some(([, s]) => s) && SAGE_TOKENS.some(([, s]) => !s), "the shared table (render/testdata/sage-token-table.json) looks empty or one-sided");
  for (const [extra, sage] of SAGE_TOKENS) assert.equal(M.wanAttentionMode({ COMFY_EXTRA_ARGS: extra }), sage ? "sage" : "default", JSON.stringify(extra));
  assert.equal(M.wanAttentionMode({}), "default");
  assert.equal(M.wanAttentionMode({ COMFY_EXTRA_ARGS: undefined }), "default");
  // the predicate itself: one token (an exact flag or an argparse prefix of 8+ characters, ComfyUI's parser allows abbreviations)
  for (const [extra, sage] of SAGE_TOKENS) {
    const tokens = extra.split(/\s+/).filter(Boolean);
    assert.equal(tokens.some(L.isSageAttentionToken), sage, JSON.stringify(extra));
  }
  assert.equal(L.isSageAttentionToken("--use-sage-attention"), true);
  assert.equal(L.isSageAttentionToken("--use-sa"), true);
  assert.equal(L.isSageAttentionToken("--use-s"), false, "begins --use-split-cross-attention too");
});

test("a plan with no sage flag, or another attention flag, is the plan it always was", () => {
  for (const extra of [undefined, "", "--use-flash-attention", "--use-split-cross-attention", "--use-s", "--use-sage-attention-extra", "--x 1"]) {
    const said = [];
    const env = extra === undefined ? { ...LEASE } : { ...LEASE, COMFY_EXTRA_ARGS: extra };
    const plan = planWith({ ...FILM, "wan-resident-gb": "3" }, { env, say: (l) => said.push(l) });
    assert.equal(plan.attention, "default", String(extra));
    assert.equal(plan.residentGb, 3);
    assert.equal(said.length, 1, String(extra));
    assert.doesNotMatch(said[0], /sage/i, String(extra));
  }
  // the default-mode refusal is byte for byte what it was
  const expected = "WAN-PLACEMENT: --wan-resident-gb 10 leaves the compute card (idx 0, 15.9 GiB) about 6.3 GiB for the sampler's activations at 832x480x49 (20280 tokens) and 0.5 GiB for the CUDA context: at most 9.1 GiB of an expert can stay resident there (the measured film-size recipe keeps 3)";
  assert.throws(() => planWith({ "wan-resident-gb": "10" }), (e) => e.message === expected);
  assert.throws(() => planWith({ "wan-resident-gb": "10" }, { env: { ...LEASE, COMFY_EXTRA_ARGS: "--use-flash-attention" } }), (e) => e.message === expected);
});

test("geometryNote under sage extrapolates from the sage figure, and the default note is what it was", () => {
  const big = { width: "1280", height: "1280", frames: "121" };
  const note = M.geometryNote(big, "sage");
  assert.match(note, /film's 15\.2 GiB/);
  assert.match(note, /about 39\.9 GiB/);
  assert.match(note, /sage/);
  const plain = M.geometryNote(big);
  assert.match(plain, /film's 12\.2 GiB/);
  assert.match(plain, /about 32\.0 GiB/);
  assert.doesNotMatch(plain, /sage/);
  assert.equal(M.geometryNote(big, "default"), plain);
  assert.equal(M.geometryNote(FILM, "sage"), "", "at the film's size there is nothing to say, under sage too");
  // through the plan, the note comes first and the one summary line after it
  const said = [];
  planWith({ ...big, "wan-resident-gb": "0" }, { env: SAGE_ENV, say: (l) => said.push(l) });
  assert.match(said[0], /^WAN-GEOMETRY-NOTE: .*film's 15\.2 GiB/);
  assert.equal(said.length, 2);
});

test("a sage plan names the attention it sized for, once, and carries it", () => {
  const said = [];
  const plan = planWith({ ...FILM, "wan-resident-gb": "0" }, { env: SAGE_ENV, say: (l) => said.push(l) });
  assert.equal(plan.attention, "sage");
  assert.equal(said.length, 1);
  assert.match(said[0], /^wan-placement: multicard on the GPU lease: storage cuda:0 = idx 2 .*; compute cuda:1 = idx 0 .*; 0 GiB of each expert resident on the compute card; compute card chosen: /);
  assert.match(said[0], /; sage attention \(COMFY_EXTRA_ARGS\): activations sized at 15\.2 GiB$/);
  // the control: without the flag the same call has no such clause
  const plain = [];
  planWith({ ...FILM, "wan-resident-gb": "0" }, { say: (l) => plain.push(l) });
  assert.doesNotMatch(plain[0], /sage/);
});

// A guard, green before and after: under sage the film clip with no resident share fits the storage card the film session measured (14,182 MiB = 13.85 GiB
// in use inside the 13.4 + 0.8 the placement assumes), so the storage constants stand.
test("sage attention at film size with no resident share fits the storage card the film session measured", () => {
  assert.equal(planWith({ ...FILM, "wan-resident-gb": "0" }, { env: SAGE_ENV }).residentGb, 0);
  assert.ok(14182 / 1024 <= M.WAN_EXPERT_GIB + M.WAN_STORAGE_OVERHEAD_GIB, "13.85 GiB measured on the storage card, 14.2 assumed");
});

// ---- sage attention and the reuse of a running ComfyUI ---------------------------------------------------------------------------------
// A kept (--keep-comfy) multicard ComfyUI keeps the flags it was launched with. A call sized for the DEFAULT attention budgets 12.2 GiB of activations at film size; an instance
// that runs sage holds 15.2, so the share the call was admitted with can run out of memory on it. That direction is refused (an instance the harness launched is replaced). The
// reverse, a kept default instance under a call sized for sage, only wastes a cold start and is reused.
test("a running ComfyUI that runs sage attention is not the placement of a call sized for the default attention, and only in that direction", () => {
  const profile = (sage) => ({ cudaDevice: "", dynamicVram: "", multicard: { cards: ["a", "b"], ...(sage === undefined ? {} : { sage }) } });
  const base = ["main.py", "--gpu-only"];
  // the hazard: the live argv carries the flag as it was typed, an argparse prefix of it included (the predicate of the sizing)
  for (const flag of ["--use-sage-attention", "--use-sa", "--use-sage"]) {
    assert.match(L.profileMismatch([...base, flag], profile(false)), /it runs with --use-sage-attention.*this launch was sized for \(the default attention\)/, flag);
  }
  // the controls: no flag, another attention flag, an ambiguous prefix and a longer token are not sage
  assert.equal(L.profileMismatch([...base], profile(false)), "");
  for (const flag of ["--use-flash-attention", "--use-s", "--use-sage-attention-extra"]) assert.equal(L.profileMismatch([...base, flag], profile(false)), "", flag);
  // the harmless direction, and the old shape of a launch that does not say which attention it was sized for
  assert.equal(L.profileMismatch([...base, "--use-sage-attention"], profile(true)), "");
  assert.equal(L.profileMismatch([...base], profile(true)), "");
  assert.equal(L.profileMismatch([...base, "--use-sage-attention"], profile(undefined)), "");
});

test("a plan records the attention it was sized for on its launch, and ensureComfy keeps it in the launch marker's profile", async (t) => {
  assert.equal(planWith().launch.sage, false);
  assert.equal(planWith({ ...FILM, "wan-resident-gb": "0" }, { env: SAGE_ENV }).launch.sage, true);
  for (const sage of [true, false]) {
    const h = lifecycleHarness(t);
    await L.ensureComfy({ ...h.opts, launch: { ...h.launch, sage } });
    assert.deepEqual(O.readLaunchOwner(h.dir, h.launch.key).profile.multicard, { cards: [U2, U0], sage });
  }
});

test("reuseVerdict: a kept sage instance is replaced (ours) or refused (not ours) for a call sized for the default attention, and reused in the other cases", async (t) => {
  const h = lifecycleHarness(t);
  const plain = runningArgv(h);
  const sageArgv = [...plain, "--use-sage-attention"];
  const marker = (argv) => ({ pid: 5, args: argv, key: h.launch.key, port: 18852, profile: { multicard: { cards: h.launch.cards } } });
  const verdictFor = (sage, mk, argv) => L.reuseVerdict({
    api: "http://127.0.0.1:18852", comfyDir: h.dir, profile: { cudaDevice: "", dynamicVram: "", multicard: { cards: h.launch.cards, sage } }, key: h.launch.key, port: 18852,
    systemArgv: async () => argv, readLaunch: () => mk, alive: () => true,
  });
  const hazard = await verdictFor(false, marker(sageArgv), sageArgv);
  assert.equal(hazard.restart, true, "ours and not in use: replaced");
  assert.match(hazard.reason, /it runs with --use-sage-attention.*the default attention/);
  const foreign = await verdictFor(false, null, sageArgv);
  assert.equal(foreign.reuse, false, "not proven ours: refused, never stopped");
  assert.match(foreign.reason, /--use-sage-attention.*not started by this harness/);
  assert.deepEqual(await verdictFor(true, marker(sageArgv), sageArgv), { reuse: true }, "a sage call on a sage instance");
  assert.deepEqual(await verdictFor(false, marker(plain), plain), { reuse: true }, "a default call on a default instance");
  assert.deepEqual(await verdictFor(true, marker(plain), plain), { reuse: true }, "a sage call on a kept default instance wastes a cold start at most, and is reused");
});
