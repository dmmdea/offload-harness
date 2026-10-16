// node-packs.mjs — the node packs a run-graph manifest may name.
//
// A manifest's node_packs makes the node `git clone` each repository, check out each commit and pip-install the
// pack's requirements into the ComfyUI venv: that is the caller's code running on the node. So a pack is taken only
// when its repository AND its commit are a row of node-packs.json (the table the shipped graph builders need, each
// at the commit it is pinned to), and the directory it installs under is the row's name. Anything else is refused
// with a typed NODE_PACK_NOT_ALLOWED defer before the first git command.
//
// The match is exact: no case folding, no trimmed whitespace, no ".git" or trailing "/" forgiven, a full 40-digit
// commit and nothing shorter. What is checked is the string `git clone -- <repo>` and `git checkout <commit>` will
// be given, so there is no spelling that passes the check and means something else to git.
//
// The table is read once, from the file beside this module. A file that cannot be read, parses to nothing usable or
// holds a malformed row yields no rows, and with no rows every pack is refused: the failure is closed. The Go node
// keeps a copy (internal/mediacap/nodepacks.go) that a test pins to this file.
//
// Dependency-free, Node 18+.
import { readFileSync } from "node:fs";
import { packNameFromRepo } from "./manifest.mjs";

const COMMIT = /^[0-9a-f]{40}$/;
const DIRNAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const REPO = /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;

// loadPinnedPacks: the rows of a table file, each reduced to {name, repo, commit}; a row that is not a plain
// directory name, an https GitHub repository and a full lower-case commit is dropped, and an unreadable file is
// an empty table.
export function loadPinnedPacks(url = new URL("./node-packs.json", import.meta.url)) {
  try {
    const file = JSON.parse(readFileSync(url, "utf8"));
    const rows = [];
    for (const p of Array.isArray(file && file.packs) ? file.packs : []) {
      if (p && typeof p === "object" && DIRNAME.test(p.name) && REPO.test(p.repo) && COMMIT.test(p.commit)) {
        rows.push(Object.freeze({ name: p.name, repo: p.repo, commit: p.commit }));
      }
    }
    return Object.freeze(rows);
  } catch {
    return Object.freeze([]);
  }
}

export const PINNED_NODE_PACKS = loadPinnedPacks();

// What a pack is called in a refusal: its name, else the name its repository gives it, else its place in the list.
function label(pack) {
  if (pack && typeof pack === "object") {
    if (typeof pack.name === "string" && pack.name) return pack.name;
    if (typeof pack.repo === "string" && pack.repo) return packNameFromRepo(pack.repo);
  }
  return "#";
}

// checkNodePack: null when pack ({name?, repo, commit}) is a pinned pair under an accepted directory name, else
// the typed defer {code, ref, detail} naming it. The directory name is the row's, or the one parseManifest derives
// from the repository when the manifest gives none (the mirror clones into its own name); nothing else, so a name
// cannot point the clone anywhere but beside the other packs.
export function checkNodePack(pack, pins = PINNED_NODE_PACKS) {
  const refuse = (why) => ({
    code: "NODE_PACK_NOT_ALLOWED",
    ref: label(pack),
    detail: `${why}; a manifest may name only the packs pinned in render/node-packs.json`,
  });
  if (!pack || typeof pack !== "object") return refuse("the pack is not an object");
  if (typeof pack.repo !== "string" || typeof pack.commit !== "string") return refuse("the pack names no repository or no commit");
  const row = pins.find((p) => p.repo === pack.repo && p.commit === pack.commit);
  if (!row) {
    const known = pins.some((p) => p.repo === pack.repo);
    return refuse(known ? `${pack.repo} is pinned at another commit than ${JSON.stringify(pack.commit)}` : `${JSON.stringify(pack.repo)} is not a pinned repository`);
  }
  if (pack.name !== undefined && pack.name !== row.name && pack.name !== packNameFromRepo(row.repo)) {
    return refuse(`the directory name ${JSON.stringify(pack.name)} is not ${row.name}`);
  }
  return null;
}

// The models a manifest may name (see internal/mediacap/manifestmodels.go): nothing here yet.
export const PINNED_MODEL_HOSTS = Object.freeze([]);
export function loadModelHosts() { return Object.freeze([]); }
export function normalizeModelPath(path) { return String(path); }
export function checkModelEntry() { return null; }
