// atomic-out.mjs — how a render helper delivers a FINAL output file. Dependency-free (Node 18+).
//
// Why it exists (2026-10-09, an overnight picture batch): the data drive filled to 0 GB mid-batch and
// `writeFileSync(out, buf)` — which opens, and so TRUNCATES, the target before it writes a byte — left
// 21 zero-byte PNGs at the jobs' `out` paths. A zero-byte file passes every exists() check, so a
// skip-existing builder never re-rendered one. The same call also destroys a previous GOOD file at
// `out` whenever the new write fails.
//
// So no helper writes its target in place any more. The bytes go to a staged sibling in the SAME
// directory (a rename never crosses a volume, which is what makes it atomic), and only a write that
// finished is renamed over `out` (POSIX rename(2); Node's renameSync on Windows is MoveFileEx with
// REPLACE_EXISTING). Any failure removes the staged file and rethrows the SAME error object, so a
// failed render never leaves a file at `out` and a good file already there is left untouched. This is
// protection against a failed write, not against a power cut: nothing is fsynced.
//
// The igpu lanes (igpu-engine.mjs partialPath/deliverFile) already worked this way for their engine's
// output; this module is the same discipline for the writers that had been writing in place.
import { cpSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { basename, dirname, extname, join } from "node:path";

let seq = 0;

// partialSibling: where a result is staged before it is delivered. The pid keeps two runners apart and
// the counter keeps two calls of one runner apart. The default name `<out>.partial-<pid>-<n>` does not
// end in the output's extension, so a leftover from a process that was killed mid-write (nothing can
// clean up after a taskkill) is never mistaken for a finished output by a `*.png` glob or a
// skip-existing check. `extLast` is for a file an ENGINE writes, such as sd-cli, which picks its format
// from the extension of its `-o`: there the extension stays last and the dot prefix hides the file.
export function partialSibling(out, { extLast = false, pid = process.pid } = {}) {
  const tag = `partial-${pid}-${++seq}`;
  if (!extLast) return `${out}.${tag}`;
  const ext = extname(out);
  return join(dirname(out), `.${basename(out, ext)}.${tag}${ext}`);
}

// A rename onto a file can fail for a moment on Windows while an antivirus scanner or the search
// indexer still holds the staged file (or a viewer holds the target), and a delivery that fails here
// throws away a render that took minutes. A few short, bounded retries; any other code, and any
// code that outlasts them, is a real failure.
const RENAME_RETRY = new Set(["EPERM", "EACCES", "EBUSY"]);
const RENAME_BACKOFF_MS = Object.freeze([50, 100, 200, 400]);
const sleepSync = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

// discardPartial: best-effort removal of a staged file or directory. It never throws, because the
// error being reported matters more than the litter, and on a full disk the removal is what gives the
// partial bytes back. Exported for the callers that stage a file through an external encoder.
export function discardPartial(partial, rm = rmSync) {
  try { rm(partial, { recursive: true, force: true }); } catch { /* best effort */ }
}

// namePath: what reaches the log says WHICH output failed. Node's message for a write that fails after
// the open names no path at all ("ENOSPC: no space left on device, write" is all the 2026-10-09 log
// carried). The SAME error object is rethrown with only its message extended, so `code` (ENOSPC,
// EDQUOT, EROFS) still reaches whatever classifies it (batch-jobs.mjs isDiskFullError).
function namePath(e, out) {
  if (e && typeof e.message === "string" && !e.message.includes(out)) {
    try { e.message = `${e.message} (writing ${out})`; } catch { /* a frozen error keeps its own message */ }
  }
  return e;
}

// commitPartial: make a finished staged file (or directory) the thing at `out`. On failure the staged
// copy is removed and the error rethrown; `out` is whatever it was before.
export function commitPartial(partial, out, { rename = renameSync, rm = rmSync, sleep = sleepSync } = {}) {
  for (let attempt = 0; ; attempt++) {
    try {
      rename(partial, out);
      return;
    } catch (e) {
      if (attempt < RENAME_BACKOFF_MS.length && RENAME_RETRY.has(e && e.code)) {
        sleep(RENAME_BACKOFF_MS[attempt]);
        continue;
      }
      discardPartial(partial, rm);
      throw namePath(e, out);
    }
  }
}

// commitAtomic: `stage(partial)` produces the complete result at the staged path; it is then committed
// over `out`. A stage that throws leaves nothing behind.
export function commitAtomic(out, stage, { partial = partialSibling(out), ...deps } = {}) {
  try {
    stage(partial);
  } catch (e) {
    discardPartial(partial, deps.rm);
    throw namePath(e, out);
  }
  commitPartial(partial, out, deps);
}

// writeFileAtomic: the replacement for writeFileSync(out, data) on a final output. An empty payload is
// refused outright: every writer here delivers media or JSON, and a 0-byte file at `out` is exactly the
// "looks finished" symptom this module exists to end (a /view that answers 200 with no body would
// otherwise be delivered as a finished render). `write` and the commit deps are injectable for tests only.
export function writeFileAtomic(out, data, { write = writeFileSync, ...deps } = {}) {
  if (data == null || data.length === 0) {
    throw new Error(`refusing to write a 0-byte output at ${out}: an empty file looks finished to anything that checks it exists`);
  }
  commitAtomic(out, (partial) => write(partial, data), deps);
}

// copyAtomic: a file or a whole directory copied to `dst` through a staged sibling, for the move that
// cannot rename (a work dir on another volume): a copy straight onto `dst` left a partial file there
// whenever the destination filled up.
export function copyAtomic(src, dst, { copy = cpSync, ...deps } = {}) {
  commitAtomic(dst, (partial) => copy(src, partial, { recursive: true }), deps);
}
