// wan-multicard.mjs — the card-only placement for Wan 2.2 14B fp8 I2V and for LTX-2.5 22B int8 (comfy-video.mjs --wan-placement multicard [--model ltx25]).
//
// LTX-2.5 (--model ltx25) is the same placement with the roles' device numbers REVERSED: its int8 DiT must compute on cuda:0 (ComfyUI-MultiGPU #220), so its
// launch lists the COMPUTE card first (multicardLaunch, order "compute-first"), under an instance key of its own (multicardInstanceKey), and the graph
// (wf-ltx25-i2v.mjs) puts the DiT's resident share on cuda:0 and the donor blocks, the VAEs and half of the text encoder on cuda:1. Everything below that
// does not say Wan applies to both: the cards, the display refusal, the lease cross-check, the roles, the packs. The sizes of LTX's own stages are
// LTX_* below. One LTX-2.5 render was made on the two cards (1280x704x121, the default 9 GiB share): the compute card peaked at 11.54 GiB, the storage
// card at 12.71 GiB, 124.0 s of ComfyUI time; no larger clip was run.
//
// WHAT IT IS. The two experts (13.3 GiB each) do not fit one 16 GiB card with the activations of a film-size
// clip (720x1280x81 needs about 12.2 GiB beyond the 3 GiB of an expert kept resident: the film-size run's 15.18 GiB peak; the 4-step benchmark
// ended before that peak and read 9.7-11.0), and the host-RAM way out (ComfyUI's dynamic-VRAM
// streaming) costs the host the whole file. This placement uses TWO cards instead and no host RAM:
//   cuda:0 = the STORAGE card: text encoder, VAE and the donor blocks of the expert in use (about 10.3 GiB);
//   cuda:1 = the COMPUTE card: a resident share of the expert (3 GiB) plus the sampler's activations; the donor
//            blocks stream to it one at a time over the card-to-card copy (about 1.7 s of a 250 s step).
// Measured on two RTX 5060 Ti 16 GB at the film's geometry (docs/systems/media-generation.md, "Card-only Wan"): the
// 2026-10-11 benchmark, a 4-step run of the 20-step film recipe, peaked at 13.3 GiB on the compute card and 11.1 GiB on the
// storage card; the full 20-step clip of that day peaked at 15.18 GiB and 10.86 GiB, with 2.3 GiB of host growth over
// ComfyUI's idle. With --use-sage-attention in COMFY_EXTRA_ARGS (2026-10-12) the activations are 3 GiB larger at the film's
// size (15.2 GiB beyond the resident share, measured at resident 0): wanAttentionMode reads it and the sizing follows.
//
// WHAT THIS FILE DECIDES, all of it before ComfyUI starts, so a wrong card is a refusal and not a render:
//   - which two cards (the lease's: GPU_LEASE_DEVICES, else the lease record; --wan-cards names them only when no lease
//     does, and under a lease it can only confirm them), read from the driver's card table; neither may drive a display,
//     and a card whose display state the driver does not report is refused too;
//   - which of them is cuda:0 and which cuda:1 (chooseRoles: the larger card computes, except that of two cards within 1 percent of one size the one with the higher
//     board power computes when it is at least 1.25 times the other's; of two equal cards the lower UUID stores and the higher computes, the order a lease sorts its ids in;
//     the environment variable WAN_COMPUTE_UUID names the compute card of the pair instead);
//   - the ComfyUI launch that placement needs (flags, env, instance key), as plain data for ensureComfy;
//   - the DisTorch2 allocation string.
// It reads the lease's cards from gpu-lock.mjs and the sage-attention token rule from comfy-lifecycle.mjs, and nothing else of the harness; the card table and the
// pack scan are injectable for tests.
import { spawnSync as nodeSpawnSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { leaseDevices } from "./gpu-lock.mjs";
import { isSageAttentionToken } from "./comfy-lifecycle.mjs";
import { WAN_PLACEMENTS, WAN_PLACEMENT_DEFAULT, WAN_RESIDENT_GB_DEFAULT, WAN_STORAGE_DEVICE, WAN_COMPUTE_DEVICE } from "./wf-wan22-i2v.mjs";
import { LTX25_RESIDENT_GB_DEFAULT, LTX25_TE_ALLOCATION_DEFAULT, LTX25_COMPUTE_DEVICE, LTX25_STORAGE_DEVICE } from "./wf-ltx25-i2v.mjs";

/** Where the harness-owned ComfyUI-side code lives (next to this file): the yaml ComfyUI is pointed at, and the node folder it names. */
export const EXT_DIR = join(dirname(fileURLToPath(import.meta.url)), "comfyui-ext");
export const EXT_YAML = join(EXT_DIR, "extra_model_paths.yaml");
export const EXT_NODE_PACK = "offload_multicard";
// The idle bound's yaml and pack name, defined in comfy-lifecycle.mjs (this file imports it; it cannot import this one back).
export { EXT_IDLE_YAML, EXT_IDLE_PACK } from "./comfy-lifecycle.mjs";

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
    // The swallowed token is one of ours in either spelling: `--wan-placement` (the other flag was spelled with '=') or `--wan-placement=multicard`
    // (the lease reads that one as the flag with its value, the parser as the value of the flag before it). A token that only STARTS like ours
    // (`--wan-placements`) is not one of ours.
    const lost = typeof value === "string" ? ownFlags.find((f) => value === "--" + f || value.startsWith("--" + f + "=")) : undefined;
    if (lost) {
      throw new Error(`--${key} took ${value.split(/\s/)[0]} as its value (this runner's parser does not split '=': a flag spelled --name=value swallows the flag after it), so the placement's own flag --${lost} would be lost and the call would run the single-card placement: write each flag as --name value`);
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

/**
 * wanResidentGb parses --wan-resident-gb: GiB of each expert kept on the compute card (default the measured 3). 0 is allowed (all weights on the storage card).
 * Under --model ltx25 the same flag is the GiB of the one DiT kept on the compute card, and `dflt` is LTX's default (planMulticard).
 */
export function wanResidentGb(flags, dflt = WAN_RESIDENT_GB_DEFAULT) {
  if (!("wan-resident-gb" in flags)) return dflt;
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
 * a number is no card (a size check on NaN would pass); a free column that is not a number reads as 0. `powerLimitW` is 0 (not read)
 * here: the board power is not a column of this query but a second one (queryCardsRanked), so that a driver which rejects it cannot cost the display
 * state its ladder.
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
      powerLimitW: 0, // the board's design power in watts; 0 = not read (queryCardsRanked fills it from a second query)
    });
  }
  return cards;
}

/**
 * queryCards asks the driver for the card table. A failed query is asked once more before a driver is taken to lack
 * display_attached (a transient failure is not a missing column); the fallback without it leaves every card's display state
 * unknown, which the selection refuses. The planners read the table through queryCardsRanked, which adds the board power.
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

/** What the board-power query asks for: the UUID to join on, and the board's design power (not the operator-adjustable power.limit). */
const POWER_QUERY = "uuid,power.default_limit";

/**
 * parsePowerTable reads `nvidia-smi --query-gpu=uuid,power.default_limit --format=csv,noheader,nounits` into a map from the lower-cased UUID to watts.
 * A power the driver does not report ([N/A], [Not Supported], an empty cell) or reports as something else than a positive number is no reading and is left
 * out, so a card without one stays at 0 (unknown); a row that is no card, or has no power column, is skipped.
 */
function parsePowerTable(text) {
  const watts = new Map();
  for (const raw of String(text || "").split(/\r?\n/)) {
    const cols = raw.split(",").map((c) => c.trim());
    const w = Number(cols[1]);
    if (/^GPU-/i.test(cols[0]) && Number.isFinite(w) && w > 0) watts.set(cols[0].toLowerCase(), w);
  }
  return watts;
}

/**
 * queryCardsRanked: the card table (queryCards, its display ladder untouched) with each board's design power joined on as powerLimitW. The power is a
 * SECOND, best-effort query, asked once more when it fails (a transient failure must not drop the ranking and flip the roles of a pair, and with them the
 * instance key): had it been one more column of the display query, a driver that rejects the column would have cost the display_attached ladder its
 * rungs. A power that cannot be read at all leaves every card at 0, which ranks nothing: the roles are then the ones the planner always gave, and the
 * placement is never refused for it. Every ask hides its console window on Windows (windowsHide).
 */
export function queryCardsRanked(spawnSync = nodeSpawnSync) {
  // forwarded through a rest parameter, not handed over as a value: queryCards' own calls carry windowsHide, and the
  // no-visible-windows scan can only judge a call it can see
  const cards = queryCards((...a) => spawnSync(...a));
  const askPower = () => { try { return spawnSync("nvidia-smi", [`--query-gpu=${POWER_QUERY}`, "--format=csv,noheader,nounits"], { encoding: "utf8", timeout: 30000, windowsHide: true }); } catch { return null; } };
  let r = askPower();
  if (!r || r.status !== 0 || !r.stdout) r = askPower();
  if (!r || r.status !== 0 || !r.stdout) return cards;
  const watts = parsePowerTable(r.stdout);
  return cards.map((c) => ({ ...c, powerLimitW: watts.get(c.uuid.toLowerCase()) || 0 }));
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

/** matchCards: the cards a token names: an nvidia-smi index (one card), or a lease id / UUID / UUID prefix of 8+ characters (every card it begins). */
function matchCards(token, cards) {
  const t = token.trim().toLowerCase();
  if (/^\d+$/.test(t)) return cards.filter((c) => c.index === Number(t));
  // a lease id is the lower-cased UUID with "gpu-" in front
  const bare = t.replace(/^gpu-/, "");
  if (bare.length < 8) return [];
  return cards.filter((c) => c.uuid.toLowerCase().replace(/^gpu-/, "").startsWith(bare));
}

/** matchCard: the one card a token names, or null when it names none or (a prefix shared by two cards) more than one. */
function matchCard(token, cards) {
  const hits = matchCards(token, cards);
  return hits.length === 1 ? hits[0] : null;
}

/**
 * Two cards whose sizes differ by no more than this fraction of the larger are one size. CHOSEN, not measured, the way WAN_COMPUTE_CONTEXT_GIB is: the exact
 * byte count of a card's memory is a few MiB apart between boards of one generation (16311 MiB against 16303 MiB on a 5060 Ti beside a 5070 Ti) and says
 * nothing about their speed, and 1 percent of 16 GiB is 163 MiB, far under any step between two memory sizes.
 */
export const VRAM_EQUAL_FRACTION = 0.01;
/**
 * Of two cards of one size, the faster board computes when its design power (power.default_limit) is at least this many times the other's. CHOSEN, not
 * measured: the pair this was written for reads 300 W against 180 W (1.67 times; measured 2026-10-12 on a 16 GB Blackwell pair with --use-sage-attention: the
 * higher-power card sampled about 2.5 times as fast), and a factory-overclocked variant of one chip sits near 1.1 times, which must stay a tie.
 */
export const POWER_RANK_RATIO = 1.25;

/**
 * computeOverride: the WAN_COMPUTE_UUID of an environment (an nvidia-smi index, a UUID or a UUID prefix of 8+ characters after the optional "GPU-"), the card
 * the operator says computes; "" when absent or blank. Both planners read it, because the typed image and edit doors can carry no flag and their runner
 * inherits the process environment.
 */
export function computeOverride(env = process.env) {
  return String((env && env.WAN_COMPUTE_UUID) || "").trim();
}

/** Two cards are one size when they differ by no more than VRAM_EQUAL_FRACTION of the larger. */
const sameSizeClass = (a, b) => Math.abs(a.totalMiB - b.totalMiB) <= VRAM_EQUAL_FRACTION * Math.max(a.totalMiB, b.totalMiB);
/** A board power is known when the driver gave a positive number (0 means not read). */
const powerKnown = (c) => Number.isFinite(c.powerLimitW) && c.powerLimitW > 0;

/**
 * chooseRoles gives the two cards their roles: { storage, compute, why }, why being the rule that chose the compute card.
 * One size (within VRAM_EQUAL_FRACTION), both board powers known and at least POWER_RANK_RATIO apart: the higher-power card computes (cuda:1).
 * Anything else is the rule the planner always had, so a power that could not be read reproduces it exactly: the larger card computes, and of two
 * equal cards the one with the higher UUID does, the lower storing (cuda:0), which is the order a lease sorts its ids in (internal/gpulease
 * NormalizeDevices): a pair named with --wan-cards gets the roles the same pair gets under a lease, whatever order the caller typed it in.
 */
export function chooseRoles(first, second) {
  const oneSize = sameSizeClass(first, second);
  const bothKnown = powerKnown(first) && powerKnown(second);
  const [slow, fast] = first.powerLimitW <= second.powerLimitW ? [first, second] : [second, first];
  if (oneSize && bothKnown && fast.powerLimitW >= POWER_RANK_RATIO * slow.powerLimitW) {
    return { storage: slow, compute: fast, why: `the faster card: ${fast.powerLimitW} W board power against ${slow.powerLimitW} W` };
  }
  let storage, compute;
  if (first.totalMiB !== second.totalMiB) [storage, compute] = second.totalMiB > first.totalMiB ? [first, second] : [second, first];
  else [storage, compute] = first.uuid.toLowerCase() <= second.uuid.toLowerCase() ? [first, second] : [second, first];
  const why = first.totalMiB !== second.totalMiB ? `the larger card (${compute.totalMiB} MiB against ${storage.totalMiB} MiB)` : "two cards of one size: the higher UUID computes";
  const unread = [first, second].filter((c) => !powerKnown(c)).map((c) => `idx ${c.index}`).join(", ");
  const remark = !oneSize ? "" : bothKnown ? `board power ${slow.powerLimitW} W and ${fast.powerLimitW} W differ by less than ${POWER_RANK_RATIO} times` : `board power not reported for ${unread}`;
  return { storage, compute, why: remark ? `${why} (${remark})` : why };
}

/** computeChosen: the clause the planners' one-line summary ends with: the rule that chose the compute card, and the note of an override that was ignored. */
export function computeChosen(pair) {
  return `compute card chosen: ${pair.why}${pair.note ? ` (${pair.note})` : ""}`;
}

/**
 * resolveWanCards turns the tokens into the two cards and their roles. It refuses (with the reason) when there are not
 * exactly two distinct cards, when a token names no card of the driver's table, or when a card drives a display.
 * Roles (chooseRoles): the larger card computes (cuda:1), of two equal cards the one with the higher UUID, and of two cards of one size (within
 * 1 percent) the one with the higher board power when the two differ by 1.25 times or more. `compute` is WAN_COMPUTE_UUID (computeOverride): a token that
 * names exactly one card of the pair makes it the compute card, beating both rules (checkResident still judges it); one that names no card of the
 * driver's table is refused (an explicit setting must not run quietly on a typo or a stale id); one that names a card outside the pair, or begins more than
 * one card of the table, is ignored: the roles are ranked and `note` says so.
 */
export function resolveWanCards({ tokens, source, cards, cardsFlag = "wan-cards", compute: computeToken = "" }) {
  if (!tokens || tokens.length === 0) {
    throw placementError(`the multicard placement needs the two cards it runs on, and none was named: run it under \`local-offload gpu reserve --devices <card>,<card>\` (the lease names them), or pass --${cardsFlag} <card>,<card>`);
  }
  const found = [];
  for (const token of tokens) {
    const hits = matchCards(token, cards);
    if (hits.length > 1) {
      throw placementError(`${source} names a card ambiguously: ${JSON.stringify(token)} matches ${hits.length} cards (${hits.map((c) => `idx ${c.index}`).join(", ")}); name the card by its nvidia-smi index or a longer prefix of its UUID`);
    }
    const card = hits[0];
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
  const token = String(computeToken || "").trim();
  let note = "";
  if (token) {
    const named = matchCards(token, cards);
    const pairText = found.map((c) => `idx ${c.index}`).join(", ");
    if (named.length === 0) throw placementError(`WAN_COMPUTE_UUID names ${JSON.stringify(token)}, which is no card in the driver's table (${cards.map((c) => `${c.index}:${c.uuid}`).join(", ")}), so it cannot say which of the two cards (${pairText}) computes: name one of them by its nvidia-smi index, its UUID or a UUID prefix of 8 or more characters after the optional "GPU-", or unset it${/^\d+$/.test(token) ? ` (a token of digits alone is read as an nvidia-smi index; a UUID prefix made of digits needs the "GPU-" spelling (GPU-${token}))` : ""}`);
    if (named.length === 1 && found.includes(named[0])) return { storage: found.find((c) => c !== named[0]), compute: named[0], source, why: "named by WAN_COMPUTE_UUID", note: "" };
    note = `WAN_COMPUTE_UUID names ${JSON.stringify(token)}, which does not name exactly one of the two cards (${pairText}); ranked instead`;
  }
  const { storage, compute, why } = chooseRoles(first, second);
  return { storage, compute, source, why, note };
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

/**
 * The order the launch lists a pair's two cards in, which is the order ComfyUI numbers them in: "storage-first" (the Wan and image placements: cuda:0 is the
 * storage card, cuda:1 the compute card) or "compute-first" (LTX-2.5: cuda:0 computes, because its int8 DiT faults on any other device, ComfyUI-MultiGPU #220).
 */
export const CARD_ORDER_STORAGE_FIRST = "storage-first";
export const CARD_ORDER_COMPUTE_FIRST = "compute-first";

/**
 * instanceKey: the multicard instance of this pair of cards (INSTANCE_KEY_RE in comfy-lifecycle.mjs). The compute-first instance has its own prefix
 * (mcc-, then the compute card's head, then the storage card's), not the same two heads the other way round: a pair whose roles an override
 * reversed (WAN_COMPUTE_UUID) has a storage-first key that spells exactly that, and the two launches number the cards differently, so a kept ComfyUI of one
 * must never be taken for the other.
 */
export function multicardInstanceKey(pair, order = CARD_ORDER_STORAGE_FIRST) {
  const head = (c) => c.uuid.replace(/^GPU-/i, "").slice(0, 8).toLowerCase();
  if (order === CARD_ORDER_COMPUTE_FIRST) return `mcc-${head(pair.compute)}-${head(pair.storage)}`;
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
 * `order` "compute-first" (LTX-2.5) lists the COMPUTE card first, so it is cuda:0, under the instance key of that order; the launch then carries `order` so the
 * device check (checkComfyDevices) knows which card should be which. Absent, the launch is the storage-first one, byte for byte.
 * The flags join the harness's own (--disable-smart-memory --cache-none --reserve-vram), which the measured recipe also used.
 */
export function multicardLaunch(pair, { comfyDir, packDirsFn = packDirs, yaml = EXT_YAML, order = CARD_ORDER_STORAGE_FIRST } = {}) {
  if (!comfyDir) throw placementError("COMFY_DIR is not set, so the custom-node packs the placement needs cannot be found");
  if (!existsSync(yaml)) throw placementError(`the harness's ComfyUI extension file is missing: ${yaml}`);
  const packs = packDirsFn(comfyDir);
  const computeFirst = order === CARD_ORDER_COMPUTE_FIRST;
  const [first, second] = computeFirst ? [pair.compute, pair.storage] : [pair.storage, pair.compute];
  return {
    key: multicardInstanceKey(pair, order),
    cards: [first.uuid, second.uuid],
    cudaVisibleDevices: `${first.uuid},${second.uuid}`,
    ...(computeFirst ? { order } : {}),
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
 * pinned to: cuda:0 should be the storage card and cuda:1 the compute card. ComfyUI numbers its cards in the order of
 * CUDA_VISIBLE_DEVICES; a count, a size or a model name that does not match means the pin did not take. Returns the problems found.
 * `order` "compute-first" (LTX-2.5) swaps the two: cuda:0 should be the compute card and cuda:1 the storage card.
 * What it can prove is the count, the size (to 5 percent) and the model name ("cuda:N <torch name> : <allocator>" holds the nvidia-smi name) of each
 * device. It cannot tell two cards of one model apart, and /system_stats carries no UUID: the order of two such cards is the pin's
 * (CUDA_VISIBLE_DEVICES lists UUIDs, storage first) and the roles' (chooseRoles: of two cards of one model the lower UUID stores, unless WAN_COMPUTE_UUID names the other).
 * A pair of two models (a faster board beside a slower one of one size) is told apart by the model name, so its roles are verified card by card.
 */
export function checkComfyDevices(devices, pair, order = CARD_ORDER_STORAGE_FIRST) {
  const problems = [];
  const cuda = (Array.isArray(devices) ? devices : []).filter((d) => d && d.type === "cuda");
  if (cuda.length !== 2) {
    problems.push(`ComfyUI sees ${cuda.length} CUDA device(s), not the 2 it was pinned to`);
    return problems;
  }
  const want = order === CARD_ORDER_COMPUTE_FIRST
    ? [["cuda:0 (compute)", pair.compute], ["cuda:1 (storage)", pair.storage]]
    : [["cuda:0 (storage)", pair.storage], ["cuda:1 (compute)", pair.compute]];
  want.forEach(([label, card], i) => {
    const got = cuda[i];
    const gib = Number(got.vram_total) / 1024 ** 3, expect = card.totalMiB / 1024;
    const squash = (s) => String(s || "").toLowerCase().replace(/\s+/g, " ").trim();
    if (!Number.isFinite(gib) || !Number.isFinite(expect) || Math.abs(gib - expect) > 0.05 * expect) {
      problems.push(`${label} should be idx ${card.index} (${expect.toFixed(1)} GiB) but ComfyUI reports ${Number.isFinite(gib) ? gib.toFixed(1) + " GiB" : "no size"}${got.name ? ` (${got.name})` : ""}`);
    } else if (squash(got.name) && squash(card.name) && !squash(got.name).includes(squash(card.name))) {
      // the same size, another model: a pin that landed on the display card (all three cards of the machine are 15.9 GiB)
      problems.push(`${label} should be idx ${card.index} (${card.name}) but ComfyUI reports ${got.name}`);
    }
  });
  return problems;
}

/**
 * refuseUnmeasuredWan: the builder (wf-wan22-i2v.mjs) refuses --fast under multicard, and its defaults for an expert nobody named
 * are the Q4_K GGUF files (they fit one card, and were not measured split), so a call without --high-unet / --low-unet would be
 * refused too. It does that after the lease is taken, llama-swap is unloaded and ComfyUI has cold-started; the same refusals are
 * made here, before any of it. The experts it accepts are the fp8 safetensors files and the Q8_0 GGUF files.
 */
function refuseUnmeasuredWan(flags) {
  if (flags.fast) throw new Error("--wan-placement multicard was measured on the native recipe only: drop --fast (or use --wan-placement single)");
  for (const flag of ["high-unet", "low-unet"]) {
    const name = flags[flag];
    if (typeof name !== "string" || !name.trim()) {
      throw new Error(`--wan-placement multicard needs --${flag} to name the expert, an fp8 safetensors file or a Q8_0 GGUF file (the builder's default experts are Q4_K GGUF files, which fit one card: use --wan-placement single for those)`);
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
/**
 * The stage sizes of the card-only placement, one set for the runner and for the lease sizing (internal/hostneed declares against the same
 * figures, and a Go test fails when either side drifts):
 *   WAN_FILM_ACTIVATIONS_GIB   what the compute card holds beyond the resident weights at the film's size (720x1280x81, 75,600 tokens): the
 *                              film-size run's compute-card peak, 15.18 GiB (nvidia-smi, 90 s before the hand-over to the second expert), less its 3 GiB resident
 *                              share, which already holds the allocator's slack and the CUDA context;
 *   WAN_COMPUTE_CONTEXT_GIB    a margin the compute card keeps free beyond that figure, CHOSEN and not measured (the figure above is the card's total in
 *                              use, so it already holds a CUDA context; the film-size run left 0.7 GiB free at its peak and this keeps 0.5 of it): with
 *                              it a 15.9 GiB card takes 3.2 GiB of an expert resident at film size (15.9 - 12.2 - 0.5). Only a 3.0 GiB share was ever
 *                              run; 3.2 is the planner's ceiling, not a measured share;
 *   WAN_STORAGE_OVERHEAD_GIB   what the storage card holds besides the donor blocks: the VAE, the CUDA context and the allocator's slack;
 *   WAN_EXPERT_GIB             an fp8 expert (internal/hostneed's documented Wan 2.2 size, rounded up);
 *   WAN_EXPERT_Q8_GIB          a Q8_0 GGUF expert (the 14.349 GiB file, rounded up), chosen by the expert's file name (wanExpertGib).
 */
export const WAN_FILM_ACTIVATIONS_GIB = 12.2;
/** The tokens of the film clip (720x1280x81, see wanGeometry): the largest size anything was measured at. Above it the planner has no figure (geometryNote). */
export const WAN_FILM_TOKENS = 75600;
export const WAN_COMPUTE_CONTEXT_GIB = 0.5;
export const WAN_STORAGE_OVERHEAD_GIB = 0.8;
export const WAN_EXPERT_GIB = 13.4;
export const WAN_EXPERT_Q8_GIB = 14.4;
/** wanIsGguf: is either expert a GGUF file (the launch then loads ComfyUI-GGUF)? */
export function wanIsGguf(flags) {
  return ["high-unet", "low-unet"].some((k) => typeof flags[k] === "string" && /\.gguf$/i.test(flags[k].trim()));
}
/** wanExpertGib: the size one expert is planned at: a Q8_0 GGUF file is 14.4 GiB, anything else the fp8 figure; the larger of the two experts. */
export function wanExpertGib(flags) {
  const q8 = ["high-unet", "low-unet"].some((k) => typeof flags[k] === "string" && /Q8_0\.gguf$/i.test(flags[k].trim()));
  return q8 ? WAN_EXPERT_Q8_GIB : WAN_EXPERT_GIB;
}
/** Below the film's size the activations follow the benchmark's allocator traces (RESULTS 3.5, compute-card peak RESERVED, which is what the card must hold). */
const ACTIVATIONS_GIB_PER_1000_TOKENS = 0.31;
/** Scaled with the tokens, never under this: even a small clip holds a context's worth of buffers. */
const ACTIVATIONS_FLOOR_GIB = 2.0;
/**
 * WAN_FILM_ACTIVATIONS_SAGE_GIB: the same figure as WAN_FILM_ACTIVATIONS_GIB under sage attention (--use-sage-attention in COMFY_EXTRA_ARGS), on the same basis (the
 * compute card's nvidia-smi peak less its resident share, the CUDA context inside). Measured 2026-10-12 on a 16 GB Blackwell pair at the film clip with
 * resident 0: 15,482 MiB in use = 15.119 GiB, rounded up to 15.2: 3.0 GiB more than the default attention's. internal/hostneed declares against the same
 * figure (wanActivationsSageGiB; a Go test pins the two together, so this stays a one-line numeric literal).
 */
export const WAN_FILM_ACTIVATIONS_SAGE_GIB = 15.2;
/**
 * The sage curve is the straight line through the two sage points measured on 2026-10-12: 12.9 GiB at 61,200 tokens (720x1280x65 with 2 GiB resident: 15,216 MiB =
 * 14.859 GiB in use, less the 2.0 GiB share = 12.859, rounded up) and WAN_FILM_ACTIVATIONS_SAGE_GIB at the film's 75,600. It is used from where it meets the default
 * curve's cap (WAN_FILM_ACTIVATIONS_GIB), about 56,800 tokens, up; below that the default curve is above it and sage is sized as the default, which already covers the
 * benchmark's 6.16 GiB reserved with sage at 20,280 tokens (the default gives 6.29 there). Chosen between the points and not measured there; only 61,200 and 75,600 were.
 */
const SAGE_POINT_TOKENS = 61200;
const SAGE_POINT_GIB = 12.9;
const SAGE_LINE_FROM_TOKENS = SAGE_POINT_TOKENS - ((SAGE_POINT_GIB - WAN_FILM_ACTIVATIONS_GIB) * (WAN_FILM_TOKENS - SAGE_POINT_TOKENS)) / (WAN_FILM_ACTIVATIONS_SAGE_GIB - SAGE_POINT_GIB);
/** What the storage card keeps back: the runner's --reserve-vram, default 1.0 (internal/hostneed's default too). A model-load margin, so the compute card's check does not use it. */
const DEFAULT_RESERVE_GIB = 1.0;

/**
 * wanActivationsGib: the sampler's activations on the compute card at this many tokens. The benchmark reserved 2.19 GiB at 7,800 tokens, 3.66 at
 * 14,040, 5.31 at 20,280 (5.67 on the native allocator, 6.16 with sage attention) and 5.50 at 32,400; 0.31 GiB per 1,000 tokens is never under
 * any of them. The film's own size is where the measurements end: the full model there was measured at 12.2 GiB beyond its resident share (the film-size run),
 * and the planner has no figure above it, so it holds 12.2 there. The activations are expected to grow with the tokens, which is why a bigger clip is
 * said to be unsized (geometryNote) and not read as one that needs no more.
 * `attention` is "sage" under --use-sage-attention (wanAttentionMode), which holds more activations: the straight line through the two sage points (SAGE_POINT_*,
 * WAN_FILM_ACTIVATIONS_SAGE_GIB) from where it meets the default's cap, held at the film's sage figure above the film's size; anything else is the default curve.
 * From SAGE_LINE_FROM_TOKENS up the line is at or above the default's cap, so sage is never under the default (a test walks the whole range).
 */
export function wanActivationsGib(tokens, attention = "default") {
  const base = Math.min(WAN_FILM_ACTIVATIONS_GIB, Math.max(ACTIVATIONS_FLOOR_GIB, (tokens / 1000) * ACTIVATIONS_GIB_PER_1000_TOKENS));
  if (attention !== "sage" || tokens < SAGE_LINE_FROM_TOKENS) return base;
  return Math.min(WAN_FILM_ACTIVATIONS_SAGE_GIB, SAGE_POINT_GIB + ((tokens - SAGE_POINT_TOKENS) * (WAN_FILM_ACTIVATIONS_SAGE_GIB - SAGE_POINT_GIB)) / (WAN_FILM_TOKENS - SAGE_POINT_TOKENS));
}

/**
 * wanAttentionMode: "sage" when COMFY_EXTRA_ARGS carries sage attention, else "default". The value is split on whitespace the way ensureComfy splits it for the launch
 * (comfy-lifecycle.mjs launchFlags), and read token by token with isSageAttentionToken: the exact flag or an argparse prefix of it. Only sage was measured; another
 * attention flag (flash, split, quad, pytorch) is sized as the default.
 */
export function wanAttentionMode(env = process.env) {
  const extra = String((env && env.COMFY_EXTRA_ARGS) || "").split(/\s+/).filter(Boolean);
  return extra.some(isSageAttentionToken) ? "sage" : "default";
}

/**
 * checkResident: a resident share one of the two cards cannot hold would run out of memory in the first sampler step, 80 minutes into a
 * film-size run's lease, with both cards held. The compute card holds the share and the sampler's activations; the storage card holds the rest of
 * the expert (the donor blocks), the VAE and the context. The compute card must hold the share + the activations + WAN_COMPUTE_CONTEXT_GIB, which is
 * the measured peak plus a chosen margin (15.9 - 12.2 - 0.5 = 3.2 GiB at film size, and the measured 3 passes); the storage card must fit its blocks inside its
 * size less --reserve-vram, which is ComfyUI's model-load margin and not an activation ceiling, so the compute card's check does not use it. Both
 * with the figures internal/hostneed declares against, at this geometry (the smaller the clip, the less the activations need).
 */
function checkResident(residentGb, pair, flags, attention = "default") {
  const expertGib = wanExpertGib(flags);
  const reserveRaw = Number(flags["reserve-vram"]);
  const reserve = Number.isFinite(reserveRaw) && reserveRaw > 0 ? reserveRaw : DEFAULT_RESERVE_GIB;
  const computeGib = pair.compute.totalMiB / 1024, storageGib = pair.storage.totalMiB / 1024;
  const { w, h, f, tokens } = wanGeometry(flags);
  const activations = wanActivationsGib(tokens, attention);
  const most = computeGib - activations - WAN_COMPUTE_CONTEXT_GIB;
  if (residentGb > most) {
    if (attention === "sage") {
      // the sage clause comes BEFORE "at most", and the default mode's tail is replaced: the measured recipe that keeps 3 is the default attention's, the very share refused here
      throw placementError(`--wan-resident-gb ${residentGb} leaves the compute card (idx ${pair.compute.index}, ${computeGib.toFixed(1)} GiB) about ${activations.toFixed(1)} GiB for the sampler's activations at ${w}x${h}x${f} (${tokens} tokens) and ${WAN_COMPUTE_CONTEXT_GIB} GiB for the CUDA context, with --use-sage-attention in COMFY_EXTRA_ARGS sizing the activations for sage attention (${WAN_FILM_ACTIVATIONS_SAGE_GIB} GiB at film size, ${WAN_FILM_ACTIVATIONS_GIB} without it): at most ${most.toFixed(1)} GiB of an expert can stay resident there (--wan-resident-gb 0 is the measured setting at film size; or drop the flag)`);
    }
    throw placementError(`--wan-resident-gb ${residentGb} leaves the compute card (idx ${pair.compute.index}, ${computeGib.toFixed(1)} GiB) about ${activations.toFixed(1)} GiB for the sampler's activations at ${w}x${h}x${f} (${tokens} tokens) and ${WAN_COMPUTE_CONTEXT_GIB} GiB for the CUDA context: at most ${most.toFixed(1)} GiB of an expert can stay resident there (the measured film-size recipe keeps ${WAN_RESIDENT_GB_DEFAULT})`);
  }
  const donor = expertGib - residentGb;
  const stored = donor + WAN_STORAGE_OVERHEAD_GIB;
  const usable = storageGib - reserve;
  if (stored > usable) {
    const least = expertGib + WAN_STORAGE_OVERHEAD_GIB - usable;
    const way = least <= most
      ? `a --wan-resident-gb of at least ${least.toFixed(1)} (and at most ${most.toFixed(1)}) keeps enough of the expert on the compute card`
      : "no resident share fits both cards at this geometry: lease two larger cards";
    throw placementError(`--wan-resident-gb ${residentGb} leaves the storage card (idx ${pair.storage.index}, ${storageGib.toFixed(1)} GiB) ${donor.toFixed(1)} GiB of donor blocks + ${WAN_STORAGE_OVERHEAD_GIB} GiB for the VAE and the context = ${stored.toFixed(1)} GiB against ${usable.toFixed(1)} GiB usable (the card less ${reserve} GiB of --reserve-vram): ${way}`);
  }
}

/**
 * The LTX-2.5 stage sizes of the card-only placement (render/wf-ltx25-i2v.mjs; internal/hostneed declares against the same figures). The file sizes are
 * measured (the int8 convrot DiT 20.03 GiB, the video VAE 1.37, the audio VAE 0.34, the x2 latent upscaler 0.93), rounded up; the overhead is the Wan one:
 *   LTX_DIT_GIB               the DiT (20.03 GiB on disk);
 *   LTX_STORAGE_VAES_GIB      what the graph puts beside the donor blocks on the storage card: the video and the audio VAE (1.71 GiB);
 *   LTX_UPSCALER_GIB          the latent upscaler, which has no device input and loads on the compute card (cuda:0, ComfyUI's default device);
 *   LTX_STORAGE_OVERHEAD_GIB  the CUDA context and the allocator's slack on the storage card, the Wan placement's 0.8 (WAN_STORAGE_OVERHEAD_GIB);
 * and the compute card keeps WAN_COMPUTE_CONTEXT_GIB free. The sampler's activations are NOT in the check. The one run (1280x704x121, a 9 GiB share) peaked at
 * 11.54 GiB on the compute card, so the sampler and the context took about 1.6 GiB beyond the share and the upscaler there; nothing says what they take at
 * 1920x1088x121. The check refuses only a share the compute card plainly cannot hold with nothing else on it, so a share above the measured one, or a bigger
 * clip, can still run out of memory in the first sampler step. The text encoder (14.32 GiB, split by LTX25_TE_ALLOCATION_DEFAULT, measured 7.07 GiB on the
 * compute card and 7.26 on the storage card) is gone from the cards before the DiT lands, so it is not a stage of either card.
 */
export const LTX_DIT_GIB = 20.1;
export const LTX_STORAGE_VAES_GIB = 1.8;
export const LTX_UPSCALER_GIB = 1.0;
export const LTX_STORAGE_OVERHEAD_GIB = WAN_STORAGE_OVERHEAD_GIB;

/** LTX-2.5 under multicard: a .safetensors DiT the DisTorch2 loader reads, and one that fits the pair at all (a bf16 DiT is about 39 GiB, the two cards hold 31.8). */
function refuseUnmeasuredLtx(flags) {
  const name = typeof flags.transformer === "string" ? flags.transformer.trim() : "";
  if (name && !/\.safetensors$/i.test(name)) {
    throw new Error(`--wan-placement multicard places the LTX-2.5 DiT as a .safetensors file through the DisTorch2 loader; --transformer ${JSON.stringify(name)} is not one: use --wan-placement single for it`);
  }
  if (/bf16/i.test(name)) {
    throw new Error(`--wan-placement multicard places the int8 LTX-2.5 DiT (20 GiB); --transformer ${JSON.stringify(name)} is the bf16 one (about 39 GiB), which does not fit two 16 GiB cards: use --wan-placement single for it`);
  }
  if ("pool-vvram-gb" in flags) {
    throw new Error("--wan-placement multicard and --pool-vvram-gb are two placements of the same DiT: pass one of them");
  }
}

/**
 * checkLtxResident: the LTX-2.5 DiT's resident share against the two cards (checkResident's counterpart). The compute card holds the share, the latent upscaler and
 * its context, and whatever the sampler needs beyond them is unmeasured (LTX_* above), so the share is refused only where nothing is left for it; the storage card
 * holds the rest of the DiT (the donor blocks), both VAEs and its context inside its size less --reserve-vram.
 */
function checkLtxResident(residentGb, pair, flags) {
  const reserveRaw = Number(flags["reserve-vram"]);
  const reserve = Number.isFinite(reserveRaw) && reserveRaw > 0 ? reserveRaw : DEFAULT_RESERVE_GIB;
  const computeGib = pair.compute.totalMiB / 1024, storageGib = pair.storage.totalMiB / 1024;
  const most = computeGib - LTX_UPSCALER_GIB - WAN_COMPUTE_CONTEXT_GIB;
  if (residentGb > most) {
    throw placementError(`--wan-resident-gb ${residentGb} (the DiT's share on the compute card for --model ltx25) leaves the compute card (idx ${pair.compute.index}, ${computeGib.toFixed(1)} GiB) ${(computeGib - residentGb).toFixed(1)} GiB for the ${LTX_UPSCALER_GIB} GiB latent upscaler, ${WAN_COMPUTE_CONTEXT_GIB} GiB of CUDA context and the sampler's activations (about 1.6 GiB measured at 1280x704x121): at most ${most.toFixed(1)} GiB can be resident there before nothing is left for them, and the default ${LTX25_RESIDENT_GB_DEFAULT} is the measured share`);
  }
  const donor = LTX_DIT_GIB - residentGb;
  const stored = donor + LTX_STORAGE_VAES_GIB + LTX_STORAGE_OVERHEAD_GIB;
  const usable = storageGib - reserve;
  if (stored > usable) {
    const least = LTX_DIT_GIB + LTX_STORAGE_VAES_GIB + LTX_STORAGE_OVERHEAD_GIB - usable;
    const way = least <= most
      ? `a --wan-resident-gb of at least ${least.toFixed(1)} (and at most ${most.toFixed(1)}) keeps enough of the DiT on the compute card`
      : "no resident share fits both cards: lease two larger cards";
    throw placementError(`--wan-resident-gb ${residentGb} leaves the storage card (idx ${pair.storage.index}, ${storageGib.toFixed(1)} GiB) ${donor.toFixed(1)} GiB of donor blocks + ${LTX_STORAGE_VAES_GIB} GiB for the two VAEs + ${LTX_STORAGE_OVERHEAD_GIB} GiB for the context = ${stored.toFixed(1)} GiB against ${usable.toFixed(1)} GiB usable (the card less ${reserve} GiB of --reserve-vram): ${way}`);
  }
}

/**
 * geometryNote: what the planner says about a clip bigger than the film's (720x1280x81), "" at the film's size and under. Nothing above that size was
 * measured: wanActivationsGib holds the film's 12.2 GiB there while the sampler's activations are expected to grow with the tokens, so the resident-share
 * check passes a bigger clip on a figure that is soft, and the run could run out of memory on the compute card with both cards held. The planner does not
 * refuse what it cannot size (the documented intent is that a bigger clip may run); it says so before the check, with what a proportional extrapolation
 * gives, and leaves the share and the wait to the caller.
 */
export function geometryNote(flags, attention = "default") {
  const { w, h, f, tokens } = wanGeometry(flags);
  if (tokens <= WAN_FILM_TOKENS) return "";
  const film = attention === "sage" ? WAN_FILM_ACTIVATIONS_SAGE_GIB : WAN_FILM_ACTIVATIONS_GIB;
  const proportional = (film * tokens) / WAN_FILM_TOKENS;
  const sageWord = attention === "sage" ? " (sage attention, --use-sage-attention in COMFY_EXTRA_ARGS)" : "";
  return `WAN-GEOMETRY-NOTE: ${w}x${h}x${f} (${tokens} tokens) is bigger than the measured film clip (720x1280x81, ${WAN_FILM_TOKENS} tokens): nothing above it was measured, so the planner holds the sampler's activations at the film's ${film} GiB${sageWord}, and in proportion to its tokens they would be about ${proportional.toFixed(1)} GiB; the resident-share check is only as good as that figure. Lower --wan-resident-gb (or use a smaller clip) if the compute card runs short, and pass --wait-sec (the ${MULTICARD_WAIT_SEC} s default is the film's)`;
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
 * unnamed expert, no cards named, not two cards, a card the lease does not hold, a display card or one whose display
 * state is unknown, a resident share the compute card cannot hold, a missing pack. `cards` reads the driver's card table
 * (injectable); `isWanGraph` says whether this invocation builds the Wan 2.2 graph; `isLtxGraph` whether it builds the LTX-2.5 one (--model ltx25: the same
 * placement with the compute card first, its own sizes and no experts, --fast or GGUF to refuse); `leaseDevicesFn` names the cards the lease holds.
 */
export function planMulticard({ flags, env = process.env, comfyDir, isWanGraph, isLtxGraph = false, spawnSync: smiRun, cards = () => queryCardsRanked(smiRun), launchFn = multicardLaunch, leaseDevicesFn = leaseDevices, say = () => {} }) {
  refuseEqualsSpelling(flags);
  const mode = wanPlacementMode(flags);
  if (mode !== "multicard") return null;
  const ltx = !isWanGraph && isLtxGraph;
  if (!isWanGraph && !ltx) {
    throw new Error(`--wan-placement multicard places the Wan 2.2 graph and the LTX-2.5 one (--model ltx25); this invocation builds ${flags.graph ? "a --graph file" : `--model ${flags.model}`}, which it does not touch`);
  }
  if (ltx) refuseUnmeasuredLtx(flags); else refuseUnmeasuredWan(flags);
  const residentGb = ltx ? wanResidentGb(flags, LTX25_RESIDENT_GB_DEFAULT) : wanResidentGb(flags);
  const { tokens, source } = wanCardTokens({ flags, env, leaseDevicesFn });
  const pair = resolveWanCards({ tokens, source, cards: cards(), compute: computeOverride(env) });
  if (source === "--wan-cards") confirmWithLease(pair, leaseDevicesFn(env));
  if (ltx) {
    // The LTX-2.5 launch lists the compute card first (cuda:0 computes: ComfyUI-MultiGPU #220), loads the MultiGPU pack and the harness's own and nothing else (the graph
    // writes its mp4 with core nodes), and is sized by LTX's own stages, measured on one run at 1280x704x121: the planner says what was and was not measured, once.
    checkLtxResident(residentGb, pair, flags);
    say("LTX-PLACEMENT-NOTE: one LTX-2.5 run was made on the two cards: 1280x704x121 with the default " + LTX25_RESIDENT_GB_DEFAULT + " GiB share peaked at 11.54 GiB on the compute card and 12.71 GiB on the storage card, 124 s of ComfyUI time; a larger share or clip is unmeasured and can run out of memory in the first sampler step (lower --wan-resident-gb if it does), and the " + MULTICARD_WAIT_SEC + " s default wait is the Wan film's (pass --wait-sec to change it)");
    const launch = launchFn(pair, { comfyDir, packDirsFn: (d) => packDirs(d, undefined, [PACK_MULTIGPU]), order: CARD_ORDER_COMPUTE_FIRST });
    const plan = { pair, launch, residentGb, family: "ltx25", env: multicardEnv(env, launch) };
    const line = (role, dev, c) => `${role} ${dev} = idx ${c.index} ${c.name} (${(c.totalMiB / 1024).toFixed(1)} GiB, ${c.uuid.slice(0, 12)})`;
    say(`ltx25-placement: multicard on ${source}: ${line("compute", LTX25_COMPUTE_DEVICE, pair.compute)}; ${line("storage", LTX25_STORAGE_DEVICE, pair.storage)}; ${residentGb} GiB of the DiT resident on the compute card; text encoder split ${LTX25_TE_ALLOCATION_DEFAULT}, released from the cards after the prompt is encoded; ${computeChosen(pair)}`);
    return plan;
  }
  const attention = wanAttentionMode(env);
  const unsized = geometryNote(flags, attention);
  if (unsized) say(unsized);
  checkResident(residentGb, pair, flags, attention);
  // `sage` rides the launch so ensureComfy records which attention this call was sized for, and a kept ComfyUI that runs sage under a call sized for the default is not reused
  // a GGUF expert is loaded by ComfyUI-GGUF, so that pack joins the whitelist (and a kept instance without it is not reused: gguf rides the launch)
  const gguf = wanIsGguf(flags);
  const launchOpts = gguf ? { comfyDir, packDirsFn: (d) => packDirs(d, undefined, [...REQUIRED_PACKS, PACK_GGUF]) } : { comfyDir };
  const launch = { ...launchFn(pair, launchOpts), sage: attention === "sage", ...(gguf ? { gguf: true } : {}) };
  const plan = { pair, launch, residentGb, attention, env: multicardEnv(env, launch) };
  const line = (role, dev, c) => `${role} ${dev} = idx ${c.index} ${c.name} (${(c.totalMiB / 1024).toFixed(1)} GiB, ${c.uuid.slice(0, 12)})`;
  const sageClause = attention === "sage" ? `; sage attention (COMFY_EXTRA_ARGS): activations sized at ${wanActivationsGib(wanGeometry(flags).tokens, attention).toFixed(1)} GiB` : "";
  say(`wan-placement: multicard on ${source}: ${line("storage", WAN_STORAGE_DEVICE, pair.storage)}; ${line("compute", WAN_COMPUTE_DEVICE, pair.compute)}; ${residentGb} GiB of each expert resident on the compute card; ${computeChosen(pair)}${sageClause}`);
  return plan;
}

/**
 * MULTICARD_WAIT_SEC: the poll budget of a card-only run when the caller named none (--wait-sec, COMFY_WAIT_SEC). The benchmark measured 250.3 s
 * per sampler step at the film's geometry and a 4-step prompt wall of 1,165.7 s, so the 20-step recipe takes about 164.5 + 20 x 250.3 = 5,170 s.
 * The single-card default of 5,400 s leaves 4.5 percent over that, and a run that is 5 percent slower loses an hour and a half of sampling
 * (the sampled latent is saved only after the second sampler). 7,200 s is 1.39 times the projection. A geometry bigger than the film's is
 * unmeasured: pass --wait-sec for it.
 */
export const MULTICARD_WAIT_SEC = 7200;

/**
 * tiledSeamFrames: the frames at which one temporal tile of the tiled decode meets the next, for a clip of `frames` frames, as ComfyUI 0.38 cuts them
 * (the builder asks for temporal_size 32 and temporal_overlap 8 frames):
 *   nodes.py VAEDecodeTiled   halves the overlap when the tile is under twice of it, then divides both by the Wan VAE's temporal compression 4
 *                             (tile at least 2, overlap at least 1 and at most half the tile);
 *   comfy/utils.py            tiled_scale_multidim: a clip of no more latent frames than one tile is decoded whole; otherwise a tile starts every
 *                             (tile - overlap) latent frames while the start is under (latent frames - overlap);
 *   comfy/sd.py               the Wan VAE writes a tile starting at latent frame s at pixel frame 4 x s (its upscale_index_formula).
 * The film's 81 frames are 21 latent frames: tiles of 8 overlapping by 2 start at 0, 6, 12 and 18, so they meet at frames 24, 48 and 72.
 * (ComfyUI's own out-of-memory fallback halves the temporal tile on its own, so ITS boundaries are elsewhere: that is why a plain decode that fell
 * back is reported as a failure and not as a list of frames.) Spatial tiles (256 px, overlap 64) meet too; this is the temporal axis only.
 */
export function tiledSeamFrames(frames, temporalSize = 32, temporalOverlap = 8) {
  const latent = Math.floor((Number(frames) - 1) / 4) + 1;
  let overlap = temporalOverlap;
  if (temporalSize < overlap * 2) overlap = Math.floor(overlap / 2);
  const tile = Math.max(2, Math.floor(temporalSize / 4));
  overlap = Math.max(1, Math.min(Math.floor(tile / 2), Math.floor(overlap / 4)));
  if (!Number.isFinite(latent) || latent <= tile) return [];
  const seams = [];
  for (let start = tile - overlap; start < latent - overlap; start += tile - overlap) seams.push(4 * start);
  return seams;
}

/** describeFrames: "frames 24, 48 and 72", "frames 24 and 48", "frame 24", or "" for none. */
export function describeFrames(list) {
  if (!list.length) return "";
  if (list.length === 1) return `frame ${list[0]}`;
  return `frames ${list.slice(0, -1).join(", ")} and ${list[list.length - 1]}`;
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
