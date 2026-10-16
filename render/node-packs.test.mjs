import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { PINNED_NODE_PACKS, loadPinnedPacks, checkNodePack } from "./node-packs.mjs";
import { NODE_PACKS } from "./comfy-nodes.mjs";

const VHS = "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite";
const VHS_PIN = "4d907bee61e92c2e65af3bd6383a4e4d356126d1";
const MGPU = "https://github.com/pollockjj/ComfyUI-MultiGPU";
const MGPU_MIRROR = "https://github.com/dmmdea/ComfyUI-MultiGPU-mirror";
const MGPU_PIN = "ed1ffaef7cec1a66f35106c6a4c7a40927c2dc83";

const file = JSON.parse(readFileSync(new URL("./node-packs.json", import.meta.url), "utf8"));

test("node-packs.json: every row is a name, an https repository and a full lower-case commit", () => {
  assert.ok(Array.isArray(file.packs) && file.packs.length > 0, "the table lists no pack");
  for (const p of file.packs) {
    assert.match(p.name, /^[A-Za-z0-9][A-Za-z0-9._-]*$/, `${p.repo}: name is not a plain directory name`);
    assert.match(p.repo, /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/, `${p.name}: repo`);
    assert.match(p.commit, /^[0-9a-f]{40}$/, `${p.name}: commit`);
    assert.ok(["documented", "unverified"].includes(p.pin), `${p.name}: pin must say whether the commit is documented or unverified`);
  }
});

test("node-packs.json: the archived pack is pinned at the documented commit, upstream and mirror", () => {
  for (const repo of [MGPU, MGPU_MIRROR]) {
    const row = file.packs.find((p) => p.repo === repo);
    assert.ok(row, `${repo} is not in the table`);
    assert.equal(row.commit, MGPU_PIN);
  }
});

test("node-packs.json: every pack the MISSING_NODE hints send an operator to is pinned", () => {
  const repos = new Set(file.packs.map((p) => p.repo));
  for (const [cls, hint] of Object.entries(NODE_PACKS)) {
    for (const m of hint.matchAll(/https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+/g)) {
      assert.ok(repos.has(m[0]), `${cls}: the hint names ${m[0]}, which has no pinned row`);
    }
  }
});

test("the module reads the file: its table is the file's rows", () => {
  assert.deepEqual(PINNED_NODE_PACKS.map((p) => [p.name, p.repo, p.commit]), file.packs.map((p) => [p.name, p.repo, p.commit]));
});

test("checkNodePack accepts a pinned pack and refuses everything else, with the pack named", () => {
  assert.equal(checkNodePack({ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: VHS_PIN }), null);
  assert.equal(checkNodePack({ name: "ComfyUI-MultiGPU", repo: MGPU_MIRROR, commit: MGPU_PIN }), null);
  const refused = [
    [{ name: "payload", repo: "https://example.invalid/payload", commit: VHS_PIN }, "payload"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: "0".repeat(40) }, "ComfyUI-VideoHelperSuite"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: "main" }, "ComfyUI-VideoHelperSuite"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: VHS_PIN.slice(0, 8) }, "ComfyUI-VideoHelperSuite"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: VHS_PIN.toUpperCase() }, "ComfyUI-VideoHelperSuite"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS + "/", commit: VHS_PIN }, "ComfyUI-VideoHelperSuite"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: MGPU_PIN }, "ComfyUI-VideoHelperSuite"],
    [{ name: "../../x", repo: VHS, commit: VHS_PIN }, "../../x"],
    [{ name: "ComfyUI-Manager", repo: VHS, commit: VHS_PIN }, "ComfyUI-Manager"],
    [{ repo: "--upload-pack=evil", commit: VHS_PIN }, "--upload-pack=evil"],
    [{ name: "x", commit: VHS_PIN }, "x"],
    [null, "#"],
  ];
  for (const [pack, named] of refused) {
    const r = checkNodePack(pack);
    assert.ok(r, `${JSON.stringify(pack)} was accepted`);
    assert.equal(r.code, "NODE_PACK_NOT_ALLOWED");
    assert.ok(String(r.ref).includes(named), `the refusal names ${JSON.stringify(r.ref)}, want ${named}`);
  }
});

test("an unreadable table is an empty one: every pack is refused", () => {
  const rows = loadPinnedPacks(new URL("./no-such-node-packs.json", import.meta.url));
  assert.deepEqual([...rows], []);
  assert.ok(checkNodePack({ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: VHS_PIN }, rows));
});

test("a malformed row is dropped, not trusted", () => {
  // a table whose only row has a short commit loads as empty
  const rows = loadPinnedPacks(new URL("./testdata/node-packs-short-commit.json", import.meta.url));
  assert.deepEqual([...rows], []);
});
