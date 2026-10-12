// wan-multicard.mjs — the card-only placement for Wan 2.2 14B fp8 I2V (comfy-video.mjs --wan-placement multicard).
//
// WHAT IT IS. The two experts (13.3 GiB each) do not fit one 16 GiB card with the activations of a film-size
// clip (720x1280x81 needs about 9.7-11 GiB beyond the weights), and the host-RAM way out (ComfyUI's dynamic-VRAM
// streaming) costs the host the whole file. This placement uses TWO cards instead and no host RAM:
//   cuda:0 = the STORAGE card: text encoder, VAE and the donor blocks of the expert in use (about 10.3 GiB);
//   cuda:1 = the COMPUTE card: a resident share of the expert (3 GiB) plus the sampler's activations; the donor
//            blocks stream to it one at a time over the card-to-card copy (about 1.7 s of a 250 s step).
// Measured on two RTX 5060 Ti 16 GB at the film's geometry (the 2026-10-11 benchmark, a 4-step run of the 20-step film
// recipe; docs/systems/media-generation.md, "Card-only Wan"): compute card 13.3 GiB, storage card 11.1 GiB peak, host
// growth 2.3 GiB over ComfyUI's idle.
//
// WHAT THIS FILE DECIDES, all of it before ComfyUI starts, so a wrong card is a refusal and not a render:
//   - which two cards (the lease's: GPU_LEASE_DEVICES, else the lease record; --wan-cards names them only when no lease
//     does, and under a lease it can only confirm them), read from the driver's card table; neither may drive a display,
//     and a card whose display state the driver does not report is refused too;
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

const OWN_FLAGS = Object.freeze(["wan-placement", "wan-resident-gb", "wan-cards"]);

/**
 * refuseEqualsSpelling: this runner's parser (comfy-video.mjs parseArgs) does not split `--flag=value`: it reads the whole thing as
 * one flag NAME, takes the next argument as its value and leaves the real value behind as a positional. The Go side that sizes the
 * lease does split it, so `--wan-placement=multicard` would declare no host RAM and then run the single-card placement, which
 * streams the whole set through RAM. Two spellings lose the placement's flags, and both are refused before anything starts: one of
 * the placement's own flags spelled with '=', and any other flag spelled with '=' that swallowed one of ours as its value
 * (`--seed=7 --wan-placement multicard`). A `=` key that swallowed something else is none of this placement's business and passes.
 */
export function refuseEqualsSpelling(flags, ownFlags = OWN_FLAGS) {
  for (const [key, value] of Object.entries(flags || {})) {
    const own = ownFlags.find((f) => key.startsWith(f + "="));
    if (own) {
      throw new Error(`--${key} is spelled with '=', which this runner's parser does not split (it reads the whole thing as one flag name, takes the next flag as its value, and the call would run the single-card placement): write --${own} ${key.slice(own.length + 1)}`);
    }
    const lost = typeof value === "string" ? ownFlags.find((f) => value === "--" + f) : undefined;
    if (lost) {
      throw new Error(`--${key} took --${lost} as its value (this runner's parser does not split '=': a flag spelled --name=value swallows the flag after it), so the placement's own flag would be lost and the call would run the single-card placement: write each flag as --name value`);
    }
  }
}

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
 * driver does not give, or gives as something else than Enabled/Disabled and Yes/No, is "unknown": `display` stays false (an
 * unknown is never a yes) and `displayUnknown` is true, which resolveWanCards refuses for a chosen card. A row whose size is not
 * a number is no card (a size check on NaN would pass); a free column that is not a number reads as 0.
 */
export function parseCardTable(text) {
  const cards = [];
  for (const raw of String(text || "").split(/\r?\n/)) {
    const cols = raw.split(",").map((c) => c.trim());
    if (cols.length < 5 || !/^\d+$/.test(cols[0]) || !/^GPU-/i.test(cols[1])) continue;
    const totalMiB = Number(cols[3]), freeMiB = Number(cols[4]);
    if (!Number.isFinite(totalMiB) || totalMiB <= 0) continue;
    const active = (cols[5] || "").toLowerCase(), attached = (cols[6] || "").toLowerCase();
    cards.push({
      index: Number(cols[0]), uuid: cols[1], name: cols[2],
      totalMiB, freeMiB: Number.isFinite(freeMiB) ? freeMiB : 0,
      display: active === "enabled" || attached === "yes",
      displayUnknown: !(["enabled", "disabled"].includes(active) && ["yes", "no"].includes(attached)),
      displayActive: cols[5] || "", displayAttached: cols[6] || "",
    });
  }
  return cards;
}

/**
 * queryCards asks the driver for the card table. A failed query is asked once more before a driver is taken to lack
 * display_attached (a transient failure is not a missing column); the fallback without it leaves every card's display state
 * unknown, which the selection refuses.
 */
export function queryCards(spawnSync = nodeSpawnSync) {
  const ask = (fields) => spawnSync("nvidia-smi", [`--query-gpu=${fields}`, "--format=csv,noheader,nounits"], { encoding: "utf8", timeout: 30000, windowsHide: true });
  const failed = (r) => !r || r.status !== 0 || !r.stdout;
  let r = ask(QUERY);
  if (failed(r)) r = ask(QUERY);
  if (failed(r)) r = ask("index,uuid,name,memory.total,memory.free,display_active");
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
 *   --wan-cards a,b                     the caller's word (a card index as nvidia-smi counts, or a UUID / UUID prefix); under a
 *                                       lease planMulticard checks that the lease holds them, so it can only confirm, never replace;
 *   GPU_LEASE_DEVICES, else the lease's own record   the cards the lease holds (gpu-lock.mjs leaseDevices);
 *   CUDA_VISIBLE_DEVICES, when it lists UUIDs        the wrapper's pin.
 * Returns { tokens, source } and no tokens when nothing says. A --wan-cards that names no card (dangling or empty) is refused,
 * not skipped: a caller who typed it meant something.
 */
export function wanCardTokens({ flags = {}, env = process.env, leaseDevicesFn = leaseDevices, cardsFlag = "wan-cards" } = {}) {
  const split = (s) => String(s || "").split(",").map((t) => t.trim()).filter(Boolean);
  const given = split(flags[cardsFlag]);
  if (cardsFlag in flags && given.length === 0) {
    throw placementError(`--${cardsFlag} needs <card>,<card> (nvidia-smi indices or UUIDs), got ${JSON.stringify(flags[cardsFlag])}`);
  }
  if (given.length) return { tokens: given, source: "--" + cardsFlag };
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
export function resolveWanCards({ tokens, source, cards, cardsFlag = "wan-cards" }) {
  if (!tokens || tokens.length === 0) {
    throw placementError(`the multicard placement needs the two cards it runs on, and none was named: run it under \`local-offload gpu reserve --devices <card>,<card>\` (the lease names them), or pass --${cardsFlag} <card>,<card>`);
  }
  const found = [];
  for (const token of tokens) {
    const card = matchCard(token, cards);
    if (!card) throw placementError(`${source} names ${JSON.stringify(token)}, which is no card in the driver's table (${cards.map((c) => `${c.index}:${c.uuid}`).join(", ")}): name the card by its nvidia-smi index or its UUID`);
    if (!found.includes(card)) found.push(card);
  }
  if (found.length !== 2) {
    throw placementError(`the multicard placement runs on exactly two cards, and ${source} names ${found.length} (${found.map((c) => `idx ${c.index}`).join(", ")}): lease the two non-display cards`);
  }
  const screen = found.filter((c) => c.display);
  if (screen.length) {
    throw placementError(`card idx ${screen[0].index} (${screen[0].name}) drives a display, and the multicard placement never uses the display card: lease the two non-display cards`);
  }
  const unsure = found.filter((c) => c.displayUnknown);
  if (unsure.length) {
    throw placementError(`cannot tell whether idx ${unsure[0].index} (${unsure[0].name}) drives a display (the driver reported display_active ${unsure[0].displayActive || "nothing"}, display_attached ${unsure[0].displayAttached || "nothing"}), and the multicard placement never uses a display card, so it will not guess: lease two cards whose display state the driver reports`);
  }
  const [first, second] = found;
  const [storage, compute] = second.totalMiB >= first.totalMiB ? [first, second] : [second, first];
  return { storage, compute, source };
}

// ---- the ComfyUI launch ---------------------------------------------------------------------------

/** The packs the image and edit recipes load (render/multicard-image.mjs), besides the harness's own. */
export const PACK_MULTIGPU = Object.freeze({ match: "comfyui-multigpu", why: "the DisTorch2 and MultiGPU loaders" });
export const PACK_GGUF = Object.freeze({ match: "comfyui-gguf", why: "the GGUF unet loader the DisTorch2 GGUF loader wraps" });

/** packDirs finds the custom-node folders a multicard launch must load, by name, in the install's custom_nodes (the Wan pair by default). */
export function packDirs(comfyDir, list = (d) => readdirSync(d, { withFileTypes: true }).filter((e) => e.isDirectory()).map((e) => e.name), required = REQUIRED_PACKS) {
  const dir = join(comfyDir, "custom_nodes");
  let names, why = "";
  try { names = list(dir); } catch (e) { names = []; why = ` (the listing failed: ${e && e.message ? e.message : e})`; }
  const out = [];
  for (const need of required) {
    const hit = names.find((n) => n.toLowerCase() === need.match);
    if (!hit) throw placementError(`ComfyUI has no ${need.match} pack in ${dir}${why} (${need.why}); the multicard placement needs it`);
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
    if (!Number.isFinite(gib) || !Number.isFinite(expect) || Math.abs(gib - expect) > 0.05 * expect) {
      problems.push(`${label} should be idx ${card.index} (${expect.toFixed(1)} GiB) but ComfyUI reports ${Number.isFinite(gib) ? gib.toFixed(1) + " GiB" : "no size"}${got.name ? ` (${got.name})` : ""}`);
    }
  });
  return problems;
}

/**
 * refuseUnmeasuredWan: the builder (wf-wan22-i2v.mjs) refuses --fast and a GGUF expert under multicard, and its defaults for an
 * expert nobody named are GGUF files, so a call without --high-unet / --low-unet would be refused too. It does that after the
 * lease is taken, llama-swap is unloaded and ComfyUI has cold-started; the same refusals are made here, before any of it.
 */
function refuseUnmeasuredWan(flags) {
  if (flags.fast) throw new Error("--wan-placement multicard was measured on the native recipe only: drop --fast (or use --wan-placement single)");
  for (const flag of ["high-unet", "low-unet"]) {
    const name = flags[flag];
    if (typeof name !== "string" || !name.trim()) {
      throw new Error(`--wan-placement multicard needs --${flag} to name the fp8 safetensors expert (the builder's default experts are GGUF files, which fit one card: use --wan-placement single for those)`);
    }
    if (/\.gguf$/i.test(name.trim())) {
      throw new Error(`--wan-placement multicard was measured with the fp8 safetensors experts, and --${flag} ${name} is a GGUF file (it fits one card: use --wan-placement single)`);
    }
  }
}

/**
 * The Wan 2.2 sampler's tokens for a clip, from the flags the builder reads (defaults 832x480x49): the latent is 1/8 per side, the patches
 * 2x2 and a latent frame holds 4 frames, so the film's 720x1280x81 is 45 x 80 x 21 = 75,600.
 */
export function wanGeometry(flags) {
  const w = Number(flags.width) || 832, h = Number(flags.height) || 480, f = Number(flags.frames) || 49;
  return { w, h, f, tokens: Math.ceil(w / 16) * Math.ceil(h / 16) * (Math.floor((f - 1) / 4) + 1) };
}
const FILM_TOKENS = 75600;
/** What the sampler needs on the compute card beyond the resident weights at the film's size: the benchmark's compute-card peak, 13.3 GiB, less its 3 GiB resident share. */
const FILM_ACTIVATIONS_GIB = 10.3;
/** Scaled with the tokens, never under this: even a small clip holds a context's worth of buffers. */
const ACTIVATIONS_FLOOR_GIB = 2.0;
/** The CUDA context and the allocator's slack on the compute card, beyond the activations. */
const COMPUTE_CARD_CONTEXT_GIB = 0.5;

/**
 * checkResident: a resident share larger than the compute card can hold beside the sampler's activations at this geometry would
 * run out of memory in the first sampler step, 80 minutes into a film-size run's lease. The estimate is the measured compute-card
 * peak scaled by the tokens (floor 2 GiB), so the film's own 3 GiB passes with room and 6 does not; the lease sizing in
 * internal/hostneed is stricter on purpose (it only decides whether a call needs host RAM declared).
 */
function checkResident(residentGb, pair, flags) {
  const cardGib = pair.compute.totalMiB / 1024;
  const { w, h, f, tokens } = wanGeometry(flags);
  const activations = Math.max(ACTIVATIONS_FLOOR_GIB, FILM_ACTIVATIONS_GIB * tokens / FILM_TOKENS);
  const most = cardGib - activations - COMPUTE_CARD_CONTEXT_GIB;
  if (residentGb > most) {
    throw placementError(`--wan-resident-gb ${residentGb} leaves the compute card (idx ${pair.compute.index}, ${cardGib.toFixed(1)} GiB) about ${activations.toFixed(1)} GiB for the sampler's activations at ${w}x${h}x${f} (${tokens} tokens) and ${COMPUTE_CARD_CONTEXT_GIB} GiB for the CUDA context: at most ${most.toFixed(1)} GiB of an expert can stay resident there (the measured film-size recipe keeps ${WAN_RESIDENT_GB_DEFAULT})`);
  }
}

/** confirmWithLease: --wan-cards (or the image runners' --cards) may name the two cards when no lease does; when a lease holds cards it can only confirm them. */
export function confirmWithLease(pair, held, cardsFlag = "wan-cards") {
  if (!held || held.length === 0) return;
  const outside = [pair.storage, pair.compute].filter((c) => !held.some((id) => matchCard(id, [c]) === c));
  if (outside.length) {
    throw placementError(`--${cardsFlag} names idx ${outside.map((c) => c.index).join(", ")}, which the GPU lease does not hold (it holds ${held.join(", ")}): the lease is the authority on which cards this run may touch, so --${cardsFlag} can only confirm them`);
  }
}

/**
 * portNote: a multicard instance with no --api and no COMFY_API listens where its instance key falls back to (COMFY_PORT_BASE + index,
 * the port the per-card instances use too). A ComfyUI already holding it stops the run, closed (COMFY-PORT-TAKEN / COMFY-PROFILE-MISMATCH),
 * so the caller is told which port it got and how to pick its own. "" when the caller gave one.
 */
export function portNote(flags, env, api, label = "wan-placement") {
  if ((flags && flags.api) || (env && env.COMFY_API)) return "";
  return `${label}: no --api or COMFY_API was given, so the multicard instance falls back to ${api}; a ComfyUI already holding that port stops the run (COMFY-PORT-TAKEN or COMFY-PROFILE-MISMATCH): give it its own port with --api http://127.0.0.1:<port>`;
}

/**
 * planMulticard is the one call the runner makes before anything starts: null for the single-card path (the default, which
 * reads no card table and touches nothing), otherwise the whole plan of the card-only placement. It refuses, with the reason,
 * every request it cannot honour: the placement's own flags spelled with '=', a graph that is not the Wan 2.2 one, --fast or a
 * GGUF / unnamed expert, no cards named, not two cards, a card the lease does not hold, a display card or one whose display
 * state is unknown, a resident share the compute card cannot hold, a missing pack. `cards` reads the driver's card table
 * (injectable); `isWanGraph` says whether this invocation builds the Wan 2.2 graph; `leaseDevicesFn` names the cards the lease holds.
 */
export function planMulticard({ flags, env = process.env, comfyDir, isWanGraph, cards = queryCards, launchFn = multicardLaunch, leaseDevicesFn = leaseDevices, say = () => {} }) {
  refuseEqualsSpelling(flags);
  const mode = wanPlacementMode(flags);
  if (mode !== "multicard") return null;
  if (!isWanGraph) {
    throw new Error(`--wan-placement multicard places the Wan 2.2 graph; this invocation builds ${flags.graph ? "a --graph file" : `--model ${flags.model}`}, which it does not touch`);
  }
  refuseUnmeasuredWan(flags);
  const residentGb = wanResidentGb(flags);
  const { tokens, source } = wanCardTokens({ flags, env, leaseDevicesFn });
  const pair = resolveWanCards({ tokens, source, cards: cards() });
  if (source === "--wan-cards") confirmWithLease(pair, leaseDevicesFn(env));
  checkResident(residentGb, pair, flags);
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

// ---- the console watch ----------------------------------------------------------------------------

/**
 * What ComfyUI's console says when its prompt worker is gone after running out of memory. The benchmark's full-size probe log shows the
 * sequence: the node's error handler logs "Got an OOM, unloading all loaded models", the unload runs out of memory as well, and the worker
 * thread dies ("Exception in thread Thread-5 (prompt_worker)"); /history then lists nothing, and /system_stats and /queue go on answering.
 * Only these two lines mean that. The same log's "FP8 _scaled_mm failed ... falling back to dequantization" is a warning ComfyUI recovers from,
 * sd.py's "Ran out of memory when regular VAE decoding, retrying" is a recovery too, and "!!! Exception during processing !!!" is a node error
 * that reaches /history, where the poll reads it: none of them is matched, because ending a healthy run on them would lose hours of sampling.
 */
const OOM_WEDGE_PATTERNS = Object.freeze([
  /Got an OOM, unloading all loaded models/,
  /Exception in thread [^\r\n]*\(prompt_worker\)/,
]);
const ANSI_CODES = /\u001b\[[0-9;]*[A-Za-z]/g;

/** oomWedgeLine: the first console line (colour codes removed, trimmed) that says the prompt worker is gone, or "". */
export function oomWedgeLine(text) {
  for (const raw of String(text || "").replace(ANSI_CODES, "").split(/\r?\n/)) {
    const line = raw.trim();
    if (OOM_WEDGE_PATTERNS.some((p) => p.test(line))) return line;
  }
  return "";
}

/**
 * createOomWatch: the `watch` pollOutputs asks while the prompt runs (comfy-submit.mjs). `cursor` is comfyLogCursor's reader of this
 * instance's console, made before the prompt was submitted. Returns null while the console is quiet, and the message that ends the wait when
 * it shows the worker gone. A console it cannot see (no file, or the capture's cap reached) is said once through `say` and is never a verdict:
 * the wait is then bounded by its budget alone, as it was.
 */
export function createOomWatch({ cursor, say = () => {} }) {
  let saidAbsent = false, saidCapped = false;
  let fellBack = false, capped = false, last = "ok";
  // one read of the console, remembering what a later read must not forget: a plain-decode retry seen once is seen, and a capture that reached
  // its cap is blind for the rest of the run
  const step = () => {
    const r = cursor.read();
    last = r.state;
    if (r.state === "capped") capped = true;
    if (decodeFellBack(r.text)) fellBack = true;
    return r;
  };
  const watch = () => {
    const { text, state } = step();
    const line = oomWedgeLine(text);
    if (line) {
      return `WAN-OOM: ComfyUI ran out of memory and its prompt worker did not survive it (its console says "${line}"), so this prompt will never finish and will never be reported as failed; the wait ends here instead of holding the two leased cards for the rest of its budget. ComfyUI must be restarted; its console is ${cursor.path}.`;
    }
    if (state === "absent" && !saidAbsent) {
      saidAbsent = true;
      say(`WAN-OOM-WATCH: ${cursor.path} does not exist (no console file), so an out-of-memory wedge of ComfyUI cannot be seen from here; the wait is bounded by its budget alone`);
    } else if (state === "capped" && !saidCapped) {
      saidCapped = true;
      say(`WAN-OOM-WATCH: the console capture ${cursor.path} is capped (5 MB), so nothing ComfyUI says from here on can be seen; an out-of-memory wedge would be bounded by the wait budget alone`);
    }
    return null;
  };
  /** drain: read what the console still holds (one read takes at most 8 MiB), so the verdict covers the whole run. */
  watch.drain = () => { for (let i = 0; i < 64; i++) { if (!step().text) break; } };
  /**
   * verdict, after the run: "fallback" when ComfyUI retried the plain decode tiled (seen on any read), "blind" when the console could not be read
   * to the end (missing, or the capture reached its cap), else "clean". A verdict of "clean" is only as good as the console it was read from.
   */
  Object.defineProperty(watch, "verdict", { get: () => (fellBack ? "fallback" : capped || last === "absent" ? "blind" : "clean") });
  return watch;
}

export { WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE };
