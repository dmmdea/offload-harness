// node --test render/comfy-input.test.mjs
// Staged input names. Runners copy their source files into the ONE input directory every
// ComfyUI instance shares, and delete them afterwards. With one instance per card, several
// runner processes stage at once, so a name built from the clock and the basename alone can
// collide: one process overwrites another's input, and its cleanup then removes it from under
// the other. A name must be unique across processes, not only inside one.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import * as I from "./comfy-input.mjs";

const scratch = () => mkdtempSync(join(tmpdir(), "comfy-input-"));

test("stagedInputName: one file staged by two processes in the same millisecond gets two names", () => {
  const base = { now: () => 5, rand: () => "aa", n: 0 };
  const a = I.stagedInputName("render_in", join("x", "ref.png"), { ...base, pid: 100 });
  assert.equal(a, "render_in_5-100-aa_0_ref.png");
  assert.notEqual(a, I.stagedInputName("render_in", join("x", "ref.png"), { ...base, pid: 101 }), "another process");
  assert.notEqual(a, I.stagedInputName("render_in", join("x", "ref.png"), { ...base, pid: 100, rand: () => "bb" }), "a pid that was reused");
  assert.notEqual(a, I.stagedInputName("render_in", join("x", "ref.png"), { ...base, pid: 100, n: 1 }), "a second file of the same run");
  assert.notEqual(a, I.stagedInputName("render_in", join("x", "ref.png"), { ...base, pid: 100, now: () => 6 }), "a later run");
});

test("stagedInputName: without injection it is unique per call and per process, and keeps the file's own name last", () => {
  const names = new Set();
  for (let i = 0; i < 200; i++) names.add(I.stagedInputName("upscale_in", join("a", "same.png")));
  assert.equal(names.size, 200, "the same file staged twice in one process never collides");
  const one = I.stagedInputName("upscale_in", join("a", "same.png"));
  assert.match(one, /^upscale_in_\d+-\d+-[0-9a-f]+_\d+_same\.png$/);
  assert.ok(one.includes("-" + process.pid + "-"), "carries this process's pid");
  const fixed = { now: () => 5, pid: 1, rand: () => "aa" };
  assert.notEqual(I.stagedInputName("x", "f.png", fixed), I.stagedInputName("x", "f.png", fixed), "an image and its mask staged in one millisecond never share a name, whatever the randomness");
});

test("stageInput: copies into the input directory under that name and returns it", () => {
  const root = scratch(); const inputDir = join(root, "input");
  writeFileSync(join(root, "src.png"), "px");
  const copies = [];
  const name = I.stageInput("inpaint_in", join(root, "src.png"), { inputDir, copy: (...a) => copies.push(a), now: () => 9, pid: 3, rand: () => "cc", n: 4 });
  assert.equal(name, "inpaint_in_9-3-cc_4_src.png");
  assert.deepEqual(copies, [[join(root, "src.png"), join(inputDir, name)]]);
  // and for real
  mkdirSync(inputDir, { recursive: true });
  const real = I.stageInput("render_in", join(root, "src.png"), { inputDir });
  assert.equal(readFileSync(join(inputDir, real), "utf8"), "px");
  assert.deepEqual(readdirSync(inputDir), [real]);
});

test("every runner stages through the shared helper; none builds a clock-only name", () => {
  for (const r of ["video", "animate", "inpaint", "upscale", "edit"]) {
    const src = readFileSync(new URL(`./comfy-${r}.mjs`, import.meta.url), "utf8");
    assert.match(src, /from "\.\/comfy-input\.mjs"/, `comfy-${r}.mjs does not use the shared staging helper`);
    assert.ok(!/_in_"\s*\+\s*Date\.now\(\)/.test(src) && !/_in_\$\{stamp\}_\$\{i\}/.test(src), `comfy-${r}.mjs still builds a clock-only staged name`);
  }
});
