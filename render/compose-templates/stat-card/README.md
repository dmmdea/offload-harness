# stat-card

A full-frame opaque stat card at 1920×1080 and 30 fps:

- a glow rises from the bottom edge and a kicker fades up;
- a giant figure (Inter 900, a gradient from the text color to the accent) pulls into focus;
- a caption with one emphasised phrase, and a small source line, follow;
- a halo behind the figure breathes through the hold, and everything fades out 0.6 s before the end.

This template is adapted from hyperframes-student-kit @0d30152 card kallaway.t1.stat.figure. The kit's
licence texts, and a record of what was taken and what was not, are in
[`_third_party/hyperframes-student-kit/`](../_third_party/hyperframes-student-kit/PROVENANCE.md).

## Variables

| id | type | default | notes |
|---|---|---|---|
| `kicker` | string (≤ 40) | `Customer survey` | uppercase lead-in above the figure, 26 px; may be empty |
| `stat` | string (≤ 12) | `82%` | the hero figure at 240 px; a figure wider than the frame wraps instead of clipping |
| `label_pre` | string (≤ 80) | `of teams said the new` | first part of the caption, 32 px |
| `label_em` | string (≤ 40) | `onboarding flow` | the emphasised phrase, bold at full brightness; may be empty |
| `label_post` | string (≤ 80) | `saved them time in week one` | last part of the caption; may be empty |
| `source` | string (≤ 48) | `Source: internal survey, n=400` | small uppercase credit at the bottom left; may be empty |
| `background` | color | `#0a1210` | the canvas |
| `accent` | color | `#34d399` | the kicker, the end of the figure's gradient and the glow |
| `text_color` | color | `#f1f5f4` | the start of the figure's gradient and the caption |
| `duration` | number, 3-30 s | `6` | total length; rewrites the root `data-duration` |

The three caption parts are one sentence. A variable is text only, never markup, so the emphasis is a
split: give the parts in reading order, and the page puts one space between them. An empty part leaves
no gap.

## Use

```json
{"template": "stat-card", "variables": {"kicker": "Launch week", "stat": "3.4x", "label_pre": "faster search across", "label_em": "every workspace", "label_post": "since the update", "source": "Source: internal benchmark", "accent": "#f59e0b", "duration": 6}}
```

The card is opaque, so render it as `mp4` (the default). `webm` and `mov` work too.

## Changes from the source card

- Motion is CSS `@keyframes` only. The card loaded a script animation library from a CDN and built its
  motion in script; this one loads nothing.
- The count-up is gone. It needed script, and the figure is free text (`3.4x`, `$1.2M`) with no single
  number to count to. The figure pulls into focus instead. A CSS `@property` counter would need the number
  split from its prefix and suffix; that was not built and is unverified.
- Text arrives through declared variables. The card's `<em>` inside its caption slot is split into
  `label_pre`, `label_em` and `label_post`.
- Inter from the shared font kit replaces the card's own faces, and its web-font import is not used.
- The palette is recoloured (a deep green-black canvas and a teal-green accent), and the placeholder
  copy, including the source line's named organisation, is replaced with neutral copy.
- Not carried over: the film-grain texture (an image embedded in the style, and a blend over the whole
  frame that costs CPU) and the frosted-glass blur. The dot texture, the halo, the glow and the vignette
  are rebuilt from gradients.
- Nothing scales above its resting size. The halo's breathing is opacity only, because an animated scale
  above 1 made frames depend on the worker count (see the contract, item 6, in the
  [templates README](../README.md)).

## Measured

Rendered through `render/compose-hyperframes.mjs` on a 36-thread Windows box with software GL, quality
`high`, with the defaults (`duration` 6), on 2026-09-30:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe read H.264 yuv420p, 1920×1080, 30 fps, 180 frames and 6.000 s, with no audio stream;
- HyperFrames' render time was 21.2 s at 1 worker and 18.5 s at `auto` (17.7 s and 15.4 s per 150 frames,
  inside the 14 to 22 s of the two older templates, focus-pull blurs included); the whole gated call took 37 s;
- rendered at 1, 2, 4 and 6 workers and at `auto` twice (draft quality), all 15 pairs of decoded frames were
  identical (`framemd5`); at `high` quality the 1-worker and `auto` files were byte-identical;
- frames read at 3.0 s (the full card) and 5.6 s (mid-exit, dimmed); the caption balances onto two lines and the
  emphasised phrase is bold and brighter than the rest. The harness vision lane read the 3.0 s frame back with
  every text exactly as written;
- every string variable at its limit in `W` (40, 12, 80, 40, 80 and 48 characters): `check` passed, the
  figure wrapped onto two lines, the caption ran to eight, and nothing clipped or overlapped;
- at `duration` 3 (the minimum) and at 30 (the maximum: 900 frames, 65.8 s at draft quality) the card renders and
  passes `lint` and `check`; at 3 the exit starts at 2.4 s, while the source line is still arriving, and the 2.7 s
  frame is half faded; at 30 the 29.7 s frame is fading and the breathing has run the whole hold;
- known limit: the glows are 8-bit radial gradients on a dark canvas, so a strong contrast stretch of an
  encoded frame shows faint contour steps (H.264 keeps them); at normal viewing they do not show. The kit hid
  them with a grain texture, which was dropped (see the changes above).
