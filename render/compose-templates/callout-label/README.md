# callout-label

A callout card over transparency, anchored at the right edge of a 1920×1080, 30 fps frame:

- a soft haze of the accent color blooms in behind the card, and the card slides in from the right with a
  small overshoot;
- the term (Inter 900) and the accent rule under it, then the detail (Inter 400), reveal in turn;
- a lit line runs along the top edge of the card;
- the whole unit slides out and fades 0.5 s before the end.

Outside the card and its haze every pixel has alpha 0, so the output is ready to lay over footage.

This template is adapted from hyperframes-student-kit @0d30152 card kallaway.t2.label.callout. The kit's
licence texts, and a record of what was taken and what was not, are in
[`_third_party/hyperframes-student-kit/`](../_third_party/hyperframes-student-kit/PROVENANCE.md).

## Variables

| id | type | default | notes |
|---|---|---|---|
| `term` | string (≤ 32) | `Cold start` | the heading, 38 px; wraps onto more lines when long |
| `detail` | string (≤ 110) | `The first request after a quiet spell is slower because the model must load.` | one or two sentences, 27 px |
| `accent` | color | `#34d399` | the lit top edge, the rule and the haze |
| `panel` | color | `rgba(12, 20, 18, 0.9)` | the card; keep it dark and translucent so footage shows faintly through and the text keeps its contrast |
| `text_color` | color | `#f1f5f4` | the term and the detail |
| `duration` | number, 3-30 s | `6` | total length; rewrites the root `data-duration` |

The card is 500 px wide, 80 px from the right edge and 340 px from the top, and it grows downward as the
text grows. For a callout on the left, or lower, copy the template and change those three numbers.

## Use

```json
{"template": "callout-label", "format": "webm", "variables": {"term": "Cache hit", "detail": "The answer came from memory, so no model was loaded.", "accent": "#f59e0b"}}
```

Pick the format for how the overlay will be used:

| format | encoding | use |
|---|---|---|
| `webm` | VP9 `yuva420p` | an overlay for the web or ffmpeg |
| `mov` | ProRes 4444 `yuva444p12le` | an overlay for an editor |
| `mp4` | H.264, the transparency flattened to white | opaque; only for a preview |

## Changes from the source card

- Motion is CSS `@keyframes` only. The card loaded a script animation library from a CDN and built its
  motion in script; this one loads nothing.
- Text arrives through declared variables; the card's `term` and `detail` slots become plain strings (the
  card styled no emphasis in either).
- The frosted-glass blur behind the card is gone. In an overlay render the page has nothing behind the
  card, so a backdrop blur blurs nothing and costs CPU. The card is a dark, translucent panel instead
  (the `panel` variable), like the lower third.
- The blurred glow behind the card is a soft radial gradient, and the top edge's lit line is drawn
  directly; the card's shadow-pulse hold animation is dropped.
- Inter from the shared font kit replaces the card's own faces, and its web-font import is not used.
- The palette is recoloured (a teal-green accent on a dark green-black panel), and the placeholder copy is
  replaced with neutral copy.
- Nothing scales above its resting size (see the contract, item 6, in the [templates README](../README.md)).

## Measured

Rendered through `render/compose-hyperframes.mjs` on a 36-thread Windows box with software GL, quality
`high`, with the defaults (`duration` 6), on 2026-09-30:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe read the `webm` as VP9 `yuva420p` with `ALPHA_MODE=1` and the `mov` as ProRes 4444 `yuva444p12le`,
  both 1920×1080, 30 fps, 180 frames, 6.000 s, no audio stream;
- HyperFrames' render time was 22.0 s at 1 worker and 22.7 s at `auto` for `webm` (18.4 s and 18.9 s per 150
  frames), and 23.1 s and 19.9 s for `mov`. The `webm` is 0.7 MB and the `mov` 73.8 MB;
- rendered at 1, 2, 4 and 6 workers and at `auto` twice, all 15 pairs of decoded `webm` frames (read with the
  libvpx decoder, so the alpha is in the comparison) were identical, and the `mov` files were byte-identical.
  The two `webm` files differ in container bytes only;
- the alpha plane was nearly empty at 0.05 s (minimum 0, maximum 11: the haze just starting), and at 3.0 s it had a
  minimum of 0, a maximum of 255 and a mean of 16.5: transparent everywhere except the card and its haze. At 5.6 s,
  mid-exit, the maximum was 244. The ProRes alpha plane had the same coverage;
- frames composited over flat grey and over flat white at 3.0 s showed the card, the lit top edge, the accent rule
  and both texts intact, with the grey or white visible all around. The harness vision lane read the card back with
  both texts exactly as written and called the surround plain flat grey;
- every string variable at its limit in `W` (32 and 110 characters): `check` passed, the term ran to four lines and
  the detail to eight, the card grew downward to 940 px, and nothing clipped;
- at `duration` 3 (the minimum) the overlay renders and passes `lint` and `check`.
