// node --test render/captions-groups.test.mjs
// captions-groups.mjs is a pure helper: offload_transcribe's <base>.segments.json in, the captions-bar
// template's words_json out. Nothing here needs HyperFrames, Chrome, ffmpeg or a network.
import { test } from "node:test";
import assert from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import {
  DEFAULTS, PACES, captionsFromSegments, groupWords, splitForVariable, toWordsJson, wordsFromSegments,
} from "./captions-groups.mjs";

const HELPER = join(dirname(fileURLToPath(import.meta.url)), "captions-groups.mjs");

// w builds a word the way wordsFromSegments returns it.
const w = (text, start, end) => ({ text, start, end });
// A run of `n` evenly spaced words with no pauses and no punctuation.
function run(n, { t0 = 0, step = 0.4, gap = 0 } = {}) {
  return Array.from({ length: n }, (_, i) => w(`w${i + 1}`, t0 + i * (step + gap), t0 + i * (step + gap) + step));
}

// The shape internal/sttclient writes to <base>.segments.json: whisper keeps a leading space on each
// word, and every segment carries its confidence fields.
const SEGMENTS = [
  { id: 0, start: 0, end: 2.4, text: " Hello there, and welcome back.", avg_logprob: -0.21, no_speech_prob: 0.01, words: [
    { word: " Hello", start: 0.0, end: 0.4, probability: 0.99 }, { word: " there,", start: 0.4, end: 0.8, probability: 0.98 },
    { word: " and", start: 0.9, end: 1.1, probability: 0.97 }, { word: " welcome", start: 1.1, end: 1.6, probability: 0.99 },
    { word: " back.", start: 1.6, end: 2.4, probability: 0.96 } ] },
  { id: 1, start: 3.0, end: 6.0, text: " Today we look at three small changes that add up.", avg_logprob: -0.3, no_speech_prob: 0.02, words: [
    { word: " Today", start: 3.0, end: 3.4, probability: 0.99 }, { word: " we", start: 3.4, end: 3.5, probability: 0.99 },
    { word: " look", start: 3.5, end: 3.8, probability: 0.98 }, { word: " at", start: 3.8, end: 3.9, probability: 0.97 },
    { word: " three", start: 3.9, end: 4.3, probability: 0.99 }, { word: " small", start: 4.3, end: 4.7, probability: 0.98 },
    { word: " changes", start: 4.7, end: 5.2, probability: 0.99 }, { word: " that", start: 5.2, end: 5.4, probability: 0.97 },
    { word: " add", start: 5.4, end: 5.6, probability: 0.96 }, { word: " up.", start: 5.6, end: 6.0, probability: 0.95 } ] },
];

// --- wordsFromSegments --------------------------------------------------------------------------

test("wordsFromSegments: reads whisper's {word,start,end,probability}, trims the leading space, keeps time order", () => {
  const words = wordsFromSegments(SEGMENTS);
  assert.equal(words.length, 15);
  assert.deepEqual(words[0], { text: "Hello", start: 0, end: 0.4 });
  assert.deepEqual(words[4], { text: "back.", start: 1.6, end: 2.4 });
  assert.deepEqual(words[5], { text: "Today", start: 3, end: 3.4 });
  for (let i = 1; i < words.length; i++) assert.ok(words[i].start >= words[i - 1].start, `word ${i} is out of order`);
});

test("wordsFromSegments: accepts the whole transcribe document as well as the bare array", () => {
  assert.deepEqual(wordsFromSegments({ language: "en", duration: 6, segments: SEGMENTS }), wordsFromSegments(SEGMENTS));
});

test("wordsFromSegments: a segment without words spreads its text over its span by character weight", () => {
  const words = wordsFromSegments([{ id: 0, start: 10, end: 13, text: " one three" }]);
  assert.deepEqual(words.map((x) => x.text), ["one", "three"]);
  assert.equal(words[0].start, 10);
  assert.equal(words[1].end, 13);
  assert.ok(words[0].end === words[1].start, "the spread is contiguous");
  assert.ok(words[0].end - words[0].start < words[1].end - words[1].start, "the longer word gets the longer slot");
});

test("wordsFromSegments: non-speech tags are dropped, bare punctuation stays attached to nothing", () => {
  const words = wordsFromSegments([
    { id: 0, start: 0, end: 2, text: " [BLANK_AUDIO]", words: [{ word: " [BLANK_AUDIO]", start: 0, end: 2, probability: 0.5 }] },
    { id: 1, start: 2, end: 4, text: " [ Music ] hi", words: [{ word: " [Music]", start: 2, end: 3, probability: 0.5 }, { word: " hi", start: 3, end: 4, probability: 0.9 }] },
    { id: 2, start: 4, end: 6, text: " [ Silence ] yes there" },
  ]);
  assert.deepEqual(words.map((x) => x.text), ["hi", "yes", "there"]);
  const kept = wordsFromSegments([{ id: 0, start: 0, end: 1, text: " [Music]", words: [{ word: " [Music]", start: 0, end: 1 }] }], { dropNonSpeech: false });
  assert.deepEqual(kept.map((x) => x.text), ["[Music]"]);
});

test("wordsFromSegments: fails loud on a shape it cannot place, never drops words silently", () => {
  assert.throws(() => wordsFromSegments("nope"), TypeError);
  assert.throws(() => wordsFromSegments({ segments: "nope" }), TypeError);
  assert.throws(() => wordsFromSegments([{ id: 0, start: 0, end: 1, words: [{ word: " a", start: "x", end: 1 }] }]), RangeError);
  assert.throws(() => wordsFromSegments([{ id: 3, start: 0, end: 1, words: [{ word: " a", start: 0.5 }] }]), /segment 3/);
  // a word whose end precedes its start is repaired, not rejected: whisper produces these
  assert.deepEqual(wordsFromSegments([{ id: 0, start: 0, end: 1, words: [{ word: " a", start: 0.8, end: 0.7 }] }]), [{ text: "a", start: 0.8, end: 0.8 }]);
  assert.deepEqual(wordsFromSegments([]), []);
});

// --- groupWords: the pace presets and the three break rules ----------------------------------------

test("groupWords: the paces are 2-3, 3-5 and 4-6 words; the default is conversational", () => {
  assert.deepEqual({ ...PACES.punchy }, { maxWords: 3 });
  assert.deepEqual({ ...PACES.conversational }, { maxWords: 5 });
  assert.deepEqual({ ...PACES.calm }, { maxWords: 6 });
  assert.equal(DEFAULTS.pace, "conversational");
  assert.equal(DEFAULTS.gapSec, 0.15);
  const words = run(12);
  assert.deepEqual(groupWords(words).map((g) => g.words), [5, 5, 2]);
  assert.deepEqual(groupWords(words, { pace: "punchy" }).map((g) => g.words), [3, 3, 3, 3]);
  assert.deepEqual(groupWords(words, { pace: "calm" }).map((g) => g.words), [6, 6]);
  assert.deepEqual(groupWords(words, { maxWords: 4 }).map((g) => g.words), [4, 4, 4]);
  assert.throws(() => groupWords(words, { pace: "frantic" }), RangeError);
  assert.throws(() => groupWords(words, { maxWords: 0 }), RangeError);
  assert.throws(() => groupWords(words, { gapSec: -1 }), RangeError);
});

test("groupWords: a sentence end closes the group even when it is short", () => {
  const words = [w("Hello", 0, 0.4), w("there.", 0.4, 0.9), w("How", 0.9, 1.2), w("are", 1.2, 1.4), w("you?", 1.4, 1.9), w("Fine", 1.9, 2.3)];
  const g = groupWords(words);
  assert.deepEqual(g.map((x) => x.text), ["Hello there.", "How are you?", "Fine"]);
  // closing quotes and brackets after the stop still count
  const q = groupWords([w("He", 0, 0.2), w('said "go."', 0.2, 0.8), w("Then", 0.8, 1.1)]);
  assert.deepEqual(q.map((x) => x.text), ['He said "go."', "Then"]);
  // a number with a decimal point is not a sentence end
  assert.deepEqual(groupWords([w("costs", 0, 0.3), w("3.5", 0.3, 0.6), w("units", 0.6, 0.9)]).map((x) => x.text), ["costs 3.5 units"]);
});

test("groupWords: a pause of gapSec or more closes the group; a shorter one does not", () => {
  const under = [w("a", 0, 0.3), w("b", 0.4, 0.7), w("c", 0.85, 1.1)]; // gaps 0.10 and 0.15
  assert.deepEqual(groupWords(under, { gapSec: 0.15 }).map((x) => x.text), ["a b", "c"], "0.15 s is a break");
  assert.deepEqual(groupWords(under, { gapSec: 0.2 }).map((x) => x.text), ["a b c"], "0.10 and 0.15 are under 0.2");
  assert.deepEqual(groupWords(under, { gapSec: 0.05 }).map((x) => x.text), ["a", "b", "c"]);
});

test("groupWords: maxChars keeps a group on one line; a word longer than the limit stands alone", () => {
  const words = [w("international", 0, 0.5), w("collaboration", 0.5, 1), w("requires", 1, 1.4), w("patience", 1.4, 1.9)];
  assert.deepEqual(groupWords(words, { maxChars: 27 }).map((x) => x.text), ["international collaboration", "requires patience"]);
  assert.deepEqual(groupWords(words, { maxChars: 5 }).map((x) => x.text), ["international", "collaboration", "requires", "patience"]);
  assert.deepEqual(groupWords([w("supercalifragilistic", 0, 1)], { maxChars: 4 }).map((x) => x.text), ["supercalifragilistic"]);
});

test("groupWords: group times, linger, minimum hold, and never two groups on screen at once", () => {
  const words = [w("a", 1, 1.2), w("b", 1.2, 1.4), w("c", 3, 3.2), w("d", 3.2, 3.3), w("e", 3.35, 3.4)];
  const g = groupWords(words, { maxWords: 2, lingerSec: 0.3, minHoldSec: 0.5, gapSec: 0.15 });
  assert.deepEqual(g.map((x) => x.text), ["a b", "c d", "e"]);
  assert.equal(g[0].start, 1);
  assert.ok(Math.abs(g[0].end - 1.7) < 1e-9, `linger: ${g[0].end}`); // 1.4 + 0.3
  assert.equal(g[1].start, 3);
  assert.ok(Math.abs(g[1].end - 3.35) < 1e-9, `capped at the next start: ${g[1].end}`); // 3.3 + 0.3 = 3.6 > 3.35
  assert.equal(g[2].start, 3.35);
  assert.ok(Math.abs(g[2].end - 3.85) < 1e-9, `minimum hold: ${g[2].end}`); // 3.4 + 0.3 = 3.7 < 3.35 + 0.5
  for (let i = 1; i < g.length; i++) assert.ok(g[i - 1].end <= g[i].start + 1e-9, "groups never overlap");
  for (const x of g) assert.ok(x.end > x.start, "every group has a positive length");
});

test("groupWords: overlapping or identical word times never yield a zero-length or overlapping group", () => {
  const words = [w("a", 1, 1.6), w("b", 1.2, 1.8), w("c", 1.2, 1.5), w("d", 1.2, 1.4)];
  const g = groupWords(words, { maxWords: 1 });
  for (const x of g) assert.ok(x.end > x.start, JSON.stringify(x));
  for (let i = 1; i < g.length; i++) assert.ok(g[i - 1].end <= g[i].start + 1e-9);
  assert.deepEqual(g.map((x) => x.text).join(" "), "a b c d");
  assert.throws(() => groupWords([w("b", 2, 3), w("a", 1, 2)]), RangeError, "words must arrive in time order");
  assert.deepEqual(groupWords([]), []);
});

test("groupWords: pure and deterministic — the input is not mutated and the same input gives the same output", () => {
  const words = Object.freeze(run(9, { gap: 0.05 }).map((x) => Object.freeze(x)));
  const a = groupWords(words);
  const b = groupWords(words);
  assert.deepEqual(a, b);
  assert.deepEqual(words, run(9, { gap: 0.05 }));
});

// --- toWordsJson: the variable the template reads ---------------------------------------------------

test("toWordsJson: compact [start,end,text] triples at two decimals, valid JSON, exact bytes", () => {
  const groups = [{ text: "Hello there.", start: 0, end: 1.234, words: 2 }, { text: 'say "hi" \\ done', start: 1.5, end: 2.006, words: 3 }];
  const json = toWordsJson(groups);
  assert.equal(json, '[[0,1.23,"Hello there."],[1.5,2.01,"say \\"hi\\" \\\\ done"]]');
  assert.deepEqual(JSON.parse(json), [[0, 1.23, "Hello there."], [1.5, 2.01, 'say "hi" \\ done']]);
  assert.equal(toWordsJson([]), "[]");
  assert.equal(toWordsJson(groups, { precision: 1 }), '[[0,1.2,"Hello there."],[1.5,2,"say \\"hi\\" \\\\ done"]]');
});

test("toWordsJson: rounding never makes a group empty or overlap the next", () => {
  // two groups a few milliseconds apart would both round to 1.00 s
  const groups = [{ text: "a", start: 1.001, end: 1.004, words: 1 }, { text: "b", start: 1.004, end: 1.5, words: 1 }, { text: "c", start: 1.5, end: 1.5, words: 1 }];
  const t = JSON.parse(toWordsJson(groups));
  for (const [i, g] of t.entries()) {
    assert.ok(g[1] > g[0], `group ${i} keeps a positive length: ${JSON.stringify(g)}`);
    if (i > 0) assert.ok(g[0] >= t[i - 1][1], `group ${i} starts after group ${i - 1} ends: ${JSON.stringify(t)}`);
  }
  assert.deepEqual(t.map((g) => g[2]), ["a", "b", "c"], "no group is dropped by the repair");
});

// --- splitForVariable: the 16 KB cap and the ten-minute cap ----------------------------------------

function manyGroups(n, { every = 2, len = 1.6 } = {}) {
  return Array.from({ length: n }, (_, i) => ({ text: `group number ${i + 1}`, start: i * every, end: i * every + len, words: 3 }));
}

test("splitForVariable: one chunk when it fits; chunk 0 keeps absolute time", () => {
  const groups = manyGroups(5);
  const chunks = splitForVariable(groups);
  assert.equal(chunks.length, 1);
  assert.equal(chunks[0].offset_sec, 0);
  assert.equal(chunks[0].group_count, 5);
  assert.ok(chunks[0].duration_sec >= groups[4].end && chunks[0].duration_sec < groups[4].end + 0.05, `duration ${chunks[0].duration_sec}`);
  assert.deepEqual(JSON.parse(chunks[0].words_json)[4], [8, 9.6, "group number 5"]);
});

test("splitForVariable: every chunk fits the character cap and the duration cap, and no group is lost or reordered", () => {
  const groups = manyGroups(700, { every: 1.5, len: 1.2 });
  const cap = 4000;
  const chunks = splitForVariable(groups, { maxChars: cap, maxSec: 200 });
  assert.ok(chunks.length > 1);
  let seen = 0;
  for (const [k, c] of chunks.entries()) {
    assert.ok(c.words_json.length <= cap, `chunk ${k}: ${c.words_json.length} chars`);
    assert.ok(c.duration_sec <= 200 && c.duration_sec >= 1, `chunk ${k}: ${c.duration_sec} s`);
    const triples = JSON.parse(c.words_json);
    assert.equal(triples.length, c.group_count);
    for (const [i, t] of triples.entries()) {
      const src = groups[seen + i];
      assert.equal(t[2], src.text);
      assert.ok(Math.abs(t[0] + c.offset_sec - src.start) < 0.011, `chunk ${k} group ${i}: rebased start`);
      assert.ok(t[1] <= c.duration_sec + 1e-9, `chunk ${k} group ${i} ends after its chunk`);
    }
    seen += triples.length;
  }
  assert.equal(seen, groups.length, "every group lands in exactly one chunk");
  assert.equal(chunks[0].offset_sec, 0);
  for (let k = 1; k < chunks.length; k++) {
    assert.ok(chunks[k].offset_sec > chunks[k - 1].offset_sec);
    assert.ok(chunks[k - 1].offset_sec + chunks[k - 1].duration_sec <= chunks[k].offset_sec + 0.011, "chunks do not overlap on the source timeline");
  }
});

test("splitForVariable: the shipped defaults are the template's own limits (16000 characters, 600 s)", () => {
  assert.equal(DEFAULTS.maxChars, 42);
  assert.equal(DEFAULTS.variableChars, 16000);
  assert.equal(DEFAULTS.variableSec, 600);
  const chunks = splitForVariable(manyGroups(2000, { every: 2, len: 1.5 }));
  assert.ok(chunks.length >= 2);
  for (const c of chunks) { assert.ok(c.words_json.length <= 16000); assert.ok(c.duration_sec <= 600); }
  assert.deepEqual(splitForVariable([]), []);
  assert.throws(() => splitForVariable(manyGroups(3), { maxChars: 5 }), RangeError, "a single group that cannot fit is an error, not a silent drop");
});

// --- the whole pipeline on a realistic transcript -----------------------------------------------------

test("captionsFromSegments: segments.json to chunk, on the real shape", () => {
  const out = captionsFromSegments(SEGMENTS, { pace: "conversational" });
  assert.equal(out.words, 15);
  assert.deepEqual(out.groups.map((g) => g.text), ["Hello there, and welcome back.", "Today we look at three", "small changes that add up."]);
  assert.equal(out.chunks.length, 1);
  const triples = JSON.parse(out.chunks[0].words_json);
  assert.equal(triples.length, 3);
  assert.equal(triples[0][0], 0);
  assert.ok(out.chunks[0].duration_sec >= 6);
  assert.equal(out.template, "captions-bar");
  assert.deepEqual(Object.keys(out.chunks[0]).sort(), ["duration_sec", "group_count", "offset_sec", "words_json"]);
});

// --- the command line: the way a skill or an operator calls it ---------------------------------------

test("command line: reads a segments.json and prints the chunks as one JSON document; a bad file fails non-zero", () => {
  const dir = mkdtempSync(join(tmpdir(), "captions-groups-"));
  const file = join(dir, "clip.segments.json");
  writeFileSync(file, JSON.stringify(SEGMENTS));
  const ok = spawnSync(process.execPath, [HELPER, file, "--pace", "punchy"], { encoding: "utf8" });
  assert.equal(ok.status, 0, ok.stderr);
  const doc = JSON.parse(ok.stdout);
  assert.equal(doc.template, "captions-bar");
  assert.ok(doc.chunks.length >= 1 && doc.groups.length > 3);
  const bad = join(dir, "bad.json");
  writeFileSync(bad, "{not json");
  const fail = spawnSync(process.execPath, [HELPER, bad], { encoding: "utf8" });
  assert.notEqual(fail.status, 0);
  assert.match(fail.stderr, /captions-groups:/);
  assert.notEqual(spawnSync(process.execPath, [HELPER], { encoding: "utf8" }).status, 0, "no file is an error");
});
