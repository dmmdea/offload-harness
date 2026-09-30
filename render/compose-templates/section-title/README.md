# section-title

A full-frame opaque section title at 1920×1080 and 30 fps:

- a quiet glow blooms in at the centre, with a soft ellipse behind the words;
- a kicker fades up, then a large headline (Inter 900, 104 px) inhales into place, one phrase of it in the
  accent color;
- through the hold the halo breathes (opacity) and the whole block rises and settles by 6 px, twice;
- everything fades out 0.6 s before the end.

This template is adapted from hyperframes-student-kit @0d30152 card kallaway.t1.section.breath. The kit's
licence texts, and a record of what was taken and what was not, are in
[`_third_party/hyperframes-student-kit/`](../_third_party/hyperframes-student-kit/PROVENANCE.md).

## Variables

| id | type | default | notes |
|---|---|---|---|
| `kicker` | string (≤ 32) | `Chapter three` | uppercase lead-in above the headline, 26 px; may be empty |
| `headline_pre` | string (≤ 32) | `Where the plan` | the first part of the headline |
| `headline_em` | string (≤ 20) | `meets` | the emphasised phrase, in the accent color; may be empty |
| `headline_post` | string (≤ 32) | `the road` | the last part of the headline; may be empty |
| `background` | color | `#0a1210` | the canvas; the vignette darkens the edges toward black, so use a dark color |
| `accent` | color | `#34d399` | the kicker, the emphasised phrase and the glow |
| `text_color` | color | `#f1f5f4` | the headline |
| `duration` | number, 3-30 s | `6` | total length; rewrites the root `data-duration` |

The three headline parts are one sentence. A variable is text only, never markup, so the emphasis is a
split: give the parts in reading order, and the page puts one space between them. An empty part leaves
no gap. The headline wraps and balances its lines; it does not need a forced line break.

## Use

```json
{"template": "section-title", "variables": {"kicker": "Part two", "headline_pre": "What we", "headline_em": "measured", "headline_post": "first", "accent": "#f59e0b", "duration": 5}}
```

The card is opaque, so render it as `mp4` (the default). `webm` and `mov` work too.

## Changes from the source card

- Motion is CSS `@keyframes` only. The card loaded a script animation library from a CDN and built its
  motion in script; this one loads nothing.
- Text arrives through declared variables. The card's `<em>` inside its headline slot is split into
  `headline_pre`, `headline_em` and `headline_post`, and its forced line break is gone in favour of
  balanced wrapping.
- Inter from the shared font kit replaces the card's own faces, and its web-font import is not used.
- The palette is recoloured (a deep green-black canvas and a teal-green accent), and the placeholder copy
  is replaced with neutral copy.
- Not carried over: the film-grain texture and the 38 px blur on the halo. The halo is a soft gradient
  instead, which costs nothing.
- The slow scale pulse on the content (to 1.018) is now a 6 px rise and settle. Nothing scales above its
  resting size: an animated scale above 1 made frames depend on the worker count (see the contract,
  item 6, in the [templates README](../README.md)).

## Measured

Rendered through `render/compose-hyperframes.mjs` on a 36-thread Windows box with software GL, quality
`high`, with the defaults (`duration` 6), on 2026-09-30:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe read H.264 yuv420p, 1920×1080, 30 fps, 180 frames and 6.000 s, with no audio stream;
- HyperFrames' render time was 21.2 s at 1 worker and 17.8 s at `auto` (17.7 s and 14.9 s per 150 frames); the
  whole gated call took 37 s;
- rendered at 1, 2, 4 and 6 workers and at `auto` twice (draft quality), all 15 pairs of decoded frames were
  identical (`framemd5`); at `high` quality the 1-worker and `auto` files were byte-identical;
- frames read at 3.0 s (kicker and headline, the emphasised word in the accent color) and 5.6 s (mid-exit,
  dimmed). The harness vision lane read the 3.0 s frame back with the right words in the accent color;
- every string variable at its limit in `W` (32, 32, 20 and 32 characters): `check` passed and the headline ran
  to seven lines that fill the frame from 150 px to 990 px, with nothing clipped or overlapping. The limits are
  sized for that case, so a real headline of up to 84 characters fits at any width of letter;
- at `duration` 3 (the minimum) the card renders and passes `lint` and `check`;
- known limit: the glows are 8-bit radial gradients on a dark canvas, so a strong contrast stretch of an
  encoded frame shows faint contour steps (H.264 keeps them); at normal viewing they do not show. The kit hid
  them with a grain texture, which was dropped (see the changes above).
