# captions-bar

A caption track over transparency at 1920×1080 and 30 fps: one short group of words at a time, in a dark
translucent bar with an accent line along its bottom edge, centred near the bottom of the frame. A group
fades and rises in over 0.14 s, holds, and fades out over its last 0.12 s. Outside the bar every pixel has
alpha 0, so the output is ready to lay over footage. The render is silent: the template carries no audio.

This template was written for the harness. Its groups are a few words each, broken at sentence ends and
pauses, as is common caption practice; no text or code from any teaching kit is in it.

## Variables

| id | type | default | notes |
|---|---|---|---|
| `words_json` | string (≤ 16000) | three sample groups over 8 s | a JSON array of `[start, end, text]` triples in seconds, in time order and not overlapping; build it with `render/captions-groups.mjs` |
| `bar` | color | `rgba(10, 16, 14, 0.82)` | the bar behind each group; keep it dark and translucent so the text keeps its contrast on any shot |
| `text_color` | color | `#f6f8f7` | the caption text |
| `accent` | color | `#34d399` | the line along the bottom edge of the bar |
| `size` | number, 32-96 | `56` | text size in pixels on the 1080 px frame; a group wider than 1600 px wraps |
| `duration` | number, 1-600 s | `8` | total length of the overlay; rewrites the root `data-duration`. Groups that end after it are cut off |

A group's text is set as text, never markup, so a caption that contains `<b>` shows the characters. A
`words_json` that is not valid JSON, is not an array of `[start, end, text]` triples, or has a group that
starts before the previous one ends makes the page throw, and `check` fails the render with the message
(`CHECK_FAILED`) instead of producing an empty overlay.

## From a transcript

`offload_transcribe` writes `<base>.segments.json`. The helper groups its words and packs them into
chunks that fit this template:

```
node render/captions-groups.mjs clip.segments.json --pace conversational --out chunks.json
```

`--pace` is `punchy` (up to 3 words a group), `conversational` (5, the default) or `calm` (6). A group also
ends at a sentence end, at a pause of 0.15 s or more (`--gap-sec`), or when it would pass 42 characters
(`--max-chars`); each group is held 0.3 s past its last word (`--linger-sec`) and at least 0.5 s
(`--min-hold-sec`), never past the next group's start. `chunks.json` holds `chunks`, each with
`offset_sec`, `duration_sec`, `group_count` and `words_json`. Render one call per chunk:

```json
{"template": "captions-bar", "format": "webm", "variables": {"words_json": "[[0.4,2.4,\"Hello there\"],[2.6,4.9,\"and welcome back\"]]", "duration": 5.5}}
```

The first chunk keeps absolute time; every later chunk is rebased to start at 0, so lay it over the footage
`offset_sec` seconds in. A chunk holds at most 16,000 characters and 600 s, which is about ten minutes of
speech.

## Laying it over footage

Render to `webm` (VP9 with alpha) or `mov` (ProRes 4444). `offload_media` has no overlay operation, so
composite with ffmpeg's `overlay` filter, or import the file as a track in an editor. The footage keeps its
own audio; when a piece is built from silent parts, `offload_media` `mux_audio` puts the audio back. Decode
the `webm` with `-c:v libvpx-vp9` so ffmpeg reads its alpha:

```
ffmpeg -i footage.mp4 -itsoffset <offset_sec> -c:v libvpx-vp9 -i chunk.webm \
  -filter_complex "[0:v][1:v]overlay=eof_action=pass:format=auto[v]" -map "[v]" -map 0:a -c:a copy out.mp4
```

| format | encoding | use |
|---|---|---|
| `webm` | VP9 `yuva420p` | an overlay for the web or ffmpeg |
| `mov` | ProRes 4444 `yuva444p12le` | an overlay for an editor |
| `mp4` | H.264, the transparency flattened to white | opaque; only for a preview |

## How it works

The page holds one caption element and no CSS animation. HyperFrames dispatches an `hf-seek` event on the
window with the time in seconds at every seek; the page finds the group that is on screen at that time
(a binary search) and sets the element's text, opacity and offset from it. A frame is a pure function of
the time, whichever worker draws it. One animated element per group was measured first and does not scale
(see the contract, item 7, in the [templates README](../README.md)). The template depends on the seek
event of the pinned HyperFrames, so a version bump re-renders it as part of its checks.

## Measured

Rendered through `render/compose-hyperframes.mjs` on a 36-thread Windows box with software GL, on
2026-09-30. The default three-group sample, `webm`, quality `high`:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe read VP9 `yuva420p`, 1920×1080, 30 fps, 240 frames and 8.000 s, no audio stream;
- HyperFrames' render time was 23.3 s at 1 worker and 23.7 s at `auto` (14.6 s and 14.8 s per 150 frames);
- rendered at 1, 2, 4 and 6 workers and at `auto` twice, all 15 pairs of decoded frames (libvpx decoder, so the
  alpha is in the comparison) were identical;
- the alpha plane was 0 everywhere at 0.3 s (before the first group), inside each group it had a maximum of
  255 and a mean of about 8, and it was 0 again in the gap at 2.8 s and after the last group ended, at 7.9 s;
- a caption composited over flat grey and over flat white showed the bar, the text and the accent line intact.

The full-size case, from a synthetic 12-minute transcript in the exact shape `offload_transcribe` writes
(159 segments, 1,529 words) through `render/captions-groups.mjs`: 450 groups of 1 to 5 words (at most 33
characters, none overlapping) in two chunks. Chunk 0 is 374 groups, 12,908 characters and 599.16 s; chunk 1 is
76 groups, 2,527 characters and 120.84 s at offset 600.11 s. Chunk 0's variable went through `lint`, `check` and
`--strict-variables` and the first 15 s rendered in 39.6 s (450 frames, draft quality). Frames at 1.47 s, 7.91 s
and 12.73 s showed exactly the groups the list holds for those times ("Helped and so told before", "each did.",
"keeps then noise and record."), and 5.28 s, in a gap between two groups, was empty; the harness vision lane read
the frames back the same way and called the gap plain flat grey. A 15,969-character list (416 groups, just
under the cap) rendered too, and its group at 4.0 s ("group 2 one two three") was the right one. The 16 KB question
was measured before the template was designed, in a probe page: a 15,968-character string (476 groups) went through
`lint`, `check` and `--strict-variables`, and the rendered frame read back its group count, its length and its last
group from `getVariables()`.

Bad input, through the runner: a `words_json` that is not JSON, is not an array, has an entry that is not
`[start, end, text]`, or has overlapping groups fails `CHECK_FAILED` with the page's own message; 16,001
characters is `BAD_INPUT`; an empty list `[]` renders an empty overlay. A caption of `<b>bold</b> & <i>x</i>` shows
those characters as they are. A group of 42 wide letters at `size` 96 wraps to three lines inside the bar,
which grows upward, and stays in the frame. `duration` 1 (the minimum) renders 30 frames.

The overlay command above was run on a 12 s clip with audio and a chunk placed 1.5 s in: the caption appeared at
3.4 s and not at 1.0 s, the bar was translucent over the footage, and the audio stream was copied unchanged.
