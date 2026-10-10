// render/comfy-ownership.mjs — durable marker so "did the harness start this ComfyUI?"
// is decidable (spec §5). Makes the up-managed vs up-external restart branch sound.
import { writeFileSync, readFileSync, rmSync, renameSync } from "node:fs";
import { join } from "node:path";

const MARKER = ".offload-owned.json";
const p = (dir) => join(dir, MARKER);

export function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === "EPERM"; }
}

// writeOwner: persist the ownership marker. The run-graph flow keys ownership on
// `manifestHash` (+ the downloaded `unverified` model list) and passes NO pid — the pid was
// the ephemeral node process, never a stable ComfyUI-ownership signal. `pid` stays optional
// for the legacy isOwnedByUs(pid-liveness) callers/tests below.
export function writeOwner(dir, { pid, manifestHash, unverified = [] }) {
  const rec = { startedAt: Date.now(), manifestHash, unverified };
  if (typeof pid === "number") rec.pid = pid;
  writeFileSync(p(dir), JSON.stringify(rec));
}
export function readOwner(dir) {
  try { return JSON.parse(readFileSync(p(dir), "utf8")); } catch { return null; }
}
export function isOwnedByUs(dir) {
  const m = readOwner(dir);
  return !!(m && typeof m.pid === "number" && pidAlive(m.pid));
}
export function clearOwner(dir) {
  try { rmSync(p(dir), { force: true }); } catch {}
}

// --- launch ownership (the launch-profile guard, comfy-lifecycle.mjs) ----------------
//
// A SEPARATE marker from run-graph's: that one is a provisioning cache keyed on the
// manifest hash and deliberately carries no pid, so folding the launch record into it
// would either break the cache or make ownership undecidable. This one records every
// ComfyUI the harness itself spawned: the python pid, the exact argv it was given, the
// profile it was launched for, and the node process that spawned it (ownerPid).
//
// It is a FINGERPRINT, not a claim: an instance counts as harness-launched only when
// the pid is alive AND /system_stats reports the very argv recorded here. A stale
// marker (the instance died, or a different server now holds the port) therefore never
// makes a foreign ComfyUI look like ours.
//
// PER-CARD INSTANCES (plan P13a). The default instance (key "") keeps this exact file
// and record. Every other instance keeps its own `.offload-launch-<key>.json`, so two
// ComfyUI processes on one box never share, overwrite or clear each other's fingerprint;
// its record also names the port and, when the launch ran under a GPU lease, the lease
// epoch (GPU_LEASE_EPOCH), so a later reader can tell whose lease an instance belongs to.
const LAUNCH_MARKER = ".offload-launch.json";
const lp = (dir, key = "") => join(dir, key ? `.offload-launch-${key}.json` : LAUNCH_MARKER);

export function writeLaunchOwner(dir, { pid, ownerPid = process.pid, args, profile = {}, key = "", port, leaseEpoch }) {
  const rec = { startedAt: Date.now(), pid, ownerPid, args, profile };
  if (key) {
    rec.key = key;
    if (typeof port === "number") rec.port = port;
    if (typeof leaseEpoch === "number") rec.leaseEpoch = leaseEpoch;
  }
  writeFileSync(lp(dir, key), JSON.stringify(rec));
}
export function readLaunchOwner(dir, key = "") {
  try { return JSON.parse(readFileSync(lp(dir, key), "utf8")); } catch { return null; }
}
/**
 * restampLaunchOwner: hand a kept keyed instance to the lease that is reusing it. The marker
 * records the lease epoch whose HOLDER stops the instance on release (internal/comfyinst matches
 * the epoch exactly); an instance that outlived its lease (a crashed or fenced-out holder: it is
 * detached) and is reused by the next lease must carry THAT lease's epoch, or the new holder's
 * release stops nothing and the instance outlives every later lease. Only the epoch changes: the
 * launch time, pid, argv and profile are the instance's own and are what prove it is ours.
 *
 * It claims only an instance that already belonged to a lease. A marker with no epoch (an instance
 * kept outside any lease: whoever kept it owns it) is left alone, or the lease would stop an
 * instance it never started. Written to a temporary file and renamed, so a reader never sees half
 * a marker. Throws on an I/O failure; the caller says so and carries on.
 */
export function restampLaunchOwner(dir, key, leaseEpoch) {
  const rec = readLaunchOwner(dir, key);
  if (!key || !rec || rec.key !== key) return { changed: false, why: "no marker" };
  if (typeof rec.leaseEpoch !== "number") return { changed: false, why: "the marker names no lease" };
  if (rec.leaseEpoch === leaseEpoch) return { changed: false, why: "already this lease's" };
  const previous = rec.leaseEpoch;
  rec.leaseEpoch = leaseEpoch;
  const path = lp(dir, key), tmp = path + ".tmp";
  writeFileSync(tmp, JSON.stringify(rec));
  try { renameSync(tmp, path); } catch (e) { try { rmSync(tmp, { force: true }); } catch {} throw e; }
  return { changed: true, previous };
}
/**
 * stampLaunchFamily: record whose weights a kept instance may hold. ComfyUI keeps the models it loaded
 * in host memory between prompts (its RAM-pressure cache evicts only under pressure of its own, which
 * on a 128 GiB box is far above where this harness needs it), so an instance that served one family and
 * is then handed a job of another probably holds BOTH until something tells it to let go (reconstructed
 * from the code and the surviving logs, not observed). The incident of 2026-10-09 was one keyed instance
 * at 57 GiB private; the probable cause is a qwen-image model cached from an earlier lease next to the
 * krea2 model of the current one (comfy-family.mjs says why that cannot be confirmed and what it does
 * not explain). `lastFamily` is that something's memory: the signature
 * (comfy-family.mjs) of the job whose weights the instance may still hold; "" clears it (the instance
 * was just told to free everything, so it holds nothing).
 *
 * Only the family changes: the pid, argv, profile and lease epoch are what prove the instance is ours.
 * It stamps only an instance that has a marker (one this harness launched); a foreign ComfyUI has none,
 * and gets none. Written beside, then renamed over, so a reader never sees half a marker. Throws on an
 * I/O failure; the caller says so and carries on.
 */
export function stampLaunchFamily(dir, key, family) {
  const rec = readLaunchOwner(dir, key);
  if (!rec || (key && rec.key !== key)) return { changed: false, why: "no marker" };
  const previous = typeof rec.lastFamily === "string" ? rec.lastFamily : "";
  const next = String(family ?? "");
  if (previous === next) return { changed: false, previous };
  if (next) rec.lastFamily = next; else delete rec.lastFamily;
  const path = lp(dir, key), tmp = path + ".tmp";
  writeFileSync(tmp, JSON.stringify(rec));
  try { renameSync(tmp, path); } catch (e) { try { rmSync(tmp, { force: true }); } catch {} throw e; }
  return { changed: true, previous };
}
export function clearLaunchOwner(dir, key = "") {
  try { rmSync(lp(dir, key), { force: true }); } catch {}
}

/** sameArgv: the recorded spawn argv and the server's sys.argv, element for element. */
export function sameArgv(a, b) {
  return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((x, i) => String(x) === String(b[i]));
}

/**
 * harnessLaunched reports whether the ComfyUI answering with `argv` is one this harness
 * spawned: a launch marker whose pid is alive and whose recorded argv is exactly it.
 */
export function harnessLaunched(marker, argv, alive = pidAlive) {
  return !!(marker && typeof marker.pid === "number" && alive(marker.pid) && sameArgv(marker.args, argv));
}
