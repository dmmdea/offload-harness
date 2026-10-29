// node-packs.mjs — the node packs a run-graph manifest may name.
//
// A manifest's node_packs makes the node `git clone` each repository it does not hold yet and pip-install the
// pack's requirements into the ComfyUI venv: that is the caller's code running on the node. So a pack is taken only
// when its repository is a row of node-packs.json (the allowlist of the packs the shipped graph builders need), and
// the directory it installs under is the row's name. Anything else is refused with a typed NODE_PACK_NOT_ALLOWED
// defer before the first git command. Admission is by repository: the node installs the head of the default branch
// and records the commit it got (manifest-satisfy.mjs recordPackInstall); a commit a manifest names is not read.
//
// The match is exact: no case folding, no trimmed whitespace, no ".git" or trailing "/" forgiven. What is checked is
// the string `git clone -- <repo>` will be given, so there is no spelling that passes the check and means something
// else to git.
//
// The table is read once, from the file beside this module. A file that cannot be read, parses to nothing usable or
// holds a malformed row yields no rows, and with no rows every pack is refused: the failure is closed. The Go node
// keeps a copy (internal/mediacap/nodepacks.go) that a test holds to this file.
//
// Dependency-free, Node 18+.
import { readFileSync } from "node:fs";
import { packNameFromRepo } from "./manifest.mjs";

const DIRNAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const REPO = /^https:\/\/github\.com\/[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;

// loadAllowedPacks: the rows of a table file, each reduced to {name, repo}; a row that is not a plain directory name
// and an https GitHub repository is dropped, and an unreadable file is an empty table.
export function loadAllowedPacks(url = new URL("./node-packs.json", import.meta.url)) {
  try {
    const file = JSON.parse(readFileSync(url, "utf8"));
    const rows = [];
    for (const p of Array.isArray(file && file.packs) ? file.packs : []) {
      if (p && typeof p === "object" && DIRNAME.test(p.name) && REPO.test(p.repo)) {
        rows.push(Object.freeze({ name: p.name, repo: p.repo }));
      }
    }
    return Object.freeze(rows);
  } catch {
    return Object.freeze([]);
  }
}

export const ALLOWED_NODE_PACKS = loadAllowedPacks();

// What a pack is called in a refusal: its name, else the name its repository gives it, else its place in the list.
function label(pack) {
  if (pack && typeof pack === "object") {
    if (typeof pack.name === "string" && pack.name) return pack.name;
    if (typeof pack.repo === "string" && pack.repo) return packNameFromRepo(pack.repo);
  }
  return "#";
}

// checkNodePack: null when pack ({name?, repo}) names an allowlisted repository under an accepted directory name, else
// the typed defer {code, ref, detail} naming it. The directory name is the row's, or the one parseManifest derives
// from the repository when the manifest gives none (the mirror clones into its own name); nothing else, so a name
// cannot point the clone anywhere but beside the other packs.
export function checkNodePack(pack, rows = ALLOWED_NODE_PACKS) {
  const refuse = (why) => ({
    code: "NODE_PACK_NOT_ALLOWED",
    ref: label(pack),
    detail: `${why}; a manifest may name only the repositories listed in render/node-packs.json`,
  });
  if (!pack || typeof pack !== "object") return refuse("the pack is not an object");
  if (typeof pack.repo !== "string") return refuse("the pack names no repository");
  const row = rows.find((p) => p.repo === pack.repo);
  if (!row) return refuse(`${JSON.stringify(pack.repo)} is not an allowlisted repository`);
  if (pack.name !== undefined && pack.name !== row.name && pack.name !== packNameFromRepo(row.repo)) {
    return refuse(`the directory name ${JSON.stringify(pack.name)} is not ${row.name}`);
  }
  return null;
}

// --- the models a manifest may name -----------------------------------------------------------------------------
// A manifest's models[] makes the node download each entry's source_url into the ComfyUI tree at its path before
// ComfyUI starts. The fleet bearer lets a caller run renders, not write files of its choosing: a path that leaves
// models/<class>/ (custom_nodes/, a ".." climb, an absolute path, a drive, a stream name) would drop code where
// ComfyUI loads it, and a source that is not an https address on a listed host would make the node fetch from where
// the caller chose. So an entry is taken only when
//   - its path, with backslashes read as separators, is models/<class>/<file> (at least three segments, the first
//     exactly "models"), no segment empty, "." or "..", none beginning or ending with a space or ending with a dot,
//     none holding a control character or one of : * ? " < > |;
//   - its source_url, when it has one, is https://<host>/<rest> with a plain DNS host name (no user name, port,
//     bracket or IP-literal form) that is, or is a subdomain of, a host of node-packs.json (model_hosts).
// internal/mediacap/manifestmodels.go makes the same checks, and render/testdata/manifest-model-cases.json drives both
// tests. No sha256 is required: the entry carries its own hash, so a hash the caller supplies authenticates nothing.
const HOST = /^[a-z0-9][a-z0-9.-]*\.[a-z]{2,}$/;
const SOURCE = /^https:\/\/([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)(\/[^\x00-\x20\x7f\\]*)?$/;

// loadModelHosts: the model_hosts of a table file; a host that is not a plain lower-case name is dropped, and an
// unreadable file lists none (every download is then refused; a model that needs none still provisions).
export function loadModelHosts(url = new URL("./node-packs.json", import.meta.url)) {
  try {
    const file = JSON.parse(readFileSync(url, "utf8"));
    const hosts = Array.isArray(file && file.model_hosts) ? file.model_hosts.filter((h) => typeof h === "string" && HOST.test(h)) : [];
    return Object.freeze(hosts);
  } catch {
    return Object.freeze([]);
  }
}

export const ALLOWED_MODEL_HOSTS = loadModelHosts();

// normalizeModelPath: the path the check reads and the satisfier joins, backslashes read as separators.
export function normalizeModelPath(path) {
  return String(path).replace(/\\/g, "/");
}

function modelPathProblem(path) {
  if (/[\x00-\x1f\x7f]/.test(path)) return "the path holds a control character";
  const segs = normalizeModelPath(path).split("/");
  if (segs.length < 3 || segs[0] !== "models") return "the path is not models/<class>/<file>";
  for (const s of segs) {
    if (s === "" || s === "." || s === "..") return 'the path has an empty, "." or ".." segment';
    if (s.endsWith(".") || s.startsWith(" ") || s.endsWith(" ")) return "a segment of the path begins with a space or ends with a space or a dot";
    if (/[:*?"<>|]/.test(s)) return "a segment of the path holds a character a file name cannot";
  }
  return "";
}

function modelSourceProblem(source, hosts) {
  const m = SOURCE.exec(source);
  if (!m) return "the source is not an https address of the form https://host/path";
  const host = m[1].toLowerCase();
  if (!hosts.some((h) => host === h || host.endsWith("." + h))) return `the source host ${host} is not a listed model host`;
  return "";
}

// checkModelEntry: null when entry ({path, source_url?}) lands under models/<class>/ and is fetched, if at all, over
// https from a listed host, else the typed defer {code, ref, detail} naming the entry. It never throws: an entry whose
// path or source is not a string is a refusal.
export function checkModelEntry(entry, hosts = ALLOWED_MODEL_HOSTS) {
  const refuse = (ref, why) => ({
    code: "MODEL_NOT_ALLOWED",
    ref,
    detail: `${why}; a manifest model lands under models/<class>/ and is fetched over https from a host listed in render/node-packs.json only`,
  });
  if (!entry || typeof entry !== "object") return refuse("#", "the model entry is not an object");
  const { path, source_url: source } = entry;
  if (typeof path !== "string") return refuse("#", "the model entry has no path, or its path is not a string");
  const badPath = modelPathProblem(path);
  if (badPath) return refuse(path, badPath);
  if (source !== undefined && source !== null && source !== "") {
    if (typeof source !== "string") return refuse(path, "source_url is not a string");
    const badSource = modelSourceProblem(source, hosts);
    if (badSource) return refuse(path, badSource);
  }
  return null;
}
