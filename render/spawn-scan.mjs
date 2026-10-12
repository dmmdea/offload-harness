// spawn-scan.mjs - test support for render/no-visible-windows.test.mjs. Never imported by a runner.
//
// Finds the child-process calls in a JavaScript source and says which of them do not set
// `windowsHide: true`. On Windows a console program started by a process that has no console of its own
// (a runner started detached, a test child started detached, a service) gets a NEW console window of its
// own unless it is started hidden, and with Windows Terminal as the default terminal that window is a
// Windows Terminal window on the operator's desktop. `windowsHide: true` is what keeps it out of sight.
//
// What it understands (a small JavaScript tokenizer, not a regex over lines):
//   - the names a file binds from node:child_process: `import { spawn, spawnSync as run } from ...`,
//     `const { spawn } = require(...)` / `await import(...)`, `import * as cp` / `const cp = require(...)`
//     (then `cp.spawn(`), and the chain `require("child_process").spawn(`;
//   - the injected seam `spawnImpl(` (a function a module takes so a test can double the real spawn);
//   - calls inside a string or template literal that holds a script to run (`node -e "<script>"`, a
//     fixture written to disk) when that string names child_process: those scripts run in a child that
//     has no console, which is exactly where the window appears;
//   - comments and regex literals are never calls; a call that only forwards its caller's arguments
//     (`spawn(...a)`) is not judged, the caller's options carry the flag.
// A call is fine when the text of its arguments holds `windowsHide: true`, or when its last argument is
// a name this file declares as an object that holds it.
//
// What it does not follow: a child_process function passed around as a value (`promisify(execFile)`, a
// wrapper stored in a table). Call the function itself, with its options, where it is used.
//
// Dependency-free (Node 18+ built-ins only).

const FUNCS = ["spawn", "spawnSync", "exec", "execSync", "execFile", "execFileSync", "fork"];
// seams: a name a module calls for the real spawn, handed in by its caller
export const INJECTED = ["spawnImpl"];
// the child_process functions a member call is taken for whatever its receiver (`.exec(` is left out: it is
// RegExp's too)
const MEMBERS = ["spawn", "spawnSync", "execFile", "execFileSync", "execSync"];

const CP = String.raw`["'](?:node:)?child_process["']`;
const HIDE = /\bwindowsHide\s*:\s*true\b/;
const REGEX_AFTER_WORD = new Set(["return", "typeof", "case", "do", "else", "in", "of", "void", "delete", "throw", "new", "await", "yield", "instanceof"]);
const REGEX_AFTER_CHAR = "(,=:[!&|?{};+-*%<>~^}";

const lineOf = (src, idx) => src.slice(0, idx).split("\n").length;
const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

// lex: kind[i] is 0 for code, 1 for a comment, 2 for the text of a string or template literal (quotes
// included), 3 for a regex literal. tokOf[i] is the id of the string token a kind-2 char belongs to, and
// tokens[id] its extent (a template's extent runs from its first backtick to its last, `${...}` included).
// Throws when the source ends inside a string, template or regex: a tokenizer that lost its place would
// miss calls, and a lint that misses calls must fail loudly instead.
export function lex(src) {
  const n = src.length;
  const kind = new Uint8Array(n);
  const tokOf = new Int32Array(n).fill(-1);
  const tokens = [];
  const stack = []; // open templates: { tok, depth } - depth counts the braces open inside its current ${...}
  let mode = "code";
  let prevChar = "";
  let prevWord = "";
  let i = 0;
  const fail = (what) => { throw new Error(`cannot tokenize (${what}) near line ${lineOf(src, Math.min(i, n))}`); };
  const mark = (a, b, k, tok = -1) => { for (let j = a; j < b && j < n; j++) { kind[j] = k; tokOf[j] = tok; } };

  if (src.startsWith("#!")) {
    let j = src.indexOf("\n");
    if (j < 0) j = n;
    mark(0, j, 1);
    i = j;
  }
  while (i < n) {
    const c = src[i];
    if (mode === "tpl") {
      const top = stack[stack.length - 1];
      if (c === "\\") { mark(i, i + 2, 2, top.tok); i += 2; continue; }
      if (c === "`") {
        mark(i, i + 1, 2, top.tok);
        tokens[top.tok].end = i + 1;
        stack.pop();
        i++;
        mode = "code";
        prevChar = "a"; prevWord = "";
        continue;
      }
      if (c === "$" && src[i + 1] === "{") { kind[i] = 0; kind[i + 1] = 0; i += 2; mode = "code"; prevChar = "{"; prevWord = ""; continue; }
      mark(i, i + 1, 2, top.tok);
      i++;
      continue;
    }
    // code
    if (c === " " || c === "\t" || c === "\r" || c === "\n") { i++; continue; }
    if (c === "/" && src[i + 1] === "/") {
      let j = src.indexOf("\n", i);
      if (j < 0) j = n;
      mark(i, j, 1);
      i = j;
      continue;
    }
    if (c === "/" && src[i + 1] === "*") {
      const j = src.indexOf("*/", i + 2);
      if (j < 0) fail("unterminated block comment");
      mark(i, j + 2, 1);
      i = j + 2;
      continue;
    }
    if (c === "'" || c === '"') {
      let j = i + 1;
      while (j < n && src[j] !== c) {
        if (src[j] === "\\") j += 2;
        else if (src[j] === "\n") { i = j; fail("unterminated string"); } else j++;
      }
      if (j >= n) fail("unterminated string");
      tokens.push({ start: i, end: j + 1 });
      mark(i, j + 1, 2, tokens.length - 1);
      i = j + 1;
      prevChar = "a"; prevWord = "";
      continue;
    }
    if (c === "`") {
      tokens.push({ start: i, end: n });
      stack.push({ tok: tokens.length - 1, depth: 0 });
      mark(i, i + 1, 2, tokens.length - 1);
      i++;
      mode = "tpl";
      continue;
    }
    if (c === "/") {
      const regexOk = prevChar === "" || REGEX_AFTER_CHAR.includes(prevChar) || (prevChar === "a" && REGEX_AFTER_WORD.has(prevWord));
      if (!regexOk) { i++; prevChar = "/"; prevWord = ""; continue; }
      let j = i + 1;
      let inClass = false;
      for (; j < n; j++) {
        const d = src[j];
        if (d === "\\") j++;
        else if (d === "\n") { i = j; fail("unterminated regex"); } else if (d === "[") inClass = true;
        else if (d === "]") inClass = false;
        else if (d === "/" && !inClass) break;
      }
      if (j >= n) fail("unterminated regex");
      j++;
      while (j < n && /[a-z]/.test(src[j])) j++;
      mark(i, j, 3);
      i = j;
      prevChar = "a"; prevWord = "";
      continue;
    }
    if (/[A-Za-z_$]/.test(c)) {
      let j = i + 1;
      while (j < n && /[\w$]/.test(src[j])) j++;
      prevWord = src.slice(i, j);
      prevChar = "a";
      i = j;
      continue;
    }
    if (/[0-9]/.test(c)) {
      let j = i + 1;
      while (j < n && /[\w.]/.test(src[j])) j++;
      i = j;
      prevChar = "a"; prevWord = "";
      continue;
    }
    if (c === "{" && stack.length) stack[stack.length - 1].depth++;
    if (c === "}" && stack.length) {
      const top = stack[stack.length - 1];
      if (top.depth === 0) { i++; mode = "tpl"; continue; }
      top.depth--;
    }
    prevChar = c; prevWord = "";
    i++;
  }
  if (mode !== "code" || stack.length) fail("unterminated template literal");
  return { kind, tokOf, tokens };
}

// the names this file binds from child_process
function bindings(src, kind) {
  const funcs = new Map(); // local name -> the child_process function it is
  const spaces = new Set(); // local names of the whole module
  const live = (m) => kind[m.index] !== 1;
  const addList = (list, sep) => {
    for (const part of list.split(",")) {
      const [orig, local] = part.split(sep).map((s) => s.trim());
      if (FUNCS.includes(orig)) funcs.set(local || orig, orig);
    }
  };
  for (const m of src.matchAll(new RegExp(String.raw`import\s*\{([^}]*)\}\s*from\s*${CP}`, "g"))) if (live(m)) addList(m[1], /\s+as\s+/);
  for (const m of src.matchAll(new RegExp(String.raw`\{([^}]*)\}\s*=\s*(?:await\s+)?(?:require|import)\(\s*${CP}\s*\)`, "g"))) if (live(m)) addList(m[1], ":");
  for (const m of src.matchAll(new RegExp(String.raw`import\s*\*\s*as\s+([\w$]+)\s+from\s*${CP}`, "g"))) if (live(m)) spaces.add(m[1]);
  for (const m of src.matchAll(new RegExp(String.raw`import\s+([\w$]+)\s+from\s*${CP}`, "g"))) if (live(m)) spaces.add(m[1]);
  for (const m of src.matchAll(new RegExp(String.raw`([\w$]+)\s*=\s*(?:await\s+)?(?:require|import)\(\s*${CP}\s*\)`, "g"))) if (live(m)) spaces.add(m[1]);
  for (const name of INJECTED) funcs.set(name, name);
  return { funcs, spaces };
}

// readBrackets: the text between the bracket at `open` and its partner, split at the commas of the first
// level. In code only the code chars count (a paren in a string or a comment does not); inside a script
// held in a string every char counts, with its own quotes tracked.
function readBrackets(src, kind, open, inString) {
  const parts = [];
  let depth = 0;
  let start = open + 1;
  let q = "";
  for (let j = open; j < src.length; j++) {
    const c = src[j];
    if (inString) {
      if (c === "\\") { j++; continue; }
      if (q) { if (c === q) q = ""; continue; }
      if (c === "'" || c === '"' || c === "`") { q = c; continue; }
    } else if (kind[j] !== 0) continue;
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") {
      depth--;
      if (depth === 0) { parts.push([start, j]); return { parts, close: j }; }
    } else if (c === "," && depth === 1) { parts.push([start, j]); start = j + 1; }
  }
  return null;
}

// the text of src[a, b) with everything that is not code blanked, so a `windowsHide: true` in a comment or
// in a message string does not count
function codeText(src, kind, a, b, inString) {
  if (inString) return src.slice(a, b);
  let out = "";
  for (let j = a; j < b; j++) out += kind[j] === 0 ? src[j] : (src[j] === "\n" ? "\n" : " ");
  return out;
}

// scanSource: every child-process call in `src`, with `ok` and a `why` for the verdict.
// -> [{ name, line, call, ok, why }]
export function scanSource(src) {
  const { kind, tokOf, tokens } = lex(src);
  const { funcs, spaces } = bindings(src, kind);
  // a parameter or variable defaulted to a bound function is that function (`spawn = nodeSpawn`)
  for (const [local, orig] of [...funcs]) {
    for (const m of src.matchAll(new RegExp(String.raw`([\w$]+)\s*=\s*${escapeRe(local)}(?![\w$.(])`, "g"))) {
      if (kind[m.index] !== 1 && !funcs.has(m[1])) funcs.set(m[1], orig);
    }
  }
  const names = [...funcs.keys()];
  const found = new Map(); // index of the call's opening paren -> { index, name }
  const add = (m, parenOffset, name) => {
    const at = m.index;
    if (kind[at] === 1 || kind[at] === 3) return;
    let inString = false;
    if (kind[at] === 2) {
      // a script is often written as several strings joined with "\n", so the string that holds the call
      // need not be the one that names child_process: any string of a file that loads it counts
      if (!src.includes("child_process")) return;
      inString = true;
    }
    const paren = at + parenOffset;
    if (src[paren] !== "(") return;
    // a declaration (`function spawn(`) is not a call
    if (/function\s*\*?\s*$/.test(src.slice(Math.max(0, at - 24), at))) return;
    found.set(paren, { index: at, paren, name, inString });
  };
  if (names.length) {
    const re = new RegExp(String.raw`(?<![\w$.])(?:${names.map(escapeRe).join("|")})\s*\(`, "g");
    for (const m of src.matchAll(re)) add(m, m[0].length - 1, m[0].replace(/\s*\($/, ""));
  }
  if (spaces.size) {
    const re = new RegExp(String.raw`(?<![\w$.])(?:${[...spaces].map(escapeRe).join("|")})\s*\.\s*(?:${FUNCS.join("|")})\s*\(`, "g");
    for (const m of src.matchAll(re)) add(m, m[0].length - 1, m[0].replace(/\s*\($/, "").replace(/\s+/g, ""));
  }
  // a member call on anything (`deps.spawn(`, `this.spawnSync(`): the receiver is a seam handed in by a caller
  for (const m of src.matchAll(new RegExp(String.raw`\.\s*(?:${[...MEMBERS, ...INJECTED].join("|")})\s*\(`, "g"))) add(m, m[0].length - 1, m[0].replace(/\s*\($/, "").replace(/\s+/g, ""));
  const chain = new RegExp(String.raw`(?:require|import)\(\s*${CP}\s*\)\s*\)?\s*\.\s*(?:${FUNCS.join("|")})\s*\(`, "g");
  for (const m of src.matchAll(chain)) add(m, m[0].length - 1, m[0].replace(/\s*\($/, "").replace(/^.*\.\s*/, "child_process."));

  const sites = [];
  for (const { index, paren, name, inString } of [...found.values()].sort((a, b) => a.index - b.index)) {
    const line = lineOf(src, index);
    const read = readBrackets(src, kind, paren, inString);
    if (!read) { sites.push({ name, line, call: src.slice(index, index + 80).split("\n")[0], ok: false, why: "the call's argument list could not be read to its closing paren" }); continue; }
    // inside a string a call needs a command and arguments: `spawn()` or `spawn(x)` in a message is prose
    if (inString && read.parts.length < 2) continue;
    const after = src.slice(read.close + 1).match(/^\s*(\S)/);
    if (after && after[1] === "{") continue; // `spawn(cmd) { ... }`: a method definition, not a call
    const call = src.slice(index, read.close + 1).replace(/\s+/g, " ");
    const shown = call.length > 120 ? call.slice(0, 117) + "..." : call;
    const argText = codeText(src, kind, paren + 1, read.close, inString);
    const last = read.parts.length ? codeText(src, kind, read.parts[read.parts.length - 1][0], read.parts[read.parts.length - 1][1], inString).trim() : "";
    if (read.parts.length === 1 && /^\.\.\.[\w$.]+$/.test(last)) { sites.push({ name, line, call: shown, ok: true, why: "forwards its caller's arguments unchanged" }); continue; }
    if (HIDE.test(argText)) { sites.push({ name, line, call: shown, ok: true, why: "windowsHide: true" }); continue; }
    if (/^[A-Za-z_$][\w$]*$/.test(last)) {
      const decl = new RegExp(String.raw`(?:const|let|var)\s+${escapeRe(last)}\s*=\s*\{`).exec(src);
      if (decl) {
        const open = decl.index + decl[0].length - 1;
        const obj = readBrackets(src, kind, open, false);
        if (obj && HIDE.test(codeText(src, kind, open, obj.close, false))) { sites.push({ name, line, call: shown, ok: true, why: `options object ${last} sets windowsHide: true` }); continue; }
      }
    }
    // where the call sits, for a tool that edits it
    sites.push({ name, line, call: shown, ok: false, why: "no `windowsHide: true` in the call's options", index, paren, close: read.close, parts: read.parts, inString });
  }
  return sites;
}

// whether a source imports or requires child_process at all (outside comments)
export function usesChildProcess(src) {
  const { kind } = lex(src);
  const re = new RegExp(String.raw`(?:from|require\(|import\()\s*${CP}`, "g");
  for (const m of src.matchAll(re)) if (kind[m.index] !== 1) return true;
  return false;
}
