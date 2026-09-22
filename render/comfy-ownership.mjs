// render/comfy-ownership.mjs — durable marker so "did the harness start this ComfyUI?"
// is decidable (spec §5). Makes the up-managed vs up-external restart branch sound.
import { writeFileSync, readFileSync, rmSync } from "node:fs";
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
const LAUNCH_MARKER = ".offload-launch.json";
const lp = (dir) => join(dir, LAUNCH_MARKER);

export function writeLaunchOwner(dir, { pid, ownerPid = process.pid, args, profile = {} }) {
  writeFileSync(lp(dir), JSON.stringify({ startedAt: Date.now(), pid, ownerPid, args, profile }));
}
export function readLaunchOwner(dir) {
  try { return JSON.parse(readFileSync(lp(dir), "utf8")); } catch { return null; }
}
export function clearLaunchOwner(dir) {
  try { rmSync(lp(dir), { force: true }); } catch {}
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
