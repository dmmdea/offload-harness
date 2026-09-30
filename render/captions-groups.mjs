#!/usr/bin/env node
// render/captions-groups.mjs — a pure helper: offload_transcribe's <base>.segments.json in, the
// captions-bar template's `words_json` variable out. No I/O in the functions, no clock, no randomness:
// the same words always give the same groups, and the input is never mutated. Only the small command
// line at the bottom reads a file.
//
// Grouping. A caption group is a few words shown together. It ends when ANY of these holds:
//   - it holds the pace's maximum number of words (punchy 3, conversational 5, calm 6: short groups
//     for a fast delivery, longer ones for a slow one);
//   - its last word ends a sentence (. ! ? or an ellipsis, optionally followed by closing quotes or
//     brackets), so a caption never straddles two sentences;
//   - the next word starts gapSec (default 0.15 s) or more after this one ends: a breath, a beat;
//   - adding the next word would push the text past maxChars (default 42, one line at caption size).
// A word longer than maxChars stands alone; it is never split.
//
// Holding. Each group appears with its first word, stays lingerSec (default 0.3 s) after its last
// word, and stays at least minHoldSec (default 0.5 s), but never past the next group's start: one
// group is on screen at a time, and no two overlap.
//
// Output. The template reads one string variable, `words_json`: compact [start,end,text] triples in
// seconds. A 16 KB string variable was measured through lint, check and --strict-variables
// (docs/systems/media-generation.md), so that is the cap: a longer transcript becomes several chunks,
// each its own overlay clip, placed at offset_sec on the source timeline. The first chunk keeps
// absolute time; later chunks are rebased so each clip starts at 0.
//
// Word shape. <base>.segments.json is the array internal/sttclient writes: segments carrying
// words[{word,start,end,probability}] (whisper keeps a leading space on each word). A segment without
// words is spread over its span by character weight.
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

export const PACES = Object.freeze({
  punchy: Object.freeze({ maxWords: 3 }),
  conversational: Object.freeze({ maxWords: 5 }),
  calm: Object.freeze({ maxWords: 6 }),
});

export const DEFAULTS = Object.freeze({
  pace: "conversational",
  gapSec: 0.15,
  maxChars: 42,
  lingerSec: 0.3,
  minHoldSec: 0.5,
  precision: 2,
  variableChars: 16000, // the captions-bar template's words_json maxLength
  variableSec: 600, // the captions-bar template's duration maximum
});

// A tiny tolerance for comparing times that came out of floating-point subtraction.
const EPS = 1e-9;

const roundTo = (x, precision) => Number(x.toFixed(precision));

function isNum(v) {
  return typeof v === "number" && Number.isFinite(v);
}

// wordsFromSegments flattens a transcript into [{text,start,end}] in the input order. It accepts the
// bare segments array or an object holding one. Whisper's non-speech markers ([BLANK_AUDIO], [Music])
// are dropped unless dropNonSpeech is false. It fails loud on a word it cannot place in time: a caption
// track that silently loses words is worse than an error.
export function wordsFromSegments(input, { dropNonSpeech = true } = {}) {
  const segments = Array.isArray(input) ? input : input && Array.isArray(input.segments) ? input.segments : null;
  if (!segments) throw new TypeError("segments must be an array, or an object with a segments array (the shape of <base>.segments.json)");
  const out = [];
  segments.forEach((seg, si) => {
    if (!seg || typeof seg !== "object") throw new TypeError(`segment ${si}: not an object`);
    const label = `segment ${seg.id !== undefined ? seg.id : si}`;
    if (Array.isArray(seg.words) && seg.words.length > 0) {
      seg.words.forEach((wd, wi) => {
        if (!wd || !isNum(wd.start) || !isNum(wd.end)) throw new RangeError(`${label}: word ${wi} has no usable start and end`);
        const raw = typeof wd.word === "string" ? wd.word : typeof wd.text === "string" ? wd.text : "";
        const text = raw.replace(/\s+/g, " ").trim();
        if (!text || (dropNonSpeech && /^\[[^\]]*\]$/.test(text))) return;
        out.push({ text, start: wd.start, end: Math.max(wd.end, wd.start) });
      });
      return;
    }
    let text = typeof seg.text === "string" ? seg.text : "";
    if (dropNonSpeech) text = text.replace(/\[[^\]]*\]/g, " ");
    const tokens = text.split(/\s+/).filter(Boolean);
    if (tokens.length === 0) return;
    if (!isNum(seg.start) || !isNum(seg.end)) throw new RangeError(`${label}: it has text but no usable start and end`);
    // Character weight (a word and the space after it): a longer word gets a longer slot. The boundaries
    // are computed once, so one word's end is exactly the next word's start.
    const total = tokens.reduce((n, t) => n + t.length + 1, 0);
    const span = Math.max(seg.end - seg.start, 0);
    let acc = 0;
    const bounds = [seg.start];
    for (let i = 0; i < tokens.length; i++) {
      acc += tokens[i].length + 1;
      bounds.push(i === tokens.length - 1 ? seg.start + span : seg.start + (acc / total) * span);
    }
    tokens.forEach((t, i) => out.push({ text: t, start: bounds[i], end: bounds[i + 1] }));
  });
  return out;
}

function resolveOptions(opts) {
  const o = { ...DEFAULTS };
  for (const [k, v] of Object.entries(opts || {})) if (v !== undefined) o[k] = v;
  const pace = PACES[o.pace];
  if (!pace) throw new RangeError(`pace "${o.pace}" is not one of ${Object.keys(PACES).join(", ")}`);
  const maxWords = o.maxWords !== undefined ? o.maxWords : pace.maxWords;
  if (!Number.isInteger(maxWords) || maxWords < 1) throw new RangeError(`maxWords must be an integer of at least 1 (got ${maxWords})`);
  for (const k of ["gapSec", "lingerSec", "minHoldSec"]) {
    if (!isNum(o[k]) || o[k] < 0) throw new RangeError(`${k} must be a number of at least 0 (got ${o[k]})`);
  }
  if (!Number.isInteger(o.maxChars) || o.maxChars < 1) throw new RangeError(`maxChars must be an integer of at least 1 (got ${o.maxChars})`);
  if (!Number.isInteger(o.precision) || o.precision < 0 || o.precision > 3) throw new RangeError(`precision must be an integer from 0 to 3 (got ${o.precision})`);
  return { ...o, maxWords };
}

const SENTENCE_END = /[.!?…]["'’”)\]]*$/;

// groupWords groups words (in time order) into caption groups: [{text,start,end,words}]. See the header
// for the rule. Times are exact here; toWordsJson rounds them.
export function groupWords(words, opts = {}) {
  const o = resolveOptions(opts);
  if (!Array.isArray(words)) throw new TypeError("words must be an array");
  for (const [i, wd] of words.entries()) {
    if (!wd || typeof wd.text !== "string" || !wd.text || !isNum(wd.start) || !isNum(wd.end)) throw new RangeError(`word ${i} needs text, start and end`);
    if (i > 0 && wd.start < words[i - 1].start - EPS) throw new RangeError(`words must arrive in time order (word ${i} starts before word ${i - 1})`);
  }
  const raw = [];
  let cur = [];
  const flush = () => {
    if (cur.length) raw.push(cur);
    cur = [];
  };
  for (const wd of words) {
    if (cur.length) {
      const prev = cur[cur.length - 1];
      const text = `${cur.map((x) => x.text).join(" ")} ${wd.text}`;
      // A word that starts no later than the group's first word cannot begin a new group: two groups
      // would start at the same instant and share the screen.
      const canSplit = wd.start > cur[0].start + EPS;
      const breaks = cur.length >= o.maxWords || SENTENCE_END.test(prev.text) || wd.start - prev.end >= o.gapSec - EPS || text.length > o.maxChars;
      if (canSplit && breaks) flush();
    }
    cur.push({ text: wd.text, start: wd.start, end: Math.max(wd.end, wd.start) });
  }
  flush();
  return raw.map((g, i) => {
    const start = g[0].start;
    const nextStart = i + 1 < raw.length ? raw[i + 1][0].start : Infinity;
    let end = Math.max(...g.map((x) => x.end)) + o.lingerSec;
    if (end - start < o.minHoldSec) end = start + o.minHoldSec;
    end = Math.min(end, nextStart);
    // never zero-length: nextStart is strictly later than start, so this stays inside the gap
    end = Math.max(end, Math.min(start + 0.01, nextStart));
    return { text: g.map((x) => x.text).join(" "), start, end, words: g.length };
  });
}

// toWordsJson renders groups as the template's variable: compact [start,end,text] triples. Rounding to
// `precision` decimals must not break the two invariants the template relies on, so a repair pass runs
// after it: every group has a positive length and none starts before the previous one ends.
export function toWordsJson(groups, { precision = DEFAULTS.precision } = {}) {
  const unit = 10 ** -precision;
  let prevEnd = 0;
  const triples = groups.map((g) => {
    const s = Math.max(roundTo(g.start, precision), prevEnd);
    const e = Math.max(roundTo(g.end, precision), roundTo(s + unit, precision));
    prevEnd = e;
    return [s, e, String(g.text)];
  });
  return JSON.stringify(triples);
}

// splitForVariable packs groups into chunks that each fit the template: at most maxChars characters of
// words_json and at most maxSec seconds of duration. Chunk 0 keeps absolute time when it can (offset 0);
// every later chunk starts at its first group and is rebased to 0. Nothing is dropped or reordered, and
// a single group that cannot fit alone is an error.
export function splitForVariable(groups, { maxChars = DEFAULTS.variableChars, maxSec = DEFAULTS.variableSec, precision = DEFAULTS.precision } = {}) {
  const chunks = [];
  let idx = 0;
  const rebased = (g, offset) => ({ text: g.text, start: g.start - offset, end: g.end - offset });
  while (idx < groups.length) {
    const first = groups[idx];
    const offset = roundTo(chunks.length === 0 && first.end <= maxSec ? 0 : first.start, precision);
    let count = 0;
    let size = 2;
    while (idx + count < groups.length) {
      const g = groups[idx + count];
      if (g.end - offset > maxSec + EPS) break;
      const piece = toWordsJson([rebased(g, offset)], { precision }).length - 2;
      const next = size + piece + (count ? 1 : 0) + 1; // +1: a repaired digit can add a character
      if (next > maxChars) break;
      size = next - 1;
      count++;
    }
    if (count === 0) {
      throw new RangeError(`group ${idx} ("${first.text.slice(0, 40)}") cannot fit one chunk: it needs more than ${maxChars} characters or ${maxSec} s`);
    }
    let json = toWordsJson(groups.slice(idx, idx + count).map((g) => rebased(g, offset)), { precision });
    while (json.length > maxChars && count > 1) {
      count--;
      json = toWordsJson(groups.slice(idx, idx + count).map((g) => rebased(g, offset)), { precision });
    }
    if (json.length > maxChars) throw new RangeError(`group ${idx} needs ${json.length} characters; the cap is ${maxChars}`);
    const triples = JSON.parse(json);
    chunks.push({
      offset_sec: offset,
      duration_sec: Math.min(maxSec, Math.max(1, triples[triples.length - 1][1])),
      group_count: count,
      words_json: json,
    });
    idx += count;
  }
  return chunks;
}

// captionsFromSegments is the whole path: segments in, chunks out.
export function captionsFromSegments(input, opts = {}) {
  const words = wordsFromSegments(input, { dropNonSpeech: opts.dropNonSpeech });
  const groups = groupWords(words, opts);
  const chunks = splitForVariable(groups, {
    maxChars: opts.variableChars !== undefined ? opts.variableChars : DEFAULTS.variableChars,
    maxSec: opts.variableSec !== undefined ? opts.variableSec : DEFAULTS.variableSec,
    precision: opts.precision !== undefined ? opts.precision : DEFAULTS.precision,
  });
  return { template: "captions-bar", words: words.length, groups, chunks };
}

// --- command line -----------------------------------------------------------------------------------
// node render/captions-groups.mjs <segments.json> [--pace punchy|conversational|calm] [--max-words N]
//   [--gap-sec S] [--max-chars N] [--linger-sec S] [--min-hold-sec S] [--out result.json]
// Prints the document captionsFromSegments returns (or writes it to --out). Exit 0 on success, 1 on a
// bad file or bad option, 2 when no file is named.
const NUMERIC = { "max-words": "maxWords", "gap-sec": "gapSec", "max-chars": "maxChars", "linger-sec": "lingerSec", "min-hold-sec": "minHoldSec" };

export function main(argv = process.argv.slice(2), { stdout = process.stdout, stderr = process.stderr } = {}) {
  const opts = {};
  let file = "";
  let out = "";
  try {
    for (let i = 0; i < argv.length; i++) {
      const a = argv[i];
      if (!a.startsWith("--")) {
        if (file) throw new Error(`unexpected argument "${a}"`);
        file = a;
        continue;
      }
      const key = a.slice(2);
      const val = argv[++i];
      if (val === undefined) throw new Error(`flag ${a} needs a value`);
      if (key === "pace") opts.pace = val;
      else if (key === "out") out = val;
      else if (NUMERIC[key]) {
        const n = Number(val);
        if (!Number.isFinite(n)) throw new Error(`flag ${a} needs a number (got "${val}")`);
        opts[NUMERIC[key]] = n;
      } else throw new Error(`unknown flag ${a}`);
    }
  } catch (e) {
    stderr.write(`captions-groups: ${e.message}\n`);
    return 1;
  }
  if (!file) {
    stderr.write("captions-groups: usage: node render/captions-groups.mjs <segments.json> [--pace punchy|conversational|calm] [--out result.json]\n");
    return 2;
  }
  try {
    const doc = captionsFromSegments(JSON.parse(readFileSync(file, "utf8")), opts);
    const text = JSON.stringify(doc);
    if (out) writeFileSync(out, text);
    else stdout.write(text + "\n");
    return 0;
  } catch (e) {
    stderr.write(`captions-groups: ${e.message}\n`);
    return 1;
  }
}

if (import.meta.url === pathToFileURL(process.argv[1] || "").href) {
  process.exitCode = main();
}
