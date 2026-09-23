# title-card

A full-frame opaque title card at 1920×1080 and 30 fps:

- an accent bar grows in;
- a headline (Inter 900) and a subtitle (Inter 400) rise in;
- a soft radial glow in the accent color drifts across the whole length, so the hold never freezes;
- everything fades out 0.6 s before the end.

## Variables

| id | type | default | notes |
|---|---|---|---|
| `title` | string (≤ 80) | `Quarterly results` | one or two short lines at 128 px |
| `subtitle` | string (≤ 140) | `What changed, and why it matters` | one supporting line |
| `background` | color | `#0f172a` | card background |
| `accent` | color | `#38bdf8` | bar and glow |
| `text_color` | color | `#f8fafc` | headline and subtitle |
| `duration` | number, 2-60 s | `5` | total length; rewrites the root `data-duration` |

## Use

```json
{"template": "title-card", "variables": {"title": "Launch day", "subtitle": "Everything that shipped", "accent": "#22c55e", "duration": 6}}
```

The card is opaque, so render it as `mp4` (the default). `webm` and `mov` work too.

## Measured

Rendered on a 36-thread Windows box through `render/compose-hyperframes.mjs` with software GL,
quality `high` and 5 s (150 frames):

- `lint` found 0 errors and 0 warnings, and `check` passed;
- the output was H.264 yuv420p at 1920×1080, 30 fps and 5.000 s;
- HyperFrames' render time was 16.4 s at 1 worker and 15.3 s / 14.6 s at `auto` (an earlier
  session on the same box: 21.2 s and 24-25 s); the whole gated call took 27-31 s;
- the 1-worker render and two `auto` renders were byte-identical MP4 files with identical
  `framemd5` over all 150 frames.

Frames read back at 1.0 s and 4.7 s showed the caller's headline and subtitle in Inter over the
navy card, the green accent bar and the glow; at 4.7 s the type is mid-fade and the glow has
drifted right.
