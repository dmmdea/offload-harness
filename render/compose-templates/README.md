# Vetted composition templates

The templates `offload_compose_video` (CLI `compose-video`, fleet task `compose-video`) renders by
name. Each is a HyperFrames composition this repo reviewed line by line; callers fill its declared
variables and never ship code. The fleet door accepts **only** these templates, because a
composition is code that HyperFrames' Chrome runs without a sandbox (ADR 0059).

| template | what it draws | alpha | default length |
|---|---|---|---|
| [`title-card`](title-card/README.md) | full-frame card: accent bar, headline, subtitle, slow background glow | no | 5 s |
| [`lower-third`](lower-third/README.md) | broadcast lower third: accent stripe, name, role, over transparency | yes (webm / mov) | 6 s |
| [`stat-card`](stat-card/README.md) | full-frame figure: kicker, giant gradient number, a caption with one emphasised phrase, a source line | no | 6 s |
| [`section-title`](section-title/README.md) | full-frame section marker: kicker and a large headline with one emphasised phrase | no | 6 s |
| [`callout-label`](callout-label/README.md) | callout card at the right edge: term, accent rule, detail, over transparency | yes (webm / mov) | 6 s |
| [`checklist-card`](checklist-card/README.md) | title over up to four rows, each with a tick that draws itself | no | 7 s |
| [`captions-bar`](captions-bar/README.md) | caption track: one short group of words at a time in a bar near the bottom edge, over transparency | yes (webm / mov) | 8 s (1 to 600 s) |

**Where they come from.** `title-card`, `lower-third` and `captions-bar` were written for this harness.
`stat-card`, `section-title`, `callout-label` and `checklist-card` are adapted from card designs in a
community teaching kit, pinned at one commit and used under the kit's own licences. Those licence texts are
kept verbatim, with a record of what was taken and what was not, in
[`_third_party/hyperframes-student-kit/`](_third_party/hyperframes-student-kit/PROVENANCE.md). Nothing from
the kit is installed or run: the four templates are rewrites in CSS, and each README lists what changed.

## The contract every template keeps

`render/compose-hyperframes.test.mjs` ("shipped templates" and "kit ports") enforces what a machine can.

1. **Offline.** No URL anywhere: no CDN script, no remote image, no Google Fonts. Every font family
   the page uses is declared with `@font-face` from [`_shared/fonts`](_shared/README.md). A family
   the page does not declare makes the HyperFrames compiler request `fonts.googleapis.com` at render
   time, with the page's character set in the query string.
2. **Deterministic.** No `Math.random`, `Date`, `performance.now` or `crypto`. HyperFrames seeds
   randomness only in distributed mode, so a local render that reads it is not reproducible.
   Animation is CSS `@keyframes`, which HyperFrames' CSS adapter seeks frame by frame (item 6 adds a
   rule for what may move, and item 7 the one template that does not use keyframes).
3. **Declared variables.** Everything a caller may change sits in the root's
   `data-composition-variables` with a type (`string`, `color`, `number`, `boolean`, `enum`) and
   a default. The runner merges the caller's values into those defaults in its copy of the page, so
   `lint` and `check` judge the real text and colors. An undeclared or mistyped value defers
   `BAD_INPUT`. A value is data whatever characters it carries: the runner writes the declaration
   through a replacer function, so `$1`, `$&` and the like in a caption or a figure stay text (a
   test pins it). Text reaches the page through `data-var-text` (text only, never markup); colors
   reach it as CSS custom properties. A string variable states its `maxLength`: the runner's silent cap when
   none is declared is 200.
4. **A duration parameter.** `template.json` names the variable (`duration_variable`). HyperFrames
   reads a composition's total length from source, never from a variable, so the runner rewrites the
   root's `data-duration` in its copy. The same variable times the exit animation through
   `var(--duration)`.
5. **A README** stating the variables, the formats that make sense and a measured render.
6. **The same frames at any worker count.** `--workers` splits a render's frames across Chrome
   instances, so a frame has to be a pure function of time. Two of the kit templates were not, on their
   first render: a halo breathing out to `scale(1.06)` and a ring popping to `scale(1.1)` gave 80 of
   180 frames (1 worker against 4) and 172 of 210 frames (1 worker against `auto`) that differed, by a
   few pixels of gradient, and two runs at 4 workers disagreed with each other. The same cards
   animating opacity instead gave 0 differing frames at 1, 4 and 4 workers; the same cards with the
   blur removed and the scale kept still differed, so blur was not the cause. An entrance that scales up
   *to* 1, from 0.6 to 0.94, was stable in every run. So a new template does not scale above 1 in an
   animation (opacity and translation are safe), and the test reads the kit ports and `captions-bar`
   for it. That is a rule of thumb, not a proven law: `title-card` predates it, drifts a large glow out
   to `scale(1.12)` while it translates, and gave 15 of 15 identical pairs of decoded frames (quality
   `high`, 150 frames). What every template has to show is the measurement itself: render it at 1, 2, 4
   and 6 workers and at `auto` twice, then compare decoded frames
   (`ffmpeg -i out.mp4 -f framemd5 -`). For `webm`, decode with
   `-c:v libvpx-vp9` so the alpha is in the comparison; two `webm` files can differ in container bytes
   and still hold identical frames.
7. **One template does not use keyframes.** `captions-bar` draws one caption group at a time from a list
   of hundreds. One animated element per group was measured first: the same 8 s clip rendered in 23 s
   with 3 groups in the page, 24 s with 20, 25 s with 40, 24 s with 80 and 34 s with 150, and a
   374-group, 12.9 KB list had written about 290 of its 450 frames after ten minutes, when it was stopped.
   Driven from the `hf-seek` event HyperFrames dispatches with the time on every seek, the same
   374-group list renders those 450 frames in 39.6 s (draft quality, clean CPU), and the frame is still a
   pure function of the time.
   The larger sizes overlapped other renders on the reference box: read the trend, not the last digit.

## Adding a template

Copy the closest template and keep the root attributes (`data-composition-id`, `data-width`,
`data-height`, `data-fps`, `data-duration`, `data-no-timeline`, `data-composition-variables`).
Render it through the runner with `--keep-work`. `lint` must report 0 errors and `check` must pass.
Look at the frames: extract two with ffmpeg and read them, and render the longest text each
variable allows in a wide letter to see that nothing clips. For an alpha template, composite the frames
over flat grey and over white and measure the alpha plane. Render it at 1, 2, 4 and 6 workers and at
`auto` twice and compare `ffmpeg -f framemd5`: every pair must match. Time it against the 14 to 22 s
per 150 frames baseline in `docs/systems/media-generation.md`. Only then add a README and open the PR.

Third-party design or code keeps its licence texts verbatim under `_third_party/<source>/`, with a
`PROVENANCE.md`, and each README that adapts it says where from. A folder starting with `_` is never a
template.
