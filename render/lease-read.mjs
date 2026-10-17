// lease-read.mjs - the one way render/ reads a lease file the Go side owns (internal/gpulease). Every Node reader of the lease
// goes through it: gpu-lock.mjs (readLease, readEpochRecord, readCardClaim, and so checkInheritedLease, the fence before an
// irreversible action) and lease-progress.mjs (the record behind GPU_LEASE_PROGRESS_FILE). Node never writes these files.
//
// WHY A READ IS NOT JUST readFileSync. The Go side replaces a lease record (e/<epoch>.json, meta.json) and the heartbeat by a
// rename over the live name (a card claim is not replaced: it is created once and deleted, never renamed, so a claim meets the
// states below only while its removal is pending). On Windows a rename that succeeded is not instant, and a reader can meet it twice:
//
//   1. For the instant a rename has marked the old entry for deletion and the new one has not taken its name, the file is
//      delete-pending and EVERY open of the name fails with ERROR_ACCESS_DENIED, whoever shares what. libuv spells that EPERM (a
//      probe with a delete-pending file reads exactly that; EACCES is its POSIX twin). The refusal lasts milliseconds.
//   2. For tens of milliseconds, after a rename that was NOT refused, the live name is ABSENT while the staged file (<path>.tmp, or
//      meta.json.restamp) still stands beside it: ENOENT. Measured on one host, 11 to 75 ms as a reader sees it, one rename that took
//      119 ms to return, about once in 74,000 renames; the mechanism is unverified.
//
// A reader that took either for "no record" answered null for a record that is there a moment later, and a null record for a card
// lease falls through to meta.json, which a card lease never writes: the fence called a held lease not current and the render
// aborted. So both are waited out:
//
//   - EPERM and EACCES are retried, pausing DENIED_PAUSE_MS between reads until DENIED_BUDGET_MS have passed (about 100 ms: the
//     bound internal/gpulease uses, removeAttempts x removePause).
//   - ENOENT is an answer only once it is SETTLED. While a staged file younger than STAGING_FRESH_MS stands beside the name (the
//     names the Go writers stage under are STAGING_SUFFIXES, pinned against Go by internal/gpulease/staging_drift_test.go; a claim
//     has none) the read waits for the rename to end, pausing ABSENT_PAUSE_MS, until ABSENT_BUDGET_MS have passed since it first met
//     it. With none, one look at the name itself decides, taken AFTER the look at the staged files: the rename can end between the
//     read and that look, and the name is then back; a name that is there is read again, within the same deadline. A file that is
//     simply not there (a released lease) costs one read and those looks, and no pause.
//   - When the ENOENT wait runs out the result is null. Node has no "cannot be read" class, so the fence reads it as "not current"
//     and aborts: loud and fail-safe, never a grant (Node only reads). The Go side returns an I/O failure at the same point.
//   - Each bound is a DEADLINE, not a count of pauses. Atomics.wait(5) sleeps one timer tick on Windows, measured 15.4 ms (1, 2, 5
//     and 10 all come back in 15 ms), so 20 pauses of 5 ms would have been 308 ms. The deadline holds each wait to its bound
//     whatever the tick.
//   - Only where a rename can cause either state: Windows. Elsewhere rename(2) is atomic, nothing is delete-pending or absent
//     mid-rename, and a permission error is a lasting refusal (a file the caller cannot read), which a wait only delays
//     reporting. A missing file is then read once and is null at once, with no look at anything. The Go side makes the same
//     choice (sharedOpenMeetsDeletePending, deniedOpenMayClear). `policy` carries it so a test can run the waits on any platform.
//   - The pause is synchronous (Atomics.wait, as atomic-out.mjs and igpu-engine.mjs sleep): the readers are sync functions the
//     fence calls inline, and Atomics.wait is allowed on Node's main thread.
//   - There is no memory of a wait that ran out, as the Go side keeps (a grant there reads one heartbeat about seven times inside
//     one hold of the epoch lock). A Node reader reads a file once per fence or beat, and a fence that gets null aborts.
//
// No npm dependencies.
import { readFileSync, statSync } from "node:fs";

/** How long a refused open is waited out, and the pause asked for between reads: the Go side's removeAttempts x removePause (20 x 5 ms). */
export const DENIED_BUDGET_MS = 100;
export const DENIED_PAUSE_MS = 5;

/** How long a read waits for a rename to end while a fresh staged file stands beside an absent name, and the pause between reads: the Go side's absentBudget and absentPause. */
export const ABSENT_BUDGET_MS = 250;
export const ABSENT_PAUSE_MS = 5;

/** How young a staged file must be to count as a write that may still be installing: the Go side's stagingFresh (the longest a writer keeps retrying its rename, 2 s, plus a second). */
export const STAGING_FRESH_MS = 3000;

/** The suffixes the Go writers stage a lease file under before they rename it over the live name (internal/gpulease stagingPath). */
export const STAGING_SUFFIXES = [".tmp", ".restamp"];

/** waitsOutDenied: are the waits worth making here? Windows only (see above). */
export const policy = { waitsOutDenied: process.platform === "win32" };

const REFUSALS = new Set(["EPERM", "EACCES"]);
const sleepSync = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

/**
 * stagingFor: the staged names a writer may have beside `path`, and none for a card claim (cards/<id>.claim), which is created once and
 * removed, never renamed over.
 */
export function stagingFor(path) {
  if (/(^|[\\/])cards[\\/][^\\/]+\.claim$/.test(path)) return [];
  return STAGING_SUFFIXES.map((suffix) => path + suffix);
}

const statQuiet = (p) => statSync(p, { throwIfNoEntry: false });

// Is a staged file beside the name that its writer may still be installing? It is there, is a file, and is younger than
// STAGING_FRESH_MS. A negative age (a clock that stepped back) counts as fresh, and so does a staged file that cannot be examined for any
// reason but being absent: neither is evidence that nothing is in flight.
function stagedInFlight(siblings, stat, wall) {
  for (const sibling of siblings) {
    let st;
    try {
      st = stat(sibling);
    } catch (e) {
      if (e && e.code === "ENOENT") continue;
      return true;
    }
    if (!st) continue;
    if (typeof st.isFile === "function" && !st.isFile()) continue; // a writer stages a file; a directory in its place is no rename
    if (wall() - st.mtimeMs < STAGING_FRESH_MS) return true;
  }
  return false;
}

// Is the name itself absent? A look that fails for any reason but the name being absent is not an answer: the name counts as there.
function nameIsAbsent(path, stat) {
  try {
    return !stat(path);
  } catch (e) {
    return !!(e && e.code === "ENOENT");
  }
}

/**
 * readSharedSync: the text of `path`, or null when it cannot be read. A permission refusal is waited out, and so is an absent name
 * with a fresh staged file beside it (Windows, above); every other failure is null at once, and so is a name that is absent with
 * nothing being installed. `read`, `sleep`, `now`, `wall`, `stat` and `staging` are seams for the tests: `now` is the monotonic clock
 * the deadlines run on, `wall` the clock a staged file's age is measured against, `stat` returns undefined for a name that is absent
 * and `staging` lists the staged names to look for.
 */
export function readSharedSync(path, {
  read = (p) => readFileSync(p, "utf8"), sleep = sleepSync, now = () => performance.now(),
  stat = statQuiet, staging = stagingFor, wall = () => Date.now(),
} = {}) {
  let started = null; // when the wait for a refused open began
  let since = null; // when the wait for a rename began
  let rechecks = 0; // times the look at the name found it there after a read that did not
  for (;;) {
    try {
      return read(path);
    } catch (e) {
      if (!policy.waitsOutDenied) return null;
      const code = e && e.code;
      if (code === "ENOENT") {
        const siblings = staging(path);
        if (siblings.length === 0) return null; // never replaced by a rename: absent is the answer, with no look at anything
        if (stagedInFlight(siblings, stat, wall)) {
          if (since === null) since = now();
          if (now() - since >= ABSENT_BUDGET_MS) return null;
          sleep(ABSENT_PAUSE_MS);
          continue;
        }
        if (nameIsAbsent(path, stat)) return null; // settled
        // The name is there although the read did not find it: the rename ended in between. Read again, within the same deadline.
        if (since === null) since = now();
        if (now() - since >= ABSENT_BUDGET_MS) return null;
        if (rechecks++ > 0) sleep(ABSENT_PAUSE_MS);
        continue;
      }
      if (!REFUSALS.has(code)) return null;
      if (started === null) started = now();
      if (now() - started >= DENIED_BUDGET_MS) return null;
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
