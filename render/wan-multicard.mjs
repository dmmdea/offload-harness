// wan-multicard.mjs — the card-only placement for Wan 2.2 14B fp8 I2V (comfy-video.mjs --wan-placement multicard).
//
// WHAT IT IS. The two experts (13.3 GiB each) do not fit one 16 GiB card with the activations of a film-size
// clip (720x1280x81 needs about 9.7-11 GiB beyond the weights), and the host-RAM way out (ComfyUI's dynamic-VRAM
// streaming) costs the host the whole file. This placement uses TWO cards instead and no host RAM:
//   cuda:0 = the STORAGE card: text encoder, VAE and the donor blocks of the expert in use (about 10.3 GiB);
//   cuda:1 = the COMPUTE card: a resident share of the expert (3 GiB) plus the sampler's activations; the donor
//            blocks stream to it one at a time over the card-to-card copy (about 1.7 s of a 250 s step).
// Measured on two RTX 5060 Ti 16 GB at the film's geometry: compute card 13.3 GiB, storage card 11.1 GiB peak, host
// growth 2.3 GiB over ComfyUI's idle (2026-10-11 benchmark; docs/systems/media-generation.md, "Card-only Wan").
//
// WHAT THIS FILE DECIDES, all of it before ComfyUI starts, so a wrong card is a refusal and not a render:
//   - which two cards (the lease's: GPU_LEASE_DEVICES, else the lease record, else --wan-cards), read from the
//     driver's card table, never typed in; neither may drive a display;
//   - which of them is cuda:0 and which cuda:1 (the larger card computes; equal cards keep the lease's order);
//   - the ComfyUI launch that placement needs (flags, env, instance key), as plain data for ensureComfy;
//   - the DisTorch2 allocation string.
// Dependency-free; the card table and the pack scan are injectable for tests.
import { spawnSync as nodeSpawnSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { leaseDevices } from "./gpu-lock.mjs";
import { WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE } from "./wf-wan22-i2v.mjs";

/** Where the harness-owned ComfyUI-side code lives (next to this file): the yaml ComfyUI is pointed at, and the node folder it names. */
export const EXT_DIR = join(dirname(fileURLToPath(import.meta.url)), "comfyui-ext");
export const EXT_YAML = join(EXT_DIR, "extra_model_paths.yaml");
export const EXT_NODE_PACK = "offload_multicard";

/** The packs ComfyUI loads on a multicard launch, besides the harness's own: matched by name, case-insensitively. */
const REQUIRED_PACKS = Object.freeze([
  { match: "comfyui-multigpu", why: "the DisTorch2 and MultiGPU loaders" },
  { match: "comfyui-videohelpersuite", why: "VHS_VideoCombine, which writes the mp4" },
]);

const placementError = (msg) => new Error("WAN-PLACEMENT: " + msg);

/** wanPlacementMode parses --wan-placement: absent is the single-card path, today's, byte for byte; anything else is refused. */
export function wanPlacementMode(flags) {
  if (!("wan-placement" in flags)) return WAN_PLACEMENT_DEFAULT;
  const raw = flags["wan-placement"];
  if (!WAN_PLACEMENTS.includes(raw)) {
    throw new Error(`--wan-placement must be ${WAN_PLACEMENTS.join("|")}, got ${JSON.stringify(raw)}`);
  }
  return raw;
}

/** wanResidentGb parses --wan-resident-gb: GiB of each expert kept on the compute card (default the measured 3). 0 is allowed (all weights on the storage card). */
export function wanResidentGb(flags) {
  if (!("wan-resident-gb" in flags)) return WAN_RESIDENT_GB_DEFAULT;
  const raw = flags["wan-resident-gb"];
  const n = Number(raw);
  if (raw === undefined || raw === "" || !Number.isFinite(n) || n < 0 || n > 64) {
    throw new Error(`--wan-resident-gb must be a number of GiB from 0 to 64, got ${JSON.stringify(raw)}`);
  }
  return n;
}

// ---- the card table -------------------------------------------------------------------------------

const QUERY = "index,uuid,name,memory.total,memory.free,display_active,display_attached";

/**
 * parseCardTable reads `nvidia-smi --query-gpu=index,uuid,name,memory.total,memory.free,display_active,display_attached
 * --format=csv,noheader,nounits`. A card drives a display when display_active is Enabled OR display_attached is Yes (the
 * harness's own rule, internal/gpuprobe/display.go: a monitor on the card counts with the screen asleep too). A column the
 * driver does not give is "unknown", and an unknown is never a yes.
 */
export function parseCardTable(text) {
  const cards = [];
  for (const raw of String(text || "").split(/\r?\n/)) {
    const cols = raw.split(",").map((c) => c.trim());
    if (cols.length < 5 || !/^\d+$/.test(cols[0]) || !/^GPU-/i.test(cols[1])) continue;
    const active = (cols[5] || "").toLowerCase(), attached = (cols[6] || "").toLowerCase();
    cards.push({
      index: Number(cols[0]), uuid: cols[1], name: cols[2],
      totalMiB: Number(cols[3]), freeMiB: Number(cols[4]),
      display: active === "enabled" || attached === "yes",
    });
  }
  return cards;
}

/** queryCards asks the driver for the card table; a driver that rejects display_attached is asked again without it. */
export function queryCards(spawnSync = nodeSpawnSync) {
  const ask = (fields) => spawnSync("nvidia-smi", [`--query-gpu=${fields}`, "--format=csv,noheader,nounits"], { encoding: "utf8", timeout: 30000 });
  let r = ask(QUERY);
  if (!r || r.status !== 0 || !r.stdout) r = ask("index,uuid,name,memory.total,memory.free,display_active");
  if (!r || r.status !== 0 || !r.stdout) {
    throw placementError("nvidia-smi gave no card table, so the two cards cannot be told from the display card" + (r && r.error ? ` (${r.error.message})` : ""));
  }
  const cards = parseCardTable(r.stdout);
  if (cards.length === 0) throw placementError("nvidia-smi listed no card");
  return cards;
}

// ---- which cards ----------------------------------------------------------------------------------

/**
 * wanCardTokens: the cards this run is allowed, as the tokens that name them, strongest source first:
 *   --wan-cards a,b                     the caller's word (a card index as nvidia-smi counts, or a UUID / UUID prefix);
 *   GPU_LEASE_DEVICES, else the lease's own record   the cards the lease holds (gpu-lock.mjs leaseDevices);
 *   CUDA_VISIBLE_DEVICES, when it lists UUIDs        the wrapper's pin.
 * Returns { tokens, source } and no tokens when nothing says.
 */
export function wanCardTokens({ flags = {}, env = process.env, leaseDevicesFn = leaseDevices } = {}) {
  const split = (s) => String(s || "").split(",").map((t) => t.trim()).filter(Boolean);
  const given = split(flags["wan-cards"]);
  if (given.length) return { tokens: given, source: "--wan-cards" };
  const held = leaseDevicesFn(env);
  if (held.length) return { tokens: held, source: "the GPU lease" };
  const visible = split(env.CUDA_VISIBLE_DEVICES).filter((t) => /^GPU-/i.test(t));
  if (visible.length) return { tokens: visible, source: "CUDA_VISIBLE_DEVICES" };
  return { tokens: [], source: "" };
}

function matchCard(token, cards) {
  const t = token.trim().toLowerCase();
  if (/^\d+$/.test(t)) return cards.find((c) => c.index === Number(t)) || null;
  // a lease id is the lower-cased UUID with "gpu-" in front; a UUID prefix of 8+ characters names one card
  const bare = t.replace(/^gpu-/, "");
  if (bare.length < 8) return null;
  const hits = cards.filter((c) => c.uuid.toLowerCase().replace(/^gpu-/, "").startsWith(bare));
  return hits.length === 1 ? hits[0] : null;
}

/**
 * resolveWanCards turns the tokens into the two cards and their roles. It refuses (with the reason) when there are not
 * exactly two distinct cards, when a token names no card of the driver's table, or when a card drives a display.
 * Roles: the larger card is the compute card (cuda:1); equal cards keep the order the caller gave (the second computes).
 */
export function resolveWanCards({ tokens, source, cards }) {
  if (!tokens || tokens.length === 0) {
    throw placementError("the multicard placement needs the two cards it runs on, and none was named: run it under `local-offload gpu reserve --devices <card>,<card>` (the lease names them), or pass --wan-cards <card>,<card>");
  }
  const found = [];
  for (const token of tokens) {
    const card = matchCard(token, cards);
    if (!card) throw placementError(`${source} names ${JSON.stringify(token)}, which is no card in the driver's table (${cards.map((c) => `${c.index}:${c.uuid}`).join(", ")})`);
    if (!found.includes(card)) found.push(card);
  }
  if (found.length !== 2) {
    throw placementError(`the multicard placement runs on exactly two cards, and ${source} names ${found.length} (${found.map((c) => `idx ${c.index}`).join(", ")}): lease the two non-display cards`);
  }
  const screen = found.filter((c) => c.display);
  if (screen.length) {
    throw placementError(`card idx ${screen[0].index} (${screen[0].name}) drives a display, and the multicard placement never uses the display card: lease the two non-display cards`);
  }
  const [first, second] = found;
  const [storage, compute] = second.totalMiB >= first.totalMiB ? [first, second] : [second, first];
  return { storage, compute, source };
}

// ---- the ComfyUI launch ---------------------------------------------------------------------------

/** packDirs finds the custom-node folders a multicard launch must load, by name, in the install's custom_nodes. */
export function packDirs(comfyDir, list = (d) => readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name)) {
  const dir = join(comfyDir, "custom_nodes");
  let names;
  try { names = list(dir); } catch { names = []; }
  const out = [];
  for (const need of REQUIRED_PACKS) {
    const hit = names.find((n) => n.toLowerCase() === need.match);
    if (!hit) throw placementError(`ComfyUI has no ${need.match} pack in ${dir} (${need.why}); the multicard placement needs it`);
    out.push(hit);
  }
  return out;
}

/** instanceKey: the multicard instance of this pair of cards (INSTANCE_KEY_RE in comfy-lifecycle.mjs). */
export function multicardInstanceKey(pair) {
  const head = (c) => c.uuid.replace(/^GPU-/i, "").slice(0, 8).toLowerCase();
  return `mc-${head(pair.storage)}-${head(pair.compute)}`;
}

/**
 * multicardLaunch: everything ensureComfy needs to start the card-only ComfyUI, as data.
 *   cudaVisibleDevices   the two UUIDs, STORAGE FIRST: CUDA numbers the visible cards in this order, so the first is
 *                        cuda:0 whatever its nvidia-smi index (a UUID names one card in every ordering);
 *   flags                --gpu-only (dynamic VRAM off, everything on the cards), --disable-pinned-memory (two visible cards on
 *                        Windows, upstream's own guidance), the yaml that adds the harness's node folder, and the custom-node
 *                        whitelist the measurement ran with (the packs found by name + the harness's own);
 *   env                  OFFLOAD_RO_LOAD=1 turns the read-only safetensors reader on inside that ComfyUI;
 *   key                  the instance key, so it has its own port, directories and launch marker.
 * The flags join the harness's own (--disable-smart-memory --cache-none --reserve-vram), which the measured recipe also used.
 */
export function multicardLaunch(pair, { comfyDir, packDirsFn = packDirs, yaml = EXT_YAML } = {}) {
  if (!comfyDir) throw placementError("COMFY_DIR is not set, so the custom-node packs the placement needs cannot be found");
  if (!existsSync(yaml)) throw placementError(`the harness's ComfyUI extension file is missing: ${yaml}`);
  const packs = packDirsFn(comfyDir);
  return {
    key: multicardInstanceKey(pair),
    cards: [pair.storage.uuid, pair.compute.uuid],
    cudaVisibleDevices: `${pair.storage.uuid},${pair.compute.uuid}`,
    flags: ["--gpu-only", "--disable-pinned-memory", "--extra-model-paths-config", yaml,
      "--disable-all-custom-nodes", "--whitelist-custom-nodes", ...packs, EXT_NODE_PACK],
    env: { CUDA_DEVICE_ORDER: "PCI_BUS_ID", OFFLOAD_RO_LOAD: "1" },
  };
}

/**
 * multicardEnv: the environment ensureComfy launches with. The film's driver (and any caller) may carry settings that
 * contradict a two-card --gpu-only launch: a single card pin (COMFY_CARD_UUID, COMFY_CUDA_DEVICE) and dynamic VRAM
 * (COMFY_DYNAMIC_VRAM=on). They are dropped here, for the launch only, and the instance key is set so the instance has
 * its own port (the caller's COMFY_API when it gave one), directories and marker.
 */
export function multicardEnv(env, launch) {
  const out = { ...env, COMFY_INSTANCE: launch.key, COMFY_CARD_UUID: "", COMFY_CUDA_DEVICE: "", COMFY_DYNAMIC_VRAM: "" };
  delete out.CUDA_VISIBLE_DEVICES; // ensureComfy sets it from the launch
  return out;
}

/**
 * checkComfyDevices compares what the running ComfyUI reports (GET /system_stats devices) with the two cards it was
 * pinned to: cuda:0 must be the storage card and cuda:1 the compute card. ComfyUI numbers its cards in the order of
 * CUDA_VISIBLE_DEVICES; a count or a size that does not match means the pin did not take. Returns the problems found.
 */
export function checkComfyDevices(devices, pair) {
  const problems = [];
  const cuda = (Array.isArray(devices) ? devices : []).filter((d) => d && d.type === "cuda");
  if (cuda.length !== 2) {
    problems.push(`ComfyUI sees ${cuda.length} CUDA device(s), not the 2 it was pinned to`);
    return problems;
  }
  const want = [["cuda:0 (storage)", pair.storage], ["cuda:1 (compute)", pair.compute]];
  want.forEach(([label, card], i) => {
    const got = cuda[i];
    const gib = Number(got.vram_total) / 1024 ** 3, expect = card.totalMiB / 1024;
    if (!Number.isFinite(gib) || Math.abs(gib - expect) > 0.05 * expect) {
      problems.push(`${label} should be idx ${card.index} (${expect.toFixed(1)} GiB) but ComfyUI reports ${Number.isFinite(gib) ? gib.toFixed(1) + " GiB" : "no size"}${got.name ? ` (${got.name})` : ""}`);
    }
  });
  return problems;
}

/**
 * planMulticard is the one call the runner makes before anything starts: null for the single-card path (the default, which
 * reads no card table and touches nothing), otherwise the whole plan of the card-only placement. It refuses, with the reason,
 * every request it cannot honour: a graph that is not the Wan 2.2 one, no cards named, not two cards, a display card, a missing
 * pack. `cards` reads the driver's card table (injectable); `isWanGraph` says whether this invocation builds the Wan 2.2 graph.
 */
export function planMulticard({ flags, env = process.env, comfyDir, isWanGraph, cards = queryCards, launchFn = multicardLaunch, say = () => {} }) {
  const mode = wanPlacementMode(flags);
  if (mode !== "multicard") return null;
  if (!isWanGraph) {
    throw new Error(`--wan-placement multicard places the Wan 2.2 graph; this invocation builds ${flags.graph ? "a --graph file" : `--model ${flags.model}`}, which it does not touch`);
  }
  const residentGb = wanResidentGb(flags);
  const { tokens, source } = wanCardTokens({ flags, env });
  const pair = resolveWanCards({ tokens, source, cards: cards() });
  const launch = launchFn(pair, { comfyDir });
  const plan = { pair, launch, residentGb, env: multicardEnv(env, launch) };
  const line = (role, dev, c) => `${role} ${dev} = idx ${c.index} ${c.name} (${(c.totalMiB / 1024).toFixed(1)} GiB, ${c.uuid.slice(0, 12)})`;
  say(`wan-placement: multicard on ${source}: ${line("storage", WAN_STORAGE_DEVICE, pair.storage)}; ${line("compute", WAN_COMPUTE_DEVICE, pair.compute)}; ${residentGb} GiB of each expert resident on the compute card`);
  return plan;
}

/** decodeFellBack: did ComfyUI retry a plain decode tiled (its own OOM fallback, which chunks time)? Reads a stretch of its console. */
export function decodeFellBack(logText) {
  return /Ran out of memory when regular VAE decoding/i.test(String(logText || ""));
}

export { WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE };
