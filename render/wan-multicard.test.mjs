// node --test render/wan-multicard.test.mjs
// The card-only placement for Wan 2.2 (comfy-video.mjs --wan-placement multicard). Pure: the card table, the pack scan, the spawn,
// the HTTP probes and the pid checks are injected; the only real I/O is scratch folders (scratchDir) under the OS temp dir, gone when their test ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync, writeFileSync, appendFileSync } from "node:fs";
import { join } from "node:path";
import {
  buildWan22I2V, WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE,
} from "./wf-wan22-i2v.mjs";
import * as M from "./wan-multicard.mjs";
import * as L from "./comfy-lifecycle.mjs";
import * as O from "./comfy-ownership.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { scratchDir } from "./test-scratch.mjs";
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
  assert.throws(() => buildWan22I2V({ ...ARGS, highUnet: "h.gguf", placement: "multicard" }), /GGUF/);
  assert.throws(() => buildWan22I2V({ ...ARGS, lowUnet: "l.GGUF", placement: "multicard" }), /GGUF/);
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
  assert.deepEqual(flags, [...BASE, ...launch.flags, "--port", "18852", "--output-directory", join("/c", "instances", launch.key, "output"), "--temp-directory", join("/c", "instances", launch.key), "--x", "1"]);
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
  assert.deepEqual(args, ["main.py", ...BASE, ...h.launch.flags, "--port", "18852", "--output-directory", join(h.dir, "instances", h.launch.key, "output"), "--temp-directory", join(h.dir, "instances", h.launch.key)]);
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

test("planMulticard refuses --fast and a GGUF or unnamed expert before anything starts, not after the cold start", () => {
  assert.doesNotThrow(() => planWith());
  assert.throws(() => planWith({ fast: true }), /native recipe only.*drop --fast/);
  assert.throws(() => planWith({ "high-unet": "h.gguf" }), /--high-unet.*h\.gguf.*GGUF/);
  assert.throws(() => planWith({ "low-unet": "L.GGUF" }), /--low-unet.*L\.GGUF.*GGUF/);
  for (const flag of ["high-unet", "low-unet"]) {
    const flags = { "wan-placement": "multicard", ...EXPERTS };
    delete flags[flag];
    assert.throws(() => planWith({}, { flags }), new RegExp(`needs --${flag}.*fp8 safetensors expert.*default experts are GGUF`));
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
