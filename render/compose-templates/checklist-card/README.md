# checklist-card

A full-frame opaque checklist at 1920×1080 and 30 fps:

- a glow rises from the bottom edge and the title (Inter 700, one phrase in the accent color) pulls into
  focus;
- up to four rows spring in one after another, 0.18 s apart, each a translucent panel with a ring;
- as each row lands its ring pulses and a tick draws itself (a stroke drawn with CSS);
- an item left empty hides its whole row, so a two-item list is two rows;
- the glow breathes through the hold, and everything fades out 0.6 s before the end.

This template is adapted from hyperframes-student-kit @0d30152 card kallaway.t1.overview.checklist. The
kit's licence texts, and a record of what was taken and what was not, are in
[`_third_party/hyperframes-student-kit/`](../_third_party/hyperframes-student-kit/PROVENANCE.md).

## Variables

| id | type | default | notes |
|---|---|---|---|
| `title_pre` | string (≤ 24) | `Before you` | the first part of the title, 54 px |
| `title_em` | string (≤ 16) | `ship` | the emphasised phrase, in the accent color; may be empty |
| `title_post` | string (≤ 24) | `a release` | the last part of the title; may be empty |
| `item1` | string (≤ 56) | `Run the full test suite on a clean checkout` | a row, 36 px; empty hides the row |
| `item2` | string (≤ 56) | `Read the changelog aloud once` | a row; empty hides the row |
| `item3` | string (≤ 56) | `Tag the commit and record its hash` | a row; empty hides the row |
| `item4` | string (≤ 56) | `Tell the team what to watch after deploy` | a row; empty hides the row |
| `background` | color | `#0a1210` | the canvas |
| `accent` | color | `#34d399` | the title emphasis, the rings, the ticks and the glow |
| `text_color` | color | `#f1f5f4` | the title and the rows |
| `duration` | number, 4-30 s | `7` | total length; rewrites the root `data-duration` |

The three title parts are one sentence. A variable is text only, never markup, so the emphasis is a
split: give the parts in reading order, and the page puts one space between them. An empty part leaves
no gap.

## Use

```json
{"template": "checklist-card", "variables": {"title_pre": "Three checks", "title_em": "before", "title_post": "lunch", "item1": "Open the dashboard", "item2": "Compare with yesterday", "item3": "Note anything odd", "item4": "", "duration": 6}}
```

The card is opaque, so render it as `mp4` (the default). `webm` and `mov` work too.

## Changes from the source card

- Motion is CSS `@keyframes` only. The card loaded a script animation library from a CDN and built its
  motion in script; this one loads nothing. The tick is the same idea (a stroke whose dash offset runs to
  zero), drawn with a CSS animation.
- Text arrives through declared variables. The card's `<em>` inside its title slot is split into
  `title_pre`, `title_em` and `title_post`. The four rows are four variables, and an empty one hides its
  row, which the card had no way to do.
- The frosted-glass blur behind each row is gone: the rows sit on an opaque canvas, so a translucent panel
  gives the same look without a per-frame blur.
- The ring's glow pulse (a growing shadow in the card) is an opacity pulse here. A first version of this
  template popped the ring by scaling it to 1.1, which made frames depend on the worker count, so nothing
  scales above its resting size (see the contract, item 6, in the
  [templates README](../README.md)).
- Inter from the shared font kit replaces the card's own faces, and its web-font import is not used.
- The palette is recoloured (a deep green-black canvas and a teal-green accent), and the placeholder copy is
  replaced with neutral copy. The film-grain texture is not carried over.

## Measured

Rendered through `render/compose-hyperframes.mjs` on a 36-thread Windows box with software GL, quality
`high`, with the defaults (`duration` 7), on 2026-09-30:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe read H.264 yuv420p, 1920×1080, 30 fps, 210 frames and 7.000 s, with no audio stream;
- HyperFrames' render time was 33.3 s at 1 worker and 25.8 s at `auto` (23.8 s and 18.4 s per 150 frames). At one
  worker that is above the 14 to 22 s of the two older templates: four rows and a title enter with a blur at
  once. The whole gated call took 50 s;
- rendered at 1, 2, 4 and 6 workers and at `auto` twice (draft quality), all 15 pairs of decoded frames were
  identical (`framemd5`); at `high` quality the 1-worker and `auto` files were byte-identical;
- frames read at 1.6 s (rows one and two done, the third's tick half drawn, the fourth still blurred and arriving),
  3.5 s (the full list) and 6.4 s (the instant before the exit). The harness vision lane read the 3.5 s frame
  back with the title and all four rows exactly as written and counted four ticks;
- with `item3`, `item4` and `title_em` empty, two rows showed and the title read as one phrase with a single space;
- every string variable at its limit in `W` (24, 16, 24 and four of 56 characters): `check` passed, the title ran to
  three lines and each row to two, the last row ended at 1005 px, and nothing clipped;
- at `duration` 4 (the minimum) the card renders and passes `lint` and `check`;
- known limit: the glows are 8-bit radial gradients on a dark canvas, so a strong contrast stretch of an
  encoded frame shows faint contour steps (H.264 keeps them); at normal viewing they do not show. The kit hid
  them with a grain texture, which was dropped (see the changes above).
