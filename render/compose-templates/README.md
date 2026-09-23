# Vetted composition templates

The templates `offload_compose_video` (CLI `compose-video`, fleet task `compose-video`) renders by
name. Each is a HyperFrames composition this repo reviewed line by line; callers fill its declared
variables and never ship code. The fleet door accepts **only** these templates, because a
composition is code that HyperFrames' Chrome runs without a sandbox (ADR 0059).

| template | what it draws | alpha | default length |
|---|---|---|---|
| [`title-card`](title-card/README.md) | full-frame card: accent bar, headline, subtitle, slow background glow | no | 5 s |
| [`lower-third`](lower-third/README.md) | broadcast lower third: accent stripe, name, role, over transparency | yes (webm / mov) | 6 s |

## The contract every template keeps

`render/compose-hyperframes.test.mjs` ("shipped templates") enforces the first four.

1. **Offline.** No URL anywhere: no CDN script, no remote image, no Google Fonts. Every font family
   the page uses is declared with `@font-face` from [`_shared/fonts`](_shared/README.md). A family
   the page does not declare makes the HyperFrames compiler request `fonts.googleapis.com` at render
   time, with the page's character set in the query string.
2. **Deterministic.** No `Math.random`, `Date`, `performance.now` or `crypto`. HyperFrames seeds
   randomness only in distributed mode, so a local render that reads it is not reproducible.
   Animation is CSS `@keyframes`, which HyperFrames' CSS adapter seeks frame by frame.
3. **Declared variables.** Everything a caller may change sits in the root's
   `data-composition-variables` with a type (`string`, `color`, `number`, `boolean`, `enum`) and
   a default. The runner merges the caller's values into those defaults in its copy of the page, so
   `lint` and `check` judge the real text and colors. An undeclared or mistyped value defers
   `BAD_INPUT`. Text reaches the page through `data-var-text` (text only, never markup); colors reach
   it as CSS custom properties.
4. **A duration parameter.** `template.json` names the variable (`duration_variable`). HyperFrames
   reads a composition's total length from source, never from a variable, so the runner rewrites the
   root's `data-duration` in its copy. The same variable times the exit animation through
   `var(--duration)`.
5. **A README** stating the variables, the formats that make sense and a measured render.

## Adding a template

Copy the closest template and keep the root attributes (`data-composition-id`, `data-width`,
`data-height`, `data-fps`, `data-duration`, `data-no-timeline`, `data-composition-variables`).
Render it through the runner with `--keep-work`. `lint` must report 0 errors and `check` must pass.
Look at the frames: extract two with ffmpeg and read them. Render it twice and compare
`ffmpeg -f framemd5`. Only then add a README and open the PR.
