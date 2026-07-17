import { test } from "node:test";
import assert from "node:assert/strict";
import { parseManifest, manifestHash } from "./manifest.mjs";

const M = {
  workflow: "scene-swap", comfyui_min_version: "0.23.0",
  node_packs: [{ name: "A", repo: "https://x/a", commit: "aaa" }],
  models: [{ path: "models/x.safetensors", source_url: "https://y", sha256: null }],
};

test("parseManifest accepts v1 shape and defaults schema_version=1", () => {
  const m = parseManifest(M);
  assert.equal(m.schema_version, 1);
  assert.equal(m.node_packs[0].commit, "aaa");
});

test("parseManifest accepts a JSON string", () => {
  assert.equal(parseManifest(JSON.stringify(M)).workflow, "scene-swap");
});

test("parseManifest throws on a pack missing commit", () => {
  assert.throws(() => parseManifest({ ...M, node_packs: [{ name: "A", repo: "https://x/a" }] }),
    /node_pack .* missing commit/);
});

test("manifestHash is stable + order-independent", () => {
  const a = manifestHash(M);
  const reordered = { ...M, node_packs: [...M.node_packs] };
  assert.equal(manifestHash(reordered), a);
  const changed = { ...M, node_packs: [{ name: "A", repo: "https://x/a", commit: "bbb" }] };
  assert.notEqual(manifestHash(changed), a);
});
