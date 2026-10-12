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
//     `import cp, { spawn } from ...`, `const { spawn } = require(...)` / `await import(...)` / `= cp` (cp a bound
//     namespace), `import * as cp` / `const cp = require(...)` (then `cp.spawn(`), the picked function
//     `const spawn = require("child_process").spawn`, and the chain `require("child_process").spawn(`;
//     a quote, a double quote or a backtick all quote the specifier;
//   - names that are one of those functions by assignment: `spawn = nodeSpawn`, `run = c ? spawn : spawnSync`,
//     `run = other ?? spawn`, `run = promisify(execFile)`;
//   - the injected seam `spawnImpl(` (a function a module takes so a test can double the real spawn);
//   - calls written `spawn(`, `spawn?.(` and `spawn.call(this, ...)`;
//   - calls inside a string or template literal that holds a script to run (`node -e "<script>"`, a
//     fixture written to disk) when the file names child_process: those scripts run in a child that
//     has no console, which is exactly where the window appears;
//   - comments and regex literals are never calls.
//
// A call is fine when its OPTIONS - the last argument that is not a callback (`exec` and `execFile` take one) -
// are an object literal whose own `windowsHide` property ends up the literal `true` (a `windowsHide` nested
// in another property, or only in a spread, does not count, and neither does a spread or a computed key after
// it, which could override it), or a name that resolves, in the scope of the call, to a `const` object
// literal that does (a parameter or an inner declaration of the same name shadows it; a `let` can be
// reassigned; a later `name.windowsHide = ...` undoes it). A call that only forwards a REST PARAMETER's
// arguments (`function f(...a) { return spawn(...a); }`) is not judged: its callers are.
//
// A child_process function (or the module) used as a VALUE - `use(spawn)`, `{ spawn }`, `spawn.bind(null)`,
// `spawn.apply(...)`, `Reflect.apply(spawn, ...)` - is a violation too: a call made through it cannot be
// seen, so the call is written out, with its options, where it is used.
//
// What it does not follow: a function reached through a table, a getter or another module.
//
// Dependency-free (Node 18+ built-ins only).

const FUNCS = ["spawn", "spawnSync", "exec", "execSync", "execFile", "execFileSync", "fork"];
// seams: a name a module calls for the real spawn, handed in by its caller
export const INJECTED = ["spawnImpl"];
// the child_process functions a member call is taken for whatever its receiver (`.exec(` is left out: it is
// RegExp's too)
const MEMBERS = ["spawn", "spawnSync", "execFile", "execFileSync", "execSync"];

const Q = "[\"'`]"; // a quote, a double quote or a backtick
const CP = Q + "(?:node:)?child_process" + Q;
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
//   funcs:  local name -> the child_process function it is
//   spaces: local names of the whole module
//   skips:  [a, b) spans of binding syntax (an import list, a destructuring, an alias definition), which
//           name a function without using it
function bindings(src, kind) {
  const funcs = new Map();
  const spaces = new Set();
  const skips = [];
  const live = (m) => kind[m.index] !== 1;
  const skip = (m) => skips.push([m.index, m.index + m[0].length]);
  const addList = (list, sep) => {
    for (const part of list.split(",")) {
      const [orig, local] = part.split(sep).map((s) => s.trim());
      if (FUNCS.includes(orig)) funcs.set(local || orig, orig);
    }
  };
  const all = (re, fn) => { for (const m of src.matchAll(new RegExp(re, "g"))) if (live(m)) { fn(m); skip(m); } };
  all(String.raw`import\s*\{([^}]*)\}\s*from\s*${CP}`, (m) => addList(m[1], /\s+as\s+/));
  all(String.raw`import\s+([\w$]+)\s*,\s*\{([^}]*)\}\s*from\s*${CP}`, (m) => { spaces.add(m[1]); addList(m[2], /\s+as\s+/); });
  all(String.raw`import\s+([\w$]+)\s*,\s*\*\s*as\s+([\w$]+)\s+from\s*${CP}`, (m) => { spaces.add(m[1]); spaces.add(m[2]); });
  all(String.raw`import\s*\*\s*as\s+([\w$]+)\s+from\s*${CP}`, (m) => spaces.add(m[1]));
  all(String.raw`import\s+([\w$]+)\s+from\s*${CP}`, (m) => spaces.add(m[1]));
  all(String.raw`\{([^}]*)\}\s*=\s*(?:await\s+)?(?:require|import)\(\s*${CP}\s*\)`, (m) => addList(m[1], ":"));
  // `const cp = require(...)`, but not `const spawn = require(...).spawn`, which picks a function off it
  all(String.raw`([\w$]+)\s*=\s*(?:await\s+)?(?:require|import)\(\s*${CP}\s*\)(?!\s*\.\s*[\w$])`, (m) => spaces.add(m[1]));
  // `const spawn = require("child_process").spawn`, `= (await import("child_process")).spawnSync`
  all(String.raw`([\w$]+)\s*=\s*\(?\s*(?:await\s+)?(?:require|import)\(\s*${CP}\s*\)\s*\)?\s*\.\s*(${FUNCS.join("|")})(?![\w$])(?!\s*(?:\?\.\s*)?\()`, (m) => funcs.set(m[1], m[2]));
  // `const { spawn: run } = cp` where cp is a bound namespace
  for (const m of src.matchAll(/\{([^}]*)\}\s*=\s*([\w$]+)(?![\w$.(])/g)) {
    if (live(m) && spaces.has(m[2])) { addList(m[1], ":"); skip(m); }
  }
  for (const name of INJECTED) funcs.set(name, name);
  // a name defaulted to, chosen from, or promisified out of a bound function is that function
  // (`spawn = nodeSpawn`, `run = c ? spawn : spawnSync`, `run = other ?? spawn`, `run = promisify(execFile)`)
  for (let changed = true; changed;) {
    changed = false;
    for (const [local, orig] of [...funcs]) {
      const L = escapeRe(local);
      const found = [
        ...src.matchAll(new RegExp(String.raw`([\w$]+)\s*=\s*(?:util\s*\.\s*)?promisify\(\s*${L}\s*\)`, "g")),
        ...src.matchAll(new RegExp(String.raw`([\w$]+)\s*=\s*[^;=\n(){}\[\],]*?(?<![\w$.])${L}(?![\w$.(])`, "g")),
      ];
      for (const m of found) {
        if (kind[m.index] === 1) continue;
        skip(m);
        if (!funcs.has(m[1])) { funcs.set(m[1], orig); changed = true; }
      }
    }
  }
  return { funcs, spaces, skips };
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

// splitTop: the pieces of `text` between the commas that sit at bracket depth 0 (quotes tracked in a script)
function splitTop(text, inString) {
  const out = [];
  let depth = 0;
  let q = "";
  let start = 0;
  for (let j = 0; j < text.length; j++) {
    const c = text[j];
    if (inString) {
      if (c === "\\") { j++; continue; }
      if (q) { if (c === q) q = ""; continue; }
      if (c === "'" || c === '"' || c === "`") { q = c; continue; }
    }
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") depth--;
    else if (c === "," && depth === 0) { out.push(text.slice(start, j)); start = j + 1; }
  }
  out.push(text.slice(start));
  return out.map((s) => s.trim()).filter((s) => s !== "");
}

// whether an object literal's own `windowsHide` property ends up `true`: the last property of that name must
// be the literal `true`, and no spread or computed key may come after it (either could override it).
// A `windowsHide` nested in another property's value, or only in a spread, does not count.
function hidesWindow(objText, inString) {
  const t = objText.trim();
  if (!t.startsWith("{") || !t.endsWith("}")) return false;
  const props = splitTop(t.slice(1, -1), inString);
  let last = -1;
  let value = "";
  props.forEach((p, i) => {
    const m = /^(?:["'`]\s*)?windowsHide(?:\s*["'`])?\s*(?::\s*([\s\S]*))?$/.exec(p);
    if (m) { last = i; value = (m[1] ?? "").trim(); }
  });
  if (last < 0 || value !== "true") return false;
  return !props.slice(last + 1).some((p) => p.startsWith("...") || p.startsWith("["));
}

// the structure a call needs to be judged in its scope: brackets paired, blocks, parameter lists
function structure(src, kind) {
  const n = src.length;
  const pair = new Int32Array(n).fill(-1);
  const blocks = []; // { open, close } of every { } in code, in order of opening
  const stack = [];
  for (let j = 0; j < n; j++) {
    if (kind[j] !== 0) continue;
    const c = src[j];
    if (c === "(" || c === "[" || c === "{") stack.push(j);
    else if (c === ")" || c === "]" || c === "}") {
      const o = stack.pop();
      if (o !== undefined) { pair[o] = j; if (c === "}") blocks.push({ open: o, close: j }); }
    }
  }
  blocks.sort((a, b) => a.open - b.open);
  const blockEnd = (p) => {
    for (let i = blocks.length - 1; i >= 0; i--) if (blocks[i].open < p && blocks[i].close > p) return blocks[i].close;
    return n;
  };
  // the end of an expression-bodied arrow: the first , ; or closing bracket at its own depth
  const exprEnd = (start) => {
    let depth = 0;
    for (let j = start; j < n; j++) {
      if (kind[j] !== 0) continue;
      const c = src[j];
      if (c === "(" || c === "[" || c === "{") depth++;
      else if (c === ")" || c === "]" || c === "}") { if (depth === 0) return j; depth--; } else if ((c === "," || c === ";") && depth === 0) return j;
    }
    return n;
  };
  const bodyRange = (after) => {
    const m = /^\s*(\{)?/.exec(src.slice(after, after + 80));
    if (m[1]) { const open = after + m[0].length - 1; return { from: open, to: pair[open] >= 0 ? pair[open] : n }; }
    return { from: after, to: exprEnd(after) };
  };
  const wordsOf = (a, b) => new Set(codeText(src, kind, a, b, false).match(/[A-Za-z_$][\w$]*/g) || []);
  const restOf = (a, b) => new Set([...codeText(src, kind, a, b, false).matchAll(/\.\.\.\s*([A-Za-z_$][\w$]*)/g)].map((m) => m[1]));
  // every parameter list: ( ... ) followed by => or {, except a control statement's head
  const groups = [];
  for (let p = 0; p < n; p++) {
    if (kind[p] !== 0 || src[p] !== "(" || pair[p] < 0) continue;
    const close = pair[p];
    const next = /^\s*(=>|\{)/.exec(src.slice(close + 1, close + 40));
    if (!next) continue;
    if (/(?:^|[^\w$.])(?:if|for|while|switch|with)\s*$/.test(src.slice(Math.max(0, p - 12), p))) continue;
    const range = next[1] === "=>" ? bodyRange(close + 1 + next[0].length) : { from: close + next[0].length, to: pair[close + next[0].length] };
    groups.push({ ...range, names: wordsOf(p + 1, close), rest: restOf(p + 1, close) });
  }
  // an arrow with one bare parameter: x => ...
  for (const m of src.matchAll(/(?<![\w$.)])([A-Za-z_$][\w$]*)\s*=>/g)) {
    if (kind[m.index] !== 0) continue;
    groups.push({ ...bodyRange(m.index + m[0].length), names: new Set([m[1]]), rest: new Set() });
  }
  return { pair, blocks, blockEnd, groups };
}

// scanSource: every child-process call in `src`, with `ok` and a `why` for the verdict.
// -> [{ name, line, call, ok, why }]
//
// A call is fine when its options (the last argument that is not a callback) are an object literal whose own
// `windowsHide` ends up `true`, or a name that resolves, in the scope of the call, to a `const` object literal
// that does. A call that only forwards a rest parameter's arguments is not judged: its callers are.
export function scanSource(src) {
  const { kind, tokOf, tokens } = lex(src);
  const { funcs, spaces, skips } = bindings(src, kind);
  const names = [...funcs.keys()];
  const found = new Map(); // index of the call's opening paren -> { index, name, ... }
  const add = (m, parenOffset, name, extra = {}) => {
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
    found.set(paren, { index: at, paren, name, inString, ...extra });
  };
  const OPT = String.raw`\s*(?:\?\.\s*)?`; // an optional call: spawn?.(
  const tidy = (m) => m[0].replace(/\s*\($/, "").replace(/\?\.$/, "").replace(/\s+/g, "");
  if (names.length) {
    const alt = names.map(escapeRe).join("|");
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(?:${alt})${OPT}\(`, "g"))) add(m, m[0].length - 1, tidy(m));
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(?:${alt})\s*\.\s*call\s*\(`, "g"))) add(m, m[0].length - 1, tidy(m).replace(/\.call$/, ""), { thisArg: true });
  }
  if (spaces.size) {
    const alt = [...spaces].map(escapeRe).join("|");
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(?:${alt})\s*\.\s*(?:${FUNCS.join("|")})${OPT}\(`, "g"))) add(m, m[0].length - 1, tidy(m));
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(?:${alt})\s*\.\s*(?:${FUNCS.join("|")})\s*\.\s*call\s*\(`, "g"))) add(m, m[0].length - 1, tidy(m).replace(/\.call$/, ""), { thisArg: true });
  }
  // a member call on anything (`deps.spawn(`, `this.spawnSync(`): the receiver is a seam handed in by a caller
  for (const m of src.matchAll(new RegExp(String.raw`\.\s*(?:${[...MEMBERS, ...INJECTED].join("|")})${OPT}\(`, "g"))) add(m, m[0].length - 1, tidy(m));
  const chain = new RegExp(String.raw`(?:require|import)\(\s*${CP}\s*\)\s*\)?\s*\.\s*(?:${FUNCS.join("|")})${OPT}\(`, "g");
  for (const m of src.matchAll(chain)) add(m, m[0].length - 1, tidy(m).replace(/^.*\)\./, "child_process."));

  let st = null;
  const structured = () => (st ??= structure(src, kind));

  // whether `name` is, at `at`, a rest parameter of an enclosing function
  const isRestParam = (name, at) => structured().groups.some((g) => g.rest.has(name) && g.from <= at && at <= g.to);

  // the verdict on an options name used at `at`: it must resolve, in scope, to a const object literal that hides
  const resolveOptions = (name, at, inString) => {
    if (inString) {
      const tok = tokens[tokOf[at]];
      const text = tok ? src.slice(tok.start, tok.end) : src;
      const decl = new RegExp(String.raw`(?:const|let|var)\s+${escapeRe(name)}\s*=\s*\{`).exec(text);
      if (decl) {
        const open = decl.index + decl[0].length - 1;
        const rb = readBrackets(text, new Uint8Array(text.length), open, true);
        if (rb && hidesWindow(text.slice(open, rb.close + 1), true)) return { ok: true, why: `options object ${name} sets windowsHide: true` };
      }
      return { ok: false, why: `the options ${name} do not resolve to an object literal with windowsHide: true in this script` };
    }
    const re = escapeRe(name);
    const cands = [];
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(const|let|var)\s+${re}(?![\w$])`, "g"))) {
      if (kind[m.index] !== 0) continue;
      const afterName = m.index + m[0].length;
      const init = /^\s*=\s*\{/.exec(src.slice(afterName, afterName + 40));
      const open = init ? afterName + init[0].length - 1 : -1;
      cands.push({ from: m.index, to: structured().blockEnd(m.index), kw: m[1], open });
    }
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])(?:const|let|var)\s*[\[{][^=;]*?(?<![\w$.])${re}(?![\w$])[^=;]*=`, "g"))) {
      if (kind[m.index] === 0) cands.push({ from: m.index, to: structured().blockEnd(m.index), kw: "pattern", open: -1 });
    }
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])function\s*\*?\s*${re}(?![\w$])`, "g"))) {
      if (kind[m.index] === 0) cands.push({ from: m.index, to: structured().blockEnd(m.index), kw: "function", open: -1 });
    }
    for (const g of structured().groups) if (g.names.has(name)) cands.push({ from: g.from, to: g.to, kw: "parameter", open: -1 });
    const here = cands.filter((c) => c.from <= at && at <= c.to).sort((a, b) => b.from - a.from)[0];
    if (!here) return { ok: false, why: `the options ${name} are not declared in scope at the call` };
    if (here.kw !== "const" || here.open < 0) return { ok: false, why: `the options ${name} resolve to a ${here.kw === "const" ? "const that is not an object literal" : here.kw} (only a const object literal with windowsHide: true is followed)` };
    const rb = readBrackets(src, kind, here.open, false);
    if (!rb || !hidesWindow(codeText(src, kind, here.open, rb.close + 1, false), false)) return { ok: false, why: `the options object ${name} does not end up with windowsHide: true` };
    if (new RegExp(String.raw`(?<![\w$.])${re}\s*(?:\.\s*windowsHide|\[\s*["']windowsHide["']\s*\])\s*=(?!=)|Object\s*\.\s*assign\(\s*${re}\b`).test(src)) return { ok: false, why: `the options object ${name} has its windowsHide changed after it is declared` };
    return { ok: true, why: `options object ${name} sets windowsHide: true` };
  };

  const isFn = (t) => /^(?:async\s+)?(?:function\b|(?:\([^()]*\)|[\w$]+)\s*=>)/.test(t);

  const sites = [];
  for (const { index, paren, name, inString, thisArg } of [...found.values()].sort((a, b) => a.index - b.index)) {
    const line = lineOf(src, index);
    const read = readBrackets(src, kind, paren, inString);
    if (!read) { sites.push({ name, line, call: src.slice(index, index + 80).split("\n")[0], ok: false, why: "the call's argument list could not be read to its closing paren" }); continue; }
    // inside a string a call needs a command and arguments: `spawn()` or `spawn(x)` in a message is prose
    if (inString && read.parts.length < 2) continue;
    const after = src.slice(read.close + 1).match(/^\s*(\S)/);
    if (after && after[1] === "{") continue; // `spawn(cmd) { ... }`: a method definition, not a call
    const call = src.slice(index, read.close + 1).replace(/\s+/g, " ");
    const shown = call.length > 120 ? call.slice(0, 117) + "..." : call;
    const fail = (why) => sites.push({ name, line, call: shown, ok: false, why, index, paren, close: read.close, parts: read.parts, inString });
    const okSite = (why) => sites.push({ name, line, call: shown, ok: true, why });
    let parts = read.parts.map(([a, b]) => ({ a, b, text: codeText(src, kind, a, b, inString).trim() }));
    while (parts.length && parts[parts.length - 1].text === "") parts.pop(); // a trailing comma
    if (thisArg) parts = parts.slice(1);
    if (parts.length === 1 && /^\.\.\.[\w$]+$/.test(parts[0].text)) {
      const rest = parts[0].text.slice(3);
      if (isRestParam(rest, index)) { okSite("forwards its caller's arguments unchanged"); continue; }
      fail(`forwards ${rest}, which is not a rest parameter of an enclosing function: a call needs its own options`);
      continue;
    }
    // the function being called, for the shapes of its arguments
    const orig = funcs.get(name) ?? name.replace(/^.*\./, "");
    let at = parts.length - 1;
    if (at >= 0 && isFn(parts[at].text)) at--; // a callback is not options
    if ((orig === "exec" || orig === "execFile") && at >= 1 && /^[A-Za-z_$][\w$]*$/.test(parts[at].text) && !inString && !resolveOptions(parts[at].text, index, false).ok) at--; // a callback by name
    if (at < 1 || parts[at].text === "") { fail("no options argument: a child_process call needs `{ windowsHide: true }` as its options"); continue; }
    const opt = parts[at].text;
    if (opt.startsWith("{")) {
      if (hidesWindow(opt, inString)) okSite("windowsHide: true");
      else fail("no `windowsHide: true` among the options object's own properties (or a later spread / computed key can override it)");
    } else if (/^[A-Za-z_$][\w$]*$/.test(opt)) {
      const r = resolveOptions(opt, index, inString);
      if (r.ok) okSite(r.why); else fail(r.why);
    } else {
      fail("the options are neither an object literal nor a name declared as one, so `windowsHide: true` cannot be checked");
    }
  }

  // a bound child_process function (or the module) used as a value is a call nobody checks: `use(spawn)`,
  // `{ spawn }`, `spawn.apply(...)`, `spawn.bind(...)`
  const inSkip = (i) => skips.some(([a, b]) => a <= i && i < b);
  const valueUse = (i, shown, why) => sites.push({ name: shown, line: lineOf(src, i), call: src.slice(i, i + 60).split("\n")[0], ok: false, why });
  const bound = [...funcs.keys()].filter((f) => !INJECTED.includes(f));
  for (const f of bound) {
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])${escapeRe(f)}(?![\w$])`, "g"))) {
      const i = m.index;
      if (kind[i] !== 0 || inSkip(i)) continue;
      const rest = src.slice(i + f.length, i + f.length + 40);
      const before = src.slice(Math.max(0, i - 40), i);
      if (/^\s*(?:\?\.\s*)?\(/.test(rest) || /^\s*\.\s*call\s*\(/.test(rest)) continue; // a call, judged above
      if (/function\s*\*?\s*$/.test(before)) continue; // a declaration
      if (/[{,]\s*$/.test(before) && /^\s*:/.test(rest)) continue; // an object key
      const how = /^\s*\.\s*(apply|bind)\b/.exec(rest);
      valueUse(i, f, how ? `${f}.${how[1]} hides the call: call ${f} directly, with its options` : `${f} is used as a value, so its calls cannot be checked: call it directly, with its options`);
    }
  }
  for (const sp of spaces) {
    for (const m of src.matchAll(new RegExp(String.raw`(?<![\w$.])${escapeRe(sp)}(?![\w$])`, "g"))) {
      const i = m.index;
      if (kind[i] !== 0 || inSkip(i)) continue;
      const rest = src.slice(i + sp.length, i + sp.length + 60);
      const before = src.slice(Math.max(0, i - 40), i);
      if (/[{,]\s*$/.test(before) && /^\s*:/.test(rest)) continue; // an object key
      const mem = new RegExp(String.raw`^\s*\.\s*(${FUNCS.join("|")})(?![\w$])\s*((?:\?\.\s*)?\(|\.\s*call\s*\()?`).exec(rest);
      if (mem && mem[2]) continue; // a call, judged above
      if (mem) { valueUse(i, `${sp}.${mem[1]}`, `${sp}.${mem[1]} is used as a value, so its calls cannot be checked: call it directly, with its options`); continue; }
      if (/^\s*\.\s*[\w$]/.test(rest)) continue; // some other member of the module
      valueUse(i, sp, `the child_process module ${sp} is used as a value, so its calls cannot be checked: call its functions directly, with their options`);
    }
  }
  return sites.sort((a, b) => a.line - b.line);
}

// whether a source imports or requires child_process at all (outside comments)
export function usesChildProcess(src) {
  const { kind } = lex(src);
  const re = new RegExp(String.raw`(?:from|require\(|import\()\s*${CP}`, "g");
  for (const m of src.matchAll(re)) if (kind[m.index] !== 1) return true;
  return false;
}
