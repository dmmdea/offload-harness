// node --test render/atomic-out.test.mjs
//
// writeFileAtomic / copyAtomic: a failed write never leaves a file at `out` and never touches a good file
// already there. A full disk is simulated by an injected writer that fails the way the real one does: the
// open succeeded (so the file exists), a few bytes landed, then the write throws ENOSPC.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, extname, join } from "node:path";
import { commitPartial, copyAtomic, partialSibling, writeFileAtomic } from "./atomic-out.mjs";

const scratch = () => mkdtempSync(join(tmpdir(), "atomic-out-"));
const fsError = (code, msg) => Object.assign(new Error(`${code}: ${msg}`), { code });
const enospc = () => fsError("ENOSPC", "no space left on device, write");
const PNG = Buffer.from("89504e470d0a1a0a0000000d4948445200000010", "hex");
const GOOD = Buffer.from("the previous good render");
const fullDisk = (e = enospc()) => (partial, data) => {
  writeFileSync(partial, data.subarray(0, 3));
  throw e;
};
const names = (dir) => readdirSync(dir).sort();

test("writeFileAtomic: the bytes land at out and no staged file is left beside it", () => {
  const d = scratch(); const out = join(d, "a.png");
  writeFileAtomic(out, PNG);
  assert.deepEqual(readFileSync(out), PNG);
  assert.deepEqual(names(d), ["a.png"]);
});

test("writeFileAtomic: the bytes are staged beside out (same directory, so the rename cannot cross a volume) under a name that is not out's", () => {
  const d = scratch(); const out = join(d, "a.png"); const seen = [];
  writeFileAtomic(out, PNG, { write: (partial, data) => { seen.push(partial); writeFileSync(partial, data); } });
  assert.equal(seen.length, 1);
  assert.equal(dirname(seen[0]), d);
  assert.match(basename(seen[0]), /^a\.png\.partial-\d+-\d+$/);
  assert.notEqual(extname(seen[0]), ".png", "a leftover from a killed process must not pass for a finished png");
});

test("writeFileAtomic replaces an existing file in one step (the rename overwrites on this platform)", () => {
  const d = scratch(); const out = join(d, "a.png");
  writeFileSync(out, GOOD);
  writeFileAtomic(out, PNG);
  assert.deepEqual(readFileSync(out), PNG);
  assert.deepEqual(names(d), ["a.png"]);
});

test("ENOSPC while writing: no file at out, nothing left over, and the SAME error comes back naming the path", () => {
  const d = scratch(); const out = join(d, "a.png"); const e = enospc();
  assert.throws(() => writeFileAtomic(out, PNG, { write: fullDisk(e) }), (got) => {
    assert.equal(got, e, "the original error object, so its code still reaches the classifier");
    assert.equal(got.code, "ENOSPC");
    assert.match(got.message, /no space left on device/);
    assert.ok(got.message.includes(out), "the line must name the output: " + got.message);
    return true;
  });
  assert.equal(existsSync(out), false, "a failed render must leave nothing at out");
  assert.deepEqual(names(d), [], "and no staged file either");
});

test("ENOSPC while writing keeps a previous good file byte for byte (a direct write truncates it)", () => {
  const d = scratch(); const out = join(d, "a.png");
  writeFileSync(out, GOOD);
  assert.throws(() => writeFileAtomic(out, PNG, { write: fullDisk() }), /ENOSPC/);
  assert.deepEqual(readFileSync(out), GOOD);
  assert.deepEqual(names(d), ["a.png"]);
});

test("a write the real filesystem refuses (a missing directory) leaves nothing and keeps its code", () => {
  const d = scratch(); const out = join(d, "missing", "a.png");
  assert.throws(() => writeFileAtomic(out, PNG), (got) => got.code === "ENOENT" && got.message.includes(out));
  assert.deepEqual(names(d), []);
});

test("a rename that fails removes the staged file, keeps the previous file and rethrows (EXDEV is not transient)", () => {
  const d = scratch(); const out = join(d, "a.png"); const e = fsError("EXDEV", "cross-device link not permitted, rename");
  writeFileSync(out, GOOD);
  let tries = 0;
  assert.throws(() => writeFileAtomic(out, PNG, {
    rename: () => { tries++; throw e; },
    sleep: () => assert.fail("EXDEV must not be retried"),
  }), (got) => got === e);
  assert.equal(tries, 1);
  assert.deepEqual(readFileSync(out), GOOD);
  assert.deepEqual(names(d), ["a.png"]);
});

for (const code of ["EBUSY", "EPERM", "EACCES"]) {
  test(`a transient ${code} on the rename is retried with a short backoff and the output is still delivered`, () => {
    const d = scratch(); const out = join(d, "a.png"); const waits = [];
    let calls = 0;
    writeFileAtomic(out, PNG, {
      rename: (from, to) => { if (++calls <= 2) throw fsError(code, "resource busy or locked, rename"); renameSync(from, to); },
      sleep: (ms) => waits.push(ms),
    });
    assert.equal(calls, 3);
    assert.deepEqual(waits, [50, 100]);
    assert.deepEqual(readFileSync(out), PNG);
    assert.deepEqual(names(d), ["a.png"]);
  });
}

test("a rename that stays busy gives up after the bounded retries: staged file removed, error rethrown, previous file kept", () => {
  const d = scratch(); const out = join(d, "a.png"); const e = fsError("EPERM", "operation not permitted, rename");
  writeFileSync(out, GOOD);
  const waits = [];
  let calls = 0;
  assert.throws(() => writeFileAtomic(out, PNG, { rename: () => { calls++; throw e; }, sleep: (ms) => waits.push(ms) }), (got) => got === e);
  assert.equal(calls, 5, "one try plus four retries");
  assert.deepEqual(waits, [50, 100, 200, 400]);
  assert.deepEqual(readFileSync(out), GOOD);
  assert.deepEqual(names(d), ["a.png"]);
});

test("an empty payload is refused before anything is staged, and a previous file is kept", () => {
  for (const empty of [Buffer.alloc(0), "", null, undefined]) {
    const d = scratch(); const out = join(d, "a.png");
    writeFileSync(out, GOOD);
    assert.throws(() => writeFileAtomic(out, empty, { write: () => assert.fail("an empty payload must not be staged") }), /refusing to write a 0-byte output/);
    assert.deepEqual(readFileSync(out), GOOD);
    assert.deepEqual(names(d), ["a.png"]);
  }
});

test("every call stages under its own name", () => {
  const seen = new Set();
  for (let i = 0; i < 5; i++) seen.add(partialSibling(join("some", "dir", "a.png")));
  assert.equal(seen.size, 5);
});

test("partialSibling: extLast keeps the extension last and stays in out's directory; the default form does not end in it", () => {
  const out = join("some", "dir", "clip.mp4");
  const engine = partialSibling(out, { extLast: true, pid: 7 });
  assert.equal(dirname(engine), join("some", "dir"));
  assert.match(basename(engine), /^\.clip\.partial-7-\d+\.mp4$/);
  assert.match(partialSibling(out, { pid: 7 }), /clip\.mp4\.partial-7-\d+$/);
});

test("commitPartial delivers a staged file the caller wrote itself, and removes it when the rename fails for good", () => {
  const d = scratch(); const out = join(d, "a.wav"); const staged = partialSibling(out, { extLast: true });
  writeFileSync(staged, PNG);
  commitPartial(staged, out);
  assert.deepEqual(readFileSync(out), PNG);
  assert.deepEqual(names(d), ["a.wav"]);

  const again = partialSibling(out, { extLast: true });
  writeFileSync(again, GOOD);
  assert.throws(() => commitPartial(again, out, { rename: () => { throw fsError("EXDEV", "rename"); } }), /EXDEV/);
  assert.deepEqual(names(d), ["a.wav"], "the staged copy is gone and the delivered file is untouched");
  assert.deepEqual(readFileSync(out), PNG);
});

test("copyAtomic: a directory arrives whole through a staged sibling", () => {
  const d = scratch(); const src = join(d, "src"); const dst = join(d, "frames");
  mkdirSync(src);
  writeFileSync(join(src, "f1.png"), "one");
  writeFileSync(join(src, "f2.png"), "two");
  copyAtomic(src, dst);
  assert.deepEqual(names(dst), ["f1.png", "f2.png"]);
  assert.deepEqual(names(d), ["frames", "src"]);
});

test("copyAtomic: a file copies too, over a previous file", () => {
  const d = scratch(); const src = join(d, "clip.mp4"); const dst = join(d, "out.mp4");
  writeFileSync(src, "new clip");
  writeFileSync(dst, "old clip");
  copyAtomic(src, dst);
  assert.equal(readFileSync(dst, "utf8"), "new clip");
  assert.deepEqual(names(d), ["clip.mp4", "out.mp4"]);
});

test("copyAtomic: a copy that fails midway leaves no destination and no staged directory; a previous file stays", () => {
  const d = scratch(); const src = join(d, "src"); mkdirSync(src);
  writeFileSync(join(src, "f1.png"), "one");
  const halfWay = (_from, partial) => {
    mkdirSync(partial);
    writeFileSync(join(partial, "f1.png"), "o");
    throw enospc();
  };

  const dst = join(d, "frames");
  assert.throws(() => copyAtomic(src, dst, { copy: halfWay }), (got) => got.code === "ENOSPC" && got.message.includes(dst));
  assert.equal(existsSync(dst), false);
  assert.deepEqual(names(d), ["src"]);

  const kept = join(d, "kept.mp4");
  writeFileSync(kept, GOOD);
  assert.throws(() => copyAtomic(src, kept, { copy: halfWay }), /ENOSPC/);
  assert.deepEqual(readFileSync(kept), GOOD);
  assert.deepEqual(names(d), ["kept.mp4", "src"]);
});
