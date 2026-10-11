// node --test render/wan-multicard.test.mjs
// The card-only placement for Wan 2.2 (comfy-video.mjs --wan-placement multicard). Pure: the card table, the pack scan, the spawn,
// the HTTP probes and the pid checks are injected; the only real I/O is scratch folders under the OS temp dir, removed after.
import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  buildWan22I2V, WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE,
} from "./wf-wan22-i2v.mjs";
import * as M from "./wan-multicard.mjs";
import * as L from "./comfy-lifecycle.mjs";
import * as O from "./comfy-ownership.mjs";
import { withGpuSlot } from "./gpu-lock.mjs";
import { buildGraphFromArgs, verifyMulticardDevices } from "./comfy-video.mjs";

// Synthetic ids: a repeated-nibble head is a placeholder to the leak gate, and none of these names a real card.
const U0 = "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee";
const U1 = "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff";
const U2 = "GPU-cccc3333-dddd-eeee-ffff-000000000000";
const CSV = [
  `0, ${U0}, Test GPU A, 16311, 16000, Disabled, No`,
  `1, ${U1}, Test GPU B, 16303, 13800, Disabled, Yes`,
  `2, ${U2}, Test GPU A, 16311, 16050, Disabled, No`,
].join("\n");
const CARDS = M.parseCardTable(CSV);
const FP8 = { highUnet: "high_fp8.safetensors", lowUnet: "low_fp8.safetensors" };
const ARGS = { imagePath: "s.png", prompt: "p", seed: 7, width: 720, height: 1280, length: 81, ...FP8 };

const scratch = [];
const tmp = () => { const d = mkdtempSync(join(tmpdir(), "wan-mc-")); scratch.push(d); return d; };
after(() => { for (const d of scratch) rmSync(d, { recursive: true, force: true }); });

const lines = () => { const got = []; return { got, warn: (l) => got.push(l) }; };
const pair = () => M.resolveWanCards({ tokens: [U2, U0], source: "the GPU lease", cards: CARDS });

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
  assert.deepEqual(M.parseCardTable("garbage\n\nindex, uuid, name"), [], "headers and noise are skipped");
});

test("queryCards asks for display_attached and falls back to display_active alone on a driver that refuses it", () => {
  const asked = [];
  const spawn = (cmd, args) => {
    asked.push(args[0]);
    return /display_attached/.test(args[0]) ? { status: 2, stdout: "" } : { status: 0, stdout: `0, ${U0}, X, 100, 50, Enabled` };
  };
  const cards = M.queryCards(spawn);
  assert.equal(asked.length, 2);
  assert.equal(cards[0].display, true);
  assert.throws(() => M.queryCards(() => ({ status: 1, stdout: "" })), /nvidia-smi gave no card table/);
  assert.throws(() => M.queryCards(() => ({ status: 0, stdout: "no cards here" })), /listed no card/);
});

test("wanCardTokens: --wan-cards, then the lease, then a UUID CUDA_VISIBLE_DEVICES; nothing says nothing", () => {
  const none = () => [];
  assert.deepEqual(M.wanCardTokens({ flags: { "wan-cards": "2, 0" }, env: { GPU_LEASE_DEVICES: "gpu-x" }, leaseDevicesFn: () => ["gpu-x"] }), { tokens: ["2", "0"], source: "--wan-cards" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: {}, leaseDevicesFn: () => ["gpu-aaaa1111", "gpu-cccc3333"] }), { tokens: ["gpu-aaaa1111", "gpu-cccc3333"], source: "the GPU lease" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: { CUDA_VISIBLE_DEVICES: `${U2},${U0}` }, leaseDevicesFn: none }), { tokens: [U2, U0], source: "CUDA_VISIBLE_DEVICES" });
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: { CUDA_VISIBLE_DEVICES: "0,2" }, leaseDevicesFn: none }), { tokens: [], source: "" }, "bare indices are no evidence: their order is not the driver's");
  assert.deepEqual(M.wanCardTokens({ flags: {}, env: {}, leaseDevicesFn: none }), { tokens: [], source: "" });
});

test("resolveWanCards: indices, UUIDs, UUID prefixes and lease ids all name the card; the second of two equal cards computes", () => {
  const ids = (tokens) => { const r = M.resolveWanCards({ tokens, source: "t", cards: CARDS }); return [r.storage.index, r.compute.index]; };
  assert.deepEqual(ids(["0", "2"]), [0, 2], "equal cards keep the given order: the second computes");
  assert.deepEqual(ids(["2", "0"]), [2, 0]);
  assert.deepEqual(ids([U2, U0]), [2, 0]);
  assert.deepEqual(ids(["gpu-cccc3333-dddd-eeee-ffff-000000000000", "gpu-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee"]), [2, 0], "the lease's own spelling");
  assert.deepEqual(ids(["cccc3333", "GPU-AAAA1111"]), [2, 0], "a prefix, either case");
  assert.deepEqual(ids(["0", U0, "2"]), [0, 2], "a card named twice counts once");
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
  assert.equal(launch.key, "mc-cccc3333-aaaa1111");
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
  const dev = (mib, type = "cuda") => ({ type, vram_total: mib * 1024 ** 2, name: "n" });
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

test("planMulticard: null for single without reading any card; a plan for multicard; refusals before anything starts", () => {
  const boom = () => { throw new Error("must not be read"); };
  assert.equal(M.planMulticard({ flags: {}, comfyDir: "/c", isWanGraph: true, cards: boom }), null);
  assert.equal(M.planMulticard({ flags: { "wan-placement": "single" }, comfyDir: "/c", isWanGraph: false, cards: boom }), null, "single on any graph is today's path");
  const said = [];
  const plan = M.planMulticard({
    flags: { "wan-placement": "multicard", "wan-resident-gb": "4" }, env: { GPU_LEASE_DEVICES: `gpu-cccc3333-dddd-eeee-ffff-000000000000,gpu-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee`, COMFY_DYNAMIC_VRAM: "on" },
    comfyDir: "/c", isWanGraph: true, cards: () => CARDS, launchFn: (p) => M.multicardLaunch(p, { comfyDir: "/c", packDirsFn: () => ["ComfyUI-MultiGPU", "ComfyUI-VideoHelperSuite"] }), say: (l) => said.push(l),
  });
  assert.equal(plan.residentGb, 4);
  assert.equal(plan.pair.storage.index, 2);
  assert.equal(plan.pair.compute.index, 0);
  assert.equal(plan.env.COMFY_DYNAMIC_VRAM, "");
  assert.equal(said.length, 1);
  assert.match(said[0], /^wan-placement: multicard on the GPU lease: storage cuda:0 = idx 2 .*; compute cuda:1 = idx 0 .*; 4 GiB of each expert resident/);
  const refuse = (over, re) => assert.throws(() => M.planMulticard({ flags: { "wan-placement": "multicard" }, env: {}, comfyDir: "/c", isWanGraph: true, cards: () => CARDS, ...over }), re);
  refuse({ isWanGraph: false, flags: { "wan-placement": "multicard", model: "ltx25" } }, /places the Wan 2\.2 graph.*--model ltx25/);
  refuse({ isWanGraph: false, flags: { "wan-placement": "multicard", graph: "g.json" } }, /--graph file/);
  refuse({}, /needs the two cards it runs on/);
  refuse({ env: { GPU_LEASE_DEVICES: `${U0},${U1}` } }, /drives a display/);
  refuse({ flags: { "wan-placement": "multicard", "wan-resident-gb": "x" } }, /--wan-resident-gb/);
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

function lifecycleHarness({ up = async () => false } = {}) {
  const dir = tmp();
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

test("ensureComfy with a multicard launch: the placement's argv, the two uuids storage first in the child env, the reader on, a marker with the pair", async () => {
  const h = lifecycleHarness();
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

test("ensureComfy without a launch leaves the child env to envFor, exactly as before", async () => {
  const h = lifecycleHarness();
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

test("reuseVerdict: a running ComfyUI is reused only when the launch marker proves it is ours, on this pair, in this order", async () => {
  const h = lifecycleHarness();
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
  const dev = (mib) => ({ type: "cuda", vram_total: mib * 1024 ** 2, name: "n" });
  const reply = (devices) => async () => ({ ok: true, json: async () => ({ devices }) });
  const plan = { pair: pair() };
  await verifyMulticardDevices("http://x", plan, { fetchImpl: reply([dev(16311), dev(16311)]) });
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: reply([dev(16311)]) }), /WAN-PLACEMENT-MISMATCH: ComfyUI sees 1 CUDA device.*Nothing was submitted/);
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: async () => ({ ok: false, status: 500 }) }), /WAN-PLACEMENT-MISMATCH: GET \/system_stats gave no device list \(HTTP 500\)/);
  await assert.rejects(verifyMulticardDevices("http://x", plan, { fetchImpl: async () => { throw new Error("refused"); } }), /gave no device list \(refused\)/);
});
