# lower-third

A broadcast lower third over transparency, at 1920×1080 and 30 fps:

- an accent stripe grows up;
- a translucent panel reveals left to right;
- the name (Inter 700) and the role (Inter 400) slide in;
- the whole unit slides out and fades 0.5 s before the end.

Outside the panel every pixel has alpha 0, so the output is ready to lay over footage.

## Variables

| id | type | default | notes |
|---|---|---|---|
| `name` | string (≤ 48) | `Alex Rivera` | 60 px, one line |
| `role` | string (≤ 72) | `Head of Research` | 34 px, one line |
| `accent` | color | `#f59e0b` | the stripe |
| `panel` | color | `rgba(15, 23, 42, 0.9)` | the panel; keep it translucent so footage shows through |
| `text_color` | color | `#f8fafc` | name and role |
| `duration` | number, 2-60 s | `6` | total length; rewrites the root `data-duration` |

## Use

```json
{"template": "lower-third", "format": "webm", "variables": {"name": "Dana Okafor", "role": "Principal Engineer, Platform", "accent": "#22c55e"}}
```

Pick the format for how the overlay will be used:

| format | encoding | use |
|---|---|---|
| `webm` | VP9 `yuva420p` | an overlay for the web or ffmpeg |
| `mov` | ProRes 4444 `yuva444p10le` | an overlay for an editor |
| `mp4` | H.264, the transparency flattened to white | opaque; only for a preview |

## Measured

Rendered on a 36-thread Windows box, with the caller's variables above, `duration` 5 and software
GL:

- `lint` found 0 errors and 0 warnings, and `check` passed;
- ffprobe with the libvpx decoder read the `webm` as VP9 `yuva420p` with `ALPHA_MODE=1`, 1920×1080,
  30 fps, 150 frames, 5.000 s;
- the alpha plane was 0 everywhere at 0.05 s (before the entrance). At 2.5 s it had a minimum of 0,
  a maximum of 255 and a mean of 11.5: transparent everywhere except the panel;
- HyperFrames' render time was 22.0 s at 1 worker and 18.1 s at `auto` for `webm`, and 14.4 s and
  17.5 s for `mp4`. An earlier session measured 30.4 s for the 1-worker `webm`;
- the 1-worker and `auto` renders decoded to identical frames (`framemd5`), for `webm` and `mp4`;
- frames composited over flat grey at 1.0 s and 2.5 s showed the navy panel, the green stripe and
  the caller's name and role, with the grey visible all around. The `mp4` frames show the same
  panel over white (RGB 253,255,255), and at 4.7 s the unit is mid-exit, faded and shifted left.
