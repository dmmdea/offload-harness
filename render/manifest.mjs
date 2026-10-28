// render/manifest.mjs — parse/validate/hash a run-graph node manifest (spec §2).
import { createHash } from "node:crypto";

// packNameFromRepo: a filesystem-safe custom_nodes dir name derived from the repo URL when
// the manifest omits `name` — its last path segment minus `.git` (e.g.
// https://github.com/x/ComfyUI-RMBG(.git) → "ComfyUI-RMBG"). NEVER returns a value containing
// "://" or "/", so it can't be used as a raw repo URL was (which would nest bogus dirs).
export function packNameFromRepo(repo) {
  const seg = String(repo || "").replace(/\.git$/i, "").replace(/[\/\\]+$/, "").split(/[\/\\]/).pop() || "";
  const safe = seg.replace(/[^A-Za-z0-9._-]/g, "-").replace(/^-+/, "");
  return safe || "pack";
}

export function parseManifest(input) {
  const m = typeof input === "string" ? JSON.parse(input) : input;
  if (!m || typeof m !== "object") throw new Error("manifest: not an object");
  const node_packs = (m.node_packs || []).map((p, i) => {
    if (!p.repo) throw new Error(`node_pack #${i} missing repo`);
    // A commit a manifest names is not read: the node installs the head of the repository's default branch.
    return { name: p.name || packNameFromRepo(p.repo), repo: p.repo };
  });
  const models = (m.models || []).map((x, i) => {
    if (!x.path) throw new Error(`model #${i} missing path`);
    // `trust` is the template run path's: "tofu" asks for trust on first use of a file that declares no hash (verifyTofuModels in
    // manifest-satisfy.mjs). It is carried only when given, so a manifest that names none parses to what it always did.
    return { path: x.path, source_url: x.source_url || "", sha256: x.sha256 ?? null, ...(typeof x.trust === "string" && x.trust !== "" ? { trust: x.trust } : {}) };
  });
  return {
    schema_version: m.schema_version ?? 1,
    workflow: m.workflow || "",
    comfyui_min_version: m.comfyui_min_version || "",
    node_packs, models,
  };
}

// manifestHash: identifies a provisioned ENVIRONMENT. Sorted so pack/model order never
// changes the hash; only the pack repos + model path/sha matter (source_url is irrelevant
// to what's on disk). A pack is identified by its repository: the commit is whatever the node recorded at install.
export function manifestHash(manifest) {
  const m = manifest.schema_version ? manifest : parseManifest(manifest);
  const packs = m.node_packs.map((p) => p.repo).sort();
  const models = m.models.map((x) => `${x.path}#${x.sha256 || ""}`).sort();
  return createHash("sha256").update(JSON.stringify({ packs, models })).digest("hex").slice(0, 16);
}
