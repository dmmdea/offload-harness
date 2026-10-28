import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { PINNED_NODE_PACKS, loadPinnedPacks, checkNodePack, PINNED_MODEL_HOSTS, loadModelHosts, checkModelEntry, normalizeModelPath } from "./node-packs.mjs";
import { NODE_PACKS } from "./comfy-nodes.mjs";

const VHS = "https://github.com/Kosinkadink/ComfyUI-VideoHelperSuite";
const MGPU = "https://github.com/pollockjj/ComfyUI-MultiGPU";
const MGPU_MIRROR = "https://github.com/dmmdea/ComfyUI-MultiGPU-mirror";

const file = JSON.parse(readFileSync(new URL("./node-packs.json", import.meta.url), "utf8"));

test("node-packs.json: every row is a name and an https repository, and no row carries a commit", () => {
  assert.ok(Array.isArray(file.packs) && file.packs.length > 0, "the table lists no pack");
  for (const p of file.packs) {
    assert.match(p.name, /^[A-Za-z0-9][A-Za-z0-9._-]*$/, `${p.repo}: name is not a plain directory name`);
    assert.match(p.repo, /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/, `${p.name}: repo`);
    assert.equal(p.commit, undefined, `${p.name}: admission is by repository, a commit is recorded at install and not written here`);
  }
});

test("node-packs.json: the archived pack is listed by repository, upstream and mirror", () => {
  for (const repo of [MGPU, MGPU_MIRROR]) {
    assert.ok(file.packs.find((p) => p.repo === repo), `${repo} is not in the table`);
  }
});

test("node-packs.json: every pack the MISSING_NODE hints send an operator to is listed", () => {
  const repos = new Set(file.packs.map((p) => p.repo));
  for (const [cls, hint] of Object.entries(NODE_PACKS)) {
    for (const m of hint.matchAll(/https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+/g)) {
      assert.ok(repos.has(m[0]), `${cls}: the hint names ${m[0]}, which has no allowlist row`);
    }
  }
});

test("the module reads the file: its table is the file's rows", () => {
  assert.deepEqual(PINNED_NODE_PACKS.map((p) => [p.name, p.repo]), file.packs.map((p) => [p.name, p.repo]));
});

test("checkNodePack admits an allowlisted repository and refuses everything else, with the pack named", () => {
  assert.equal(checkNodePack({ name: "ComfyUI-VideoHelperSuite", repo: VHS }), null);
  assert.equal(checkNodePack({ name: "ComfyUI-MultiGPU", repo: MGPU_MIRROR }), null);
  // admission is by repository: a commit the manifest names is not read
  assert.equal(checkNodePack({ name: "ComfyUI-VideoHelperSuite", repo: VHS, commit: "main" }), null);
  const refused = [
    [{ name: "payload", repo: "https://example.invalid/payload" }, "payload"],
    [{ name: "ComfyUI-VideoHelperSuite", repo: VHS + "/" }, "ComfyUI-VideoHelperSuite"],
    [{ name: "../../x", repo: VHS }, "../../x"],
    [{ name: "ComfyUI-Manager", repo: VHS }, "ComfyUI-Manager"],
    [{ repo: "--upload-pack=evil" }, "upload-pack-evil"],
    [{ name: "x" }, "x"],
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
  assert.ok(checkNodePack({ name: "ComfyUI-VideoHelperSuite", repo: VHS }, rows));
});

// --- the models a manifest may name -----------------------------------------------------------------------------
// The same rows drive internal/mediacap/manifestmodels_test.go, so the node's door and this satisfier take and
// refuse the same entries.
const cases = JSON.parse(readFileSync(new URL("./testdata/manifest-model-cases.json", import.meta.url), "utf8")).cases;

test("node-packs.json lists the hosts a model may be fetched from, and the module reads them", () => {
  assert.ok(Array.isArray(file.model_hosts) && file.model_hosts.length > 0, "the table lists no model_hosts");
  for (const h of file.model_hosts) assert.match(h, /^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$/, `${h} is not a plain lower-case host name`);
  assert.deepEqual([...PINNED_MODEL_HOSTS], file.model_hosts);
});

for (const c of cases) {
  test(`checkModelEntry: ${c.name}`, () => {
    const r = checkModelEntry({ path: c.path, source_url: c.source_url, sha256: null });
    if (c.ok) {
      assert.equal(r, null, `refused an entry the node provisions: ${JSON.stringify(r)}`);
    } else {
      assert.ok(r, `took path ${JSON.stringify(c.path)} from ${JSON.stringify(c.source_url)}`);
      assert.equal(r.code, "MODEL_NOT_ALLOWED");
      assert.equal(r.ref, c.path);
    }
  });
}

test("checkModelEntry refuses an entry that is not a path and a source, without throwing", () => {
  for (const entry of [null, undefined, "models/vae/x.safetensors", 5, { path: 5 }, { path: ["models/vae/x.safetensors"] }, { path: { toString: () => "models/vae/x.safetensors" } },
    { path: "models/vae/x.safetensors", source_url: 5 }, { path: "models/vae/x.safetensors", source_url: ["https://huggingface.co/x"] }]) {
    const r = checkModelEntry(entry);
    assert.ok(r, `${JSON.stringify(entry)} was accepted`);
    assert.equal(r.code, "MODEL_NOT_ALLOWED");
  }
});

test("normalizeModelPath is the path the check read: backslashes are separators", () => {
  assert.equal(normalizeModelPath("models\\vae\\ae.safetensors"), "models/vae/ae.safetensors");
  assert.equal(normalizeModelPath("models/vae/ae.safetensors"), "models/vae/ae.safetensors");
});

test("an unreadable table lists no host: every download is refused, a model already on disk is not", () => {
  const hosts = loadModelHosts(new URL("./no-such-node-packs.json", import.meta.url));
  assert.deepEqual([...hosts], []);
  assert.ok(checkModelEntry({ path: "models/vae/x.safetensors", source_url: "https://huggingface.co/x/y" }, hosts));
  assert.equal(checkModelEntry({ path: "models/vae/x.safetensors", source_url: "" }, hosts), null);
});
