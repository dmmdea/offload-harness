// lease-read.mjs - the one way render/ reads a lease file the Go side owns. NOT FINISHED: this is the plain read the
// readers had; the next commit adds the wait.
import { readFileSync } from "node:fs";

export const DENIED_RETRIES = 20;
export const DENIED_PAUSE_MS = 5;
export const policy = { waitsOutDenied: process.platform === "win32" };

export function readSharedSync(path, { read = (p) => readFileSync(p, "utf8") } = {}) {
  try { return read(path); } catch { return null; }
}

export function readSharedJson(path, opts) {
  const raw = readSharedSync(path, opts);
  if (raw === null) return null;
  try { return JSON.parse(raw); } catch { return null; }
}
