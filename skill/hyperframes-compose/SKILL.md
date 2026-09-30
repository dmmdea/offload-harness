---
name: hyperframes-compose
description: Use when the user wants designed motion graphics rendered to video for free on the local machine with the harness's HyperFrames lane — title cards, lower thirds, stat cards, section titles, callouts, checklists, caption overlays, or any transparent overlay (webm / mov with alpha) to lay over footage — or wants a new compose template authored or checked. Routes to offload_compose_video (CLI compose-video, fleet task compose-video); never to npx hyperframes. Triggers on "title card", "lower third", "stat card", "captions overlay", "alpha overlay", "motion graphics", "offload_compose_video", "compose-video", "HyperFrames".
---

# HyperFrames compose lane (offload_compose_video)

The harness renders designed, text-exact motion graphics from HTML and CSS with a **pinned, environment-scrubbed
HyperFrames** (ADR 0059). It is CPU-class: software GL and CPU encode, no GPU lease, so it runs beside every
render and every text seat. Same inputs give the same frames. This file only routes and states the rules;
the template contract and the measured numbers are in `render/compose-templates/README.md`, and the lane's
design is in `docs/systems/media-generation.md` (Composition).

## Hard bans

- **Never run `npx hyperframes`, and never run the `hyperframes` CLI by hand, on any machine.** The harness
  runner is the only door. It pins the version, scrubs the environment, forces `--json` (the one switch that
  skips the CLI's network checks) and refuses everything except `lint`, `check`, `render`, `snapshot`,
  `browser ensure|path` and `--version`.
- **Never `init`, `skills`, `upgrade`, `add`, `capture`, `preview`, `tts`, `transcribe`, `publish`, `cloud`,
  `lambda` or `cloudrun`.** `init` and `skills` write agent skills into your global skills folder from
  unpinned upstream; `upgrade`, `add`, `tts` and `transcribe` pull code or models from the network;
  `capture` calls cloud services; the rest are refused by the runner and would be a cloud path if they
  were not.
- **Never `npx skills add` (or any skill installer) for HyperFrames, its registry, or a teaching kit for
  it.** The house skill is this one. Do not install upstream's or a kit's skills.
- **`html` and `project_dir` are trusted code only.** HyperFrames' Chrome runs without a sandbox. Never
  pass a third-party page. The fleet door accepts only the vetted templates under
  `render/compose-templates/`, by name, with typed variables.
- No hosted service, no scheduled job, no daemon: this lane is a CLI call that exits.

## Which tool

| You need | Use |
|---|---|
| Designed text and shapes that move: titles, lower thirds, stat cards, callouts, checklists, captions, overlays with alpha | `offload_compose_video` (this skill) |
| One still vector graphic: a diagram, an icon, a badge | `offload_generate_svg` |
| Pictures or footage that are generated, not designed | `offload_generate_image`, `offload_generate_video` |
| Music or a voice track | `offload_generate_audio` |
| Cut, join, convert, extract frames, put audio on a silent file, probe | `offload_media` (it has no overlay op: lay an alpha overlay over footage with ffmpeg's `overlay` filter, see the ffmpeg skill, or in an editor) |
| Words with timings from speech | `offload_transcribe`, then `render/captions-groups.mjs` (below) |

The compose lane makes silent video. Audio never goes inside a template: render silent, then mux with
`offload_media` (`mux_audio`), or leave the audio on the footage the overlay sits over.

## Catalog

The vetted templates on this checkout. `offload_status` (`media.routes.compose_video`) lists what a given
machine has installed. Every template is 1920 x 1080 at 30 fps and takes a `duration` variable in seconds.

| template | draws | alpha | default | variables you usually set |
|---|---|---|---|---|
| `title-card` | full-frame title with a bar, headline and subtitle | no | 5 s | `title`, `subtitle`, `accent` |
| `lower-third` | broadcast lower third: stripe, name, role | yes | 6 s | `name`, `role`, `accent` |
| `stat-card` | full-frame figure with a kicker, an emphasised caption and a source line | no | 6 s | `stat`, `kicker`, `label_pre`, `label_em`, `label_post`, `source` |
| `section-title` | full-frame kicker and headline with one emphasised phrase | no | 6 s | `kicker`, `headline_pre`, `headline_em`, `headline_post` |
| `callout-label` | callout card at the right edge: term, rule, detail | yes | 6 s | `term`, `detail`, `accent` |
| `checklist-card` | title over up to four ticked rows | no | 7 s | `title_pre`, `title_em`, `title_post`, `item1` to `item4` |
| `captions-bar` | one caption group at a time in a bar near the bottom edge | yes | 8 s | `words_json`, `size`, `duration` |

Colors are variables too (`background`, `accent`, `text_color`, `panel`, `bar`). Read the declared
variables, their limits and defaults in the template's own `README.md` before calling: a value that is
not declared, is mistyped or is over its `maxLength` defers `BAD_INPUT`. A `*_pre` / `*_em` / `*_post`
group is one sentence split around an emphasised phrase; any part may be an empty string.

## Calling it

```json
{"template": "stat-card", "variables": {"kicker": "Launch week", "stat": "3.4x", "label_pre": "faster search across", "label_em": "every workspace", "label_post": "since the update", "duration": 6}, "snapshots": [3, 5.6]}
```

- **Opaque templates** render to `mp4` (the default). **Alpha templates** (`lower-third`, `callout-label`,
  `captions-bar`) render to `webm` (VP9 with alpha, for the web and ffmpeg) or `mov` (ProRes 4444, for an
  editor). `mp4` flattens the alpha to white and is only a preview.
- `snapshots` lists seconds to save as PNG frames next to the output. Always ask for two: one in the middle
  of the hold and one just before the exit starts.
- The returned fields are **measured** by ffprobe on the file, not echoed from the request: `width`,
  `height`, `fps`, `duration_sec`, `codec`, `has_alpha`, `has_audio`, `render_ms`, and `lint` and `check`
  results. Any failure is `deferred: true` with a typed class: `BAD_INPUT`, `LINT_ERRORS`, `CHECK_FAILED`,
  `RENDER_FAILED`, `BROWSER_MISSING`, `FFMPEG_MISSING`, `CLI_MISSING`, `SPAWN_EBUSY`, `DISK_HEADROOM`,
  `TIMEOUT`, or `compose_busy` when another composition holds the slot. Fix the cause the class names;
  never retry blindly and never reach for a cloud tool.
- On the CLI: `local-offload compose-video --template stat-card --variables-file v.json --snapshots 3,5.6`.

## Verify what you made (the loop)

A successful return is not a look at the output. Every time:

1. **Read the returned fields** against what you asked for: size, fps, length, `has_alpha` true for `webm`
   and `mov`, `has_audio` false, `lint.errors` 0, `check.ok` true.
2. **Open the snapshot PNGs** with an image reader and look: the text is the text you sent, nothing is
   clipped at the frame edge, nothing overlaps, the exit has started in the late frame.
3. **Alpha overlays: composite before you judge.** Lay the snapshot over flat grey and over white
   (`ffmpeg -f lavfi -i color=c=gray:s=1920x1080 -i snap.png -filter_complex "[0][1]overlay=format=auto" -frames:v 1 out.png`)
   and read both. A translucent panel looks different on each, and transparency you cannot see is not
   proof of transparency: measure the alpha plane (`alphaextract,signalstats`) or read the returned
   `has_alpha`.
4. **Push the copy to its limit once.** Render the longest text each variable allows in a wide letter
   (`WWWW`) and look for clipping. The templates are built to wrap, not to clip, but a new template is not
   known to be until you have looked.
5. `offload_video_watch` is a detector for coarse problems, not a substitute for looking, and small vision
   models misread overlay text: read the frames yourself.
6. **Determinism.** For a template you authored, render it at 1, 2, 4 and 6 workers and at `auto` twice, and
   compare the decoded frames (`ffmpeg -i out.mp4 -f framemd5 -`; decode a `webm` with `-c:v libvpx-vp9` so the
   alpha is compared too). They must be identical: a frame is a pure function of time.

## Captions from a transcript

`offload_transcribe` writes `<base>.segments.json`. The pure helper groups its words and packs them for the
`captions-bar` template:

```
node render/captions-groups.mjs <base>.segments.json --pace conversational --out chunks.json
```

`--pace` is `punchy` (up to 3 words a group), `conversational` (5) or `calm` (6). A group also ends at a
sentence end, at a pause of 0.15 s or more, or when it would pass 42 characters. The output is a list of
chunks, each with `words_json`, `duration_sec` and `offset_sec`. Render one clip per chunk
(`{"template": "captions-bar", "format": "webm", "workers": 1, "variables": {"words_json": "...", "duration": ...}}`)
and lay it over the footage at `offset_sec`. The first chunk keeps absolute time; later chunks start at 0. The
template takes a chunk of at most 16,000 characters and 600 s (about ten minutes of speech). The overlay is silent.

**Render every chunk with `workers: 1`; the helper cuts at 300 s by default, and `--chunk-sec` sets another cap up
to the template's 600 s.** At `auto`, HyperFrames stores every frame on disk (8.3 MB at 1080p) and defers
`DISK_HEADROOM` on a long clip: a 300 s chunk would store about 75 GB. At one worker a 300 s chunk took 17 minutes,
so a 600 s chunk would pass the default 30-minute timeout (`TIMEOUT`). That is why the default is 300 s: a
`--chunk-sec` above it needs `compose_timeout_sec` raised.

## Authoring or changing a template

Copy the closest template in `render/compose-templates/` and keep its contract (the README there is the
full text and `render/compose-hyperframes.test.mjs` enforces the part a machine can):

- **Offline.** No URL, no remote script, no CDN font. Fonts come from `_shared/fonts` with `@font-face`; a
  new face needs its `woff2`, its licence text and its sha256 in `_shared/README.md`.
- **Deterministic.** No `Math.random`, `Date`, `performance.now`, timers or `crypto`. Motion is CSS
  `@keyframes` (or, for a long list of timed items, one element driven from the `hf-seek` event, as
  `captions-bar` does). **Do not scale an element above its resting size in an animation:** on two of the
  ported cards it made frames depend on the render worker count. Entrances that scale up to 1, opacity and
  translation are safe. It is a rule of thumb, so measure what you write (the determinism step above).
- **Declared variables.** Everything a caller may change is in the root's `data-composition-variables`
  with a type, a label, a default and, for strings, an explicit `maxLength`. Text arrives through
  `data-var-text` (text only); emphasis is a `_pre` / `_em` / `_post` split, never markup.
- **A duration parameter** named in `template.json` (`duration_variable`); the runner rewrites the root's
  `data-duration` from it.
- **Design for wrap, not clip**, with `overflow-wrap: anywhere` and `text-wrap: balance`, and check the
  longest text with a wide letter.
- A `README.md` with a variable table, a call example, the changes from any source, and a **measured** render
  (lint, check, ffprobe fields, `render_ms`, and the worker-count comparison).
- Third-party design or code keeps its licence texts verbatim under `render/compose-templates/_third_party/`
  and each README states where it was adapted from. The authoring rules above follow HyperFrames' own
  guidance (Apache-2.0); this text is written for the harness lane and reuses none of it verbatim.

## Failure modes

| You see | It means | Do |
|---|---|---|
| `BAD_INPUT` naming a variable | not declared, wrong type, or over `maxLength` | read the template README's table and fix the value |
| `LINT_ERRORS` or `CHECK_FAILED` | the composition failed its own gates (runtime error, layout, contrast) | read `check.findings`; for a template you edited, fix the page |
| `RENDER_FAILED` after a good `check` | the output failed the ffprobe gate | read the detail; wrong size or missing alpha is a template or format bug |
| `DISK_HEADROOM` | not enough free space for the frames (above one worker every frame is stored, 8.3 MB at 1080p) | render the clip with `workers: 1`, which streams the frames; else point `compose_cache_dir` at a larger drive |
| `TIMEOUT` | `compose_timeout_sec` elapsed | shorten the clip, or split it (captions: a smaller `--chunk-sec` than the 300 s default), then retry |
| `compose_busy` | another composition holds the slot | wait and call again |
| a template is missing from `offload_status` | this machine has an older checkout | the templates ship with the harness release; deploy it |
