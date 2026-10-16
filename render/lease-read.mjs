// lease-read.mjs - the one way render/ reads a lease file the Go side owns (internal/gpulease). Every Node reader of the lease
// goes through it: gpu-lock.mjs (readLease, readEpochRecord, readCardClaim, and so checkInheritedLease, the fence before an
// irreversible action) and lease-progress.mjs (the record behind GPU_LEASE_PROGRESS_FILE). Node never writes these files.
//
// WHY A READ IS NOT JUST readFileSync. The Go side replaces a lease record (e/<epoch>.json, meta.json), a card claim and the
// heartbeat by a rename over the live name. On Windows, for the instant a rename has marked the old entry for deletion and the new
// one has not taken its name, the file is delete-pending and EVERY open of the name fails with ERROR_ACCESS_DENIED, whoever
// shares what. libuv spells that EPERM (a probe with a delete-pending file reads exactly that; EACCES is its POSIX twin). A reader
// that took the failure for "no record" answered null for a record that is there a moment later, and a null record for a card
// lease falls through to meta.json, which a card lease never writes: the fence called a held lease not current and the render
// aborted. The refusal lasts milliseconds, so the read waits it out.
//
//   - Only EPERM and EACCES are retried, DENIED_RETRIES times at DENIED_PAUSE_MS (about 100 ms in all: the bound
//     internal/gpulease uses, removeAttempts x removePause). Anything else, a MISSING FILE above all, is read once and is null
//     at once: a missing file is an answer ("no such lease"), so a released lease still reads absent without a wait.
//   - Only where a rename can cause the refusal: Windows. Elsewhere rename(2) is atomic, nothing is delete-pending, and a
//     permission error is a lasting refusal (a file the caller cannot read), which a wait only delays reporting. The Go side
//     makes the same choice (sharedOpenMeetsDeletePending). `policy` carries it so a test can run the wait on any platform.
//   - The pause is synchronous (Atomics.wait, as atomic-out.mjs and igpu-engine.mjs sleep): the readers are sync functions the
//     fence calls inline, and Atomics.wait is allowed on Node's main thread.
//
// No npm dependencies.
import { readFileSync } from "node:fs";

/** Retries after the first read, and the pause between them: 20 x 5 ms, the Go side's removeAttempts x removePause. */
export const DENIED_RETRIES = 20;
export const DENIED_PAUSE_MS = 5;

/** waitsOutDenied: is a permission refusal of an open worth waiting out here? Windows only (see above). */
export const policy = { waitsOutDenied: process.platform === "win32" };

const REFUSALS = new Set(["EPERM", "EACCES"]);
const sleepSync = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

/**
 * readSharedSync: the text of `path`, or null when it cannot be read. A permission refusal is waited out (Windows, above);
 * every other failure, a missing file included, is null at once. `read` and `sleep` are seams for the tests.
 */
export function readSharedSync(path, { read = (p) => readFileSync(p, "utf8"), sleep = sleepSync } = {}) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return read(path);
    } catch (e) {
      if (!policy.waitsOutDenied || !REFUSALS.has(e && e.code) || attempt >= DENIED_RETRIES) return null;
      sleep(DENIED_PAUSE_MS);
    }
  }
}

/** readSharedJson: readSharedSync, parsed. Null for a file that cannot be read and for text that does not parse. */
export function readSharedJson(path, opts) {
  const raw = readSharedSync(path, opts);
  if (raw === null) return null;
  try { return JSON.parse(raw); } catch { return null; }
}
