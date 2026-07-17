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
    if (!p.commit) throw new Error(`node_pack ${p.name || i} missing commit`);
    return { name: p.name || packNameFromRepo(p.repo), repo: p.repo, commit: p.commit };
  });
  const models = (m.models || []).map((x, i) => {
    if (!x.path) throw new Error(`model #${i} missing path`);
    return { path: x.path, source_url: x.source_url || "", sha256: x.sha256 ?? null };
  });
  return {
    schema_version: m.schema_version ?? 1,
    workflow: m.workflow || "",
    comfyui_min_version: m.comfyui_min_version || "",
    node_packs, models,
  };
}

// manifestHash: identifies a provisioned ENVIRONMENT. Sorted so pack/model order never
// changes the hash; only repo@commit + model path/sha matter (source_url is irrelevant
// to what's on disk).
export function manifestHash(manifest) {
  const m = manifest.schema_version ? manifest : parseManifest(manifest);
  const packs = m.node_packs.map((p) => `${p.repo}@${p.commit}`).sort();
  const models = m.models.map((x) => `${x.path}#${x.sha256 || ""}`).sort();
  return createHash("sha256").update(JSON.stringify({ packs, models })).digest("hex").slice(0, 16);
}
