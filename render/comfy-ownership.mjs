// render/comfy-ownership.mjs — durable marker so "did the harness start this ComfyUI?"
// is decidable (spec §5). Makes the up-managed vs up-external restart branch sound.
import { writeFileSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";

const MARKER = ".offload-owned.json";
const p = (dir) => join(dir, MARKER);

function pidAlive(pid) {
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
